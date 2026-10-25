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

func collect(t *testing.T, path string) []string {
	t.Helper()
	var got []string
	l, err := Open(path, Options{}, func(p []byte) error { got = append(got, string(p)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	return got
}

func TestAppendAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "ledger.wal")
	l, err := Open(path, Options{}, func([]byte) error { t.Fatal("empty log replayed a record"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append([]byte("a"), []byte(`{"b":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := l.Append([]byte("c")); err != nil {
		t.Fatal(err)
	}
	if l.Syncs() != 2 || l.Err() != nil {
		t.Fatalf("syncs=%d err=%v", l.Syncs(), l.Err())
	}
	size := l.Size()
	l.Close()
	if got := collect(t, path); strings.Join(got, "|") != `a|{"b":1}|c` {
		t.Fatalf("replayed %q", got)
	}
	st, _ := os.Stat(path)
	if st.Size() != size || st.Mode().Perm() != 0o600 {
		t.Fatalf("size %d (want %d), mode %v", st.Size(), size, st.Mode())
	}
}

func TestPayloadValidation(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "w"), Options{}, func([]byte) error { return nil })
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
	if l.Err() != nil || l.Size() != 0 {
		t.Fatal("a rejected payload must not touch the file")
	}
}

func TestTornTailIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w")
	l, _ := Open(path, Options{}, func([]byte) error { return nil })
	l.Append([]byte("one"), []byte("two"))
	l.Close()
	good, _ := os.ReadFile(path)
	for _, tail := range []string{"0", "deadbeef {\"x\"", "00000000 bad\n"} {
		os.WriteFile(path, append(append([]byte{}, good...), tail...), 0o600)
		if got := collect(t, path); len(got) != 2 {
			t.Fatalf("tail %q: replayed %q", tail, got)
		}
		if b, _ := os.ReadFile(path); !bytes.Equal(b, good) {
			t.Fatalf("tail %q was not truncated", tail)
		}
	}
}

func TestCorruptionInTheMiddleIsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w")
	l, _ := Open(path, Options{}, func([]byte) error { return nil })
	l.Append([]byte("one"), []byte("two"), []byte("three"))
	l.Close()
	good, _ := os.ReadFile(path)
	cases := map[string]func([]byte) []byte{
		"flipped payload byte": func(b []byte) []byte { b[10] ^= 1; return b },
		"bad checksum hex":     func(b []byte) []byte { b[0] = 'z'; return b },
		"missing separator":    func(b []byte) []byte { b[8] = 'x'; return b },
		"short line":           func(b []byte) []byte { return append([]byte("abc\n"), b...) },
		"oversize line":        func(b []byte) []byte { return append(bytes.Repeat([]byte("a"), MaxRecord+100), b...) },
	}
	for name, mutate := range cases {
		os.WriteFile(path, mutate(append([]byte{}, good...)), 0o600)
		_, err := Open(path, Options{}, func([]byte) error { return nil })
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
	os.WriteFile(path, good, 0o600)
	_, err := Open(path, Options{}, func(p []byte) error {
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
			path := filepath.Join(t.TempDir(), "w")
			var ff *faultyFile
			l, err := Open(path, Options{WrapFile: func(f File) File { ff = &faultyFile{File: f}; return ff }}, func([]byte) error { return nil })
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
			if err := l.Compact(func(p []byte) ([]byte, bool, error) { return p, true, nil }); !errors.Is(err, ErrBroken) {
				t.Fatalf("compaction on a broken log: %v", err)
			}
			if l.Err() == nil {
				t.Fatal("Err() is nil")
			}
			l.Close()
			// Recovery keeps the acknowledged record and drops any torn bytes.
			got := collect(t, path)
			if len(got) < 1 || got[0] != "ok" || (mode != "sync" && len(got) != 1) {
				t.Fatalf("recovered %q", got)
			}
		})
	}
}

func TestCompact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w")
	l, _ := Open(path, Options{}, func([]byte) error { return nil })
	for i := 0; i < 10; i++ {
		l.Append([]byte(fmt.Sprint(i)))
	}
	err := l.Compact(func(p []byte) ([]byte, bool, error) {
		if p[0]%2 == 1 {
			return nil, false, nil
		}
		return append([]byte("x"), p...), true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got := strings.Join(collect(t, path), ","); got != "x0,x2,x4,x6,x8,tail" {
		t.Fatalf("after compaction: %s", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
}

func TestCompactFailureLeavesLogIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w")
	var wraps int
	var second *faultyFile
	l, _ := Open(path, Options{WrapFile: func(f File) File {
		wraps++
		ff := &faultyFile{File: f}
		if wraps == 2 {
			second = ff
			ff.failSync = true
		}
		return ff
	}}, func([]byte) error { return nil })
	l.Append([]byte("a"), []byte("b"))
	if err := l.Compact(func(p []byte) ([]byte, bool, error) { return p, true, nil }); err == nil || second == nil {
		t.Fatal("compaction with a failing fsync succeeded")
	}
	if err := l.Compact(func(p []byte) ([]byte, bool, error) { return nil, false, errors.New("filter says no") }); err == nil {
		t.Fatal("filter error ignored")
	}
	if err := l.Compact(func(p []byte) ([]byte, bool, error) { return []byte("a\nb"), true, nil }); err == nil {
		t.Fatal("invalid output accepted")
	}
	if err := l.Append([]byte("c")); err != nil {
		t.Fatalf("log unusable after failed compaction: %v", err)
	}
	l.Close()
	if got := strings.Join(collect(t, path), ","); got != "a,b,c" {
		t.Fatalf("got %s", got)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	if _, err := Open(filepath.Join(blocker, "w"), Options{}, nil); err == nil {
		t.Fatal("opened a log under a regular file")
	}
	if _, err := Open(dir, Options{}, nil); err == nil {
		t.Fatal("opened a directory as a log")
	}
}

func FuzzReplay(f *testing.F) {
	f.Add(Encode(Encode(nil, []byte("a")), []byte(`{"x":1}`)))
	f.Add([]byte("00000000 \n"))
	f.Add([]byte("garbage"))
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
			return
		}
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
	})
}

func BenchmarkAppend(b *testing.B) {
	l, err := Open(filepath.Join(b.TempDir(), "w"), Options{}, func([]byte) error { return nil })
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
