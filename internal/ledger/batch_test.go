package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// batchCmd links transfer commands into one batch.
func batchCmd(cs ...Command) Command {
	b := &BatchSpec{}
	for _, c := range cs {
		b.Transfers = append(b.Transfers, *c.CreateTransfer)
	}
	return Command{CreateBatch: b}
}

// fingerprint describes everything a command can change: every account's
// totals, version, rows, status and limits, every transfer, the journal head,
// open reservations and their timeouts, and the secondary indexes.
func fingerprint(t testing.TB, l *Ledger) string {
	t.Helper()
	l.mu.RLock()
	defer l.mu.RUnlock()
	var b strings.Builder
	order := append([]string(nil), l.st.order...)
	sort.Strings(order) // a snapshot restores accounts in ID order
	for _, id := range order {
		a := l.st.accounts[id]
		fmt.Fprintf(&b, "%s %+v v%d rows%d %v %v\n", id, a.bal, a.version, len(a.rows), a.life, a.limits)
	}
	ids := make([]string, 0, len(l.st.transfers))
	for id := range l.st.transfers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p, _ := json.Marshal(l.st.transfers[id])
		b.Write(p)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "head %d open %d expiries %v ix %d/%d\n", l.st.head(), l.st.open, liveExpiries(l.st),
		l.st.ix.accounts.Len(), l.st.ix.transfers.Len())
	if v := checkIndexes(l.st); len(v) > 0 {
		t.Fatalf("indexes disagree with a full scan: %v", v)
	}
	return b.String()
}

// logRecords returns the decoded records of a ledger's log.
func logRecords(t testing.TB, dir string) []record {
	t.Helper()
	var out []record
	if _, err := wal.Read(filepath.Join(dir, walDir), func(_ uint64, p []byte) error {
		r, err := decodeRecord(p)
		out = append(out, r)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func itemOf(err error) int {
	var le *Error
	if errors.As(err, &le) && le.Item != nil {
		return *le.Item
	}
	return -1
}

// A batch is one log record holding every transfer, its entries are
// consecutive, later transfers see the effects of earlier ones, and replay
// from the log alone or from a snapshot rebuilds exactly the same state.
func TestBatchCommitsAsOneRecord(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	before := len(logRecords(t, e.dir))
	head := e.l.Head()

	// alice only has 100: paying bob 120 works because the first transfer
	// lands first, and the hold uses what is left.
	out := e.do(batchCmd(
		move(dr("equity", 50), cr("alice", 50)),
		move(dr("alice", 120), cr("bob", 120)),
		hold(60, dr("alice", 30), cr("merchant", 30)),
	))
	if len(out.Transfers) != 3 || out.Transfer != nil {
		t.Fatalf("outcome %+v", out)
	}
	for i, tr := range out.Transfers {
		if len(tr.Entries) != 1 || tr.Entries[0].Seq != head+uint64(i)+1 {
			t.Fatalf("transfer %d entries %+v, want seq %d", i, tr.Entries, head+uint64(i)+1)
		}
		if got, err := e.l.Transfer(tr.ID); err != nil || got != tr {
			t.Fatalf("transfer %s reads back as %+v, %v", tr.ID, got, err)
		}
	}
	if out.Transfers[2].State != StatePending || e.l.Stats().OpenPending != 1 {
		t.Fatalf("the hold is %s, open %d", out.Transfers[2].State, e.l.Stats().OpenPending)
	}
	e.wantBalance("alice", 30, -30, 0)
	e.wantBalance("bob", 120, 0, 120)
	e.wantBalance("merchant", 0, 30, 0)
	e.invariants()

	recs := logRecords(t, e.dir)
	if len(recs) != before+1 {
		t.Fatalf("the batch wrote %d records, want 1", len(recs)-before)
	}
	r := recs[len(recs)-1]
	if r.Type != recCmd || r.Cmd.CreateBatch == nil || r.Seq != head+3 || r.ID != "" || len(r.IDs) != 3 {
		t.Fatalf("batch record %+v", r)
	}
	for i, id := range r.IDs {
		if id != out.Transfers[i].ID {
			t.Fatalf("record ids %v, transfers %v", r.IDs, out.Transfers)
		}
	}

	e.l.Close()
	full := openAt(t, copyDir(t, e.dir, false), e.clock, nil)
	fromSnap := openAt(t, copyDir(t, e.dir, true), e.clock, nil)
	sameState(t, "batch", full, fromSnap)
	e.open()
	sameState(t, "batch after restart", full, e.l)
	// The hold's timeout came back with it.
	e.clock.Advance(61 * time.Second)
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	e.wantBalance("alice", 30, 0, 30)
	e.invariants()
}

// A batch that fails anywhere changes nothing: not the accounts the
// transfers before the failure moved, not the transfers they created, not
// open reservations or their timeouts, not the indexes, and nothing is
// logged without a key. The error names the transfer that failed.
func TestBatchIsAllOrNothing(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	e.do(freezeCmd("merchant", "review"))
	v := e.account("alice").Version
	ver := func(n uint64) *uint64 { return &n }
	withPre := func(c Command, account string, version uint64) Command {
		c.CreateTransfer.Preconditions = []Precondition{{Account: account, Version: ver(version)}}
		return c
	}
	cases := []struct {
		name string
		cmd  Command
		code Code
		item int
	}{
		{"first transfer, unknown account", batchCmd(move(dr("alice", 1), cr("nobody", 1)), move(dr("equity", 1), cr("bob", 1))), CodeNotFound, 0},
		{"after money moved in", batchCmd(move(dr("equity", 10), cr("alice", 10)), move(dr("alice", 200), cr("bob", 200))), CodeLimitExceeded, 1},
		{"after a hold", batchCmd(hold(5, dr("alice", 10), cr("bob", 10)), move(dr("equity", 1), cr("merchant", 1))), CodeAccountFrozen, 1},
		{"after a hold and a move", batchCmd(move(dr("equity", 5), cr("bob", 5)), hold(5, dr("alice", 90), cr("bob", 90)), move(dr("alice", 20), cr("bob", 20))), CodeLimitExceeded, 2},
		{"currency", batchCmd(move(dr("equity", 1), cr("alice", 1)), Command{CreateTransfer: &TransferSpec{Currency: "EUR", Postings: []PostingSpec{dr("equity", 1), cr("bob", 1)}}}), CodeCurrencyMismatch, 1},
		{"stale precondition after an earlier transfer", batchCmd(move(dr("equity", 1), cr("alice", 1)), withPre(move(dr("alice", 1), cr("bob", 1)), "alice", v)), CodePrecondition, 1},
		{"overflow", batchCmd(move(dr("equity", 1), cr("bob", 1)), move(dr("equity", math.MaxInt64), cr("bob", math.MaxInt64))), CodeOverflow, 1},
	}
	for _, c := range cases {
		want := fingerprint(t, e.l)
		records := len(logRecords(t, e.dir))
		e.l.mu.RLock()
		heapLen := len(e.l.st.expiries)
		e.l.mu.RUnlock()
		_, err := e.l.Do(context.Background(), c.cmd, nil)
		if codeOf(err) != c.code || itemOf(err) != c.item {
			t.Fatalf("%s: err %v (item %d), want %s at %d", c.name, err, itemOf(err), c.code, c.item)
		}
		if !strings.HasPrefix(err.(*Error).Message, fmt.Sprintf("transfers[%d]: ", c.item)) {
			t.Fatalf("%s: message %q does not name the transfer", c.name, err.(*Error).Message)
		}
		if got := fingerprint(t, e.l); got != want {
			t.Fatalf("%s: the failed batch changed the ledger:\n%s\nwant\n%s", c.name, got, want)
		}
		e.l.mu.RLock()
		grown := len(e.l.st.expiries) - heapLen
		e.l.mu.RUnlock()
		if grown != 0 {
			t.Fatalf("%s: the failed batch left %d timeouts scheduled", c.name, grown)
		}
		if n := len(logRecords(t, e.dir)); n != records {
			t.Fatalf("%s: an unkeyed refusal wrote %d records", c.name, n-records)
		}
	}
	// The holds that were rolled back left no timeout behind.
	e.clock.Advance(10 * time.Second)
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("expired %d rolled-back holds, %v", n, err)
	}
	// A precondition on the version an earlier transfer in the batch
	// produced passes.
	out := e.do(batchCmd(move(dr("equity", 1), cr("alice", 1)), withPre(move(dr("alice", 1), cr("bob", 1)), "alice", v+1)))
	if len(out.Transfers) != 2 || e.account("alice").Version != v+2 {
		t.Fatalf("linked precondition: %+v", out)
	}
	e.invariants()
	full := fingerprint(t, e.l)
	e.restart()
	if got := fingerprint(t, e.l); got != full {
		t.Fatalf("restart changed the state:\n%s\nwas\n%s", got, full)
	}
}

// Generated IDs that collide inside a batch are refused, and the transfer
// created before the collision is taken back out.
func TestBatchRollsBackCreatedTransfers(t *testing.T) {
	e := newEnv(t, false)
	e.l.Close()
	var mu sync.Mutex
	n := 0
	e.opts.NewID = func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 3 {
			return "tr_dup"
		}
		if n == 4 {
			return "tr_dup" // the batch's second transfer collides with its first
		}
		return fmt.Sprintf("tr_%d", n)
	}
	e.open()
	e.standard()
	e.do(move(dr("equity", 50), cr("alice", 50))) // tr_1
	e.do(move(dr("equity", 50), cr("bob", 50)))   // tr_2
	want := fingerprint(t, e.l)
	_, err := e.l.Do(context.Background(), batchCmd(hold(5, dr("alice", 10), cr("bob", 10)), move(dr("alice", 1), cr("bob", 1))), nil)
	if codeOf(err) != CodeAlreadyExists || itemOf(err) != 1 {
		t.Fatalf("collision: %v", err)
	}
	if _, err := e.l.Transfer("tr_dup"); codeOf(err) != CodeNotFound {
		t.Fatalf("the rolled-back transfer is readable: %v", err)
	}
	if got := fingerprint(t, e.l); got != want {
		t.Fatalf("collision changed the ledger:\n%s\nwant\n%s", got, want)
	}
	e.invariants()
}

// A failed batch is taken back out of a group commit it shares with other
// commands, which commit as if it had never been there.
func TestBatchRollbackInsideAGroupCommit(t *testing.T) {
	e := newEnv(t, true)
	e.l.Close()
	g := &gate{ch: make(chan struct{})}
	e.opts.WrapFile = func(f wal.File) wal.File { return gatedFile{f, g} }
	e.open()
	e.standard()
	e.do(move(dr("equity", 100), cr("alice", 100)))
	ctx := context.Background()
	syncs := e.l.log.Syncs() // read before the gate closes: Syncs waits for an fsync in progress
	g.armed.Store(true)
	blocker := make(chan error, 1)
	go func() { _, err := e.l.Do(ctx, move(dr("equity", 1), cr("merchant", 1)), nil); blocker <- err }()
	time.Sleep(20 * time.Millisecond)
	for len(e.l.queue) > 0 {
		time.Sleep(time.Millisecond) // the writer takes the blocker and waits in its fsync
	}

	cmds := []Command{
		move(dr("equity", 5), cr("alice", 5)),
		batchCmd(move(dr("alice", 1), cr("bob", 1)), hold(30, dr("alice", 2), cr("bob", 2)), move(dr("bob", 1_000_000), cr("merchant", 1_000_000))),
		move(dr("equity", 2), cr("bob", 2)),
		batchCmd(move(dr("equity", 7), cr("merchant", 7)), move(dr("merchant", 7), cr("alice", 7))),
		batchCmd(hold(30, dr("alice", 3), cr("bob", 3))),
	}
	type res struct {
		out Outcome
		err error
	}
	results := make([]chan res, len(cmds))
	for i, c := range cmds {
		results[i] = make(chan res, 1)
		go func() { o, err := e.l.Do(ctx, c, nil); results[i] <- res{o, err} }()
	}
	for len(e.l.queue) < len(cmds) {
		time.Sleep(time.Millisecond)
	}
	g.armed.Store(false)
	close(g.ch)
	if err := <-blocker; err != nil {
		t.Fatal(err)
	}
	for i := range cmds {
		r := <-results[i]
		switch i {
		case 1:
			if codeOf(r.err) != CodeLimitExceeded || itemOf(r.err) != 2 {
				t.Fatalf("failing batch: %v", r.err)
			}
		default:
			if r.err != nil {
				t.Fatalf("command %d: %v", i, r.err)
			}
		}
	}
	if got := e.l.log.Syncs() - syncs; got != 2 {
		t.Fatalf("%d fsyncs after the blocker's; the queued commands should share one", got)
	}
	e.wantBalance("alice", 112, -3, 109)
	e.wantBalance("bob", 2, 3, 2)
	e.wantBalance("merchant", 1, 0, 1)
	if s := e.l.Stats(); s.OpenPending != 1 {
		t.Fatalf("open pending %d", s.OpenPending)
	}
	e.invariants()
	e.l.Close()
	full := openAt(t, copyDir(t, e.dir, false), e.clock, nil)
	e.opts.WrapFile = nil
	e.open()
	sameState(t, "group commit", full, e.l)
}

// A keyed batch replays its transfers, and a keyed refusal replays the same
// error naming the same transfer, from memory, from the log and from a
// snapshot, even once the batch would succeed.
func TestBatchIdempotency(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	ctx := context.Background()
	ok := batchCmd(move(dr("equity", 40), cr("alice", 40)), move(dr("alice", 15), cr("bob", 15)))
	bad := batchCmd(move(dr("equity", 1), cr("merchant", 1)), move(dr("bob", 500), cr("merchant", 500)))
	first, err := e.l.Do(ctx, ok, key("s", "pay", "ok"))
	if err != nil || first.Replayed {
		t.Fatal(err)
	}
	_, err = e.l.Do(ctx, bad, key("s", "bad", "bad"))
	if codeOf(err) != CodeLimitExceeded || itemOf(err) != 1 {
		t.Fatalf("refusal: %v", err)
	}
	refusal := *err.(*Error)
	check := func(stage string, l *Ledger) {
		t.Helper()
		out, err := l.Do(ctx, ok, key("s", "pay", "ok"))
		if err != nil || !out.Replayed || len(out.Transfers) != 2 {
			t.Fatalf("%s: replay %+v, %v", stage, out, err)
		}
		for i, tr := range out.Transfers {
			if tr.ID != first.Transfers[i].ID || tr.Postings[0].Posted != first.Transfers[i].Postings[0].Posted {
				t.Fatalf("%s: replayed transfer %d is %+v, want %+v", stage, i, tr, first.Transfers[i])
			}
		}
		_, err = l.Do(ctx, bad, key("s", "bad", "bad"))
		var le *Error
		if !errors.As(err, &le) || le.Code != refusal.Code || le.Message != refusal.Message || le.Item == nil || *le.Item != 1 {
			t.Fatalf("%s: replayed refusal %v", stage, err)
		}
		if _, err := l.Do(ctx, ok, key("s", "pay", "other")); codeOf(err) != CodeKeyReused {
			t.Fatalf("%s: reused key: %v", stage, err)
		}
		if p, _, _ := (&env{t: t, l: l}).balance("alice"); p != 25 {
			t.Fatalf("%s: alice %d; the batch ran again", stage, p)
		}
	}
	check("memory", e.l)
	e.do(move(dr("equity", 1000), cr("bob", 1000))) // bad would now succeed
	check("memory, bob funded", e.l)
	e.l.Close()
	check("log", openAt(t, copyDir(t, e.dir, false), e.clock, nil))
	check("snapshot", openAt(t, copyDir(t, e.dir, true), e.clock, nil))
}

func TestBatchValidation(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	head := e.l.Head()
	many := make([]Command, MaxBatchTransfers+1)
	for i := range many {
		many[i] = move(dr("equity", 1), cr("alice", 1))
	}
	badItem := batchCmd(move(dr("equity", 1), cr("alice", 1)), move(dr("equity", 1), cr("alice", 1)), move(dr("equity", 0), cr("alice", 0)))
	both := batchCmd(move(dr("equity", 1), cr("alice", 1)))
	both.CreateTransfer = move(dr("equity", 1), cr("alice", 1)).CreateTransfer
	for name, c := range map[string]Command{
		"empty":        {CreateBatch: &BatchSpec{}},
		"too many":     batchCmd(many...),
		"bad item":     badItem,
		"two commands": both,
		"bad timeout":  batchCmd(move(dr("equity", 1), cr("alice", 1)), hold(int64(MaxTimeout/time.Second)+1, dr("equity", 1), cr("alice", 1))),
	} {
		_, err := e.l.Do(context.Background(), c, key("s", name, name))
		if codeOf(err) != CodeInvalid {
			t.Fatalf("%s: %v", name, err)
		}
		switch name {
		case "bad item":
			if itemOf(err) != 2 || !strings.HasPrefix(err.Error(), "invalid_argument: transfers[2]: ") {
				t.Fatalf("bad item: %v (item %d)", err, itemOf(err))
			}
		case "bad timeout":
			if itemOf(err) != 1 {
				t.Fatalf("bad timeout: item %d", itemOf(err))
			}
		default:
			if itemOf(err) != -1 {
				t.Fatalf("%s names item %d", name, itemOf(err))
			}
		}
	}
	// Exactly the maximum is accepted.
	out := e.do(batchCmd(many[:MaxBatchTransfers]...))
	if len(out.Transfers) != MaxBatchTransfers || e.l.Head() != head+MaxBatchTransfers {
		t.Fatalf("%d transfers, head %d", len(out.Transfers), e.l.Head())
	}
	// Defaults are filled in for every transfer, as for a single one.
	out = e.do(batchCmd(hold(0, dr("equity", 1), cr("alice", 1)), hold(0, dr("equity", 1), cr("alice", 1))))
	for _, tr := range out.Transfers {
		if got := tr.ExpiresAt.Sub(tr.CreatedAt); got != DefaultTimeout {
			t.Fatalf("default timeout %v", got)
		}
	}
	// Replay refuses a record whose IDs do not fit its command.
	tx := newTxn(e.l.st, e.clock.Now())
	if _, err := tx.exec(&Command{CreateTransfer: move(dr("equity", 1), cr("alice", 1)).CreateTransfer}, "tr_x", []string{"tr_y"}); err == nil || err.Code != CodeInvalid {
		t.Fatalf("ids for a transfer: %v", err)
	}
	if _, err := tx.exec(&both, "", []string{"tr_y", "tr_z"}); err == nil || err.Code != CodeInvalid {
		t.Fatalf("too many ids: %v", err)
	}
	e.invariants()
}

// A batch near the size limit is refused before it runs, and any batch that
// passes validation fits in one log record even with the longest IDs, key
// and scope the ledger allows.
func TestBatchSizeIsBoundedByTheRecord(t *testing.T) {
	meta := map[string]string{}
	for i := 0; i < MaxMetadataKeys; i++ {
		meta[fmt.Sprintf("%s%02d", strings.Repeat("k", MaxMetadataKey-2), i)] = strings.Repeat("v", MaxMetadataValue)
	}
	acct := func(i int) string { return strings.Repeat("a", MaxIDLength-2) + fmt.Sprintf("%02d", i) }
	spec := func(n int) Command {
		b := &BatchSpec{Transfers: make([]TransferSpec, n)}
		for i := range b.Transfers {
			b.Transfers[i] = TransferSpec{Currency: "USD", Postings: []PostingSpec{dr(acct(0), math.MaxInt64), cr(acct(1), math.MaxInt64)}, Metadata: meta}
		}
		return Command{CreateBatch: b}
	}
	worst := func(c *Command) int {
		ids := make([]string, len(c.CreateBatch.Transfers))
		for i := range ids {
			ids[i] = strings.Repeat("t", MaxIDLength)
		}
		p, err := encodeRecord(record{Type: recCmd, Time: time.Now(), Seq: math.MaxUint64, Cmd: c, IDs: ids,
			Idem: &idemField{Scope: strings.Repeat("<", MaxScope), Key: strings.Repeat("<", MaxIdempotencyKey), Hash: strings.Repeat("f", 64)}})
		if err != nil {
			t.Fatal(err)
		}
		return len(p)
	}
	// Grow until validation refuses; the last accepted batch must fit.
	lo, hi := 1, 4000
	for lo < hi {
		mid := (lo + hi + 1) / 2
		c := spec(mid)
		if c.Validate() == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	accepted := spec(lo)
	if size := worst(&accepted); size > wal.MaxRecord || size < wal.MaxRecord-64<<10 {
		t.Fatalf("largest accepted batch (%d transfers) takes %d bytes; the record limit is %d", lo, size, wal.MaxRecord)
	}
	refused := spec(lo + 1)
	err := refused.Validate()
	if err == nil || err.Code != CodeInvalid || err.Item != nil || !strings.Contains(err.Message, "split it") {
		t.Fatalf("oversized batch: %v", err)
	}
}

// The largest batch, 8,190 transfers in one record of more than a megabyte,
// is written with one fsync, read back by a restart from the log alone, and
// by verify.
func TestLargestBatchSurvivesRestart(t *testing.T) {
	e := newEnv(t, true)
	e.l.Close()
	e.opts.SnapshotEvery = -1
	e.open()
	long := func(s string) string { return s + strings.Repeat("x", MaxIDLength-len(s)) }
	e.do(openAcct(long("from"), "USD", Debit))
	e.do(openAcct(long("to"), "USD", Credit))
	cs := make([]Command, MaxBatchTransfers)
	for i := range cs {
		cs[i] = move(dr(long("from"), int64(i+1)), cr(long("to"), int64(i+1)))
		cs[i].CreateTransfer.Reference = fmt.Sprintf("payout-%05d", i)
	}
	syncs := e.l.log.Syncs()
	out, err := e.l.Do(context.Background(), batchCmd(cs...), key("s", "big", "big"))
	if err != nil || len(out.Transfers) != MaxBatchTransfers {
		t.Fatal(err)
	}
	if got := e.l.log.Syncs() - syncs; got != 1 {
		t.Fatalf("%d fsyncs for one batch", got)
	}
	recs := 0
	maxLen := 0
	wal.Read(filepath.Join(e.dir, walDir), func(_ uint64, p []byte) error {
		recs++
		maxLen = max(maxLen, len(p))
		return nil
	})
	if maxLen <= 1<<20 {
		t.Fatalf("the batch record is %d bytes; this test needs one longer than the reader's buffer", maxLen)
	}
	want := fingerprint(t, e.l)
	e.restart()
	if got := fingerprint(t, e.l); got != want {
		t.Fatal("restart from the log changed the state")
	}
	if a := e.account(long("to")); a.Balance.Posted != MaxBatchTransfers*(MaxBatchTransfers+1)/2 || a.Version != MaxBatchTransfers {
		t.Fatalf("to: %+v", a)
	}
	again, err := e.l.Do(context.Background(), batchCmd(cs...), key("s", "big", "big"))
	if err != nil || !again.Replayed || again.Transfers[MaxBatchTransfers-1].ID != out.Transfers[MaxBatchTransfers-1].ID {
		t.Fatalf("replay after restart: %v", err)
	}
	e.l.Close()
	rep, err := Verify(e.dir)
	if err != nil || rep.Status != StatusSound {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	e.open()
}

// Version 3 snapshots, written before batches, still load.
func TestVersionThreeSnapshotsLoad(t *testing.T) {
	old := snapWriteVersion
	snapWriteVersion = 3
	t.Cleanup(func() { snapWriteVersion = old })
	dir, clock := writeSmallSnapshot(t, 3)
	snapWriteVersion = old
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "version 3", fromSnap, full)
}

// Inside one txn, as in one group commit, a failed batch leaves nothing a
// later command could see: not the transfers it created (so their IDs are
// free), not the account changes, entries, rows, timeouts or open count.
// Committing gives exactly what running only the other commands gives.
func TestRollbackInsideOneTxn(t *testing.T) {
	run := func(withFailures bool) *env {
		e := newEnv(t, false)
		e.standard()
		e.do(move(dr("equity", 100), cr("alice", 100)))
		e.do(hold(600, dr("alice", 10), cr("merchant", 10))) // tr_000002, pending
		tx := newTxn(e.l.st, e.clock.Now())
		mustExec := func(c Command, id string, ids ...string) {
			t.Helper()
			if _, err := tx.exec(&c, id, ids); err != nil {
				t.Fatalf("%+v: %v", c, err)
			}
		}
		mustExec(move(dr("equity", 1), cr("bob", 1)), "x1")
		if withFailures {
			c := batchCmd(hold(5, dr("alice", 20), cr("bob", 20)), move(dr("equity", 3), cr("alice", 3)), move(dr("bob", 999), cr("alice", 999)))
			if _, err := tx.exec(&c, "", []string{"x2", "x3", "x4"}); err == nil || *err.Item != 2 {
				t.Fatalf("batch: %v", err)
			}
			if _, ok := tx.transfer("x2"); ok {
				t.Fatal("a rolled-back transfer is still visible in the txn")
			}
			// A batch refused at its last transfer after touching accounts
			// the first batch also touched.
			c = batchCmd(move(dr("alice", 1), cr("merchant", 1)), move(dr("alice", 1), cr("nobody", 1)))
			if _, err := tx.exec(&c, "", []string{"x5", "x6"}); err == nil || *err.Item != 1 {
				t.Fatalf("second batch: %v", err)
			}
		}
		// The IDs the failed batch used are free again.
		mustExec(move(dr("equity", 2), cr("alice", 2)), "x2")
		mustExec(Command{CreateBatch: batchCmd(move(dr("alice", 5), cr("bob", 5)), hold(60, dr("alice", 4), cr("bob", 4))).CreateBatch}, "", "x3", "x5")
		mustExec(postCmd("tr_000002", 10), "")
		e.l.mu.Lock()
		tx.commit()
		e.l.mu.Unlock()
		return e
	}
	clean, rolled := run(false), run(true)
	if a, b := fingerprint(t, clean.l), fingerprint(t, rolled.l); a != b {
		t.Fatalf("a txn with failed batches committed\n%s\nwithout them\n%s", b, a)
	}
	rolled.invariants()
}
