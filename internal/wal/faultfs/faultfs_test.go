package faultfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/laiwf94/ledgerd/internal/wal"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func create(t *testing.T, f *FS, name string) wal.FSFile {
	t.Helper()
	h, err := f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	must(t, err)
	return h
}

func writeString(t *testing.T, h wal.FSFile, s string) {
	t.Helper()
	if _, err := h.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

// read returns a file's contents, or "<none>" when it does not exist.
func read(t *testing.T, f *FS, name string) string {
	t.Helper()
	b, err := f.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "<none>"
	}
	must(t, err)
	return string(b)
}

func TestFileSemantics(t *testing.T) {
	f := New(1)
	must(t, f.Mkdir("/d", 0o700))
	h := create(t, f, "/d/x")
	writeString(t, h, "hello")
	if _, err := h.Seek(1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	writeString(t, h, "EL")
	if _, err := h.Seek(-1, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	if n, err := h.Read(b); n != 1 || err != nil || b[0] != 'o' {
		t.Fatalf("read %q %v", b[:n], err)
	}
	if _, err := h.Read(b); err != io.EOF {
		t.Fatalf("read at the end: %v", err)
	}
	if _, err := h.Seek(0, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Seek(-10, io.SeekCurrent); err == nil {
		t.Fatal("seek before the start succeeded")
	}
	if _, err := h.Seek(0, 7); err == nil {
		t.Fatal("bad whence accepted")
	}
	must(t, h.Truncate(4))
	if st, err := h.Stat(); err != nil || st.Size() != 4 || st.Name() != "x" || st.IsDir() || st.Mode() != 0o600 || !st.ModTime().IsZero() || st.Sys() != nil {
		t.Fatalf("stat %+v %v", st, err)
	}
	if err := h.Truncate(-1); err == nil {
		t.Fatal("negative truncate accepted")
	}
	must(t, h.Close())
	if err := h.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("second close: %v", err)
	}
	if _, err := h.Write([]byte("x")); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
	if got := read(t, f, "/d/x"); got != "hELl" {
		t.Fatalf("contents %q", got)
	}

	// Writing past the end leaves a hole of zeros; O_APPEND always appends.
	h = create(t, f, "/d/hole")
	_, err := h.Seek(3, io.SeekStart)
	must(t, err)
	writeString(t, h, "z")
	h.Close()
	if got := read(t, f, "/d/hole"); got != "\x00\x00\x00z" {
		t.Fatalf("hole %q", got)
	}
	a, err := f.OpenFile("/d/hole", os.O_WRONLY|os.O_APPEND, 0)
	must(t, err)
	_, err = a.Seek(0, io.SeekStart)
	must(t, err)
	writeString(t, a, "!")
	if _, err := a.Read(b); err == nil {
		t.Fatal("read from a write-only file succeeded")
	}
	a.Close()
	if got := read(t, f, "/d/hole"); got != "\x00\x00\x00z!" {
		t.Fatalf("append %q", got)
	}
	tr, err := f.OpenFile("/d/hole", os.O_RDWR|os.O_TRUNC, 0)
	must(t, err)
	tr.Close()
	if got := read(t, f, "/d/hole"); got != "" {
		t.Fatalf("O_TRUNC left %q", got)
	}

	ro, err := f.OpenFile("/d/x", os.O_RDONLY, 0)
	must(t, err)
	if _, err := ro.Write([]byte("x")); err == nil {
		t.Fatal("write to a read-only file succeeded")
	}
	if err := ro.Truncate(0); err == nil {
		t.Fatal("truncate of a read-only file succeeded")
	}
	if n, err := ro.Write(nil); n != 0 || err == nil {
		t.Fatal("empty write to a read-only file succeeded")
	}
	ro.Close()

	for _, c := range []struct {
		name string
		flag int
		want error
	}{
		{"/d/x", os.O_RDWR | os.O_CREATE | os.O_EXCL, fs.ErrExist},
		{"/d/missing", os.O_RDONLY, fs.ErrNotExist},
		{"/missing/x", os.O_RDWR | os.O_CREATE, fs.ErrNotExist},
	} {
		if _, err := f.OpenFile(c.name, c.flag, 0o600); !errors.Is(err, c.want) {
			t.Errorf("open %s: %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := f.OpenFile("/d", os.O_RDWR, 0); err == nil {
		t.Error("opened a directory for writing")
	}
	if _, err := f.OpenFile("/d/x/y", os.O_RDWR|os.O_CREATE, 0o600); err == nil {
		t.Error("created a file under a file")
	}
	if _, err := f.OpenFile("/", os.O_RDWR|os.O_CREATE, 0o600); err == nil {
		t.Error("opened the root as a file")
	}
	d, err := f.OpenFile("/d", os.O_RDONLY, 0)
	must(t, err)
	if _, err := d.Read(b); err == nil {
		t.Error("read a directory")
	}
	must(t, d.Sync())
	d.Close()
}

func TestDirectorySemantics(t *testing.T) {
	f := New(1)
	must(t, f.Mkdir("/d", 0o700))
	if err := f.Mkdir("/d", 0o700); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second mkdir: %v", err)
	}
	if err := f.Mkdir("/nope/d", 0o700); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("mkdir without a parent: %v", err)
	}
	for _, n := range []string{"c", "a", "b"} {
		create(t, f, "/d/"+n).Close()
	}
	must(t, f.Mkdir("/d/sub", 0o700))
	ents, err := f.ReadDir("/d")
	must(t, err)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "a,b,c,sub" || !ents[3].IsDir() {
		t.Fatalf("readdir %v", names)
	}
	if _, err := f.ReadDir("/d/a"); err == nil {
		t.Fatal("readdir of a file succeeded")
	}
	if _, err := f.ReadDir("/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readdir of a missing directory: %v", err)
	}
	if st, err := f.Stat("/d/sub"); err != nil || !st.IsDir() || st.Name() != "sub" {
		t.Fatalf("stat %v %v", st, err)
	}
	if _, err := f.Stat("/d/a/b"); err == nil {
		t.Fatal("stat through a file succeeded")
	}

	create(t, f, "/d/sub/x").Close()
	if err := f.Remove("/d/sub"); err == nil {
		t.Fatal("removed a directory that is not empty")
	}
	must(t, f.Remove("/d/sub/x"))
	must(t, f.Remove("/d/sub"))
	if err := f.Remove("/d/sub"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("second remove: %v", err)
	}

	// Rename replaces the target; links share one file.
	must(t, f.WriteFile("/d/a", []byte("A")))
	must(t, f.WriteFile("/d/b", []byte("B")))
	must(t, f.Rename("/d/a", "/d/b"))
	must(t, f.Rename("/d/b", "/d/b"))
	if read(t, f, "/d/a") != "<none>" || read(t, f, "/d/b") != "A" {
		t.Fatal("rename did not replace its target")
	}
	if err := f.Rename("/d/missing", "/d/z"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rename of a missing file: %v", err)
	}
	must(t, f.Mkdir("/e", 0o700))
	if err := f.Rename("/d/b", "/e/b"); err == nil {
		t.Fatal("rename across directories succeeded")
	}
	if err := f.Rename("/d/b", "/e"); err == nil {
		t.Fatal("rename over a directory succeeded")
	}
	must(t, f.Link("/d/b", "/e/b"))
	if err := f.Link("/d/b", "/e/b"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("link over a file: %v", err)
	}
	if err := f.Link("/d", "/e/d"); err == nil {
		t.Fatal("linked a directory")
	}
	if err := f.Link("/d/missing", "/e/m"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("link of a missing file: %v", err)
	}
	x, _ := f.Stat("/d/b")
	y, _ := f.Stat("/e/b")
	z, _ := f.Stat("/d/c")
	if !f.SameFile(x, y) || f.SameFile(x, z) || f.SameFile(x, nil) {
		t.Fatal("SameFile is wrong")
	}
	must(t, f.Remove("/d/b"))
	if read(t, f, "/e/b") != "A" {
		t.Fatal("removing one link removed the file")
	}
	if err := f.SyncDir("/d/c"); err == nil {
		t.Fatal("SyncDir of a file succeeded")
	}
	if err := f.SyncDir("/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SyncDir of a missing directory: %v", err)
	}
}

func TestLock(t *testing.T) {
	f := New(1)
	if _, err := f.Lock("/LOCK", false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lock without create: %v", err)
	}
	l1, err := f.Lock("/LOCK", true)
	must(t, err)
	if _, err := f.Lock("/LOCK", false); !errors.Is(err, wal.ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	must(t, l1.Close())
	must(t, l1.Close())
	l2, err := f.Lock("/LOCK", false)
	must(t, err)
	must(t, f.SyncDir("/"))
	img := f.PowerCut()
	l2.Close() // a dead lock; releasing it changes nothing on img
	l3, err := img.Lock("/LOCK", false)
	if err != nil {
		t.Fatalf("a lock survived the power cut: %v", err)
	}
	l3.Close()
}

// TestSyncedDataSurvives checks the promise faultfs keeps: what was synced,
// file data and directory entries, is exactly what comes back.
func TestSyncedDataSurvives(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		f := New(seed)
		must(t, f.Mkdir("/d", 0o700))
		must(t, f.SyncDir("/"))
		h := create(t, f, "/d/log")
		writeString(t, h, "one\n")
		writeString(t, h, "two\n")
		must(t, h.Sync())
		must(t, f.SyncDir("/d"))
		must(t, f.Link("/d/log", "/d/link"))
		must(t, f.SyncDir("/d"))
		writeString(t, h, "three\n") // pending
		img := f.PowerCut()
		got := read(t, img, "/d/log")
		if !strings.HasPrefix(got, "one\ntwo\n") || !strings.HasPrefix("one\ntwo\nthree\n", got) {
			t.Fatalf("seed %d: %q", seed, got)
		}
		if read(t, img, "/d/link") != got {
			t.Fatalf("seed %d: the link and the file differ after a cut", seed)
		}
		x, _ := img.Stat("/d/log")
		y, _ := img.Stat("/d/link")
		if !img.SameFile(x, y) {
			t.Fatalf("seed %d: a hard link became two files", seed)
		}
		// The image is durable through and through: a second cut loses nothing.
		if again := read(t, img.PowerCut(), "/d/log"); again != got {
			t.Fatalf("seed %d: a second cut changed %q to %q", seed, got, again)
		}
	}
}

// TestPendingWrites checks that a cut keeps a valid selection of pending
// writes, and that over many seeds it makes every kind of selection.
func TestPendingWrites(t *testing.T) {
	outcomes := map[string]bool{}
	var stats Stats
	for seed := int64(0); seed < 2000; seed++ {
		f := New(seed)
		must(t, f.WriteFile("/x", []byte("base")))
		h, err := f.OpenFile("/x", os.O_WRONLY|os.O_APPEND, 0)
		must(t, err)
		writeString(t, h, "aaaa")
		writeString(t, h, "bbbb")
		writeString(t, h, "cccc")
		img := f.PowerCut()
		stats.Add(img.CutStats())
		got := read(t, img, "/x")
		if !strings.HasPrefix(got, "base") || len(got) > 16 {
			t.Fatalf("seed %d: %q", seed, got)
		}
		// Each byte is what was written there, or a zero where a later
		// write landed without an earlier one.
		want := "baseaaaabbbbcccc"
		for i := 4; i < len(got); i++ {
			if got[i] != want[i] && got[i] != 0 {
				t.Fatalf("seed %d: byte %d of %q", seed, i, got)
			}
		}
		outcomes[strings.ReplaceAll(got, "\x00", "0")] = true
	}
	for _, want := range []string{"base", "baseaaaabbbbcccc", "baseaaaabb", "base0000bbbb", "baseaaaa0000cccc"} {
		if !outcomes[want] {
			t.Errorf("never got %q", want)
		}
	}
	for m := DropAll; m < numModes; m++ {
		if stats.Files[m] == 0 {
			t.Errorf("mode %s never chosen: %+v", m, stats)
		}
	}
	if stats.Writes != 3*2000 || stats.LostWrites == 0 || stats.TornWrites == 0 || stats.OutOfOrder == 0 {
		t.Errorf("stats %+v", stats)
	}
	if stats.Dirs != [numModes]int{} || stats.DirOps != 0 {
		t.Errorf("directory changes counted though every one was synced: %+v", stats)
	}
}

// TestOverlappingWritesReorder checks that reordering is visible when
// writes overlap: the older write can end up on top.
func TestOverlappingWritesReorder(t *testing.T) {
	seen := map[string]bool{}
	for seed := int64(0); seed < 500; seed++ {
		f := New(seed)
		h := create(t, f, "/x")
		writeString(t, h, "old!")
		_, err := h.Seek(0, io.SeekStart)
		must(t, err)
		writeString(t, h, "new!")
		must(t, f.SyncDir("/"))
		seen[read(t, f.PowerCut(), "/x")] = true
	}
	for _, want := range []string{"", "old!", "new!"} {
		if !seen[want] {
			t.Errorf("never got %q (got %v)", want, seen)
		}
	}
}

func TestPendingTruncate(t *testing.T) {
	seen := map[string]bool{}
	for seed := int64(0); seed < 200; seed++ {
		f := New(seed)
		must(t, f.WriteFile("/x", []byte("abcdef")))
		h, err := f.OpenFile("/x", os.O_RDWR, 0)
		must(t, err)
		must(t, h.Truncate(2))
		got := read(t, f.PowerCut(), "/x")
		if got != "abcdef" && got != "ab" {
			t.Fatalf("seed %d: %q", seed, got)
		}
		seen[got] = true
	}
	if len(seen) != 2 {
		t.Fatalf("outcomes %v", seen)
	}
}

// TestDirectoryChanges checks that unsynced creations, removals and
// renames can each be lost, that a rename is never half done, and that a
// synced directory keeps its entries.
func TestDirectoryChanges(t *testing.T) {
	seen := map[string]bool{}
	var dirStats Stats
	for seed := int64(0); seed < 1000; seed++ {
		f := New(seed)
		must(t, f.Mkdir("/d", 0o700))
		must(t, f.SyncDir("/"))
		must(t, f.WriteFile("/d/target", []byte("old")))
		must(t, f.WriteFile("/d/gone", []byte("g")))
		// Unsynced: a new file, a removal, a mkdir, and the atomic
		// replace of target by a fully synced temporary file.
		h := create(t, f, "/d/new")
		writeString(t, h, "n")
		must(t, h.Sync())
		must(t, f.Remove("/d/gone"))
		must(t, f.Mkdir("/d/sub", 0o700))
		tmp := create(t, f, "/d/target.tmp")
		writeString(t, tmp, "new")
		must(t, tmp.Sync())
		must(t, f.Rename("/d/target.tmp", "/d/target"))
		img := f.PowerCut()
		dirStats.Add(img.CutStats())
		target := read(t, img, "/d/target")
		if target != "old" && target != "new" {
			t.Fatalf("seed %d: rename left target as %q", seed, target)
		}
		if n := read(t, img, "/d/new"); n != "n" && n != "<none>" {
			t.Fatalf("seed %d: synced new file reads %q", seed, n)
		}
		_, subErr := img.Stat("/d/sub")
		key := fmt.Sprintf("target=%s new=%s gone=%s sub=%v tmp=%s", target, read(t, img, "/d/new"),
			read(t, img, "/d/gone"), subErr == nil, read(t, img, "/d/target.tmp"))
		seen[key] = true
	}
	var keys []string
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	all := strings.Join(keys, "\n")
	for _, want := range []string{"target=old", "target=new", "new=n", "new=<none>", "gone=g", "gone=<none>", "sub=true", "sub=false",
		"tmp=new", "tmp=<none>"} {
		if !strings.Contains(all, want) {
			t.Errorf("never saw %s in\n%s", want, all)
		}
	}
	if strings.Contains(all, "target=new new=n gone=g sub=true tmp=new") || strings.Contains(all, "target=new new=n gone=<none> sub=true tmp=new") {
		t.Error("the renamed file survived under both names")
	}
	for m := DropAll; m < numModes; m++ {
		if got := dirStats.Dirs[m]; (got == 0) != (m == Reordered) {
			t.Errorf("directories cut with mode %s %d times", m, got)
		}
	}
}

func TestCutAfter(t *testing.T) {
	f := New(1)
	h := create(t, f, "/x") // 1
	f.CutAfter(2)
	writeString(t, h, "a") // 2
	must(t, h.Sync())      // 3
	if f.Off() {
		t.Fatal("power went early")
	}
	if _, err := h.Write([]byte("b")); !errors.Is(err, ErrPowerLost) {
		t.Fatalf("write past the budget: %v", err)
	}
	if !f.Off() {
		t.Fatal("power still on")
	}
	img := f.PowerCut()
	if f.PowerCut() != img {
		t.Fatal("a second PowerCut returned another image")
	}
	if New(1).CutStats() != (Stats{}) {
		t.Fatal("a new FS has cut stats")
	}
	// Everything on the dead file system fails; Close still works.
	checks := map[string]error{}
	_, checks["read"] = h.Read(make([]byte, 1))
	_, checks["seek"] = h.Seek(0, io.SeekStart)
	_, checks["stat"] = h.Stat()
	checks["truncate"] = h.Truncate(0)
	checks["sync"] = h.Sync()
	_, checks["open"] = f.OpenFile("/x", os.O_RDONLY, 0)
	_, checks["fstat"] = f.Stat("/x")
	_, checks["readdir"] = f.ReadDir("/")
	checks["mkdir"] = f.Mkdir("/m", 0o700)
	checks["remove"] = f.Remove("/x")
	checks["rename"] = f.Rename("/x", "/y")
	checks["link"] = f.Link("/x", "/y")
	checks["syncdir"] = f.SyncDir("/")
	_, checks["lock"] = f.Lock("/x", true)
	for op, err := range checks {
		if !errors.Is(err, ErrPowerLost) {
			t.Errorf("%s after the cut: %v", op, err)
		}
	}
	must(t, h.Close())
	// The file was synced but its directory entry was not.
	if got := read(t, img, "/x"); got != "a" && got != "<none>" {
		t.Fatalf("after the cut: %q", got)
	}

	// The budget counts creations, removals, renames, links, mkdirs,
	// truncations and directory syncs too.
	for _, op := range []func(f *FS) error{
		func(f *FS) error { _, err := f.OpenFile("/n", os.O_RDWR|os.O_CREATE, 0o600); return err },
		func(f *FS) error { return f.Remove("/base") },
		func(f *FS) error { return f.Rename("/base", "/r") },
		func(f *FS) error { return f.Link("/base", "/l") },
		func(f *FS) error { return f.Mkdir("/m", 0o700) },
		func(f *FS) error { return f.SyncDir("/") },
		func(f *FS) error { _, err := f.OpenFile("/base", os.O_RDWR|os.O_TRUNC, 0); return err },
		func(f *FS) error {
			h, err := f.OpenFile("/base", os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return h.Truncate(1)
		},
	} {
		f := New(2)
		must(t, f.WriteFile("/base", []byte("data")))
		f.CutAfter(0)
		if err := op(f); !errors.Is(err, ErrPowerLost) || !f.Off() {
			t.Errorf("an operation ran with no budget left: %v", err)
		}
	}
}

// TestDeterministic checks that a seed and a sequence of calls decide
// exactly what a cut keeps.
func TestDeterministic(t *testing.T) {
	run := func(seed int64) string {
		f := New(seed)
		must(t, f.Mkdir("/d", 0o700))
		var out []string
		for i := 0; i < 20; i++ {
			h := create(t, f, fmt.Sprintf("/d/f%02d", i))
			for j := 0; j < 5; j++ {
				writeString(t, h, fmt.Sprintf("%d.%d;", i, j))
				if j == 2 {
					must(t, h.Sync())
				}
			}
		}
		img := f.PowerCut()
		ents, _ := img.ReadDir("/d")
		for _, e := range ents {
			out = append(out, e.Name()+"="+read(t, img, "/d/"+e.Name()))
		}
		return fmt.Sprint(out, img.CutStats())
	}
	if run(7) != run(7) {
		t.Fatal("the same seed lost different things")
	}
	if run(7) == run(8) {
		t.Fatal("different seeds lost the same things")
	}
}

func TestModeNames(t *testing.T) {
	var names []string
	for m := DropAll; m < numModes; m++ {
		names = append(names, m.String())
	}
	if got := strings.Join(names, ","); got != "drop-all,keep-all,torn-tail,subset,reordered" {
		t.Fatal(got)
	}
}

func TestWriteFileFailures(t *testing.T) {
	f := New(1)
	if err := f.WriteFile("/missing/x", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WriteFile without a directory: %v", err)
	}
	f.CutAfter(1)
	if err := f.WriteFile("/x", []byte("a")); !errors.Is(err, ErrPowerLost) {
		t.Fatalf("WriteFile across a cut: %v", err)
	}
	if !bytes.Equal([]byte(read(t, New(1), "/x")), []byte("<none>")) {
		t.Fatal("a new file system is not empty")
	}
}
