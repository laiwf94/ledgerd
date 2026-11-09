package wal

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// longPayload returns n bytes of printable data that differ from position to
// position, so a misplaced chunk changes the checksum.
func longPayload(n int, seed byte) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = 'a' + byte((i/7+int(seed))%26)
	}
	return p
}

// Records longer than the pooled reader's buffer are assembled line by line
// up to MaxRecord, in segments and in legacy single-file logs, and a record
// of exactly MaxRecord bytes survives a restart, a roll and a seal.
func TestRecordsLongerThanTheReaderBuffer(t *testing.T) {
	dir := t.TempDir()
	sizes := []int{10, readerSize - 10, readerSize, readerSize + 1, 3*readerSize + 5, MaxRecord}
	var want [][]byte
	l, err := Open(dir, Options{SegmentBytes: 4 << 10}, nop)
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range sizes {
		p := longPayload(n, byte(i))
		want = append(want, p)
		if err := l.Append(p, []byte("short")); err != nil {
			t.Fatalf("append %d bytes: %v", n, err)
		}
		want = append(want, []byte("short"))
	}
	l.Close()

	check := func(stage string, got [][]byte) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d records, want %d", stage, len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("%s: record %d differs (%d bytes, want %d)", stage, i+1, len(got[i]), len(want[i]))
			}
		}
	}
	var got [][]byte
	l, err = Open(dir, Options{SegmentBytes: 4 << 10}, func(_ uint64, p []byte) error {
		got = append(got, append([]byte(nil), p...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.Segments() < len(sizes) {
		t.Fatalf("only %d segments; every long record should have rolled the log", l.Segments())
	}
	l.Close()
	check("open", got)
	got = nil
	if _, err := Read(dir, func(_ uint64, p []byte) error { got = append(got, append([]byte(nil), p...)); return nil }); err != nil {
		t.Fatal(err)
	}
	check("read", got)
	n := 0
	rep, err := Inspect(dir, "", func(uint64, []byte) error { n++; return nil })
	if err != nil || len(rep.Damage) > 0 || len(rep.Repairable) > 0 || n != len(want) {
		t.Fatalf("inspect: %d records, %v %v, %v", n, rep.Damage, rep.Repairable, err)
	}

	// The same lines as a legacy single-file log.
	var legacy []byte
	for _, p := range want {
		legacy = Encode(legacy, p)
	}
	got = nil
	end, err := Replay(bytes.NewReader(legacy), func(p []byte) error { got = append(got, append([]byte(nil), p...)); return nil })
	if err != nil || end != int64(len(legacy)) {
		t.Fatalf("replay: end %d of %d, %v", end, len(legacy), err)
	}
	check("legacy replay", got)

	// A long record torn anywhere is a torn tail, not damage.
	for _, cut := range []int{1, readerSize - 1, readerSize + 7, len(legacy) - len(Encode(nil, want[len(want)-1])) - 1} {
		got = nil
		torn := legacy[:len(legacy)-cut]
		if _, err := Replay(bytes.NewReader(torn), func(p []byte) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if len(got) >= len(want) {
			t.Fatalf("cut %d: replayed %d records from a torn log", cut, len(got))
		}
	}
}

// A line longer than any record can be is damage wherever it is, even when
// it ends the file, because no write the log made could have produced it.
func TestLineLongerThanAnyRecordIsCorrupt(t *testing.T) {
	over := append(bytes.Repeat([]byte("b"), maxLine+1), '\n')
	if _, err := Replay(bytes.NewReader(over), func([]byte) error { return nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("legacy replay of an over-long line: %v", err)
	}
	dir := t.TempDir()
	l, err := Open(dir, Options{}, nop)
	if err != nil {
		t.Fatal(err)
	}
	l.Append([]byte("one"))
	l.Close()
	path := activePath(t, dir)
	good, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(append([]byte{}, good...), over...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}, nop); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("segment with an over-long line: %v", err)
	}
}
