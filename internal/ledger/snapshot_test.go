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
	"sort"
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
	data, err := os.ReadFile(filepath.Join(src, logName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, logName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if withSnapshots {
		copySnapshots(t, src, dst)
	}
	return dst
}

func snapshotFiles(t testing.TB, dir string) []uint64 {
	t.Helper()
	nums, err := listSnapshots(dir)
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

// liveExpiries lists the scheduled timeouts that still apply.
func liveExpiries(st *state) []string {
	var out []string
	for _, e := range st.expiries {
		if tr, ok := st.transfers[e.id]; ok && tr.State == StatePending && tr.ExpiresAt.Equal(e.at) {
			out = append(out, e.id+"@"+e.at.String())
		}
	}
	sort.Strings(out)
	return out
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
	g := &gen{r: r, m: m, accts: []string{"a", "b", "c", "d", "e", "f", "g", "h"}}
	for _, c := range []Command{openAcct("a", "USD", Debit), openAcct("b", "USD", Credit), openAcct("c", "USD", Credit, "nonneg"),
		openAcct("d", "USD", Debit, "nonneg"), openAcct("e", "USD", Debit, "nonpos"), openAcct("g", "EUR", Credit)} {
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
			id := ""
			if out.Transfer != nil && (c.CreateTransfer != nil || c.Reverse != nil) {
				id = out.Transfer.ID
			}
			m.exec(&c, id)
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
	path := filepath.Join(crashed, logName)
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
func writeSmallSnapshot(t *testing.T) (dir string, clock *fakeClock) {
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
	e.l.Close()
	if nums := snapshotFiles(t, e.dir); len(nums) != 1 {
		t.Fatalf("snapshots %v", nums)
	}
	return e.dir, e.clock
}

func TestSnapshotRoundTripsEveryField(t *testing.T) {
	dir, clock := writeSmallSnapshot(t)
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
	}
	// The expiry schedule came back: the 5 second hold expires on time.
	clock.Advance(6 * time.Second)
	n, err := fromSnap.ExpireDue(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
}

func TestDamagedSnapshotsAreRejected(t *testing.T) {
	dir, _ := writeSmallSnapshot(t)
	path := filepath.Join(dir, snapName(1))
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSnapshot(path); err != nil {
		t.Fatal(err)
	}
	try := func(what string, data []byte) {
		t.Helper()
		p := filepath.Join(t.TempDir(), snapName(1))
		os.WriteFile(p, data, 0o600)
		if _, _, err := readSnapshot(p); err == nil {
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
	dir, clock := writeSmallSnapshot(t)
	l := openAt(t, dir, clock, func(o *Options) { o.SnapshotEvery = 0 })
	l.Do(context.Background(), move(dr("bank", 1), cr("alice", 1)), nil)
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
	dir, _ := writeSmallSnapshot(t)
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
	logData, _ := os.ReadFile(filepath.Join(d, logName))
	idx := bytes.Index(logData, snapRecordPrefix)
	start := bytes.LastIndexByte(logData[:idx], '\n') + 1
	os.WriteFile(filepath.Join(d, logName), logData[:start], 0o600) // log ends before the marker
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
	dir, clock := writeSmallSnapshot(t)
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

// Snapshots record the log position as a numbered record, so they keep
// working after compaction rewrites the log.
func TestSnapshotSurvivesCompaction(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	ctx := context.Background()
	e.l.Do(ctx, move(dr("bank", 1), cr("alice", 1)), key("", "old", "old"))
	e.clock.Advance(DefaultIdempotencyTTL + time.Second)
	e.l.Do(ctx, move(dr("bank", 2), cr("alice", 2)), key("", "new", "new"))
	e.restart() // compacts, then loads snapshot 1
	if s := e.l.Stats(); s.IdempotencyKeys != 1 {
		t.Fatalf("keys after compaction: %+v", s)
	}
	e.do(move(dr("bank", 4), cr("alice", 4)))
	e.restart()
	e.wantBalance("alice", 7, 0, 7)
	sameState(t, "compacted", e.l, openAt(t, copyDir(t, e.dir, false), e.clock, nil))
}

func TestReadSnapshotErrors(t *testing.T) {
	if _, _, err := readSnapshot(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("read a missing file")
	}
	p := filepath.Join(t.TempDir(), snapName(1))
	os.WriteFile(p, []byte("LDGSNAP9"), 0o600)
	if _, _, err := readSnapshot(p); !errors.Is(err, errSnapshot) {
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
		if _, _, err := readSnapshot(p); !errors.Is(err, errSnapshot) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
