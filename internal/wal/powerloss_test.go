package wal_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/laiwf94/ledgerd/internal/wal"
	"github.com/laiwf94/ledgerd/internal/wal/faultfs"
)

// TestPowerCutsNeverLoseAcknowledgedRecords appends, rolls, drops and
// migrates a log on faultfs and cuts the power thousands of times, at every
// kind of point: inside Open while it finishes a roll, removes orphans or
// adopts a legacy file, between a batch's write and its fsync, in the
// middle of sealing a segment and listing the next, and inside DropBefore.
// After every cut the log must open, and replay must give back every
// acknowledged record at its index, followed by at most a prefix of the
// batch that was being written.
func TestPowerCutsNeverLoseAcknowledgedRecords(t *testing.T) {
	seeds, cuts := 24, 250
	if testing.Short() {
		seeds, cuts = 6, 100
	}
	seen := map[string]int{}
	var stats faultfs.Stats
	for seed := 1; seed <= seeds; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) { runLogPowerCuts(t, int64(seed), cuts, seen, &stats) })
	}
	t.Logf("outcomes: %v", seen)
	t.Logf("faults: %+v", stats)
	if t.Failed() {
		return
	}
	for _, want := range []string{"cut-in-open", "cut-in-append", "cut-in-drop", "partial-batch-survived", "batch-lost",
		"legacy-adopted", "rolled", "dropped"} {
		if seen[want] == 0 {
			t.Errorf("never saw %q (saw %v)", want, seen)
		}
	}
	for m := faultfs.DropAll; m <= faultfs.Reordered; m++ {
		if stats.Files[m] == 0 || (m != faultfs.Reordered && stats.Dirs[m] == 0) {
			t.Errorf("no file or directory was cut with mode %s: %+v", m, stats)
		}
	}
	if stats.LostWrites == 0 || stats.LostDirOps == 0 || stats.TornWrites == 0 {
		t.Errorf("the cuts never lost or tore anything: %+v", stats)
	}
}

func runLogPowerCuts(t *testing.T, seed int64, cuts int, seen map[string]int, stats *faultfs.Stats) {
	r := rand.New(rand.NewSource(seed))
	fsys := faultfs.New(seed)
	const dir, legacy = "/srv/data/wal", "/srv/data/ledger.wal"
	var acked []string // acked[i] is the payload of record i+1
	n := 0
	payload := func() string {
		n++
		return fmt.Sprintf("s%d-r%d-%s", seed, n, strings.Repeat("x", r.Intn(48)))
	}
	if seed%3 == 0 {
		// A single-file log from an earlier version, adopted on first open.
		var b []byte
		for i := 0; i < 1+r.Intn(20); i++ {
			p := payload()
			acked = append(acked, p)
			b = wal.Encode(b, []byte(p))
		}
		if err := wal.MkdirAll(fsys, "/srv/data"); err != nil {
			t.Fatal(err)
		}
		if err := fsys.WriteFile(legacy, b); err != nil {
			t.Fatal(err)
		}
	}
	opts := wal.Options{SegmentBytes: 64 + int64(r.Intn(512)), Legacy: legacy}
	keep := uint64(1)     // records from here on must survive; older ones may have been dropped
	var inflight []string // the batch being appended when the power went

	for cut := 0; cut < cuts; cut++ {
		if r.Intn(4) == 0 {
			fsys.CutAfter(r.Intn(8)) // cut inside Open
		}
		opts.FS = fsys
		var got []string
		first := uint64(0)
		l, err := wal.Open(dir, opts, func(i uint64, p []byte) error {
			if first == 0 {
				first = i
			} else if i != first+uint64(len(got)) {
				return fmt.Errorf("record %d after %d", i, first+uint64(len(got))-1)
			}
			got = append(got, string(p))
			return nil
		})
		if err != nil {
			if !errors.Is(err, faultfs.ErrPowerLost) {
				t.Fatalf("cut %d: open: %v", cut, err)
			}
			seen["cut-in-open"]++
			fsys = cutPower(fsys, stats)
			continue
		}
		fsys.CutAfter(-1)
		if seed%3 == 0 && cut == 0 {
			seen["legacy-adopted"]++
		}
		if first == 0 {
			first = l.NextIndex()
		}
		if first != l.FirstIndex() || first > keep {
			t.Fatalf("cut %d: the log starts at record %d (first replayed %d), records from %d must be kept", cut, l.FirstIndex(), first, keep)
		}
		want := acked[min(int(first)-1, len(acked)):]
		if len(got) < len(want) || strings.Join(got[:len(want)], "\n") != strings.Join(want, "\n") {
			t.Fatalf("cut %d: replayed records %d.. = %q, acknowledged %q", cut, first, got, want)
		}
		extra := got[len(want):]
		if len(extra) > len(inflight) || strings.Join(extra, "\n") != strings.Join(inflight[:len(extra)], "\n") {
			t.Fatalf("cut %d: replay has %q past the acknowledged records; the batch in flight was %q", cut, extra, inflight)
		}
		switch {
		case len(inflight) == 0:
		case len(extra) == 0:
			seen["batch-lost"]++
		default:
			seen["partial-batch-survived"]++
		}
		acked, inflight = append(acked, extra...), nil
		if l.NextIndex() != uint64(len(acked))+1 {
			t.Fatalf("cut %d: next index %d after %d records", cut, l.NextIndex(), len(acked))
		}
		if l.Segments() > 1 {
			seen["rolled"]++
		}

		fsys.CutAfter(r.Intn(40))
		for {
			if r.Intn(6) == 0 {
				x := l.FirstIndex() + uint64(r.Int63n(int64(l.NextIndex()-l.FirstIndex()+1)))
				keep = max(keep, x)
				k, err := l.DropBefore(x)
				if err != nil {
					seen["cut-in-drop"]++
					break
				}
				if k > 0 {
					seen["dropped"]++
				}
				continue
			}
			batch := make([]string, 1+r.Intn(4))
			bs := make([][]byte, len(batch))
			for i := range batch {
				batch[i] = payload()
				bs[i] = []byte(batch[i])
			}
			inflight = batch
			if err := l.Append(bs...); err != nil {
				if !errors.Is(err, wal.ErrBroken) || !fsys.Off() {
					t.Fatalf("cut %d: append failed with the power on: %v", cut, err)
				}
				seen["cut-in-append"]++
				break
			}
			acked, inflight = append(acked, batch...), nil
		}
		l.Close()
		fsys = cutPower(fsys, stats)
	}
}

func cutPower(fsys *faultfs.FS, stats *faultfs.Stats) *faultfs.FS {
	img := fsys.PowerCut()
	stats.Add(img.CutStats())
	return img
}

// TestMkdirAllSurvivesPowerCut checks that the directories MkdirAll creates
// are durable, so a file synced into them cannot vanish with its directory.
func TestMkdirAllSurvivesPowerCut(t *testing.T) {
	for seed := int64(1); seed <= 200; seed++ {
		fsys := faultfs.New(seed)
		if err := wal.MkdirAll(fsys, "/a/b/c"); err != nil {
			t.Fatal(err)
		}
		if err := wal.MkdirAll(fsys, "/a/b/c"); err != nil { // already there
			t.Fatal(err)
		}
		if err := fsys.WriteFile("/a/b/c/f", []byte("data")); err != nil {
			t.Fatal(err)
		}
		img := fsys.PowerCut()
		if b, err := img.ReadFile("/a/b/c/f"); err != nil || !bytes.Equal(b, []byte("data")) {
			t.Fatalf("seed %d: after a cut the file reads %q, %v", seed, b, err)
		}
	}
	fsys := faultfs.New(1)
	if err := fsys.WriteFile("/f", nil); err != nil {
		t.Fatal(err)
	}
	if err := wal.MkdirAll(fsys, "/f/g"); err == nil {
		t.Fatal("MkdirAll under a file succeeded")
	}
	if err := wal.MkdirAll(wal.OSFS{}, t.TempDir()+"/x/y"); err != nil {
		t.Fatal(err)
	}
}
