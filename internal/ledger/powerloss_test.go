package ledger

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
	"github.com/laiwf94/ledgerd/internal/wal/faultfs"
)

// TestReferenceModelAcrossPowerCuts runs the reference-model workload on
// faultfs and cuts the power over and over: in the middle of a round of
// concurrent commands, during an expiry sweep, while a snapshot is being
// written in the background, or while old segments are being dropped. Each
// cut throws away a random selection of everything that was not synced.
// The ledger must then open, and every command it acknowledged before the
// cut must still be there with the result it was given. Commands that were
// still waiting may or may not have survived; whatever the recovered log
// holds is fed to the model, which must agree with the ledger on every
// balance, transfer and invariant.
func TestReferenceModelAcrossPowerCuts(t *testing.T) {
	seeds, cuts := 16, 160
	if testing.Short() {
		seeds, cuts = 4, 40
	}
	seen := map[string]int{}
	var stats faultfs.Stats
	for seed := 1; seed <= seeds; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) { runPowerModel(t, int64(seed), cuts, seen, &stats) })
	}
	t.Logf("outcomes: %v", seen)
	t.Logf("faults: %+v", stats)
	if t.Failed() {
		return
	}
	if seen["cut"] != seeds*cuts {
		t.Errorf("%d power cuts, want %d", seen["cut"], seeds*cuts)
	}
	// The cuts must land in the places that matter and lose what they can,
	// or a passing run proves little.
	for _, want := range []string{"cut:during-commands", "cut:between-rounds", "acked-then-cut", "unacked-lost", "unacked-survived",
		"ok:create_transfer", "ok:post", "ok:void", "ok:reverse", "expired", "replayed", "snapshot-at-recovery", "segments-dropped"} {
		if seen[want] == 0 {
			t.Errorf("the workload never produced %q (saw %v)", want, seen)
		}
	}
	for m := faultfs.DropAll; m <= faultfs.Reordered; m++ {
		if stats.Files[m] == 0 || (m != faultfs.Reordered && stats.Dirs[m] == 0) {
			t.Errorf("no file or directory was cut with mode %s: %+v", m, stats)
		}
	}
	// The ledger syncs after every write, so a file rarely has more than
	// one pending write and reordering is left to faultfs's own tests.
	if stats.LostWrites == 0 || stats.LostDirOps == 0 || stats.TornWrites == 0 {
		t.Errorf("the cuts never lost or tore anything: %+v", stats)
	}
}

func runPowerModel(t *testing.T, seed int64, cuts int, seen map[string]int, stats *faultfs.Stats) {
	r := rand.New(rand.NewSource(seed))
	clock := newClock()
	fsys := faultfs.New(seed)
	const dir = "/var/lib/ledgerd"
	opts := Options{Dir: dir, FS: fsys, Clock: clock.Now, NewID: seqIDs(), SweepInterval: -1, MaxBatch: 1 + r.Intn(16),
		SnapshotEvery: 1 + r.Intn(20), SegmentBytes: 256 + int64(r.Intn(2048))}
	l, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	held := false
	hold := func() { l.dropMu.Lock(); held = true }
	release := func() {
		if held {
			held = false
			l.dropMu.Unlock()
		}
	}
	defer func() {
		if l != nil {
			release()
			l.Close()
		}
	}()

	m := newModel(clock.Now())
	g := &gen{r: r, m: m, accts: []string{"a", "b", "c", "d", "e", "f", "g", "h"}}
	results := map[string]mResult{}
	hashes := map[string][32]byte{}
	setup := []Command{
		openAcct("a", "USD", Debit), openAcct("b", "USD", Credit), openAcct("c", "USD", Credit, "nonneg"),
		openAcct("d", "USD", Debit, "nonneg"), openAcct("e", "USD", Debit, "nonpos"), openAcct("f", "USD", Credit, "nonpos"),
		openAcct("g", "EUR", Credit),
	}
	for i, c := range setup {
		o := newOp(fmt.Sprintf("setup-%d", i), c)
		if _, err := l.Do(context.Background(), o.command(), &Idem{Key: o.key, Hash: o.hash}); err != nil {
			t.Fatal(err)
		}
		m.exec(&c, "")
		results[o.key], hashes[o.key] = mResult{}, o.hash
	}
	hold()
	_, offset := readRecordsFS(t, fsys, dir, 0)
	release()

	// applyRecords feeds records the log holds past offset to the model, in
	// log order, checking each against the operation that sent it.
	applyRecords := func(round int, recs []record, ops []op) map[string]bool {
		executed := map[string]bool{}
		for _, rec := range recs {
			if rec.Type == recExpire {
				due := m.due()
				if i := sort.SearchStrings(due, rec.ID); i == len(due) || due[i] != rec.ID {
					t.Fatalf("round %d: log expired %s, model has %v due", round, rec.ID, due)
				}
				m.expire(rec.ID)
				seen["expired"]++
				continue
			}
			if rec.Idem == nil {
				t.Fatalf("round %d: record without a key: %+v", round, rec)
			}
			k := rec.Idem.Key
			if executed[k] {
				t.Fatalf("round %d: key %s executed twice", round, k)
			}
			executed[k] = true
			var o *op
			for i := range ops {
				if ops[i].key == k && fmt.Sprintf("%x", ops[i].hash) == rec.Idem.Hash {
					o = &ops[i]
					break
				}
			}
			if o == nil {
				t.Fatalf("round %d: log has key %s that no operation sent", round, k)
			}
			c := o.command()
			res := m.exec(&c, rec.ID)
			if (rec.Type == recReject) != (res.code != "") || (rec.Err != nil && rec.Err.Code != res.code) {
				t.Fatalf("round %d op %s (%s): ledger recorded %s %+v, model says %q", round, k, o.body, rec.Type, rec.Err, res.code)
			}
			results[k], hashes[k] = res, o.hash
			if res.code == "" {
				seen["ok:"+opName(&c)]++
			} else {
				seen[string(res.code)]++
			}
		}
		return executed
	}

	// expire runs a sweep and feeds what it logged to the model.
	// When the power may go at any moment, the records are left to be read
	// back after the cut with whatever else survived.
	expire := func(round int, live bool) error {
		if _, err := l.ExpireDue(context.Background()); err != nil {
			return err
		}
		if !live {
			return nil
		}
		recs, end := readRecordsFS(t, fsys, dir, offset)
		for _, rec := range recs {
			if rec.Type != recExpire {
				t.Fatalf("round %d: unexpected %s record during expiry", round, rec.Type)
			}
		}
		applyRecords(round, recs, nil)
		offset = end
		return nil
	}

	armed := false // a cut is scheduled and may already have happened
	done := 0
	for round := 0; done < cuts; round++ {
		hold()
		offBefore := fsys.Off()
		if offBefore {
			seen["cut:between-rounds"]++
		}
		cutNow := armed || r.Intn(3) == 0
		if !armed && cutNow {
			// Enough budget to land anywhere in the round, sometimes past it.
			fsys.CutAfter(r.Intn(48))
			armed = true
		}
		failed := false
		if r.Intn(3) == 0 {
			clock.Advance(time.Duration(r.Intn(4)) * time.Second)
			m.now = clock.Now()
			if err := expire(round, !cutNow); err != nil {
				if !fsys.Off() {
					t.Fatalf("round %d: expiry failed with the power on: %v", round, err)
				}
				failed = true
			}
		}

		var ops []op
		var acts []actual
		if !failed {
			ops = make([]op, 1+r.Intn(8))
			for i := range ops {
				ops[i] = g.next()
			}
			acts = make([]actual, len(ops))
			var wg sync.WaitGroup
			for i := range ops {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					o := ops[i]
					out, err := l.Do(context.Background(), o.command(), &Idem{Key: o.key, Hash: o.hash})
					acts[i] = actual{out, err}
				}(i)
			}
			wg.Wait()
		}

		var executed map[string]bool
		if cutNow {
			if !offBefore && fsys.Off() {
				seen["cut:during-commands"]++
			}
			img := fsys.PowerCut()
			stats.Add(img.CutStats())
			release()
			l.Close() // the old process is gone; its errors mean nothing now
			fsys, opts.FS, armed = img, img, false
			seen["cut"]++
			done++
			if nums, _, _ := scanSnapshots(fsys, dir); len(nums) > 0 {
				seen["snapshot-at-recovery"]++
			}

			// Read what survived before Open, which may drop segments that
			// hold records past offset once two newer snapshots survived.
			recs, end := readRecordsFS(t, fsys, dir, offset)
			if l, err = Open(opts); err != nil {
				t.Fatalf("round %d: recovery after a power cut failed: %v", round, err)
			}
			hold()
			if l.log.FirstIndex() > 1 {
				seen["segments-dropped"]++
			}
			executed = applyRecords(round, recs, ops)
			offset = end
			// Expiries that were due before the cut and did not survive are
			// due again; run them now, as the next command would.
			if err := expire(round, true); err != nil {
				t.Fatalf("round %d: expiry after recovery: %v", round, err)
			}
		} else {
			recs, end := readRecordsFS(t, fsys, dir, offset)
			executed = applyRecords(round, recs, ops)
			offset = end
		}

		for i, o := range ops {
			a := acts[i]
			want, known := results[o.key]
			if !acknowledged(a.err) {
				if !cutNow {
					t.Fatalf("round %d op %s failed with the power on: %v", round, o.key, a.err)
				}
				if executed[o.key] {
					seen["unacked-survived"]++
				} else if !known {
					seen["unacked-lost"]++
				}
				continue
			}
			if cutNow {
				seen["acked-then-cut"]++
			}
			switch {
			case hashes[o.key] != o.hash && known:
				if codeOf(a.err) != CodeKeyReused {
					t.Fatalf("round %d: key %s reused for a different request: got %v", round, o.key, a.err)
				}
				continue
			case !known:
				t.Fatalf("round %d op %s (%s): acknowledged with %v but lost by the power cut", round, o.key, o.body, a.err)
			}
			if codeOf(a.err) != want.code {
				t.Fatalf("round %d op %s (%s): got %v, model says %q", round, o.key, o.body, a.err, want.code)
			}
			if a.out.Replayed {
				seen["replayed"]++
			}
			if !executed[o.key] && !a.out.Replayed {
				t.Fatalf("round %d op %s: acknowledged, not marked replayed, and not in the log", round, o.key)
			}
			if want.code == "" && want.transfer != "" {
				if a.out.Transfer == nil || transferSnapshot(a.out.Transfer) != want.snapshot {
					t.Fatalf("round %d op %s: got transfer %+v, model returned %s", round, o.key, a.out.Transfer, want.snapshot)
				}
			}
		}
		for _, o := range ops {
			if _, known := results[o.key]; known && hashes[o.key] == o.hash {
				g.done = append(g.done, o)
			}
		}
		compare(t, round, l, m)
		release()

		// Sometimes the power goes between rounds: during a snapshot still
		// being written or segments being dropped in the background.
		if !armed && r.Intn(6) == 0 {
			fsys.CutAfter(r.Intn(12))
			armed = true
		}
	}
	if v := l.CheckInvariants(); len(v) > 0 {
		t.Fatal(v)
	}
	if _, err := wal.SegmentPathsFS(fsys, dir+"/"+walDir); err != nil {
		t.Fatal(err)
	}
}

// acknowledged reports whether a command's reply promised an outcome: a
// result or a refusal, as opposed to the ledger being unable to say.
func acknowledged(err error) bool {
	c := codeOf(err)
	return c != CodeUnavailable && !strings.HasPrefix(string(c), "go:")
}
