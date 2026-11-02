package ledger

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// verifyFixture writes a ledger with a random workload, snapshots every 30
// records and the given segment size, and shuts it down cleanly. Small
// segments mean old segments are dropped behind snapshots.
func verifyFixture(t *testing.T, segBytes int64) (string, *fakeClock) {
	t.Helper()
	clock := newClock()
	dir := t.TempDir()
	l := openAt(t, dir, clock, func(o *Options) {
		o.SnapshotEvery, o.SegmentBytes, o.NewID = 30, segBytes, seqIDs()
	})
	randomWorkload(t, l, clock, rand.New(rand.NewSource(7)), 300)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, clock
}

// treeDigest hashes every file name and content under dir.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		h.Write([]byte(path + "\x00"))
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			h.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(h.Sum(nil))
}

// mustVerify runs Verify and checks it changed nothing on disk.
func mustVerify(t *testing.T, dir string) *VerifyReport {
	t.Helper()
	before := treeDigest(t, dir)
	r, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if treeDigest(t, dir) != before {
		t.Fatal("Verify changed the directory")
	}
	return r
}

func wantStatus(t *testing.T, r *VerifyReport, want Status, contains string) {
	t.Helper()
	all := strings.Join(append(append([]string{}, r.Damage...), r.Repairable...), "\n")
	if r.Status != want || !strings.Contains(all, contains) {
		t.Fatalf("status %s, want %s with %q; damage %q, repairable %q", r.Status, want, contains, r.Damage, r.Repairable)
	}
}

// reopenHead opens the directory the way the server does and returns the
// journal head it recovers.
func reopenHead(t *testing.T, dir string, clock *fakeClock) uint64 {
	t.Helper()
	l, err := Open(Options{Dir: dir, Clock: clock.Now, SweepInterval: -1, SnapshotEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Head()
}

func TestVerifySoundLedger(t *testing.T) {
	for _, tc := range []struct {
		name     string
		segBytes int64
	}{{"segments dropped", 1024}, {"whole log kept", 1 << 30}} {
		t.Run(tc.name, func(t *testing.T) {
			dir, clock := verifyFixture(t, tc.segBytes)
			r := mustVerify(t, dir)
			if r.Status != StatusSound || len(r.Damage)+len(r.Repairable) > 0 {
				t.Fatalf("status %s: %q %q", r.Status, r.Damage, r.Repairable)
			}
			if len(r.Snapshots) != 2 {
				t.Fatalf("snapshots %+v", r.Snapshots)
			}
			for _, s := range r.Snapshots {
				if !s.Matched || s.Record == 0 || s.Problem != "" {
					t.Fatalf("snapshot not checked against the log: %+v", s)
				}
			}
			if tc.segBytes == 1024 {
				if r.Log.FirstIndex == 1 || r.ReplayFrom != r.Snapshots[1].Number || len(r.Log.Segments) < 2 {
					t.Fatalf("expected replay from the older snapshot after drops: first %d, from %d, %d segments",
						r.Log.FirstIndex, r.ReplayFrom, len(r.Log.Segments))
				}
			} else if r.Log.FirstIndex != 1 || r.ReplayFrom != 0 {
				t.Fatalf("expected a full replay: first %d, from %d", r.Log.FirstIndex, r.ReplayFrom)
			}
			if r.Head == 0 || r.Accounts == 0 || r.Head != reopenHead(t, dir, clock) {
				t.Fatalf("report head %d, accounts %d", r.Head, r.Accounts)
			}
		})
	}
}

func TestVerifyRefusesALedgerInUse(t *testing.T) {
	dir, clock := verifyFixture(t, 1024)
	l := openAt(t, dir, clock, nil)
	if _, err := Verify(dir); !errors.Is(err, wal.ErrLocked) {
		t.Fatalf("err = %v", err)
	}
	l.Close()
	if _, err := Verify(dir); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRepairable(t *testing.T) {
	cases := map[string]struct {
		damage func(t *testing.T, dir string)
		want   string
	}{
		"torn final write": {func(t *testing.T, dir string) {
			f, _ := os.OpenFile(activeSegment(t, dir), os.O_APPEND|os.O_WRONLY, 0)
			f.WriteString("0badf00d {\"t\":")
			f.Close()
		}, "torn write of 14 bytes"},
		"temporary files": {func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, snapName(99)+snapTmpSuffix), []byte("half"), 0o600)
			os.WriteFile(filepath.Join(dir, walDir, "MANIFEST.tmp"), []byte("half"), 0o600)
		}, "temporary"},
		"damaged newest snapshot": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, snapName(snapshotFiles(t, dir)[0]))
			data, _ := os.ReadFile(p)
			data[len(data)/2] ^= 0x40
			os.WriteFile(p, data, 0o600)
		}, "unreadable"},
		"interrupted roll": {func(t *testing.T, dir string) {
			paths, _ := wal.SegmentPaths(filepath.Join(dir, walDir))
			last := filepath.Base(paths[len(paths)-1])
			first := strings.TrimSuffix(last, ".seg")
			n := 0
			for _, c := range first {
				n = n*10 + int(c-'0')
			}
			os.WriteFile(filepath.Join(dir, walDir, segName(uint64(n+1_000_000))), nil, 0o600)
		}, "interrupted roll"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, clock := verifyFixture(t, 1024)
			sound := mustVerify(t, dir)
			tc.damage(t, dir)
			r := mustVerify(t, dir)
			wantStatus(t, r, StatusRepairable, tc.want)
			// Repairable means the server starts and loses nothing.
			if h := reopenHead(t, dir, clock); h != sound.Head {
				t.Fatalf("head after the repair %d, want %d", h, sound.Head)
			}
		})
	}
}

func segName(first uint64) string { return fmt.Sprintf("%020d.seg", first) }

// rewriteSnapshot replaces snapshot n with a correctly checksummed one
// holding the state mutate makes of it.
func rewriteSnapshot(t *testing.T, dir string, n uint64, mutate func(st *state)) {
	t.Helper()
	s, st, err := readSnapshot(wal.OSFS{}, filepath.Join(dir, snapName(n)))
	if err != nil {
		t.Fatal(err)
	}
	l := offline()
	l.install(s, st)
	mutate(l.st)
	if _, err := writeSnapshot(wal.OSFS{}, dir, l.capture(n, s.time)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDamaged(t *testing.T) {
	cases := map[string]struct {
		segBytes int64
		damage   func(t *testing.T, dir string)
		want     string
	}{
		"flipped byte in a sealed segment": {1024, func(t *testing.T, dir string) {
			paths, _ := wal.SegmentPaths(filepath.Join(dir, walDir))
			data, _ := os.ReadFile(paths[0])
			data[20] ^= 1
			os.WriteFile(paths[0], data, 0o600)
		}, "segment"},
		"missing segment": {1024, func(t *testing.T, dir string) {
			paths, _ := wal.SegmentPaths(filepath.Join(dir, walDir))
			os.Remove(paths[1])
		}, "missing"},
		"snapshots gone after their segments were dropped": {1024, func(t *testing.T, dir string) {
			for _, n := range snapshotFiles(t, dir) {
				os.Remove(filepath.Join(dir, snapName(n)))
			}
		}, "no intact snapshot"},
		"snapshot that disagrees with the log": {1 << 30, func(t *testing.T, dir string) {
			rewriteSnapshot(t, dir, snapshotFiles(t, dir)[1], func(st *state) {
				a := *st.accounts["a"]
				a.Metadata = map[string]string{"forged": "yes"}
				st.accounts["a"] = &a
			})
		}, "differs from the log at record"},
		"snapshot that breaks an invariant": {1 << 30, func(t *testing.T, dir string) {
			rewriteSnapshot(t, dir, snapshotFiles(t, dir)[0], func(st *state) {
				for id, tr := range st.transfers {
					if tr.State != StatePosted || tr.Postings[0].Posted == tr.Postings[0].Reversed {
						continue
					}
					c := *tr
					c.Postings = append([]Posting(nil), tr.Postings...)
					c.Postings[0].Reversed++
					st.transfers[id] = &c
					return
				}
				t.Fatal("no posted transfer to damage")
			})
		}, "breaks"},
		"snapshots beside an empty log": {1 << 30, func(t *testing.T, dir string) {
			os.RemoveAll(filepath.Join(dir, walDir))
		}, "no log"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := verifyFixture(t, tc.segBytes)
			tc.damage(t, dir)
			r := mustVerify(t, dir)
			wantStatus(t, r, StatusDamaged, tc.want)
		})
	}
}

// The newest intact snapshot must be marked in the log, or the server
// refuses to start.
func TestVerifyNewestSnapshotWithoutRecord(t *testing.T) {
	dir, clock := verifyFixture(t, 1<<30)
	nums := snapshotFiles(t, dir)
	rewriteSnapshot(t, dir, nums[0], func(*state) {})
	s, st, _ := readSnapshot(wal.OSFS{}, filepath.Join(dir, snapName(nums[0])))
	l := offline()
	l.install(s, st)
	if _, err := writeSnapshot(wal.OSFS{}, dir, l.capture(nums[0]+5, s.time)); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, dir)
	wantStatus(t, r, StatusDamaged, "newest intact snapshot but has no record")
	if _, err := Open(Options{Dir: dir, Clock: clock.Now, SweepInterval: -1, SnapshotEvery: -1}); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("Open: %v", err)
	}
}

// Damage stops the replay but not the checksums: later segments are still
// read and reported.
func TestVerifyKeepsCheckingAfterDamage(t *testing.T) {
	dir, _ := verifyFixture(t, 1024)
	paths, _ := wal.SegmentPaths(filepath.Join(dir, walDir))
	data, _ := os.ReadFile(paths[0])
	data[20] ^= 1
	os.WriteFile(paths[0], data, 0o600)
	data, _ = os.ReadFile(paths[2])
	data[20] ^= 1
	os.WriteFile(paths[2], data, 0o600)
	r := mustVerify(t, dir)
	if r.Status != StatusDamaged || r.Log.Segments[0].Problem == "" || r.Log.Segments[2].Problem == "" ||
		r.Log.Segments[1].Problem != "" || r.Log.Segments[len(paths)-1].Records == 0 {
		t.Fatalf("segments %+v", r.Log.Segments)
	}
}

func TestVerifyLegacyLog(t *testing.T) {
	dir := writeLog(t, `{"t":"header","v":1}`)
	r := mustVerify(t, dir)
	if r.Status != StatusSound || !r.Log.Legacy || r.Log.NextIndex != 2 {
		t.Fatalf("%+v %+v", r, r.Log)
	}
	os.WriteFile(filepath.Join(dir, legacyLogName), []byte("00000000 {}\n"+`0badf00d {"t":"header"}`+"\n"), 0o600)
	wantStatus(t, mustVerify(t, dir), StatusDamaged, legacyLogName)
}

func TestVerifyErrors(t *testing.T) {
	if _, err := Verify(t.TempDir()); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("empty directory: %v", err)
	}
	if _, err := Verify(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := Verify(file); err == nil {
		t.Fatal("a file was accepted as a directory")
	}
}

// An intact snapshot the log does not mark is skipped for a newer one, as
// Open does, and reported as repairable.
func TestVerifySkipsAnUnmarkedOlderSnapshot(t *testing.T) {
	dir, clock := verifyFixture(t, 1024)
	nums := snapshotFiles(t, dir)
	s, st, err := readSnapshot(wal.OSFS{}, filepath.Join(dir, snapName(nums[1])))
	if err != nil {
		t.Fatal(err)
	}
	l := offline()
	l.install(s, st)
	if _, err := writeSnapshot(wal.OSFS{}, dir, l.capture(1, s.time)); err != nil { // snapshot 1's record was dropped long ago
		t.Fatal(err)
	}
	r := mustVerify(t, dir)
	wantStatus(t, r, StatusRepairable, snapName(1)+" has no record in the log")
	if r.ReplayFrom != nums[1] || len(r.Snapshots) != 3 {
		t.Fatalf("replay from %d, snapshots %+v", r.ReplayFrom, r.Snapshots)
	}
	if h := reopenHead(t, dir, clock); h != r.Head {
		t.Fatalf("Open recovers head %d, verify %d", h, r.Head)
	}
}

func TestStateDiff(t *testing.T) {
	dir, _ := verifyFixture(t, 1<<30)
	path := filepath.Join(dir, snapName(snapshotFiles(t, dir)[0]))
	load := func() (*Ledger, *snapshot, *state) {
		s, st, err := readSnapshot(wal.OSFS{}, path)
		if err != nil {
			t.Fatal(err)
		}
		l := offline()
		s2, st2, _ := readSnapshot(wal.OSFS{}, path)
		l.install(s2, st2)
		return l, s, st
	}
	anyID := func(m map[string]*Transfer, want TransferState) string {
		for id, tr := range m {
			if want == "" || tr.State == want {
				return id
			}
		}
		t.Fatalf("no transfer in state %q", want)
		return ""
	}
	cases := map[string]func(l *Ledger){
		"journal head":       func(l *Ledger) { l.st.entries = l.st.entries[:len(l.st.entries)-1] },
		"account a differs":  func(l *Ledger) { l.st.accounts["a"].Metadata = map[string]string{"forged": "yes"} },
		"accounts, the log":  func(l *Ledger) { delete(l.st.accounts, "a"); delete(l.st.accounts, "b") },
		"transfer":           func(l *Ledger) { l.st.transfers[anyID(l.st.transfers, "")].Reference = "forged" },
		"transfers, the log": func(l *Ledger) { delete(l.st.transfers, anyID(l.st.transfers, "")) },
		"journal entry 1":    func(l *Ledger) { l.st.entries[0].Kind = "forged" },
		"open pending":       func(l *Ledger) { l.st.open++ },
		"timeouts differ":    func(l *Ledger) { l.st.expiries = nil },
		"idempotency key":    func(l *Ledger) { clear(l.idem) },
	}
	l, s, st := load()
	if d := stateDiff(l, s, st); d != "" {
		t.Fatalf("identical states differ: %s", d)
	}
	if st.open == 0 || len(s.idem) == 0 {
		t.Fatalf("fixture has %d open transfers and %d keys", st.open, len(s.idem))
	}
	for want, mutate := range cases {
		l, s, st := load()
		mutate(l)
		if d := stateDiff(l, s, st); !strings.Contains(d, want) {
			t.Errorf("%s: diff %q", want, d)
		}
	}
}
