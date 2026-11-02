package wal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nop(uint64, []byte) error { return nil }

// collect opens the log in dir and returns its records as "index:payload".
func collect(t *testing.T, dir string, opts Options) []string {
	t.Helper()
	var got []string
	l, err := Open(dir, opts, func(i uint64, p []byte) error { got = append(got, fmt.Sprintf("%d:%s", i, p)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	return got
}

func activePath(t *testing.T, dir string) string {
	t.Helper()
	paths, err := SegmentPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	return paths[len(paths)-1]
}

func TestAppendAndReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "wal")
	l, err := Open(dir, Options{}, func(uint64, []byte) error { t.Fatal("empty log replayed a record"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if l.NextIndex() != 1 || l.FirstIndex() != 1 || l.Segments() != 1 {
		t.Fatalf("new log: next %d first %d segments %d", l.NextIndex(), l.FirstIndex(), l.Segments())
	}
	if err := l.Append([]byte("a"), []byte(`{"b":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := l.Append([]byte("c")); err != nil {
		t.Fatal(err)
	}
	if l.Syncs() != 2 || l.Err() != nil || l.NextIndex() != 4 {
		t.Fatalf("syncs=%d err=%v next=%d", l.Syncs(), l.Err(), l.NextIndex())
	}
	size := l.Size()
	l.Close()
	if got := collect(t, dir, Options{}); strings.Join(got, "|") != `1:a|2:{"b":1}|3:c` {
		t.Fatalf("replayed %q", got)
	}
	st, _ := os.Stat(filepath.Join(dir, segName(1)))
	if st.Size() != size || st.Mode().Perm() != 0o600 {
		t.Fatalf("size %d (want %d), mode %v", st.Size(), size, st.Mode())
	}
	var read []string
	next, err := Read(dir, func(i uint64, p []byte) error { read = append(read, fmt.Sprintf("%d:%s", i, p)); return nil })
	if err != nil || next != 4 || len(read) != 3 {
		t.Fatalf("Read: next %d, %q, %v", next, read, err)
	}
}

func TestPayloadValidation(t *testing.T) {
	l, err := Open(t.TempDir(), Options{}, nop)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Append([]byte("a\nb")); err == nil {
		t.Fatal("newline accepted")
	}
	if err := l.Append(make([]byte, MaxRecord+1)); err == nil {
		t.Fatal("oversize record accepted")
	}
	if l.Err() != nil || l.Size() != 0 || l.NextIndex() != 1 {
		t.Fatal("a rejected payload must not touch the file")
	}
}

func TestTornTailIsDropped(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, Options{}, nop)
	l.Append([]byte("one"), []byte("two"))
	l.Close()
	path := activePath(t, dir)
	good, _ := os.ReadFile(path)
	torn := []string{"0", "deadbeef {\"x\"", "00000000 bad\n", "#seal 1 2", "#seal 1 2 34 00000000 00000000\n"}
	for _, tail := range torn {
		os.WriteFile(path, append(append([]byte{}, good...), tail...), 0o600)
		if got := collect(t, dir, Options{}); len(got) != 2 {
			t.Fatalf("tail %q: replayed %q", tail, got)
		}
		if b, _ := os.ReadFile(path); !bytes.Equal(b, good) {
			t.Fatalf("tail %q was not truncated", tail)
		}
	}
}

func TestCorruptionInTheMiddleIsFatal(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, Options{}, nop)
	l.Append([]byte("one"), []byte("two"), []byte("three"))
	l.Close()
	path := activePath(t, dir)
	good, _ := os.ReadFile(path)
	cases := map[string]func([]byte) []byte{
		"flipped payload byte": func(b []byte) []byte { b[10] ^= 1; return b },
		"bad checksum hex":     func(b []byte) []byte { b[0] = 'z'; return b },
		"missing separator":    func(b []byte) []byte { b[8] = 'x'; return b },
		"short line":           func(b []byte) []byte { return append([]byte("abc\n"), b...) },
		"oversize line":        func(b []byte) []byte { return append(bytes.Repeat([]byte("a"), MaxRecord+100), b...) },
		"seal in the middle":   func(b []byte) []byte { return append(encodeFooter(footer{first: 1}), b...) },
		"wrong seal at the end": func(b []byte) []byte {
			return append(b, encodeFooter(footer{first: 1, count: 2, bytes: int64(len(b))})...)
		},
	}
	for name, mutate := range cases {
		os.WriteFile(path, mutate(append([]byte{}, good...)), 0o600)
		_, err := Open(dir, Options{}, nop)
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
	os.WriteFile(path, good, 0o600)
	_, err := Open(dir, Options{}, func(i uint64, p []byte) error {
		if string(p) == "two" {
			return errors.New("bad state")
		}
		return nil
	})
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "bad state") {
		t.Fatalf("apply error: %v", err)
	}
}

// faultyFile fails writes or fsyncs while the matching flag is set.
type faultyFile struct {
	File
	failWrite, failSync bool
	partial             bool
}

func (f *faultyFile) Write(p []byte) (int, error) {
	if f.failWrite {
		if f.partial {
			n, _ := f.File.Write(p[:len(p)/2])
			return n, errors.New("disk full")
		}
		return 0, errors.New("disk full")
	}
	return f.File.Write(p)
}

func (f *faultyFile) Sync() error {
	if f.failSync {
		return errors.New("I/O error")
	}
	return f.File.Sync()
}

func TestWriteAndSyncFailuresBreakTheLog(t *testing.T) {
	for _, mode := range []string{"write", "partial-write", "sync"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			var ff *faultyFile
			l, err := Open(dir, Options{WrapFile: func(f File) File { ff = &faultyFile{File: f}; return ff }}, nop)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Append([]byte("ok")); err != nil {
				t.Fatal(err)
			}
			ff.failWrite = mode != "sync"
			ff.partial = mode == "partial-write"
			ff.failSync = mode == "sync"
			if err := l.Append([]byte("lost")); !errors.Is(err, ErrBroken) {
				t.Fatalf("err = %v", err)
			}
			ff.failWrite, ff.failSync = false, false
			if err := l.Append([]byte("after")); !errors.Is(err, ErrBroken) {
				t.Fatalf("log accepted a write after a failure: %v", err)
			}
			if _, err := l.DropBefore(10); !errors.Is(err, ErrBroken) {
				t.Fatalf("drop on a broken log: %v", err)
			}
			if l.Err() == nil {
				t.Fatal("Err() is nil")
			}
			l.Close()
			// Recovery keeps the acknowledged record and drops any torn bytes.
			got := collect(t, dir, Options{})
			if len(got) < 1 || got[0] != "1:ok" || (mode != "sync" && len(got) != 1) {
				t.Fatalf("recovered %q", got)
			}
		})
	}
}

// fill appends n records, one per batch, of about 20 bytes each.
func fill(t *testing.T, l *Log, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if err := l.Append([]byte(fmt.Sprintf("record-%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSegmentsRollAndReplay(t *testing.T) {
	dir := t.TempDir()
	opts := Options{SegmentBytes: 100}
	l, err := Open(dir, opts, nop)
	if err != nil {
		t.Fatal(err)
	}
	fill(t, l, 0, 20) // 20-byte lines, so 5 per segment
	// A batch never spans segments, even one larger than the limit.
	big := [][]byte{[]byte("b0"), []byte(strings.Repeat("x", 200)), []byte("b2")}
	if err := l.Append(big...); err != nil {
		t.Fatal(err)
	}
	fill(t, l, 20, 3)
	if l.Segments() != 6 || l.NextIndex() != 27 {
		t.Fatalf("segments %d next %d", l.Segments(), l.NextIndex())
	}
	size := l.Size()
	l.Close()

	paths, err := SegmentPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	wantFirst := []uint64{1, 6, 11, 16, 21, 24}
	for i, p := range paths {
		if filepath.Base(p) != segName(wantFirst[i]) {
			t.Fatalf("segment %d is %s", i, filepath.Base(p))
		}
		data, _ := os.ReadFile(p)
		total += int64(len(data))
		// Every segment but the newest is sealed and checks out on its own.
		s, err := scanSegment(bytes.NewReader(data), wantFirst[i], nop)
		if err != nil || s.sealed != (i < len(paths)-1) || s.end != int64(len(data)) {
			t.Fatalf("segment %d: %+v %v", i, s, err)
		}
	}
	if total != size {
		t.Fatalf("Size() %d, files hold %d", size, total)
	}
	got := collect(t, dir, opts)
	if len(got) != 26 || got[0] != "1:record-000" || got[20] != "21:b0" || got[25] != "26:record-022" {
		t.Fatalf("replayed %q", got)
	}
	// A reopened log keeps rolling where it left off.
	l, _ = Open(dir, opts, nop)
	fill(t, l, 23, 10)
	l.Close()
	if got := collect(t, dir, opts); len(got) != 36 || got[35] != "36:record-032" {
		t.Fatalf("after reopening: %q", got)
	}
}

func TestDamagedSealedSegmentIsFatal(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, Options{SegmentBytes: 60}, nop)
	fill(t, l, 0, 6) // segments of 3 records
	l.Close()
	paths, _ := SegmentPaths(dir)
	sealed := paths[0]
	good, _ := os.ReadFile(sealed)
	try := func(what string, data []byte) {
		t.Helper()
		os.WriteFile(sealed, data, 0o600)
		if _, err := Open(dir, Options{}, nop); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: err = %v", what, err)
		}
		if _, err := Read(dir, nop); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: Read err = %v", what, err)
		}
	}
	for i := range good {
		for _, bit := range []byte{0x01, 0x80} {
			data := append([]byte{}, good...)
			data[i] ^= bit
			try(fmt.Sprintf("bit %#x of byte %d", bit, i), data)
		}
	}
	for n := 0; n < len(good); n++ {
		try(fmt.Sprintf("cut at %d", n), good[:n])
	}
	try("data after the seal", append(append([]byte{}, good...), Encode(nil, []byte("x"))...))
	try("torn data after the seal", append(append([]byte{}, good...), 'x'))
	os.WriteFile(sealed, good, 0o600)
	if got := collect(t, dir, Options{}); len(got) != 6 {
		t.Fatalf("restored segment: %q", got)
	}
}

func TestManifestAndSegmentSetDamage(t *testing.T) {
	build := func(t *testing.T) string {
		dir := t.TempDir()
		l, _ := Open(dir, Options{SegmentBytes: 60}, nop)
		fill(t, l, 0, 9) // segments 1, 4, 7
		l.Close()
		return dir
	}
	cases := map[string]func(dir string){
		"missing segment": func(dir string) { os.Remove(filepath.Join(dir, segName(4))) },
		"missing newest":  func(dir string) { os.Remove(filepath.Join(dir, segName(7))) },
		"flipped manifest": func(dir string) {
			p := filepath.Join(dir, manifestName)
			b, _ := os.ReadFile(p)
			b[len(manifestMagic)+1] ^= 1
			os.WriteFile(p, b, 0o600)
		},
		"truncated manifest": func(dir string) {
			p := filepath.Join(dir, manifestName)
			b, _ := os.ReadFile(p)
			os.WriteFile(p, b[:len(b)-3], 0o600)
		},
		"gap in the manifest": func(dir string) { writeManifest(OSFS{}, dir, []uint64{1, 7}, nil) },
		"unlisted segment inside the log": func(dir string) {
			writeManifest(OSFS{}, dir, []uint64{1, 7}, nil)
			os.Rename(filepath.Join(dir, segName(4)), filepath.Join(dir, segName(5)))
		},
		"non-empty segment past the end": func(dir string) {
			os.WriteFile(filepath.Join(dir, segName(50)), Encode(nil, []byte("x")), 0o600)
		},
		"segments without a manifest": func(dir string) { os.Remove(filepath.Join(dir, manifestName)) },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := build(t)
			damage(dir)
			if _, err := Open(dir, Options{}, nop); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	for _, bad := range []string{"", "x\n", manifestMagic + "\ncrc 00000000\n", "ledgerd-wal 1\n1\n"} {
		if _, err := decodeManifest([]byte(bad)); err == nil {
			t.Errorf("manifest %q accepted", bad)
		}
	}
	for _, firsts := range [][]uint64{{0}, {3, 3}, {5, 2}} {
		if _, err := decodeManifest(encodeManifest(firsts)); err == nil {
			t.Errorf("manifest %v accepted", firsts)
		}
	}
	if _, err := Read(t.TempDir(), nop); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read of an empty directory: %v", err)
	}
}

// The steps of a roll and a drop, each interrupted by a crash, must reopen
// to exactly the acknowledged records.
func TestInterruptedRollAndDrop(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, Options{SegmentBytes: 60}, nop)
	fill(t, l, 0, 5) // segment 1 sealed with 3 records, segment 4 holds 2
	l.Close()
	want := collect(t, dir, Options{SegmentBytes: 60})

	// Sealed, but the next segment was never listed: Open finishes the roll.
	crash := t.TempDir()
	copyLog(t, dir, crash)
	os.Remove(filepath.Join(crash, segName(4)))
	writeManifest(OSFS{}, crash, []uint64{1}, nil)
	os.WriteFile(filepath.Join(crash, segName(4)), nil, 0o600) // created, not listed
	got := collect(t, crash, Options{})
	if len(got) != 3 || activePath(t, crash) != filepath.Join(crash, segName(4)) {
		t.Fatalf("after a crash mid-roll: %q, active %s", got, activePath(t, crash))
	}
	l, _ = Open(crash, Options{}, nop)
	fill(t, l, 3, 2)
	l.Close()
	if got := collect(t, crash, Options{}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after finishing the roll: %q, want %q", got, want)
	}

	// A seal torn halfway: the segment is still the active one.
	crash = t.TempDir()
	copyLog(t, dir, crash)
	os.Remove(filepath.Join(crash, segName(4)))
	writeManifest(OSFS{}, crash, []uint64{1}, nil)
	seg1 := filepath.Join(crash, segName(1))
	b, _ := os.ReadFile(seg1)
	os.WriteFile(seg1, b[:len(b)-5], 0o600)
	if got := collect(t, crash, Options{}); len(got) != 3 {
		t.Fatalf("after a torn seal: %q", got)
	}
	if paths, _ := SegmentPaths(crash); len(paths) != 1 {
		t.Fatalf("a torn seal started a new segment: %v", paths)
	}

	// Manifest replaced by a drop, files not yet removed.
	crash = t.TempDir()
	copyLog(t, dir, crash)
	writeManifest(OSFS{}, crash, []uint64{4}, nil)
	if got := collect(t, crash, Options{}); strings.Join(got, ",") != "4:record-003,5:record-004" {
		t.Fatalf("after a crash mid-drop: %q", got)
	}
	if _, err := os.Stat(filepath.Join(crash, segName(1))); !os.IsNotExist(err) {
		t.Fatal("the dropped segment was left behind")
	}

	// Created, but the manifest was never written.
	crash = t.TempDir()
	os.WriteFile(filepath.Join(crash, segName(1)), nil, 0o600)
	l, err := Open(crash, Options{}, nop)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	os.WriteFile(filepath.Join(crash, manifestName+".tmp"), []byte("half"), 0o600)
	collect(t, crash, Options{})
	if _, err := os.Stat(filepath.Join(crash, manifestName+".tmp")); !os.IsNotExist(err) {
		t.Fatal("a leftover temporary manifest was kept")
	}
}

func copyLog(t *testing.T, src, dst string) {
	t.Helper()
	entries, _ := os.ReadDir(src)
	for _, e := range entries {
		if e.Name() == lockName {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600)
	}
}

func TestDropBefore(t *testing.T) {
	dir := t.TempDir()
	opts := Options{SegmentBytes: 60}
	l, _ := Open(dir, opts, nop)
	fill(t, l, 0, 10) // segments 1, 4, 7, 10
	before := l.Size()
	if n, err := l.DropBefore(3); n != 0 || err != nil {
		t.Fatalf("dropped %d (%v) though record 3 is still needed", n, err)
	}
	if n, err := l.DropBefore(8); n != 2 || err != nil {
		t.Fatalf("dropped %d, %v", n, err)
	}
	if l.FirstIndex() != 7 || l.Segments() != 2 || l.Size() >= before {
		t.Fatalf("first %d segments %d size %d", l.FirstIndex(), l.Segments(), l.Size())
	}
	// The newest segment is never dropped, even when nothing in it is needed.
	if n, _ := l.DropBefore(1000); n != 1 || l.FirstIndex() != 10 || l.Segments() != 1 {
		t.Fatalf("dropped %d, first %d", n, l.FirstIndex())
	}
	fill(t, l, 10, 1)
	l.Close()
	if got := collect(t, dir, opts); strings.Join(got, ",") != "10:record-009,11:record-010" {
		t.Fatalf("after dropping: %q", got)
	}
	if segs, _ := listSegments(OSFS{}, dir); len(segs) != 1 {
		t.Fatalf("files left: %v", segs)
	}

	// A drop whose manifest cannot be written changes nothing.
	dir = t.TempDir()
	var fail bool
	l, _ = Open(dir, Options{SegmentBytes: 60, WrapFile: func(f File) File { return &faultyFile{File: f, failWrite: fail} }}, nop)
	fill(t, l, 0, 7)
	fail = true
	if n, err := l.DropBefore(5); n != 0 || err == nil || l.Err() != nil {
		t.Fatalf("failed drop: %d, %v, broken %v", n, err, l.Err())
	}
	fail = false
	fill(t, l, 7, 1)
	l.Close()
	if got := collect(t, dir, Options{}); len(got) != 8 || got[0] != "1:record-000" {
		t.Fatalf("after a failed drop: %q", got)
	}
}

func TestRollFailureBreaksTheLog(t *testing.T) {
	for step := 1; step <= 4; step++ {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			dir := t.TempDir()
			var files []*faultyFile
			l, err := Open(dir, Options{SegmentBytes: 60, WrapFile: func(f File) File {
				ff := &faultyFile{File: f}
				files = append(files, ff)
				return ff
			}}, nop)
			if err != nil {
				t.Fatal(err)
			}
			fill(t, l, 0, 3)
			n := len(files)
			active := files[n-1]
			// The roll writes the seal, fsyncs it, then creates, fsyncs and
			// lists the next segment.
			switch step {
			case 1:
				active.failWrite = true
			case 2:
				active.failSync = true
			}
			wrap := l.opts.WrapFile
			l.opts.WrapFile = func(f File) File {
				ff := wrap(f).(*faultyFile)
				ff.failSync = step == 3 && len(files) == n+1 // the new segment
				ff.failWrite = step == 4                     // the manifest
				return ff
			}
			if err := l.Append([]byte("rolls")); !errors.Is(err, ErrBroken) {
				t.Fatalf("append during a failed roll: %v", err)
			}
			l.Close()
			if got := collect(t, dir, Options{}); len(got) != 3 {
				t.Fatalf("recovered %q", got)
			}
		})
	}
}

func TestLegacyLogIsAdopted(t *testing.T) {
	var data []byte
	for _, r := range []string{"h", "a", "b"} {
		data = Encode(data, []byte(r))
	}
	root := t.TempDir()
	legacy := filepath.Join(root, "ledger.wal")
	dir := filepath.Join(root, "wal")
	opts := Options{Legacy: legacy}

	os.WriteFile(legacy, append(append([]byte{}, data...), "0badf"...), 0o600) // with a torn tail
	if got := collect(t, dir, opts); strings.Join(got, ",") != "1:h,2:a,3:b" {
		t.Fatalf("adopted %q", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("the legacy file is still there")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, segName(1))); !bytes.Equal(b, data) {
		t.Fatal("the adopted segment differs from the legacy log")
	}

	// Interrupted after linking, before the manifest.
	root = t.TempDir()
	legacy, dir = filepath.Join(root, "ledger.wal"), filepath.Join(root, "wal")
	os.WriteFile(legacy, data, 0o600)
	os.MkdirAll(dir, 0o700)
	os.Link(legacy, filepath.Join(dir, segName(1)))
	if got := collect(t, dir, Options{Legacy: legacy}); len(got) != 3 {
		t.Fatalf("after an interrupted link: %q", got)
	}

	// Interrupted after the manifest, before the legacy name was removed.
	root = t.TempDir()
	legacy, dir = filepath.Join(root, "ledger.wal"), filepath.Join(root, "wal")
	os.WriteFile(legacy, data, 0o600)
	os.MkdirAll(dir, 0o700)
	os.Link(legacy, filepath.Join(dir, segName(1)))
	writeManifest(OSFS{}, dir, []uint64{1}, nil)
	if got := collect(t, dir, Options{Legacy: legacy}); len(got) != 3 {
		t.Fatalf("after an interrupted removal: %q", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("the legacy link is still there")
	}

	// A separate legacy log beside a segmented one is ambiguous.
	os.WriteFile(legacy, data, 0o600)
	if _, err := Open(dir, Options{Legacy: legacy}, nop); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("two logs: %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	if _, err := Open(filepath.Join(blocker, "w"), Options{}, nop); err == nil {
		t.Fatal("opened a log under a regular file")
	}
	if _, err := Open(blocker, Options{}, nop); err == nil {
		t.Fatal("opened a regular file as a log directory")
	}
	if _, err := Open(t.TempDir(), Options{WrapFile: func(f File) File { return &faultyFile{File: f, failSync: true} }}, nop); err == nil {
		t.Fatal("created a log although nothing could be fsynced")
	}
}

func TestSecondOpenIsRefused(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, Options{SegmentBytes: 60}, nop)
	if err != nil {
		t.Fatal(err)
	}
	l.Append([]byte("a"))
	if _, err := Open(dir, Options{}, nop); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open: %v", err)
	}
	// Still locked after rolling and dropping segments.
	fill(t, l, 0, 10)
	l.DropBefore(l.NextIndex())
	if _, err := Open(dir, Options{}, nop); !errors.Is(err, ErrLocked) {
		t.Fatalf("open after a roll and a drop: %v", err)
	}
	l.Close()
	l2, err := Open(dir, Options{}, nop)
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	l2.Close()
}

func TestSegmentNames(t *testing.T) {
	if n, ok := parseSegName(segName(48211)); !ok || n != 48211 {
		t.Fatalf("round trip: %d %v", n, ok)
	}
	for _, bad := range []string{"1.seg", "00000000000000000000.seg", "0000000000000000001x.seg", "00000000000000000001.log", "99999999999999999999.seg"} {
		if _, ok := parseSegName(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, bad := range []string{"#seal 1 2 3 00000000\n", "#seal 1 2 3 0000000g 00000000\n", "#seal x 2 3 00000000 00000000\n"} {
		if _, err := decodeFooter([]byte(bad)); err == nil {
			t.Errorf("seal %q accepted", bad)
		}
	}
}

func FuzzReplay(f *testing.F) {
	f.Add(Encode(Encode(nil, []byte("a")), []byte(`{"x":1}`)))
	f.Add([]byte("00000000 \n"))
	f.Add([]byte("garbage"))
	sealed := Encode(nil, []byte("a"))
	f.Add(append(sealed, encodeFooter(footer{first: 1, count: 1, bytes: int64(len(sealed)), crc: crcOf(sealed)})...))
	f.Fuzz(func(t *testing.T, data []byte) {
		var got [][]byte
		end, err := Replay(bytes.NewReader(data), func(p []byte) error {
			got = append(got, append([]byte{}, p...))
			return nil
		})
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("unexpected error type: %v", err)
			}
		} else {
			if end < 0 || end > int64(len(data)) {
				t.Fatalf("end %d out of range", end)
			}
			// The intact prefix re-encodes to exactly the bytes it was read from.
			var re []byte
			for _, p := range got {
				re = Encode(re, p)
			}
			if !bytes.Equal(re, data[:end]) {
				t.Fatalf("prefix does not round-trip")
			}
		}

		// Read as a segment: the records and an accepted seal must re-encode
		// to exactly the bytes kept.
		var recs [][]byte
		s, err := scanSegment(bytes.NewReader(data), 1, func(i uint64, p []byte) error {
			if i != uint64(len(recs))+1 {
				t.Fatalf("index %d for record %d", i, len(recs)+1)
			}
			recs = append(recs, append([]byte{}, p...))
			return nil
		})
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("segment: unexpected error type: %v", err)
			}
			return
		}
		var re []byte
		for _, p := range recs {
			re = Encode(re, p)
		}
		if int64(len(re)) != s.good || s.count != uint64(len(recs)) || s.crc != crcOf(re) {
			t.Fatalf("segment scan %+v disagrees with its %d records", s, len(recs))
		}
		if s.sealed {
			re = append(re, encodeFooter(footer{first: 1, count: s.count, bytes: s.good, crc: s.crc})...)
		}
		if !bytes.Equal(re, data[:s.end]) {
			t.Fatalf("segment prefix does not round-trip")
		}
	})
}

func crcOf(b []byte) uint32 { return crc32Checksum(b) }

func BenchmarkAppend(b *testing.B) {
	l, err := Open(b.TempDir(), Options{}, nop)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	rec := bytes.Repeat([]byte("x"), 200)
	for b.Loop() {
		if err := l.Append(rec); err != nil {
			b.Fatal(err)
		}
	}
}
