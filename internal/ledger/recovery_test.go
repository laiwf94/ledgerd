package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// digest renders everything recovery must restore: balances, versions,
// transfers, the journal head and the idempotency keys.
func digest(l *Ledger) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var b strings.Builder
	ids := make([]string, 0, len(l.st.accounts))
	for id := range l.st.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := l.st.accounts[id]
		fmt.Fprintf(&b, "%s %+v v%d|", id, a.bal, a.version)
	}
	tids := make([]string, 0, len(l.st.transfers))
	for id := range l.st.transfers {
		tids = append(tids, id)
	}
	sort.Strings(tids)
	for _, id := range tids {
		b.WriteString(transferSnapshot(l.st.transfers[id]) + "|")
	}
	fmt.Fprintf(&b, "head %d open %d|", l.st.head(), l.st.open)
	l.idemMu.Lock()
	keys := make([]string, 0, len(l.idem))
	for k, e := range l.idem {
		keys = append(keys, fmt.Sprintf("%q=%v", k, e.err))
	}
	l.idemMu.Unlock()
	sort.Strings(keys)
	b.WriteString(strings.Join(keys, ","))
	return b.String()
}

// replayBytes recovers a ledger from raw log bytes, the way Open does but
// without touching the disk.
func replayBytes(t *testing.T, data []byte) (*Ledger, error) {
	t.Helper()
	l := &Ledger{opts: Options{Clock: newClock().Now, IdempotencyTTL: DefaultIdempotencyTTL}, st: newState(),
		idem: map[string]*idemEntry{}, inflight: map[string]*flight{}}
	first := true
	_, err := wal.Replay(bytes.NewReader(data), func(p []byte) error {
		err := l.replay(p, first)
		first = false
		return err
	})
	return l, err
}

func TestCrashAtEveryByte(t *testing.T) {
	e := newEnv(t, true)
	e.l.Close()
	e.opts.MaxBatch = 1
	e.open()
	ctx := context.Background()
	snapshots := []string{digest(e.l)}
	step := func(c Command, k string) {
		var idem *Idem
		if k != "" {
			idem = key("", k, k)
		}
		if _, err := e.l.Do(ctx, c, idem); err != nil && idem == nil {
			return // rejected without a key: nothing was written
		}
		snapshots = append(snapshots, digest(e.l))
	}
	step(openAcct("bank", "USD", Debit), "")
	step(openAcct("alice", "USD", Credit, "nonneg"), "")
	step(openAcct("shop", "USD", Credit, "nonneg"), "")
	step(move(dr("bank", 1000), cr("alice", 1000)), "fund")
	step(move(dr("alice", 5000), cr("shop", 5000)), "overdraft") // rejected, recorded under its key
	step(hold(30, dr("alice", 300), cr("shop", 300)), "auth-1")
	auth1 := "tr_000002"
	step(hold(10, dr("alice", 200), cr("shop", 200)), "auth-2")
	step(postCmd(auth1, 250), "capture")
	step(reverseCmd(auth1, 100), "refund")
	e.clock.Advance(11 * time.Second)
	if n, err := e.l.ExpireDue(ctx); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	snapshots = append(snapshots, digest(e.l))
	step(hold(30, dr("alice", 50), cr("shop", 50)), "auth-3")
	step(voidCmd("tr_000005"), "")
	e.l.Close()

	data, err := os.ReadFile(filepath.Join(e.dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	index := map[string]int{}
	for i, s := range snapshots {
		index[s] = i
	}
	stride := 1
	if testing.Short() {
		stride = 7
	}
	last := 0
	for off := 0; off <= len(data); off += stride {
		l, err := replayBytes(t, data[:off])
		if err != nil {
			t.Fatalf("offset %d: %v", off, err)
		}
		i, ok := index[digest(l)]
		if !ok && off > 0 {
			t.Fatalf("offset %d: recovered a state that was never acknowledged:\n%s", off, digest(l))
		}
		if i < last {
			t.Fatalf("offset %d: recovered an older state (%d) than a shorter prefix did (%d)", off, i, last)
		}
		last = i
		if v := l.CheckInvariants(); len(v) > 0 {
			t.Fatalf("offset %d: %v", off, v)
		}
	}
	if last != len(snapshots)-1 {
		t.Fatalf("the whole log recovered state %d of %d", last, len(snapshots)-1)
	}

	// Full Open (truncation, compaction, invariant check) at a sample of offsets.
	for off := 0; off <= len(data); off += 97 {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, logName), data[:off], 0o600)
		l, err := Open(Options{Dir: dir, Clock: e.clock.Now, SweepInterval: -1})
		if err != nil {
			t.Fatalf("open at offset %d: %v", off, err)
		}
		if _, ok := index[digest(l)]; !ok && l.Head() > 0 {
			t.Fatalf("open at offset %d: unexpected state", off)
		}
		l.Close()
	}
}

func writeLog(t *testing.T, records ...string) string {
	t.Helper()
	dir := t.TempDir()
	var data []byte
	for _, r := range records {
		data = wal.Encode(data, []byte(r))
	}
	os.WriteFile(filepath.Join(dir, logName), data, 0o600)
	return dir
}

func TestReplayRejectsBadLogs(t *testing.T) {
	const header = `{"t":"header","v":1}`
	acct := `{"t":"cmd","time":"2020-01-01T00:00:00Z","cmd":{"create_account":{"id":"a","currency":"USD","normal_balance":"debit"}}}`
	cases := map[string][]string{
		"no header":         {acct},
		"two headers":       {header, header},
		"future version":    {`{"t":"header","v":2}`},
		"unknown type":      {header, `{"t":"mystery"}`},
		"unknown field":     {header, `{"t":"cmd","extra":1}`},
		"not json":          {header, `nope`},
		"trailing data":     {header, `{"t":"cmd"} {}`},
		"empty command":     {header, `{"t":"cmd"}`},
		"invalid command":   {header, `{"t":"cmd","cmd":{"void":{"transfer_id":"-"}}}`},
		"failing command":   {header, `{"t":"cmd","cmd":{"void":{"transfer_id":"tr_x"}}}`},
		"wrong seq":         {header, acct, `{"t":"cmd","seq":5,"cmd":{"create_account":{"id":"b","currency":"USD","normal_balance":"debit"}}}`},
		"bad expire":        {header, `{"t":"expire","id":"tr_x","seq":1}`},
		"reject without":    {header, `{"t":"reject"}`},
		"bad idem hash":     {header, `{"t":"reject","idem":{"key":"k","hash":"zz"},"err":{"code":"x","message":"y"}}`},
		"bad idem key":      {header, `{"t":"reject","idem":{"key":"","hash":"` + strings.Repeat("0", 64) + `"},"err":{"code":"x","message":"y"}}`},
		"duplicate account": {header, acct, acct},
	}
	for name, recs := range cases {
		dir := writeLog(t, recs...)
		_, err := Open(Options{Dir: dir, SweepInterval: -1})
		if !errors.Is(err, wal.ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
	if _, err := Open(Options{Dir: writeLog(t, header, acct), SweepInterval: -1}); err != nil {
		t.Fatalf("valid log: %v", err)
	}
}

// failingFile fails writes or fsyncs while the matching flag is set.
type failingFile struct {
	wal.File
	failWrite, failSync *atomic.Bool
}

func (f failingFile) Write(p []byte) (int, error) {
	if f.failWrite.Load() {
		return 0, errors.New("no space left on device")
	}
	return f.File.Write(p)
}

func (f failingFile) Sync() error {
	if f.failSync.Load() {
		return errors.New("input/output error")
	}
	return f.File.Sync()
}

func TestStorageFailureStopsWrites(t *testing.T) {
	for _, mode := range []string{"write", "fsync"} {
		t.Run(mode, func(t *testing.T) {
			var failWrite, failSync atomic.Bool
			var logs bytes.Buffer
			e := newEnv(t, true)
			e.l.Close()
			e.opts.WrapFile = func(f wal.File) wal.File { return failingFile{f, &failWrite, &failSync} }
			e.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			e.open()
			e.standard()
			ctx := context.Background()
			if mode == "write" {
				failWrite.Store(true)
			} else {
				failSync.Store(true)
			}
			fund := move(dr("equity", 70), cr("alice", 70))
			_, err := e.l.Do(ctx, fund, key("", "fund", "f"))
			if le, _ := err.(*Error); le == nil || le.Code != CodeUnavailable {
				t.Fatalf("err = %v", err)
			}
			if e.l.Err() == nil || !strings.Contains(logs.String(), "stopped accepting writes") {
				t.Fatalf("failure not surfaced: %v / %s", e.l.Err(), logs.String())
			}
			failWrite.Store(false)
			failSync.Store(false)
			// Fail-stop: nothing more is accepted, but reads still serve the
			// last durable state.
			if _, err := e.l.Do(ctx, move(dr("equity", 1), cr("bob", 1)), nil); err == nil {
				t.Fatal("write accepted after a storage failure")
			}
			if _, err := e.l.ExpireDue(ctx); err == nil {
				t.Fatal("sweep accepted after a storage failure")
			}
			e.wantBalance("alice", 0, 0, 0)
			e.invariants()

			// After a restart the client retries with its key and learns what
			// happened: an fsync error may still have left the data on disk.
			e.restart()
			out, err := e.l.Do(ctx, fund, key("", "fund", "f"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "fsync" && !out.Replayed {
				t.Fatal("the record reached the file before fsync failed; the retry should replay it")
			}
			if mode == "write" && out.Replayed {
				t.Fatal("nothing was written; the retry should execute")
			}
			e.wantBalance("alice", 70, 0, 70)
			e.invariants()
		})
	}
}

func TestCanceledRequestsDoNotExecute(t *testing.T) {
	e := newEnv(t, false)
	e.standard()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.l.Do(ctx, move(dr("equity", 5), cr("alice", 5)), key("", "k", "k"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	e.wantBalance("alice", 0, 0, 0)
	// The key was not consumed.
	if _, err := e.l.Do(context.Background(), move(dr("equity", 5), cr("alice", 5)), key("", "k", "k")); err != nil {
		t.Fatal(err)
	}
	e.wantBalance("alice", 5, 0, 5)
}

func TestClose(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	// Commands racing with Close either complete or get ErrClosed.
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.l.Do(context.Background(), move(dr("equity", 1), cr("alice", 1)), nil)
			if err == nil {
				ok.Add(1)
			} else if err != ErrClosed {
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	time.Sleep(time.Millisecond)
	if err := e.l.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := e.l.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
	if _, err := e.l.Do(context.Background(), move(dr("equity", 1), cr("alice", 1)), nil); err != ErrClosed {
		t.Fatalf("after close: %v", err)
	}
	e.open()
	e.wantBalance("alice", ok.Load(), 0, ok.Load())
}

func TestInvariantCheckerFindsDamage(t *testing.T) {
	cases := map[string]struct {
		damage func(st *state)
		want   string
	}{
		"balance":   {func(st *state) { st.accounts["alice"].bal.CreditsPosted++ }, "does not match its journal"},
		"version":   {func(st *state) { st.accounts["alice"].version++ }, "version"},
		"entry":     {func(st *state) { st.entries[0].Lines[0].Posted++ }, "does not balance"},
		"limit":     {func(st *state) { st.accounts["alice"].NonPositive = true }, "non_positive"},
		"overdraft": {func(st *state) { st.accounts["alice"].bal.DebitsPending += 10_000 }, "non_negative"},
		"open":      {func(st *state) { st.open++ }, "open pending count"},
		"reserved": {func(st *state) {
			for id, tr := range st.transfers {
				if tr.State == StatePending {
					c := tr.clone()
					c.State = StateVoided
					st.transfers[id] = c
				}
			}
		}, "open pending transfers hold"},
		"reversed": {func(st *state) {
			for id, tr := range st.transfers {
				if tr.State == StatePosted && tr.Reverses == "" {
					c := tr.clone()
					c.Postings[0].Reversed = 1
					st.transfers[id] = c
					return
				}
			}
		}, "reversals add up"},
		"history":  {func(st *state) { st.accounts["alice"].rows[0].line = 9 }, "history row"},
		"running":  {func(st *state) { st.accounts["alice"].rows[0].balance.CreditsPosted = 9 }, "running balance"},
		"currency": {func(st *state) { st.accounts["alice"].Currency = "EUR" }, "holds another currency"},
	}
	for name, c := range cases {
		e := newEnv(t, false)
		e.standard()
		e.do(move(dr("bank", 500), cr("alice", 500)))
		e.do(hold(60, dr("alice", 100), cr("bob", 100)))
		e.invariants()
		e.l.mu.Lock()
		c.damage(e.l.st)
		e.l.mu.Unlock()
		v := e.l.CheckInvariants()
		if !strings.Contains(strings.Join(v, "\n"), c.want) {
			t.Errorf("%s: violations %v, want one mentioning %q", name, v, c.want)
		}
	}
}

func TestWideAccumulator(t *testing.T) {
	var a, b wide
	for i := 0; i < 4; i++ {
		a.add(1 << 62)
	}
	b.add(1 << 62)
	b.add(1 << 62)
	b.add(1 << 62)
	b.add(1 << 62)
	if a != b || a.hi != 1 || a.lo != 0 {
		t.Fatalf("2^64 = %+v", a)
	}
	a.add(-1)
	if a.hi != 0 || a.lo != 1<<64-1 {
		t.Fatalf("2^64-1 = %+v", a)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := Open(Options{Dir: file}); err == nil {
		t.Fatal("opened a ledger inside a regular file")
	}
	_, err := Open(Options{Dir: t.TempDir(), WrapFile: func(f wal.File) wal.File {
		var no, yes atomic.Bool
		yes.Store(true)
		return failingFile{f, &yes, &no}
	}})
	if err == nil {
		t.Fatal("open succeeded although the header could not be written")
	}
}

func TestCompactionFailureIsNotFatal(t *testing.T) {
	e := newEnv(t, true)
	e.standard()
	// An expired key gives the next start something to compact.
	if _, err := e.l.Do(context.Background(), move(dr("bank", 1), cr("equity", 1)), key("", "old", "old")); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(DefaultIdempotencyTTL + time.Second)
	e.l.Close()
	var logs bytes.Buffer
	var calls atomic.Int32
	e.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	e.opts.WrapFile = func(f wal.File) wal.File {
		var w, s atomic.Bool
		if calls.Add(1) == 2 { // the compaction's temporary file
			s.Store(true)
		}
		return failingFile{f, &w, &s}
	}
	e.open()
	if !strings.Contains(logs.String(), "compaction failed") {
		t.Fatalf("logs: %s", logs.String())
	}
	e.do(move(dr("bank", 1), cr("equity", 1)))
	e.restart()
	e.wantBalance("bank", 2, 0, 2)
}
