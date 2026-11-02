package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dirState renders every file in dir and its contents.
func dirState(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		fmt.Fprintf(&b, "%s=%q\n", e.Name(), data)
	}
	return b.String()
}

// inspect runs Inspect, checks it changed nothing, and returns the report
// and the records it applied.
func inspect(t *testing.T, dir, legacy string) (*Report, []string) {
	t.Helper()
	around := func() string {
		if legacy == "" {
			return dirState(t, dir)
		}
		return dirState(t, dir) + dirState(t, filepath.Dir(legacy))
	}
	before := around()
	var got []string
	rep, err := Inspect(dir, legacy, func(i uint64, p []byte) error { got = append(got, fmt.Sprintf("%d:%s", i, p)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if around() != before {
		t.Fatal("Inspect changed the directory")
	}
	return rep, got
}

func threeSegments(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir, Options{SegmentBytes: 60}, nop)
	if err != nil {
		t.Fatal(err)
	}
	fill(t, l, 0, 9) // segments 1, 4, 7
	l.Close()
	return dir
}

func TestInspectSoundLog(t *testing.T) {
	dir := threeSegments(t)
	rep, got := inspect(t, dir, "")
	if len(rep.Damage)+len(rep.Repairable) > 0 || rep.Empty || rep.Legacy {
		t.Fatalf("report %+v", rep)
	}
	if want := collect(t, dir, Options{}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("applied %q, want %q", got, want)
	}
	if rep.FirstIndex != 1 || rep.NextIndex != 10 || len(rep.Segments) != 3 {
		t.Fatalf("report %+v", rep)
	}
	for i, s := range rep.Segments {
		if s.First != uint64(1+3*i) || s.Records != 3 || s.Sealed != (i < 2) || s.Problem != "" || s.Torn != 0 || s.Bytes <= 0 {
			t.Fatalf("segment %d: %+v", i, s)
		}
	}
}

func TestInspectFindsWhatOpenRefuses(t *testing.T) {
	cases := map[string]func(dir string){
		"missing segment": func(dir string) { os.Remove(filepath.Join(dir, segName(4))) },
		"missing newest":  func(dir string) { os.Remove(filepath.Join(dir, segName(7))) },
		"flipped manifest": func(dir string) {
			p := filepath.Join(dir, manifestName)
			b, _ := os.ReadFile(p)
			b[len(manifestMagic)+1] ^= 1
			os.WriteFile(p, b, 0o600)
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
		"flipped byte in a sealed segment": func(dir string) {
			p := filepath.Join(dir, segName(4))
			b, _ := os.ReadFile(p)
			b[12] ^= 1
			os.WriteFile(p, b, 0o600)
		},
		"data after a seal": func(dir string) {
			f, _ := os.OpenFile(filepath.Join(dir, segName(1)), os.O_APPEND|os.O_WRONLY, 0)
			f.Write(Encode(nil, []byte("late")))
			f.Close()
		},
		"sealed segment cut short": func(dir string) {
			p := filepath.Join(dir, segName(1))
			b, _ := os.ReadFile(p)
			os.WriteFile(p, b[:len(b)-3], 0o600)
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := threeSegments(t)
			damage(dir)
			if _, err := Open(dir, Options{}, nop); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open: %v", err)
			}
			dir = threeSegments(t)
			damage(dir)
			if rep, _ := inspect(t, dir, ""); len(rep.Damage) == 0 {
				t.Fatalf("no damage reported: %+v", rep)
			}
		})
	}
}

// Damage stops apply but every segment is still checked.
func TestInspectKeepsCheckingAfterDamage(t *testing.T) {
	dir := threeSegments(t)
	p := filepath.Join(dir, segName(1))
	b, _ := os.ReadFile(p)
	b[12] ^= 1
	os.WriteFile(p, b, 0o600)
	rep, got := inspect(t, dir, "")
	if len(got) != 0 || rep.Segments[0].Problem == "" || rep.Segments[1].Problem != "" || rep.Segments[2].Records != 3 || rep.NextIndex != 10 {
		t.Fatalf("applied %q, report %+v", got, rep.Segments)
	}

	dir = threeSegments(t)
	calls := 0
	rep, err := Inspect(dir, "", func(i uint64, p []byte) error {
		calls++
		if i == 5 {
			return errors.New("bad payload")
		}
		return nil
	})
	if err != nil || calls != 5 || len(rep.Damage) != 1 || !strings.Contains(rep.Damage[0], "record 5: bad payload") || rep.NextIndex != 10 {
		t.Fatalf("calls %d, err %v, report %+v", calls, err, rep)
	}
}

func TestInspectFindsWhatOpenRepairs(t *testing.T) {
	cases := map[string]struct {
		damage func(dir string)
		want   string
	}{
		"torn tail": {func(dir string) {
			f, _ := os.OpenFile(filepath.Join(dir, segName(7)), os.O_APPEND|os.O_WRONLY, 0)
			f.WriteString("0badf")
			f.Close()
		}, "torn write of 5 bytes"},
		"sealed newest segment": {func(dir string) {
			os.Remove(filepath.Join(dir, segName(7)))
			writeManifest(OSFS{}, dir, []uint64{1, 4}, nil)
			os.WriteFile(filepath.Join(dir, segName(7)), nil, 0o600)
		}, "interrupted roll"},
		"older segment left by a drop": {func(dir string) { writeManifest(OSFS{}, dir, []uint64{4, 7}, nil) }, "interrupted drop"},
		"temporary manifest": {func(dir string) {
			os.WriteFile(filepath.Join(dir, manifestName+".tmp"), []byte("x"), 0o600)
		}, "temporary manifest"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := threeSegments(t)
			tc.damage(dir)
			rep, _ := inspect(t, dir, "")
			if len(rep.Damage) != 0 || !strings.Contains(strings.Join(rep.Repairable, "\n"), tc.want) {
				t.Fatalf("damage %q, repairable %q", rep.Damage, rep.Repairable)
			}
			if _, err := Open(dir, Options{}, nop); err != nil {
				t.Fatalf("Open: %v", err)
			}
			if rep, _ := inspect(t, dir, ""); len(rep.Damage)+len(rep.Repairable) != 0 {
				t.Fatalf("after Open: damage %q, repairable %q", rep.Damage, rep.Repairable)
			}
		})
	}

	// Created, but the manifest was never written.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, segName(1)), nil, 0o600)
	if rep, _ := inspect(t, dir, ""); rep.Empty || len(rep.Damage) != 0 || len(rep.Repairable) != 1 {
		t.Fatalf("report %+v", rep)
	}
	if rep, _ := inspect(t, t.TempDir(), ""); !rep.Empty || len(rep.Damage)+len(rep.Repairable) != 0 {
		t.Fatalf("empty directory: %+v", rep)
	}
}

func TestInspectLegacyLog(t *testing.T) {
	var data []byte
	for _, r := range []string{"h", "a", "b"} {
		data = Encode(data, []byte(r))
	}
	root := t.TempDir()
	legacy, dir := filepath.Join(root, "ledger.wal"), filepath.Join(root, "wal")
	os.WriteFile(legacy, append(append([]byte{}, data...), "0badf"...), 0o600)
	rep, got := inspect(t, dir, legacy)
	if !rep.Legacy || strings.Join(got, ",") != "1:h,2:a,3:b" || rep.NextIndex != 4 || len(rep.Damage) != 0 ||
		len(rep.Repairable) != 1 || rep.Segments[0].Torn != 5 {
		t.Fatalf("legacy: applied %q, report %+v", got, rep)
	}

	// Interrupted after the manifest, before the legacy name was removed.
	os.WriteFile(legacy, data, 0o600)
	os.MkdirAll(dir, 0o700)
	os.Link(legacy, filepath.Join(dir, segName(1)))
	writeManifest(OSFS{}, dir, []uint64{1}, nil)
	rep, got = inspect(t, dir, legacy)
	if len(got) != 3 || len(rep.Damage) != 0 || !strings.Contains(strings.Join(rep.Repairable, ""), "interrupted upgrade") {
		t.Fatalf("interrupted upgrade: %q %+v", got, rep)
	}

	// A separate file under the legacy name next to a segmented log.
	os.Remove(legacy)
	os.WriteFile(legacy, data, 0o600)
	if rep, _ := inspect(t, dir, legacy); len(rep.Damage) == 0 {
		t.Fatalf("two logs: %+v", rep)
	}

	// Damage in the middle of a legacy log.
	root = t.TempDir()
	legacy, dir = filepath.Join(root, "ledger.wal"), filepath.Join(root, "wal")
	bad := append([]byte{}, data...)
	bad[10] ^= 1
	os.WriteFile(legacy, bad, 0o600)
	if rep, _ := inspect(t, dir, legacy); len(rep.Damage) == 0 || rep.Segments[0].Problem == "" {
		t.Fatalf("damaged legacy log: %+v", rep)
	}
}

func TestHoldLock(t *testing.T) {
	dir := t.TempDir()
	release, err := HoldLock(dir) // never opened: nothing to hold
	if err != nil {
		t.Fatal(err)
	}
	release()
	l, err := Open(dir, Options{}, nop)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HoldLock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("held by an open log: %v", err)
	}
	l.Close()
	release, err = HoldLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}, nop); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open while a reader holds the lock: %v", err)
	}
	release()
	if l, err := Open(dir, Options{}, nop); err != nil {
		t.Fatal(err)
	} else {
		l.Close()
	}
}
