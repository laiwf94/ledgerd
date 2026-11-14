package ledger

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// copySnapshots copies every snapshot file (finished or not) from src to dst.
// Files that disappear while being copied were pruned and are skipped.
func copySnapshots(t testing.TB, src, dst string) {
	t.Helper()
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), snapPrefix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// copyDir copies the log and, when withSnapshots is set, the snapshots.
func copyDir(t testing.TB, src string, withSnapshots bool) string {
	t.Helper()
	dst := t.TempDir()
	copyLog(t, src, dst)
	if withSnapshots {
		copySnapshots(t, src, dst)
	}
	return dst
}

// copyLog copies the log's manifest and segments from data directory src to
// dst. Nothing may be appending to the log.
func copyLog(t testing.TB, src, dst string) {
	t.Helper()
	from, to := filepath.Join(src, walDir), filepath.Join(dst, walDir)
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := wal.SegmentPaths(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(paths, filepath.Join(from, "MANIFEST")) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, filepath.Base(p)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// activeSegment returns the path of the segment taking appends in the log
// of data directory dir.
func activeSegment(t testing.TB, dir string) string {
	t.Helper()
	paths, err := wal.SegmentPaths(filepath.Join(dir, walDir))
	if err != nil {
		t.Fatal(err)
	}
	return paths[len(paths)-1]
}

// logBytes returns the contents of a log that is still a single segment.
func logBytes(t testing.TB, dir string) []byte {
	t.Helper()
	paths, err := wal.SegmentPaths(filepath.Join(dir, walDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("the log has %d segments, want 1", len(paths))
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func snapshotFiles(t testing.TB, dir string) []uint64 {
	t.Helper()
	nums, err := listSnapshots(wal.OSFS{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	return nums
}

// sameState fails unless a and b hold identical committed state: every
// account with its totals, version and history rows, every transfer, the
// journal, the open reservations and their timeouts, and every kept
// idempotency key with its stored result.
func sameState(t testing.TB, what string, a, b *Ledger) {
	t.Helper()
	a.mu.RLock()
	defer a.mu.RUnlock()
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !reflect.DeepEqual(a.st.accounts, b.st.accounts) {
		t.Fatalf("%s: accounts differ", what)
	}
	if !reflect.DeepEqual(a.st.transfers, b.st.transfers) {
		t.Fatalf("%s: transfers differ", what)
	}
	if !reflect.DeepEqual(a.st.entries, b.st.entries) {
		t.Fatalf("%s: journals differ (heads %d and %d)", what, a.st.head(), b.st.head())
	}
	if a.st.open != b.st.open {
		t.Fatalf("%s: open pending %d and %d", what, a.st.open, b.st.open)
	}
	if d := indexDiff(a.st.ix, b.st.ix); d != "" {
		t.Fatalf("%s: secondary indexes differ: %s", what, d)
	}
	for _, l := range []*Ledger{a, b} {
		if v := checkIndexes(l.st); len(v) > 0 {
			t.Fatalf("%s: indexes disagree with a full scan: %v", what, v)
		}
	}
	if ea, eb := liveExpiries(a.st), liveExpiries(b.st); !reflect.DeepEqual(ea, eb) {
		t.Fatalf("%s: expiry schedules differ: %v and %v", what, ea, eb)
	}
	a.idemMu.Lock()
	defer a.idemMu.Unlock()
	b.idemMu.Lock()
	defer b.idemMu.Unlock()
	if !reflect.DeepEqual(a.idem, b.idem) {
		t.Fatalf("%s: idempotency keys differ: %d and %d", what, len(a.idem), len(b.idem))
	}
}

func openAt(t testing.TB, dir string, clock *fakeClock, extra func(*Options)) *Ledger {
	t.Helper()
	o := Options{Dir: dir, Clock: clock.Now, SweepInterval: -1, SnapshotEvery: -1}
	if extra != nil {
		extra(&o)
	}
	l, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// randomWorkload drives a ledger with every kind of command, with and
// without idempotency keys, letting reservations expire as time passes.
func randomWorkload(t testing.TB, l *Ledger, clock *fakeClock, r *rand.Rand, ops int) {
	t.Helper()
	ctx := context.Background()
	m := newModel(clock.Now())
	g := newGen(r, m)
	for _, c := range modelSetup() {
		l.Do(ctx, c, nil)
		m.exec(&c, "")
	}
	for i := 0; i < ops; i++ {
		if r.Intn(10) == 0 {
			clock.Advance(time.Duration(r.Intn(3)) * time.Second)
			m.now = clock.Now()
			l.ExpireDue(ctx)
			for _, id := range m.due() {
				m.expire(id)
			}
		}
		o := g.next()
		c := o.command()
		var idem *Idem
		if r.Intn(2) == 0 {
			idem = &Idem{Scope: "s", Key: o.key, Hash: o.hash}
		}
		out, err := l.Do(ctx, c, idem)
		if err == nil && !out.Replayed {
			id, ids := "", []string(nil)
			if out.Transfer != nil && (c.CreateTransfer != nil || c.Reverse != nil) {
				id = out.Transfer.ID
			}
			for _, tr := range out.Transfers {
				ids = append(ids, tr.ID)
			}
			m.exec(&c, id, ids...)
		}
		g.done = append(g.done, o)
	}
}

func TestSnapshotPlusTailEqualsFullReplay(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			clock := newClock()
			dir := t.TempDir()
			every := 1 + r.Intn(25)
			l := openAt(t, dir, clock, func(o *Options) {
				o.SnapshotEvery, o.NewID, o.MaxBatch = every, seqIDs(), 1+r.Intn(8)
			})
			for round := 0; round < 8; round++ {
				randomWorkload(t, l, clock, r, 40+r.Intn(80))
				// Without a clean shutdown: the newest snapshot plus the log
				// after it must rebuild what the whole log does.
				crashed := copyDir(t, dir, true)
				if len(snapshotFiles(t, crashed)) == 0 && round > 0 {
					t.Fatalf("round %d: no snapshot after %d-record intervals", round, every)
				}
				fromSnap := openAt(t, crashed, clock, nil)
				full := openAt(t, copyDir(t, dir, false), clock, nil)
				sameState(t, fmt.Sprintf("round %d", round), fromSnap, full)
				sameState(t, fmt.Sprintf("round %d live", round), l, full)
				fromSnap.Close()
				full.Close()
			}
			// A clean shutdown snapshots everything; the next start replays
			// no command at all.
			l.Close()
			nums := snapshotFiles(t, dir)
			if len(nums) == 0 || len(nums) > snapKeep {
				t.Fatalf("snapshots on disk after close: %v", nums)
			}
			var logs bytes.Buffer
			again := openAt(t, dir, clock, func(o *Options) { o.Logger = slog.New(slog.NewJSONHandler(&logs, nil)) })
			if !strings.Contains(logs.String(), fmt.Sprintf(`"snapshot":%d`, nums[0])) {
				t.Fatalf("did not load snapshot %d: %s", nums[0], logs.String())
			}
			sameState(t, "after clean shutdown", again, openAt(t, copyDir(t, dir, false), clock, nil))
		})
	}
}

// Recovery does not re-run the commands a snapshot already holds: a log
// whose prefix could not be replayed still opens from the snapshot, and the
// tail after it is replayed in full.
func TestSnapshotSkipsTheReplayedPrefix(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	e.do(move(dr("bank", 10), cr("alice", 10)))
	e.l.Close() // snapshot 1
	e.open()
	e.do(move(dr("bank", 5), cr("alice", 5)))
	e.l.Close() // snapshot 2
	if nums := snapshotFiles(t, e.dir); fmt.Sprint(nums) != "[2 1]" {
		t.Fatalf("snapshots %v", nums)
	}
	e.open()
	e.do(move(dr("bank", 1), cr("alice", 1))) // the tail after snapshot 2
	crashed := copyDir(t, e.dir, true)

	// Swap the first transfer, long since summarised, for a well-formed
	// record that would fail if it were run.
	path := activeSegment(t, crashed)
	data, _ := os.ReadFile(path)
	i := bytes.Index(data, []byte(`"create_transfer"`))
	start := bytes.LastIndexByte(data[:i], '\n') + 1
	end := start + bytes.IndexByte(data[start:], '\n') + 1
	swapped := wal.Encode(nil, []byte(`{"t":"cmd","cmd":{"void":{"transfer_id":"tr_x"}}}`))
	data = append(append(append([]byte(nil), data[:start]...), swapped...), data[end:]...)
	os.WriteFile(path, data, 0o600)

	l := openAt(t, crashed, e.clock, nil)
	if a, _ := l.Account("alice", 0); a.Balance.Posted != 16 {
		t.Fatalf("alice posted %d, want 16", a.Balance.Posted)
	}
	if _, err := Open(Options{Dir: copyDir(t, crashed, false), SweepInterval: -1}); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("full replay of the swapped log: err = %v", err)
	}
}

func TestSnapshotsArePruned(t *testing.T) {
	e := newEnv(t, true)
	e.l.Close()
	e.opts.SnapshotEvery = 3
	e.open()
	e.standard()
	for i := 0; i < 40; i++ {
		e.do(move(dr("bank", 1), cr("alice", 1)))
	}
	e.l.Close()
	nums := snapshotFiles(t, e.dir)
	if len(nums) != snapKeep || nums[0] <= snapKeep {
		t.Fatalf("snapshots on disk: %v", nums)
	}
	e.open()
	e.wantBalance("alice", 40, 0, 40)
}

// writeSmallSnapshot builds a ledger with every kind of state and returns its
// directory after a clean shutdown, with exactly one snapshot.
// writeSmallSnapshot writes a ledger using what snapshot format ver can
// hold: account status from version 2, limit changes from version 3.
func writeSmallSnapshot(t *testing.T, ver int) (dir string, clock *fakeClock) {
	t.Helper()
	e := newEnv(t, true)
	ctx := context.Background()
	e.do(openAcct("bank", "USD", Debit))
	e.do(Command{CreateAccount: &AccountSpec{ID: "alice", Currency: "USD", NormalBalance: Credit, NonNegative: true,
		Metadata: map[string]string{"tier": "gold", "region": "eu"}}})
	e.do(openAcct("shop", "USD", Credit))
	e.do(openAcct("jpy", "JPY", Debit))
	e.l.Do(ctx, move(dr("bank", 1000), cr("alice", 1000)), key("k1", "fund", "fund"))
	e.l.Do(ctx, move(dr("alice", 9000), cr("shop", 9000)), key("k1", "over", "over"))
	out, err := e.l.Do(ctx, Command{CreateTransfer: &TransferSpec{Currency: "USD", Pending: true, TimeoutSeconds: 60, Reference: "ord-1",
		Metadata: map[string]string{"order": "1"}, Postings: []PostingSpec{dr("alice", 300), cr("shop", 300)}}}, key("k2", "hold", "hold"))
	if err != nil {
		t.Fatal(err)
	}
	e.do(postCmd(out.Transfer.ID, 200))
	if _, err := e.l.Do(ctx, reverseCmd(out.Transfer.ID, 50), key("", "refund", "refund")); err != nil {
		t.Fatal(err)
	}
	e.do(hold(5, dr("alice", 10), cr("shop", 10)))
	e.do(openAcct("late", "EUR", Credit))
	e.l.Do(ctx, openAcct("late2", "EUR", Credit), key("", "acct", "acct"))
	if ver >= 2 {
		// Every status, with reasons, keyed results and a keyed refusal.
		if _, err := e.l.Do(ctx, freezeCmd("alice", "review"), key("k3", "freeze", "freeze")); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Second)
		e.do(freezeCmd("shop", ""))
		e.do(unfreezeCmd("shop", "done"))
		if _, err := e.l.Do(ctx, closeCmd("late", "unused"), key("k3", "close", "close")); err != nil {
			t.Fatal(err)
		}
		e.l.Do(ctx, closeCmd("shop", ""), key("k3", "close-shop", "close-shop"))
	}
	if ver >= 3 {
		// Limit changes of both kinds, a removal, a keyed result and a
		// keyed refusal on the closed account.
		if _, err := e.l.Do(ctx, limitCmd("alice", LimitOverdraft, 200, "line"), key("k4", "limit", "limit")); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Second)
		e.do(limitCmd("bank", LimitCredit, 5000, ""))
		e.do(move(dr("bank", 7), cr("shop", 7)))
		e.do(noLimitCmd("bank", LimitCredit, "lifted"))
		e.l.Do(ctx, limitCmd("late", LimitCredit, 1, ""), key("k4", "late", "late"))
	}
	if ver >= 4 {
		// A keyed batch with a hold in it, and a keyed batch refused at its
		// second transfer because alice is frozen.
		if _, err := e.l.Do(ctx, batchCmd(move(dr("bank", 3), cr("shop", 3)), hold(60, dr("bank", 4), cr("shop", 4))), key("k5", "batch", "batch")); err != nil {
			t.Fatal(err)
		}
		e.l.Do(ctx, batchCmd(move(dr("bank", 1), cr("shop", 1)), move(dr("alice", 1), cr("shop", 1))), key("k5", "batch-frozen", "batch-frozen"))
	}
	if ver >= 5 {
		// A keyed balancing transfer cut down to what the sweep account
		// holds, and a balancing hold cut down to the pool's credit limit.
		e.do(openAcct("sweep", "USD", Credit, "nonneg"))
		e.do(openAcct("pool", "USD", Credit))
		e.do(move(dr("bank", 40), cr("sweep", 40)))
		if _, err := e.l.Do(ctx, balancing(true, false, dr("sweep", 100), cr("pool", 100)), key("k6", "sweep", "sweep")); err != nil {
			t.Fatal(err)
		}
		e.do(limitCmd("pool", LimitCredit, 60, ""))
		c := balancing(false, true, dr("bank", 100), cr("pool", 100))
		c.CreateTransfer.Pending, c.CreateTransfer.TimeoutSeconds = true, 60
		e.do(c)
	}
	if ver >= 6 {
		// A keyed increase and two extensions of one hold, so the journal
		// has entries with no lines and with a timeout.
		h := e.do(hold(60, dr("bank", 20), cr("shop", 20))).Transfer
		if _, err := e.l.Do(ctx, increaseCmd(h.ID, 5), key("k7", "grow", "grow")); err != nil {
			t.Fatal(err)
		}
		e.do(extendCmd(h.ID, 90))
		e.clock.Advance(time.Second)
		if _, err := e.l.Do(ctx, extendCmd(h.ID, 120), key("k7", "extend", "extend")); err != nil {
			t.Fatal(err)
		}
	}
	if ver >= 7 {
		// Exchange accounts, a keyed conversion with a fee, a conversion
		// hold posted in part, a partial reversal at the rate and a keyed
		// refusal off the rate.
		e.do(exchangeAcct("fx_usd", "USD"))
		e.do(exchangeAcct("fx_eur", "EUR"))
		e.do(openAcct("wallet_eur", "EUR", Credit, "nonneg"))
		c, err := e.l.Do(ctx, convert("0.9234", dr("bank", 1010), cr("fx_usd", 1000), cr("sweep", 10),
			eur(dr("fx_eur", 923)), eur(cr("wallet_eur", 923))), key("k8", "convert", "convert"))
		if err != nil {
			t.Fatal(err)
		}
		h := e.do(convertHold("0.9234", 60, dr("bank", 2000), cr("fx_usd", 2000), eur(dr("fx_eur", 1847)), eur(cr("wallet_eur", 1847)))).Transfer
		e.do(postCmd(h.ID, 0, PartialAmount{"bank", 1000}, PartialAmount{"fx_usd", 1000}, PartialAmount{"fx_eur", 923}, PartialAmount{"wallet_eur", 923}))
		e.do(reverseCmd(c.Transfer.ID, 0, PartialAmount{"bank", 500}, PartialAmount{"fx_usd", 500}, PartialAmount{"fx_eur", 462}, PartialAmount{"wallet_eur", 462}))
		e.l.Do(ctx, convert("0.9234", dr("bank", 100), cr("fx_usd", 100), eur(dr("fx_eur", 1)), eur(cr("wallet_eur", 1))), key("k8", "off", "off"))
	}
	e.l.Close()
	if nums := snapshotFiles(t, e.dir); len(nums) != 1 {
		t.Fatalf("snapshots %v", nums)
	}
	return e.dir, e.clock
}

func TestSnapshotRoundTripsEveryField(t *testing.T) {
	dir, clock := writeSmallSnapshot(t, snapVersion)
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "small", fromSnap, full)
	// Idempotent replays answer from the snapshot exactly as from the log.
	ctx := context.Background()
	for _, l := range []*Ledger{fromSnap, full} {
		out, err := l.Do(ctx, move(dr("alice", 9000), cr("shop", 9000)), key("k1", "over", "over"))
		if codeOf(err) != CodeLimitExceeded || !out.Replayed {
			t.Fatalf("replayed rejection: %v %+v", err, out)
		}
		out, err = l.Do(ctx, openAcct("late2", "EUR", Credit), key("", "acct", "acct"))
		if err != nil || !out.Replayed || out.Account == nil || out.Account.ID != "late2" {
			t.Fatalf("replayed account: %v %+v", err, out)
		}
		out, err = l.Do(ctx, freezeCmd("alice", "review"), key("k3", "freeze", "freeze"))
		if err != nil || !out.Replayed || out.Account.Status != StatusFrozen || out.Account.StatusReason != "review" || out.Account.StatusChangedAt.IsZero() {
			t.Fatalf("replayed freeze: %v %+v", err, out.Account)
		}
		out, err = l.Do(ctx, closeCmd("shop", ""), key("k3", "close-shop", "close-shop"))
		if codeOf(err) != CodePrecondition || !out.Replayed {
			t.Fatalf("replayed refused close: %v %+v", err, out)
		}
		for id, want := range map[string]AccountStatus{"alice": StatusFrozen, "shop": StatusOpen, "late": StatusClosed, "bank": StatusOpen} {
			if a, err := l.Account(id, 0); err != nil || a.Status != want {
				t.Fatalf("%s: %+v %v", id, a, err)
			}
		}
		if a, _ := l.Account("shop", 0); a.StatusReason != "done" {
			t.Fatalf("shop reason %q", a.StatusReason)
		}
		out, err = l.Do(ctx, limitCmd("alice", LimitOverdraft, 200, "line"), key("k4", "limit", "limit"))
		if err != nil || !out.Replayed || out.Account.Limits.Overdraft == nil || *out.Account.Limits.Overdraft != 200 || out.Account.Limits.EffectiveSeq == 0 {
			t.Fatalf("replayed limit: %v %+v", err, out.Account)
		}
		if out, err := l.Do(ctx, limitCmd("late", LimitCredit, 1, ""), key("k4", "late", "late")); codeOf(err) != CodeAccountClosed || !out.Replayed {
			t.Fatalf("replayed refused limit: %v", err)
		}
		if _, changes, err := l.LimitHistory("bank"); err != nil || len(changes) != 2 || changes[1].Amount != nil || changes[1].Reason != "lifted" {
			t.Fatalf("bank limit history: %+v %v", changes, err)
		}
		out, err = l.Do(ctx, batchCmd(move(dr("bank", 3), cr("shop", 3)), hold(60, dr("bank", 4), cr("shop", 4))), key("k5", "batch", "batch"))
		if err != nil || !out.Replayed || len(out.Transfers) != 2 || out.Transfers[1].State != StatePending || out.Transfers[0].Postings[0].Posted != 3 {
			t.Fatalf("replayed batch: %v %+v", err, out)
		}
		_, err = l.Do(ctx, batchCmd(move(dr("bank", 1), cr("shop", 1)), move(dr("alice", 1), cr("shop", 1))), key("k5", "batch-frozen", "batch-frozen"))
		if le, _ := err.(*Error); le == nil || le.Code != CodeAccountFrozen || le.Item == nil || *le.Item != 1 {
			t.Fatalf("replayed refused batch: %v", err)
		}
		out, err = l.Do(ctx, balancing(true, false, dr("sweep", 100), cr("pool", 100)), key("k6", "sweep", "sweep"))
		if err != nil || !out.Replayed || !reflect.DeepEqual(out.Transfer.Balancing, &Balancing{Debit: true, Requested: 100, Amount: 40}) {
			t.Fatalf("replayed balancing transfer: %v %+v", err, out.Transfer)
		}
		hid := extendedHold(t, l)
		out, err = l.Do(ctx, increaseCmd(hid, 5), key("k7", "grow", "grow"))
		if err != nil || !out.Replayed || out.Transfer.Postings[0].Amount != 25 || len(out.Transfer.Entries) != 2 {
			t.Fatalf("replayed increase: %v %+v", err, out.Transfer)
		}
		out, err = l.Do(ctx, extendCmd(hid, 120), key("k7", "extend", "extend"))
		if err != nil || !out.Replayed || len(out.Transfer.Entries) != 4 || out.Transfer.Entries[3].Kind != KindExtendPending {
			t.Fatalf("replayed extension: %v %+v", err, out.Transfer)
		}
	}
	for _, l := range []*Ledger{fromSnap, full} {
		out, err := l.Do(ctx, convert("0.9234", dr("bank", 1010), cr("fx_usd", 1000), cr("sweep", 10),
			eur(dr("fx_eur", 923)), eur(cr("wallet_eur", 923))), key("k8", "convert", "convert"))
		if err != nil || !out.Replayed || out.Transfer.FX == nil || out.Transfer.FX.Rate != "0.9234" || out.Transfer.Postings[3].Currency != "EUR" ||
			len(out.Transfer.Reversals) != 0 {
			t.Fatalf("replayed conversion: %v %+v", err, out.Transfer)
		}
		if a, _ := l.Account("fx_eur", 0); !a.Exchange || a.Balance.Posted != -923-923+462 {
			t.Fatalf("fx_eur %+v", a)
		}
		if a, _ := l.Account("wallet_eur", 0); a.Balance.Posted != 923+923-462 {
			t.Fatalf("wallet_eur %+v", a)
		}
		if _, err := l.Do(ctx, convert("0.9234", dr("bank", 100), cr("fx_usd", 100), eur(dr("fx_eur", 1)), eur(cr("wallet_eur", 1))), key("k8", "off", "off")); codeOf(err) != CodeInvalid {
			t.Fatalf("replayed refused conversion: %v", err)
		}
	}
	// The expiry schedule came back: the 5 second hold expires on time, and
	// the batch's and the balancing 60 second holds later.
	clock.Advance(6 * time.Second)
	n, err := fromSnap.ExpireDue(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	clock.Advance(60 * time.Second)
	n, err = fromSnap.ExpireDue(ctx)
	if err != nil || n != 2 {
		t.Fatalf("expired %d, %v", n, err)
	}
}

func TestDamagedSnapshotsAreRejected(t *testing.T) {
	dir, _ := writeSmallSnapshot(t, snapVersion)
	path := filepath.Join(dir, snapName(1))
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSnapshot(wal.OSFS{}, path); err != nil {
		t.Fatal(err)
	}
	try := func(what string, data []byte) {
		t.Helper()
		p := filepath.Join(t.TempDir(), snapName(1))
		os.WriteFile(p, data, 0o600)
		if _, _, err := readSnapshot(wal.OSFS{}, p); err == nil {
			t.Fatalf("%s: damaged snapshot accepted", what)
		}
	}
	for off := 0; off < len(good); off++ {
		try(fmt.Sprintf("cut at %d", off), good[:off])
	}
	for off := 0; off < len(good); off++ {
		for _, bit := range []byte{0x01, 0x80} {
			bad := append([]byte(nil), good...)
			bad[off] ^= bit
			try(fmt.Sprintf("bit %x at %d", bit, off), bad)
		}
	}
	try("trailing byte", append(append([]byte(nil), good...), 0))
	try("frame repeated", append(append([]byte(nil), good...), good[len(snapMagic):]...))
}

// A damaged newest snapshot is skipped in favour of an older one or a full
// replay, and the recovered state is the same either way.
func TestDamagedSnapshotFallsBack(t *testing.T) {
	dir, clock := writeSmallSnapshot(t, snapVersion)
	l := openAt(t, dir, clock, func(o *Options) { o.SnapshotEvery = 0 })
	if _, err := l.Do(context.Background(), move(dr("bank", 1), cr("shop", 1)), nil); err != nil {
		t.Fatal(err)
	}
	l.Close() // snapshot 2
	if nums := snapshotFiles(t, dir); fmt.Sprint(nums) != "[2 1]" {
		t.Fatalf("snapshots %v", nums)
	}
	want := openAt(t, copyDir(t, dir, false), clock, nil)

	for _, damage := range []string{"newest", "both"} {
		d := copyDir(t, dir, true)
		for _, n := range []uint64{2, 1} {
			if n == 2 || damage == "both" {
				p := filepath.Join(d, snapName(n))
				data, _ := os.ReadFile(p)
				data[len(data)/2] ^= 0xff
				os.WriteFile(p, data, 0o600)
			}
		}
		var logs bytes.Buffer
		got := openAt(t, d, clock, func(o *Options) { o.Logger = slog.New(slog.NewJSONHandler(&logs, nil)) })
		if !strings.Contains(logs.String(), "skipping unreadable snapshot") {
			t.Fatalf("%s: damage not reported: %s", damage, logs.String())
		}
		if damage == "newest" && !strings.Contains(logs.String(), `"msg":"loaded snapshot","snapshot":1`) {
			t.Fatalf("older snapshot not used: %s", logs.String())
		}
		sameState(t, damage, got, want)
	}
}

// A snapshot is only trusted at the point the log says it was taken.
func TestSnapshotWithoutItsLogRecordStopsStartup(t *testing.T) {
	dir, _ := writeSmallSnapshot(t, snapVersion)
	other := newEnv(t, true)
	other.standard()
	other.l.Close()
	os.Remove(filepath.Join(other.dir, snapName(1)))
	copySnapshots(t, dir, other.dir) // snapshot 1 from a different ledger
	// other's log has a snapshot-1 record at its own head, which does not
	// match the borrowed snapshot.
	if _, err := Open(other.opts); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("borrowed snapshot: err = %v", err)
	}

	// A snapshot numbered past every record in the log.
	d := copyDir(t, dir, false)
	data, _ := os.ReadFile(filepath.Join(dir, snapName(1)))
	os.WriteFile(filepath.Join(d, snapName(1)), data, 0o600)
	logData := logBytes(t, d)
	idx := bytes.Index(logData, snapRecordPrefix)
	start := bytes.LastIndexByte(logData[:idx], '\n') + 1
	os.WriteFile(activeSegment(t, d), logData[:start], 0o600) // log ends before the marker
	if _, err := Open(Options{Dir: d, SweepInterval: -1}); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("snapshot ahead of the log: err = %v", err)
	}

	// Snapshots beside an empty log.
	d = t.TempDir()
	os.WriteFile(filepath.Join(d, snapName(1)), data, 0o600)
	if _, err := Open(Options{Dir: d, SweepInterval: -1}); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("snapshot with an empty log: err = %v", err)
	}
}

func TestSnapshotLeftoversAreIgnored(t *testing.T) {
	dir, clock := writeSmallSnapshot(t, snapVersion)
	tmp := filepath.Join(dir, snapName(7)+snapTmpSuffix)
	os.WriteFile(tmp, []byte("half written"), 0o600)
	os.WriteFile(filepath.Join(dir, "snapshot-notes.txt"), []byte("x"), 0o600)
	l := openAt(t, dir, clock, nil)
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left behind: %v", err)
	}
	sameState(t, "leftovers", l, openAt(t, copyDir(t, dir, false), clock, nil))
}

// Failing to write a snapshot costs restart time, never data or
// availability.
func TestSnapshotWriteFailureIsNotFatal(t *testing.T) {
	e := newEnv(t, true)
	e.l.Close()
	var logs bytes.Buffer
	e.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	e.opts.SnapshotEvery = 4
	e.open()
	// A directory where the temporary file belongs makes the write fail.
	if err := os.Mkdir(filepath.Join(e.dir, snapName(1)+snapTmpSuffix), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.dir, snapName(1)+snapTmpSuffix, "keep"), nil, 0o600)
	e.standard()
	for i := 0; i < 5; i++ {
		e.do(move(dr("bank", 1), cr("alice", 1)))
	}
	e.l.Close()
	if !strings.Contains(logs.String(), "writing a snapshot failed") {
		t.Fatalf("logs: %s", logs.String())
	}
	if e.l.Err() != nil {
		t.Fatal(e.l.Err())
	}
	e.open()
	e.wantBalance("alice", 5, 0, 5)
	e.invariants()
}

// Snapshots are taken while clients keep writing; nothing is lost or
// duplicated, and the result matches a full replay.
func TestSnapshotsUnderConcurrentLoad(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	l := openAt(t, dir, clock, func(o *Options) { o.SnapshotEvery = 25 })
	ctx := context.Background()
	l.Do(ctx, openAcct("src", "USD", Debit), nil)
	for i := 0; i < 8; i++ {
		l.Do(ctx, openAcct(fmt.Sprintf("dst%d", i), "USD", Credit), nil)
	}
	var wg sync.WaitGroup
	for c := 0; c < 8; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i := 0; i < 150; i++ {
				k := key("load", fmt.Sprintf("%d-%d", c, i), "x")
				if _, err := l.Do(ctx, move(dr("src", 1), cr(fmt.Sprintf("dst%d", c), 1)), k); err != nil {
					t.Error(err)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	crashed := copyDir(t, dir, true)
	l.Close()
	got := openAt(t, crashed, clock, nil)
	if a, _ := got.Account("src", 0); a.Balance.Posted != 8*150 {
		t.Fatalf("src posted %d", a.Balance.Posted)
	}
	sameState(t, "concurrent", got, openAt(t, copyDir(t, crashed, false), clock, nil))
	again := openAt(t, dir, clock, nil)
	sameState(t, "after close", again, got)
}

// waitSnapshots waits until no snapshot is being written, so the segment
// drop that follows one has happened too.
func waitSnapshots(l *Ledger) {
	<-l.snapIdle
	l.snapIdle <- struct{}{}
}

// Segments holding only records before the oldest kept snapshot are
// deleted, so the log stays a few segments long however old the ledger is.
func TestSegmentsAreDroppedBehindSnapshots(t *testing.T) {
	e := newEnv(t, true)
	e.opts.SegmentBytes, e.opts.SnapshotEvery = 2048, 20
	e.restart()
	e.standard()
	for i := 0; i < 400; i++ {
		e.do(move(dr("bank", 1), cr("alice", 1)))
	}
	waitSnapshots(e.l)
	lg := e.l.log
	segs, _ := wal.SegmentPaths(filepath.Join(e.dir, walDir))
	files, _ := filepath.Glob(filepath.Join(e.dir, walDir, "*.seg"))
	if lg.FirstIndex() == 1 || len(segs) != lg.Segments() || len(files) != len(segs) {
		t.Fatalf("first record %d, %d segments, %d listed, %d files", lg.FirstIndex(), lg.Segments(), len(segs), len(files))
	}
	// Exactly the segments from the one holding the oldest kept snapshot's
	// record are kept. The two snapshots are usually 20 records apart, but
	// one is only marked once the previous one is on disk, so on a busy
	// machine they can be further apart and keep more segments.
	kept := snapshotFiles(t, e.dir)
	e.l.snapMu.Lock()
	index, ok := e.l.snapIndex[kept[len(kept)-1]]
	e.l.snapMu.Unlock()
	first := func(i int) uint64 {
		n, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(segs[i]), ".seg"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if !ok || e.l.mSegDropped.Value() == 0 || first(0) > index || (len(segs) > 1 && first(1) <= index) {
		t.Fatalf("oldest kept snapshot %v at record %d (known %v), %d segments kept from record %d, %d dropped",
			kept, index, ok, len(segs), first(0), e.l.mSegDropped.Value())
	}
	want := digest(e.l)
	e.restart()
	if got := digest(e.l); got != want {
		t.Fatalf("after restart:\n%s\nwant\n%s", got, want)
	}
	e.do(move(dr("bank", 1), cr("alice", 1)))
	e.l.Close()
	want = digest(e.l)

	// The records the dropped segments held are only in the snapshots now.
	if _, err := Open(Options{Dir: copyDir(t, e.dir, false), SweepInterval: -1}); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("open without snapshots: %v", err)
	}
	// The older of the two kept snapshots still has its record in the log,
	// so a damaged newest one falls back to it.
	d := copyDir(t, e.dir, true)
	nums := snapshotFiles(t, d)
	if len(nums) != 2 {
		t.Fatalf("snapshots %v", nums)
	}
	newest := filepath.Join(d, snapName(nums[0]))
	data, _ := os.ReadFile(newest)
	data[len(data)/2] ^= 0x80
	os.WriteFile(newest, data, 0o600)
	l := openAt(t, d, e.clock, nil)
	if got := digest(l); got != want {
		t.Fatalf("from the older snapshot:\n%s\nwant\n%s", got, want)
	}
}

func TestReadSnapshotErrors(t *testing.T) {
	if _, _, err := readSnapshot(wal.OSFS{}, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("read a missing file")
	}
	p := filepath.Join(t.TempDir(), snapName(1))
	os.WriteFile(p, []byte("LDGSNAP9"), 0o600)
	if _, _, err := readSnapshot(wal.OSFS{}, p); !errors.Is(err, errSnapshot) {
		t.Fatalf("wrong magic: %v", err)
	}
	// Frames that decode but describe impossible state.
	for name, build := range map[string]func(w *snapWriter){
		"version": func(w *snapWriter) { w.tag(snapItemHeader); w.u(9) },
		"head": func(w *snapWriter) {
			w.tag(snapItemHeader)
			w.u(1)
			w.u(1)
			w.u(5)
			w.when(time.Time{})
			w.u(0)
			w.u(0)
			w.u(0)
			w.u(0)
		},
		"wrong item": func(w *snapWriter) { w.tag(snapItemFooter) },
		"huge count": func(w *snapWriter) {
			w.tag(snapItemHeader)
			w.u(1)
			w.u(1)
			w.u(0)
			w.when(time.Time{})
			w.u(1)
			w.u(0)
			w.u(0)
			w.u(0)
			w.tag(snapItemAcct)
			w.str("a")
			w.str("USD")
			w.i(2)
			w.str("debit")
			w.flag(false)
			w.flag(false)
			w.u(1 << 40)
		},
		"bad currency": func(w *snapWriter) {
			w.tag(snapItemHeader)
			w.u(1)
			w.u(1)
			w.u(0)
			w.when(time.Time{})
			w.u(1)
			w.u(0)
			w.u(0)
			w.u(0)
			w.tag(snapItemAcct)
			w.str("a")
			w.str("XXX")
			w.i(2)
		},
	} {
		var buf bytes.Buffer
		buf.WriteString(snapMagic)
		w := &snapWriter{w: bufio.NewWriter(&buf)}
		build(w)
		w.flush()
		w.w.Flush()
		os.WriteFile(p, buf.Bytes(), 0o600)
		if _, _, err := readSnapshot(wal.OSFS{}, p); !errors.Is(err, errSnapshot) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
