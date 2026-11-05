package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
	"github.com/laiwf94/ledgerd/internal/wal/faultfs"
)

// This file extends the byte-offset truncation and storage-failure tests in
// recovery_test.go from a single log file to segment boundaries and
// snapshots: crash images cut at every byte of a multi-segment log with and
// without snapshots beside it, a failure at each step of a segment roll and
// of a snapshot write, and power cuts at each step of a snapshot write,
// including between the rename and the directory fsync.

// hookFS passes every call on to FS after asking hook, which sees the
// operation, the path (the new name for a rename) and, for writes, the
// bytes. A hook that returns an error makes the call fail without running.
type hookFS struct {
	wal.FS
	mu   sync.Mutex
	hook func(op, path string, data []byte) error
}

func (h *hookFS) set(fn func(op, path string, data []byte) error) {
	h.mu.Lock()
	h.hook = fn
	h.mu.Unlock()
}

func (h *hookFS) call(op, path string, data []byte) error {
	h.mu.Lock()
	fn := h.hook
	h.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(op, path, data)
}

func (h *hookFS) OpenFile(name string, flag int, perm fs.FileMode) (wal.FSFile, error) {
	op := "open"
	if flag&os.O_CREATE != 0 {
		op = "create"
	}
	if err := h.call(op, name, nil); err != nil {
		return nil, err
	}
	f, err := h.FS.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &hookFile{FSFile: f, h: h, name: name}, nil
}

func (h *hookFS) Remove(name string) error {
	if err := h.call("remove", name, nil); err != nil {
		return err
	}
	return h.FS.Remove(name)
}

func (h *hookFS) Rename(oldname, newname string) error {
	if err := h.call("rename", newname, []byte(oldname)); err != nil {
		return err
	}
	return h.FS.Rename(oldname, newname)
}

func (h *hookFS) SyncDir(name string) error {
	if err := h.call("syncdir", name, nil); err != nil {
		return err
	}
	return h.FS.SyncDir(name)
}

type hookFile struct {
	wal.FSFile
	h    *hookFS
	name string
}

func (f *hookFile) Write(p []byte) (int, error) {
	if err := f.h.call("write", f.name, p); err != nil {
		return 0, err
	}
	return f.FSFile.Write(p)
}

func (f *hookFile) Sync() error {
	if err := f.h.call("sync", f.name, nil); err != nil {
		return err
	}
	return f.FSFile.Sync()
}

func (f *hookFile) Truncate(size int64) error {
	if err := f.h.call("truncate", f.name, nil); err != nil {
		return err
	}
	return f.FSFile.Truncate(size)
}

const (
	segSuffix      = ".seg"
	manifestFile   = "MANIFEST"
	snapFileEnding = snapSuffix
	snapTmpEnding  = snapSuffix + snapTmpSuffix
)

func parseSegFile(name string) (uint64, bool) {
	digits, ok := strings.CutSuffix(name, segSuffix)
	if !ok || len(digits) != 20 {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	return n, err == nil && n > 0
}

// settle waits until the writer has finished the batch before and any
// snapshot that batch started. ExpireDue queues behind the batch, and the
// writer only takes it after maybeSnapshot; with nothing due it writes
// nothing.
func settle(t testing.TB, l *Ledger) {
	t.Helper()
	if _, err := l.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-l.snapIdle
	l.snapIdle <- struct{}{}
}

// lastSnapshot returns the number of the newest snapshot the writer has
// marked. It reads it while holding the snapshot token: markSnapshot writes
// lastSnap only while the token is held, and the token comes back by a
// channel send, so this read is ordered after the write. Even after settle
// the writer may start one more snapshot, so a plain read would race.
func lastSnapshot(l *Ledger) uint64 {
	<-l.snapIdle
	defer func() { l.snapIdle <- struct{}{} }()
	return l.lastSnap
}

// crashSeg is one segment of a finished log, parsed.
type crashSeg struct {
	first  uint64
	data   []byte
	ends   []int    // end offset of each record line in data
	snaps  []uint64 // snapshot number of each record, 0 when it is not a snapshot record
	sealed bool
}

func parseCrashSeg(t *testing.T, first uint64, data []byte) crashSeg {
	t.Helper()
	s := crashSeg{first: first, data: data}
	for off := 0; off < len(data); {
		n := bytes.IndexByte(data[off:], '\n')
		if n < 0 {
			t.Fatalf("segment %d ends in a partial line", first)
		}
		line := data[off : off+n]
		off += n + 1
		if bytes.HasPrefix(line, []byte("#seal ")) {
			if off != len(data) {
				t.Fatalf("segment %d has data after its seal", first)
			}
			s.sealed = true
			break
		}
		payload := line[9:]
		var snap uint64
		if bytes.HasPrefix(payload, snapRecordPrefix) {
			r, err := decodeRecord(payload)
			if err != nil {
				t.Fatal(err)
			}
			snap = r.Snap
		}
		s.ends = append(s.ends, off)
		s.snaps = append(s.snaps, snap)
	}
	return s
}

// buildImage assembles a data directory on a fresh faultfs: the given
// segments (the last one possibly cut short), a manifest listing them, the
// given snapshot files and, if tmp is set, a temporary snapshot file.
func buildImage(segs []crashSeg, last []byte, snaps map[uint64][]byte, present []uint64, tmpNum uint64, tmp []byte) (*faultfs.FS, error) {
	img := faultfs.New(1)
	const dir = "/d"
	wdir := filepath.Join(dir, walDir)
	if err := wal.MkdirAll(img, wdir); err != nil {
		return nil, err
	}
	put := func(path string, data []byte) error {
		f, err := img.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(data); err != nil {
			return err
		}
		return f.Sync()
	}
	firsts := make([]uint64, 0, len(segs))
	for i, s := range segs {
		data := s.data
		if i == len(segs)-1 {
			data = last
		}
		if err := put(filepath.Join(wdir, fmt.Sprintf("%020d%s", s.first, segSuffix)), data); err != nil {
			return nil, err
		}
		firsts = append(firsts, s.first)
	}
	if err := put(filepath.Join(wdir, manifestFile), wal.EncodeManifest(firsts)); err != nil {
		return nil, err
	}
	for _, n := range present {
		if err := put(filepath.Join(dir, snapName(n)), snaps[n]); err != nil {
			return nil, err
		}
	}
	if tmp != nil {
		if err := put(filepath.Join(dir, snapName(tmpNum)+snapTmpSuffix), tmp); err != nil {
			return nil, err
		}
	}
	if err := img.SyncDir(wdir); err != nil {
		return nil, err
	}
	return img, img.SyncDir(dir)
}

// TestCrashAtEveryByteAcrossSegmentsAndSnapshots writes a workload with
// segments of 768 bytes and a snapshot every three records, keeping every
// segment and snapshot file it ever produced. It then rebuilds the data
// directory as a crash would leave it with the log cut at every byte offset
// (inside records, inside seals, right after a seal before the next segment
// exists, and in a new empty segment), in three versions:
//
//   - the log alone, every segment from record 1
//   - the two newest snapshots whose records the cut log holds, with the
//     segments before the older of them dropped
//   - the newest such snapshot never renamed (a half-written temporary file
//     instead), the two before it present, and segments dropped behind them
//
// The log alone must recover a state the ledger acknowledged, never an older
// one than a shorter cut, and the whole log the final state. Each version
// with snapshots must recover exactly what the log alone recovers.
func TestCrashAtEveryByteAcrossSegmentsAndSnapshots(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	opts := Options{Dir: dir, Clock: clock.Now, NewID: seqIDs(), SweepInterval: -1, MaxBatch: 1,
		SnapshotEvery: 3, SegmentBytes: 768}
	l, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { l.Close() }()

	segFiles := map[uint64][]byte{}
	snapFiles := map[uint64][]byte{}
	archive := func() {
		ents, err := os.ReadDir(filepath.Join(dir, walDir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			first, ok := parseSegFile(e.Name())
			if !ok {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, walDir, e.Name()))
			if errors.Is(err, fs.ErrNotExist) {
				continue // dropped since the listing
			} else if err != nil {
				t.Fatal(err)
			}
			if old := segFiles[first]; len(data) >= len(old) {
				if !bytes.HasPrefix(data, old) {
					t.Fatalf("segment %d was rewritten, not appended to", first)
				}
				segFiles[first] = data
			}
		}
		nums, _, err := scanSnapshots(wal.OSFS{}, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nums {
			data, err := os.ReadFile(filepath.Join(dir, snapName(n)))
			if errors.Is(err, fs.ErrNotExist) {
				continue // pruned since the listing
			} else if err != nil {
				t.Fatal(err)
			}
			if old, ok := snapFiles[n]; ok && !bytes.Equal(old, data) {
				t.Fatalf("snapshot %d changed after it was written", n)
			}
			snapFiles[n] = data
		}
	}

	ctx := context.Background()
	states := []string{digest(l)}
	ids := 0
	step := func(c Command, k string) string {
		var idem *Idem
		if k != "" {
			idem = key("", k, k)
		}
		_, err := l.Do(ctx, c, idem)
		if c.CreateTransfer != nil || c.Reverse != nil {
			ids++ // an ID is drawn even for a rejected command
		}
		if err != nil && idem == nil {
			return "" // rejected without a key: nothing was written
		}
		settle(t, l)
		archive()
		states = append(states, digest(l))
		return fmt.Sprintf("tr_%06d", ids)
	}
	step(openAcct("bank", "USD", Debit), "")
	step(openAcct("alice", "USD", Credit, "nonneg"), "")
	step(openAcct("shop", "USD", Credit, "nonneg"), "")
	for i := 0; i < 4; i++ {
		k := func(s string) string { return fmt.Sprintf("%s-%d", s, i) }
		step(move(dr("bank", 1000), cr("alice", 1000)), k("fund"))
		step(move(dr("alice", 50000), cr("shop", 50000)), k("overdraft")) // rejected, kept under its key
		auth := step(hold(30, dr("alice", 300), cr("shop", 300)), k("auth"))
		step(hold(10, dr("alice", 200), cr("shop", 200)), k("short"))
		step(postCmd(auth, 250), k("capture"))
		step(reverseCmd(auth, 100), k("refund"))
		clock.Advance(11 * time.Second)
		if n, err := l.ExpireDue(ctx); err != nil || n != 1 {
			t.Fatalf("expired %d, %v", n, err)
		}
		settle(t, l)
		archive()
		states = append(states, digest(l))
		gone := step(hold(30, dr("alice", 50), cr("shop", 50)), k("auth-void"))
		step(voidCmd(gone), "")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	archive()

	// The archive must hold one unbroken log and every snapshot its records name.
	firsts := make([]uint64, 0, len(segFiles))
	for f := range segFiles {
		firsts = append(firsts, f)
	}
	sort.Slice(firsts, func(i, j int) bool { return firsts[i] < firsts[j] })
	var segs []crashSeg
	next := uint64(1)
	var allSnaps []uint64
	for i, f := range firsts {
		s := parseCrashSeg(t, f, segFiles[f])
		if s.first != next {
			t.Fatalf("segment %d follows a segment ending before record %d", s.first, next)
		}
		if s.sealed != (i < len(firsts)-1) {
			t.Fatalf("segment %d: sealed = %v", s.first, s.sealed)
		}
		next += uint64(len(s.ends))
		for _, n := range s.snaps {
			if n != 0 {
				allSnaps = append(allSnaps, n)
			}
		}
		segs = append(segs, s)
	}
	for i, n := range allSnaps {
		if n != uint64(i+1) {
			t.Fatalf("snapshot records %v are not numbered 1, 2, 3 ...", allSnaps)
		}
		if snapFiles[n] == nil {
			t.Fatalf("snapshot %d was never seen on disk", n)
		}
	}
	if len(segs) < 6 || len(allSnaps) < 6 {
		t.Fatalf("the workload produced %d segments and %d snapshots; too few to test boundaries", len(segs), len(allSnaps))
	}

	index := map[string]int{}
	for i, s := range states {
		index[s] = i
	}
	recover := func(img *faultfs.FS) (string, uint64, error) {
		l, err := Open(Options{Dir: "/d", FS: img, Clock: clock.Now, NewID: seqIDs(), SweepInterval: -1,
			SnapshotEvery: -1, SegmentBytes: 768})
		if err != nil {
			return "", 0, err
		}
		defer l.Close()
		return digest(l), l.log.FirstIndex(), nil
	}

	// Each cut is examined on its own, spread over the CPUs; the checks
	// that compare cuts with each other run afterwards, in offset order.
	type mark struct{ num, index uint64 }
	type cut struct {
		k, o int
		want string // what the log alone recovers
		seen []string
		err  error
	}
	examine := func(c *cut) {
		s := segs[c.k]
		where := fmt.Sprintf("segment %d at byte %d", s.first, c.o)
		// Snapshot records the cut log still holds, oldest first, with
		// their record numbers.
		var marks []mark
		for _, p := range segs[:c.k+1] {
			for i, n := range p.snaps {
				if n != 0 && (p.first != s.first || p.ends[i] <= c.o) {
					marks = append(marks, mark{n, p.first + uint64(i)})
				}
			}
		}
		if c.o == len(s.data) && s.sealed {
			c.seen = append(c.seen, "sealed-newest") // Open must finish the roll
		}
		if c.k > 0 && c.o == 0 {
			c.seen = append(c.seen, "empty-newest")
		}
		if s.sealed && c.o > s.ends[len(s.ends)-1] && c.o < len(s.data) {
			c.seen = append(c.seen, "torn-seal")
		}
		img, err := buildImage(segs[:c.k+1], s.data[:c.o], snapFiles, nil, 0, nil)
		if err == nil {
			c.want, _, err = recover(img)
		}
		if err != nil {
			c.err = fmt.Errorf("%s, log only: %v", where, err)
			return
		}
		type variant struct {
			name    string
			present []mark
			tmp     *mark
		}
		var variants []variant
		if n := len(marks); n > 0 {
			variants = append(variants,
				variant{"snapshots", marks[max(0, n-snapKeep):], nil},
				variant{"newest not renamed", marks[max(0, n-1-snapKeep) : n-1], &marks[n-1]})
		}
		for _, v := range variants {
			kept := segs[:c.k+1]
			if len(v.present) > 0 {
				// What DropBefore leaves: sealed segments whose records
				// all precede the oldest kept snapshot's record go.
				oldest := v.present[0].index
				d := 0
				for d < c.k && kept[d].first+uint64(len(kept[d].ends)) <= oldest {
					d++
				}
				kept = kept[d:]
				if d > 0 {
					c.seen = append(c.seen, "segments-dropped")
				}
			}
			var nums []uint64
			for _, m := range v.present {
				nums = append(nums, m.num)
			}
			var tmp []byte
			var tmpNum uint64
			if v.tmp != nil {
				tmpNum = v.tmp.num
				full := snapFiles[tmpNum]
				tmp = full[:(c.o*7919)%len(full)] // a different cut for every offset
				c.seen = append(c.seen, "tmp-left")
			}
			img, err := buildImage(kept, s.data[:c.o], snapFiles, nums, tmpNum, tmp)
			if err != nil {
				c.err = err
				return
			}
			got, firstIndex, err := recover(img)
			if err != nil {
				c.err = fmt.Errorf("%s, %s %v: %v", where, v.name, nums, err)
				return
			}
			if got != c.want {
				c.err = fmt.Errorf("%s, %s %v: snapshot plus tail differs from the log alone:\n%s\nwant\n%s", where, v.name, nums, got, c.want)
				return
			}
			if firstIndex > 1 {
				c.seen = append(c.seen, "recovered-without-record-1")
			}
			c.seen = append(c.seen, v.name)
		}
	}

	var cuts []*cut
	for k, s := range segs {
		for o := 0; o <= len(s.data); o++ {
			// The bytes near a segment's start and end matter most; in
			// short mode the rest are sampled.
			if testing.Short() && o%13 != 0 && o > 64 && len(s.data)-o > 160 {
				continue
			}
			cuts = append(cuts, &cut{k: k, o: o})
		}
	}
	work := make(chan *cut)
	var wg sync.WaitGroup
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				examine(c)
			}
		}()
	}
	for _, c := range cuts {
		work <- c
	}
	close(work)
	wg.Wait()

	seen := map[string]int{}
	last, total := 0, len(cuts)
	for _, c := range cuts {
		if c.err != nil {
			t.Fatal(c.err)
		}
		for _, s := range c.seen {
			seen[s]++
		}
		where := fmt.Sprintf("segment %d at byte %d", segs[c.k].first, c.o)
		i, ok := index[c.want]
		if !ok {
			t.Fatalf("%s, log only: recovered a state that was never acknowledged:\n%s", where, c.want)
		}
		if i < last {
			t.Fatalf("%s, log only: recovered state %d after a shorter cut recovered %d", where, i, last)
		}
		last = i
	}
	if last != len(states)-1 {
		t.Fatalf("the whole log recovered state %d of %d", last, len(states)-1)
	}
	t.Logf("%d segments, %d snapshots, %d cuts: %v", len(segs), len(allSnaps), total, seen)
	for _, want := range []string{"sealed-newest", "empty-newest", "torn-seal", "segments-dropped", "tmp-left",
		"recovered-without-record-1", "snapshots", "newest not renamed"} {
		if seen[want] == 0 {
			t.Errorf("no crash image covered %q", want)
		}
	}
}

// rollFailures are the steps of a segment roll, in order, each as a hook
// that fails that step once armed.
func rollFailures(walPath string) map[string]func() func(op, path string, data []byte) error {
	isSeg := func(p string) bool { _, ok := parseSegFile(filepath.Base(p)); return ok }
	boom := errors.New("input/output error")
	once := func(match func(op, path string, data []byte) bool) func() func(string, string, []byte) error {
		return func() func(string, string, []byte) error {
			var mu sync.Mutex
			fired := false
			return func(op, path string, data []byte) error {
				mu.Lock()
				defer mu.Unlock()
				if !fired && match(op, path, data) {
					fired = true
					return boom
				}
				return nil
			}
		}
	}
	// after returns a matcher that fires on the first match of second once
	// first has been seen.
	after := func(first, second func(op, path string, data []byte) bool) func(op, path string, data []byte) bool {
		armed := false
		return func(op, path string, data []byte) bool {
			if armed && second(op, path, data) {
				return true
			}
			if first(op, path, data) {
				armed = true
			}
			return false
		}
	}
	sealWrite := func(op, path string, data []byte) bool {
		return op == "write" && isSeg(path) && bytes.HasPrefix(data, []byte("#seal "))
	}
	create := func(op, path string, _ []byte) bool { return op == "create" && isSeg(path) }
	manifestWrite := func(op, path string, _ []byte) bool {
		return op == "write" && filepath.Base(path) == manifestFile+".tmp"
	}
	manifestRename := func(op, path string, _ []byte) bool {
		return op == "rename" && filepath.Base(path) == manifestFile
	}
	return map[string]func() func(op, path string, data []byte) error{
		"seal write":         once(sealWrite),
		"seal fsync":         once(after(sealWrite, func(op, path string, _ []byte) bool { return op == "sync" && isSeg(path) })),
		"create":             once(create),
		"new fsync":          once(after(create, func(op, path string, _ []byte) bool { return op == "sync" && isSeg(path) })),
		"dir fsync":          once(after(create, func(op, path string, _ []byte) bool { return op == "syncdir" && path == walPath })),
		"manifest":           once(manifestWrite),
		"rename":             once(manifestRename),
		"manifest dir fsync": once(after(manifestRename, func(op, path string, _ []byte) bool { return op == "syncdir" && path == walPath })),
	}
}

// TestStorageFailureWhileRolling fails each step of sealing a segment and
// starting the next. The batch that needed the new segment is refused, the
// ledger stops accepting writes, and after a restart the client's retry runs
// the command once, the log rolls again and the directory verifies sound.
func TestStorageFailureWhileRolling(t *testing.T) {
	for name := range rollFailures("") {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			e := newEnv(t, true)
			e.l.Close()
			hfs := &hookFS{FS: wal.OSFS{}}
			var logs bytes.Buffer
			e.opts.FS = hfs
			e.opts.SegmentBytes = 1024
			e.opts.SnapshotEvery = -1
			e.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			e.open()
			e.standard()
			ctx := context.Background()
			hfs.set(rollFailures(filepath.Join(e.dir, walDir))[name]())
			segments := e.l.log.Segments()
			var paid int64
			var failed Command
			var failedKey string
			for i := 0; ; i++ {
				if i > 50 {
					t.Fatal("the log never rolled")
				}
				c := move(dr("equity", 10), cr("alice", 10))
				k := fmt.Sprint("pay-", i)
				_, err := e.l.Do(ctx, c, key("", k, k))
				if err == nil {
					paid += 10
					continue
				}
				if le, _ := err.(*Error); le == nil || le.Code != CodeUnavailable {
					t.Fatalf("err = %v", err)
				}
				failed, failedKey = c, k
				break
			}
			if e.l.Err() == nil || !strings.Contains(logs.String(), "stopped accepting writes") {
				t.Fatalf("failure not surfaced: %v", e.l.Err())
			}
			hfs.set(nil)
			if _, err := e.l.Do(ctx, move(dr("equity", 1), cr("bob", 1)), nil); err == nil {
				t.Fatal("write accepted after a failed roll")
			}
			e.wantBalance("alice", paid, 0, paid)
			e.l.Close()

			e.opts.FS = nil
			e.open()
			e.wantBalance("alice", paid, 0, paid)
			out, err := e.l.Do(ctx, failed, key("", failedKey, failedKey))
			if err != nil {
				t.Fatal(err)
			}
			if out.Replayed {
				t.Fatal("the roll comes before the batch is written; the retry should execute")
			}
			paid += 10
			for i := 0; i < 20; i++ {
				e.do(move(dr("equity", 1), cr("bob", 1)))
			}
			if e.l.log.Segments() <= segments {
				t.Fatalf("%d segments after the restart, %d before; the log stopped rolling", e.l.log.Segments(), segments)
			}
			e.wantBalance("alice", paid, 0, paid)
			e.wantBalance("bob", 20, 0, 20)
			e.invariants()
			before := digest(e.l)
			e.restart()
			if digest(e.l) != before {
				t.Fatal("a restart changed the state")
			}
			e.l.Close()
			rep, err := Verify(e.dir)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Status != StatusSound {
				t.Fatalf("verify: %s %+v", rep.Status, rep)
			}
		})
	}
}

// snapshotSteps are the file system calls of writing one snapshot and
// dropping the segments it makes redundant, in order. Each matcher fires on
// its step for the snapshot armed.
func snapshotSteps(dir string) []struct {
	name  string
	match func(op, path string) bool
} {
	isTmp := func(p string) bool { return strings.HasSuffix(p, snapTmpEnding) }
	isSnap := func(p string) bool { return strings.HasSuffix(p, snapFileEnding) && filepath.Dir(p) == dir }
	isSeg := func(p string) bool { _, ok := parseSegFile(filepath.Base(p)); return ok }
	return []struct {
		name  string
		match func(op, path string) bool
	}{
		{"create temporary file", func(op, p string) bool { return op == "create" && isTmp(p) }},
		{"write", func(op, p string) bool { return op == "write" && isTmp(p) }},
		{"fsync", func(op, p string) bool { return op == "sync" && isTmp(p) }},
		{"rename", func(op, p string) bool { return op == "rename" && isSnap(p) }},
		{"directory fsync", func(op, p string) bool { return op == "syncdir" && p == dir }},
		{"prune", func(op, p string) bool { return op == "remove" && isSnap(p) }},
		{"drop segments", func(op, p string) bool { return op == "remove" && isSeg(p) }},
	}
}

// TestSnapshotStorageFailures fails each step of writing a snapshot once
// segments are already being dropped behind snapshots. The ledger keeps
// accepting writes, drops no segment on the strength of the failed
// snapshot, takes the next snapshot as usual, restarts to the same state and
// verifies sound.
func TestSnapshotStorageFailures(t *testing.T) {
	for _, st := range snapshotSteps("") {
		t.Run(strings.ReplaceAll(st.name, " ", "_"), func(t *testing.T) {
			e := newEnv(t, true)
			e.l.Close()
			hfs := &hookFS{FS: wal.OSFS{}}
			e.opts.FS = hfs
			e.opts.SegmentBytes = 1024
			e.opts.SnapshotEvery = 4
			e.opts.MaxBatch = 1
			e.opts.Logger = slog.New(slog.NewJSONHandler(new(bytes.Buffer), nil))
			e.open()
			e.standard()
			for e.l.mSegDropped.Value() == 0 || lastSnapshot(e.l) < 3 {
				e.do(move(dr("equity", 1), cr("alice", 1)))
				settle(t, e.l)
			}
			steps := snapshotSteps(e.dir)
			var step func(op, path string) bool
			for _, s := range steps {
				if s.name == st.name {
					step = s.match
				}
			}
			boom := errors.New("no space left on device")
			var mu sync.Mutex
			fired, armed := false, false
			hfs.set(func(op, path string, _ []byte) error {
				mu.Lock()
				defer mu.Unlock()
				if strings.HasSuffix(path, snapTmpEnding) && op == "create" {
					armed = true // the next snapshot's steps follow
				}
				if armed && !fired && step(op, path) {
					fired = true
					return boom
				}
				return nil
			})
			failedBefore := e.l.mSnapFailed.Value()
			for i := 0; ; i++ {
				first := e.l.log.FirstIndex()
				e.do(move(dr("equity", 1), cr("alice", 1)))
				settle(t, e.l)
				mu.Lock()
				done := fired
				mu.Unlock()
				if done {
					want := failedBefore + 1
					if st.name == "drop segments" {
						want = failedBefore // the snapshot itself was written
					}
					if e.l.mSnapFailed.Value() != want {
						t.Fatalf("snapshot failures %d, want %d", e.l.mSnapFailed.Value(), want)
					}
					if st.name != "drop segments" && e.l.log.FirstIndex() != first {
						t.Fatalf("segments were dropped behind a snapshot that failed (first record %d, was %d)", e.l.log.FirstIndex(), first)
					}
					break
				}
				if i > 40 {
					t.Fatalf("the step %q never ran", st.name)
				}
			}
			if e.l.Err() != nil {
				t.Fatal(e.l.Err())
			}
			// The next snapshot succeeds and drops segments again.
			ok, dropped := e.l.mSnapOK.Value(), e.l.mSegDropped.Value()
			for e.l.mSnapOK.Value() == ok || e.l.mSegDropped.Value() == dropped {
				e.do(move(dr("equity", 1), cr("bob", 1)))
				settle(t, e.l)
			}
			e.invariants()
			before := digest(e.l)
			e.restart()
			if got := digest(e.l); got != before {
				t.Fatalf("a restart changed the state:\n%s\nwant\n%s", got, before)
			}
			e.l.Close()
			rep, err := Verify(e.dir)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Status != StatusSound {
				t.Fatalf("verify: %s %+v", rep.Status, rep)
			}
		})
	}
}

// TestPowerCutsInsideSnapshotWrites cuts the power right before each step
// of writing a snapshot and dropping segments behind it, and in particular
// between the snapshot's rename and the fsync of its directory: the rename
// may or may not survive, and the temporary file's own directory entry was
// never synced either. Whatever comes back must open to exactly the state
// acknowledged before the cut, and the ledger must carry on from it.
func TestPowerCutsInsideSnapshotWrites(t *testing.T) {
	seeds := 48
	if testing.Short() {
		seeds = 12
	}
	const dir = "/var/lib/ledgerd"
	for _, st := range snapshotSteps(dir) {
		t.Run(strings.ReplaceAll(st.name, " ", "_"), func(t *testing.T) {
			seen := map[string]int{}
			for seed := 1; seed <= seeds; seed++ {
				runSnapshotCut(t, int64(seed), dir, st.match, seen)
			}
			t.Logf("%v", seen)
			if seen["cut"] != seeds {
				t.Fatalf("%d cuts, want %d", seen["cut"], seeds)
			}
			if st.name == "directory fsync" && (seen["rename-survived"] == 0 || seen["rename-lost"] == 0) {
				t.Errorf("a cut between rename and directory fsync must sometimes keep and sometimes lose the rename: %v", seen)
			}
		})
	}
}

func runSnapshotCut(t *testing.T, seed int64, dir string, match func(op, path string) bool, seen map[string]int) {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	clock := newClock()
	ff := faultfs.New(seed)
	hfs := &hookFS{FS: ff}
	opts := Options{Dir: dir, FS: hfs, Clock: clock.Now, NewID: seqIDs(), SweepInterval: -1, MaxBatch: 1,
		SnapshotEvery: 2 + r.Intn(3), SegmentBytes: 512 + int64(r.Intn(512)),
		Logger: slog.New(slog.NewJSONHandler(new(bytes.Buffer), nil))}
	l, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if l != nil {
			l.Close()
		}
	}()
	ctx := context.Background()
	accts := []string{"a", "b", "c", "d"}
	for _, a := range accts {
		if _, err := l.Do(ctx, openAcct(a, "USD", Credit), nil); err != nil {
			t.Fatal(err)
		}
		settle(t, l) // so that no snapshot from here runs into the cut below
	}
	armAt := 4 + r.Intn(30) // past a few snapshots, so pruning and drops happen
	var mu sync.Mutex
	var recovered *faultfs.FS
	var snapNum uint64
	inSnap := false
	hfs.set(func(op, path string, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if recovered != nil {
			return faultfs.ErrPowerLost
		}
		if op == "create" && strings.HasSuffix(path, snapTmpEnding) {
			fmt.Sscanf(filepath.Base(path), snapPrefix+"%020d", &snapNum)
			inSnap = snapNum >= uint64(armAt)/2
		}
		if inSnap && match(op, path) {
			recovered = ff.PowerCut()
			return faultfs.ErrPowerLost
		}
		return nil
	})
	var want string
	cut := func() bool { mu.Lock(); defer mu.Unlock(); return recovered != nil }
	for i := 0; !cut(); i++ {
		if i > 400 {
			t.Fatalf("seed %d: the step never ran", seed)
		}
		from, to := accts[r.Intn(len(accts))], accts[r.Intn(len(accts))]
		k := fmt.Sprint("k", i)
		n := int64(1 + r.Intn(50))
		if _, err := l.Do(ctx, move(dr(from, n), cr(to, n)), key("", k, k)); err != nil {
			var le *Error
			if !errors.As(err, &le) || le.Code == CodeUnavailable {
				t.Fatalf("seed %d: %v", seed, err)
			}
		}
		want = digest(l)
		settle(t, l)
	}
	l.Close()
	l = nil
	seen["cut"]++

	name := filepath.Join(dir, snapName(snapNum))
	if _, err := recovered.Stat(name); err == nil {
		seen["rename-survived"]++
	} else {
		seen["rename-lost"]++
	}
	if _, err := recovered.Stat(name + snapTmpSuffix); err == nil {
		seen["tmp-left"]++
	}
	opts.FS = recovered
	l2, err := Open(opts)
	if err != nil {
		t.Fatalf("seed %d: open after the cut: %v", seed, err)
	}
	if got := digest(l2); got != want {
		l2.Close()
		t.Fatalf("seed %d: recovered\n%s\nwant\n%s", seed, got, want)
	}
	// It carries on: more writes, snapshots past the one cut short, a restart.
	for i := 0; i < 10; i++ {
		if _, err := l2.Do(ctx, move(dr("a", 1), cr("b", 1)), nil); err != nil {
			t.Fatalf("seed %d: after recovery: %v (%v)", seed, err, l2.Err())
		}
	}
	settle(t, l2)
	if lastSnapshot(l2) <= snapNum {
		t.Fatalf("seed %d: no snapshot after %d", seed, snapNum)
	}
	after := digest(l2)
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	l3, err := Open(opts)
	if err != nil {
		t.Fatalf("seed %d: reopen: %v", seed, err)
	}
	defer l3.Close()
	if got := digest(l3); got != after {
		t.Fatalf("seed %d: reopen changed the state", seed)
	}
	if v := l3.CheckInvariants(); len(v) > 0 {
		t.Fatalf("seed %d: %v", seed, v)
	}
}
