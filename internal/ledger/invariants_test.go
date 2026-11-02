package ledger

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withAuditChunk makes the checker drop the read lock every n items.
func withAuditChunk(t testing.TB, n int) {
	old := auditChunk
	auditChunk = n
	t.Cleanup(func() { auditChunk = old })
}

// TestAuditChecksAFrozenView commits transfers, reservations, captures,
// voids, reversals and new accounts at every point where the checker holds no
// lock. The checker must report the head it froze and no violation: if it
// read any account or transfer as it is after a later commit, the folds
// would not match.
func TestAuditChecksAFrozenView(t *testing.T) {
	withAuditChunk(t, 2)
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 10_000), cr("alice", 10_000)))
	e.do(move(dr("bank", 10_000), cr("bob", 10_000)))
	var holds []string
	for i := 0; i < 6; i++ {
		holds = append(holds, e.do(hold(600, dr("alice", 10), cr("merchant", 10))).Transfer.ID)
	}
	var paid []string
	for i := 0; i < 6; i++ {
		paid = append(paid, e.do(move(dr("bob", 100), cr("merchant", 100))).Transfer.ID)
	}
	head := e.l.Head()

	steps := map[string]int{}
	n := 0
	var kept, replaced int
	e.l.auditStep = func(step string) {
		steps[step]++
		n++
		e.do(move(dr("bank", 1), cr("alice", 1)))
		e.do(hold(600, dr("bob", 1), cr("alice", 1)))
		switch {
		case n%3 == 0 && len(holds) > 0:
			e.do(postCmd(holds[0], 5))
			holds = holds[1:]
		case n%3 == 1 && len(holds) > 0:
			e.do(voidCmd(holds[0]))
			holds = holds[1:]
		case len(paid) > 0:
			e.do(reverseCmd(paid[0], 40))
			paid = paid[1:]
		}
		id := fmt.Sprintf("late%d", n)
		e.do(openAcct(id, "USD", Credit))
		e.do(move(dr("bank", 3), cr(id, 3)))
		e.l.mu.RLock()
		if v := e.l.st.view; v != nil {
			kept, replaced = len(v.accounts), len(v.replaced)
		}
		e.l.mu.RUnlock()
	}
	got, v := e.l.Audit()
	e.l.auditStep = nil
	if len(v) > 0 {
		t.Fatalf("violations while commits ran during the check: %v", v)
	}
	if got != head {
		t.Fatalf("checked head %d, want the head when the check started, %d", got, head)
	}
	if steps["frozen"] != 1 || steps["accounts"] < 3 || steps["transfers"] < 5 {
		t.Fatalf("checker paused at %v; want several account and transfer chunks", steps)
	}
	if kept == 0 || replaced == 0 {
		t.Fatalf("commits during the check saved %d accounts and %d transfers; the test did not exercise the view", kept, replaced)
	}
	if e.l.Head() <= head {
		t.Fatal("no commit ran during the check")
	}
	e.l.mu.RLock()
	view := e.l.st.view
	e.l.mu.RUnlock()
	if view != nil {
		t.Fatal("the view outlived the check")
	}
	e.invariants() // and the state the commits produced is sound
}

// TestAuditFindsDamageBehindSavedCopies damages the state, then changes the
// damaged account and transfer during the check. The checker reads the saved
// copies, which still hold the damage, and must report it.
func TestAuditFindsDamageBehindSavedCopies(t *testing.T) {
	withAuditChunk(t, 1)
	e := newEnv(t, false)
	e.standard()
	e.do(move(dr("bank", 500), cr("alice", 500)))
	paid := e.do(move(dr("alice", 100), cr("merchant", 100))).Transfer.ID
	pending := e.do(hold(600, dr("alice", 50), cr("bob", 50))).Transfer.ID

	e.l.mu.Lock()
	e.l.st.accounts["alice"].bal.CreditsPosted++
	c := e.l.st.transfers[paid].clone()
	c.Postings[0].Reversed = 7
	e.l.st.transfers[paid] = c
	e.l.mu.Unlock()

	once := false
	e.l.auditStep = func(string) {
		if once {
			return
		}
		once = true
		// Every later entry for alice is fine on its own; only the copy
		// saved before it shows the damage.
		e.do(move(dr("bank", 1), cr("alice", 1)))
		e.do(voidCmd(pending))
		e.do(reverseCmd(paid, 10))
	}
	_, v := e.l.Audit()
	e.l.auditStep = nil
	all := strings.Join(v, "\n")
	for _, want := range []string{"account alice balance", "reversals add up"} {
		if !strings.Contains(all, want) {
			t.Errorf("violations %v, want one mentioning %q", v, want)
		}
	}
}

// TestAuditReportsJournalTransferMismatch covers the checks that tie the
// transfer map to the journal.
func TestAuditReportsJournalTransferMismatch(t *testing.T) {
	cases := map[string]struct {
		damage func(st *state)
		want   string
	}{
		"missing": {func(st *state) {
			for id := range st.transfers {
				delete(st.transfers, id)
				return
			}
		}, "which does not exist"},
		"extra": {func(st *state) {
			st.transfers["tr_ghost"] = &Transfer{ID: "tr_ghost", State: StatePosted, Entries: []EntryRef{{Seq: 1}}}
		}, "but the journal creates"},
		"twice": {func(st *state) {
			st.entries[len(st.entries)-1].TransferID = st.entries[len(st.entries)-2].TransferID
		}, "more than one journal entry"},
		"orphan reversal": {func(st *state) {
			for id, tr := range st.transfers {
				if tr.Reverses != "" {
					c := tr.clone()
					c.Reverses = "tr_nowhere"
					st.transfers[id] = c
				}
			}
		}, "points at missing transfer"},
		"unlisted account": {func(st *state) {
			st.order = slices.DeleteFunc(st.order, func(id string) bool { return id == "merchant" })
		}, "does not exist or holds another currency"},
		"listed but missing": {func(st *state) { delete(st.accounts, "bob") }, "listed but missing"},
	}
	for name, c := range cases {
		e := newEnv(t, false)
		e.standard()
		e.do(move(dr("bank", 500), cr("alice", 500)))
		id := e.do(move(dr("alice", 100), cr("merchant", 100))).Transfer.ID
		e.do(reverseCmd(id, 30))
		e.do(move(dr("bank", 5), cr("bob", 5)))
		e.do(move(dr("bank", 5), cr("bob", 5)))
		e.invariants()
		e.l.mu.Lock()
		c.damage(e.l.st)
		e.l.mu.Unlock()
		if v := e.l.CheckInvariants(); !strings.Contains(strings.Join(v, "\n"), c.want) {
			t.Errorf("%s: violations %v, want one mentioning %q", name, v, c.want)
		}
	}
}

// TestAuditUnderConcurrentLoad runs checks back to back while clients run a
// mixed workload. Every check must be clean, and run with -race this also
// shows the view needs no more locking than it takes.
func TestAuditUnderConcurrentLoad(t *testing.T) {
	withAuditChunk(t, 3)
	e := newEnv(t, false)
	e.standard()
	for _, w := range []string{"alice", "bob", "merchant"} {
		e.do(move(dr("bank", 1_000_000), cr(w, 1_000_000)))
	}
	ctx := context.Background()
	stop := make(chan struct{})
	var ops atomic.Int64
	var wg sync.WaitGroup
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			wallets := []string{"alice", "bob", "merchant"}
			var held, paid []string
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				from, to := wallets[rng.Intn(3)], wallets[rng.Intn(3)]
				if from == to {
					to = "equity"
				}
				var cmd Command
				switch r := rng.Intn(10); {
				case r < 3:
					amt := 1 + rng.Int63n(5)
					cmd = move(dr(from, amt), cr(to, amt))
				case r < 5:
					cmd = hold(600, dr(from, 2), cr(to, 2))
				case r < 7 && len(held) > 0:
					cmd, held = postCmd(held[0], 1+rng.Int63n(2)), held[1:]
				case r < 8 && len(held) > 0:
					cmd, held = voidCmd(held[0]), held[1:]
				case r < 9 && len(paid) > 0:
					cmd, paid = reverseCmd(paid[0], 1), paid[1:]
				default:
					cmd = openAcct(fmt.Sprintf("acct-%d-%d", seed, i), "USD", Credit)
				}
				out, err := e.l.Do(ctx, cmd, nil)
				if err != nil {
					continue // limits refuse some; the checker still has to agree
				}
				ops.Add(1)
				if tr := out.Transfer; tr != nil && cmd.CreateTransfer != nil {
					if tr.State == StatePending {
						held = append(held, tr.ID)
					} else {
						paid = append(paid, tr.ID)
					}
				}
			}
		}(int64(c))
	}
	var last uint64
	checks := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) || checks < 5 {
		head, v := e.l.Audit()
		if len(v) > 0 {
			close(stop)
			wg.Wait()
			t.Fatalf("check %d at head %d: %v", checks, head, v)
		}
		if head < last {
			t.Fatalf("check went back from head %d to %d", last, head)
		}
		last = head
		checks++
	}
	close(stop)
	wg.Wait()
	if ops.Load() < 100 {
		t.Fatalf("only %d commands committed during %d checks", ops.Load(), checks)
	}
	e.invariants()
	t.Logf("%d checks clean during %d commands", checks, ops.Load())
}

// bulkLoad fills an in-memory ledger with about n journal entries spread
// over n/100 accounts: single-phase transfers, with every 16th a reservation
// captured by the next entry and every 64th a partial reversal. It applies
// commands straight to the state, without the log, while the writer is idle.
func bulkLoad(tb testing.TB, l *Ledger, n int) {
	tb.Helper()
	accounts := max(n/100, 16)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	t := newTxn(l.st, now)
	apply := func(c Command, id string) Outcome {
		out, err := t.apply(&c, id)
		if err != nil {
			tb.Fatalf("bulk load: %v", err)
		}
		return out
	}
	flush := func() {
		t.commit()
		t = newTxn(l.st, now)
	}
	for i := 0; i < accounts; i++ {
		apply(openAcct(fmt.Sprintf("acct%07d", i), "USD", Credit), "")
	}
	flush()
	var last string
	for i := 0; l.st.head()+uint64(len(t.entries)) < uint64(n); i++ {
		from, to := fmt.Sprintf("acct%07d", i%accounts), fmt.Sprintf("acct%07d", (i*7+1)%accounts)
		if from == to {
			continue
		}
		id := "bulk" + strconv.Itoa(i) // apart from the IDs seqIDs hands out
		switch {
		case i%64 == 63 && last != "":
			apply(reverseCmd(last, 1), id)
		case i%16 == 15:
			p := apply(hold(3600, dr(from, 2), cr(to, 2)), id).Transfer.ID
			apply(postCmd(p, 1), "")
		default:
			last = apply(move(dr(from, 3), cr(to, 3)), id).Transfer.ID
		}
		if len(t.entries) >= 4096 {
			flush()
		}
	}
	flush()
}

type latencies []time.Duration

func (d latencies) pct(p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

func (d latencies) String() string {
	return fmt.Sprintf("n=%d p50=%v p99=%v max=%v", len(d), d.pct(0.5), d.pct(0.99), d.pct(1))
}

// TestCommitLatencyStaysFlatDuringAudit times single transfers while the
// checker scans a large ledger. Before the checker used a frozen view, it
// held the read lock for the whole scan and a commit waited for all of it;
// now a commit waits for one chunk at most. LEDGERD_AUDIT_ENTRIES sets the
// ledger size (default 200,000; the roadmap figure is 10,000,000, which
// needs about 8 GB of memory).
func TestCommitLatencyStaysFlatDuringAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a large ledger")
	}
	n := 200_000
	if s := os.Getenv("LEDGERD_AUDIT_ENTRIES"); s != "" {
		var err error
		if n, err = strconv.Atoi(s); err != nil {
			t.Fatal(err)
		}
	}
	l, err := Open(Options{SweepInterval: -1, NewID: seqIDs()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	start := time.Now()
	bulkLoad(t, l, n)
	runtime.GC()
	t.Logf("loaded %d entries, %d accounts, %d transfers in %v", l.Head(), len(l.st.accounts), len(l.st.transfers), time.Since(start).Round(time.Millisecond))

	ctx := context.Background()
	transfer := func(i int) time.Duration {
		from, to := fmt.Sprintf("acct%07d", i%16), fmt.Sprintf("acct%07d", 16+i%16)
		start := time.Now()
		if _, err := l.Do(ctx, move(dr(from, 1), cr(to, 1)), nil); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	var idle latencies
	for i := 0; i < 2000; i++ {
		idle = append(idle, transfer(i))
	}

	done := make(chan struct{})
	var audit time.Duration
	var violations []string
	frozen := make(chan struct{})
	l.auditStep = func(step string) {
		if step == "frozen" {
			close(frozen)
		}
	}
	go func() {
		defer close(done)
		start := time.Now()
		_, violations = l.Audit()
		audit = time.Since(start)
	}()
	<-frozen
	var during latencies
	for i := 0; ; i++ {
		select {
		case <-done:
		default:
			during = append(during, transfer(i))
			continue
		}
		break
	}
	l.auditStep = nil
	if len(violations) > 0 {
		t.Fatalf("violations: %v", violations)
	}
	t.Logf("check took %v", audit.Round(time.Millisecond))
	t.Logf("commit latency idle:         %v", idle)
	t.Logf("commit latency during check: %v", during)
	// A commit that had to wait for the scan would take as long as the
	// rest of it; the first one would take nearly all of it.
	if len(during) < 20 {
		t.Fatalf("only %d commits finished during a %v check", len(during), audit)
	}
	if worst := during.pct(1); worst > audit/4 {
		t.Fatalf("a commit took %v during a %v check: it waited for the scan", worst, audit)
	}
	if p99 := during.pct(0.99); p99 > audit/20 {
		t.Fatalf("p99 commit latency %v during a %v check", p99, audit)
	}
}
