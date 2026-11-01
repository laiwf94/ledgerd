package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/laiwf94/ledgerd/internal/wal"
)

// Status is the verdict of Verify.
type Status string

const (
	// StatusSound means every file checks out and the server would start
	// without changing anything.
	StatusSound Status = "sound"
	// StatusRepairable means no acknowledged write is lost and the server
	// would start, but the directory holds something left by a crash or
	// damage the server works around: a torn final write, an interrupted
	// roll, drop or upgrade, a leftover temporary file, or a snapshot it
	// would skip. A normal start (or deleting the named snapshot) repairs it.
	StatusRepairable Status = "repairable"
	// StatusDamaged means the server would refuse to start, or would start
	// with state the log does not support.
	StatusDamaged Status = "damaged"
)

// ErrNoLedger means a directory holds neither a log nor snapshots.
var ErrNoLedger = errors.New("no ledger found")

// SnapshotReport is what Verify found in one snapshot file.
type SnapshotReport struct {
	Name   string `json:"name"`
	Number uint64 `json:"number"`
	Bytes  int64  `json:"bytes"`
	Head   uint64 `json:"head,omitempty"` // journal head it holds, when readable
	// Record is the log index of the snapshot's record, when it was found.
	Record uint64 `json:"record,omitempty"`
	// Matched is true when the state the log replays to at Record equals
	// the snapshot, or when the snapshot was where replay started.
	Matched bool `json:"matched"`
	// Problem is empty when the snapshot reads back intact, keeps every
	// invariant and agrees with the log.
	Problem string `json:"problem,omitempty"`
	intact  bool   // readable, consistent and keeps every invariant
	broken  bool   // readable but breaks an invariant
}

// VerifyReport is the result of Verify.
type VerifyReport struct {
	Dir       string           `json:"dir"`
	Status    Status           `json:"status"`
	Log       *wal.Report      `json:"log"`
	Snapshots []SnapshotReport `json:"snapshots"` // newest first
	// ReplayFrom is 0 when replay started at the first record of the log,
	// otherwise the number of the snapshot it started from.
	ReplayFrom  uint64   `json:"replay_from"`
	Head        uint64   `json:"head"`
	Accounts    int      `json:"accounts"`
	Transfers   int      `json:"transfers"`
	OpenPending int      `json:"open_pending"`
	Damage      []string `json:"damage,omitempty"`
	Repairable  []string `json:"repairable,omitempty"`
}

// Verify checks the ledger in dir without starting it and without changing
// any file. It holds the log's lock while it runs, so it refuses a directory
// a server has open (wal.ErrLocked) and no server can start meanwhile.
//
// It checks every checksum in every segment and snapshot, that segments
// follow each other without a gap, that each snapshot is consistent on its
// own and keeps every invariant, that replaying the log reproduces each
// snapshot at the point its record marks, and that the replayed state keeps
// every invariant CheckInvariants knows. Replay starts at the first record
// when the log still has it, so every snapshot is checked against the log;
// otherwise at the oldest snapshot that is intact and marked in the log.
//
// The error is for failures that stop the check itself; whatever is wrong
// with the data is in the report.
func Verify(dir string) (*VerifyReport, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	logDir := filepath.Join(dir, walDir)
	if _, err := os.Stat(logDir); err == nil {
		release, err := wal.HoldLock(logDir)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	v := &verifier{dir: dir, rep: &VerifyReport{Dir: dir}}
	if err := v.readSnapshots(); err != nil {
		return nil, err
	}
	if err := v.replayLog(); err != nil {
		return nil, err
	}
	v.judgeSnapshots()
	r := v.rep
	if r.Log.Empty && len(r.Snapshots) == 0 && len(r.Repairable) == 0 {
		return nil, fmt.Errorf("%w in %s", ErrNoLedger, dir)
	}
	switch {
	case len(r.Damage) > 0:
		r.Status = StatusDamaged
	case len(r.Repairable) > 0:
		r.Status = StatusRepairable
	default:
		r.Status = StatusSound
	}
	return r, nil
}

type verifier struct {
	dir string
	rep *VerifyReport
}

func (v *verifier) damage(format string, args ...any) {
	v.rep.Damage = append(v.rep.Damage, fmt.Sprintf(format, args...))
}

func (v *verifier) repairable(format string, args ...any) {
	v.rep.Repairable = append(v.rep.Repairable, fmt.Sprintf(format, args...))
}

// offline returns an empty ledger that only replays: no writer, no log.
func offline() *Ledger {
	return &Ledger{opts: Options{Clock: time.Now, IdempotencyTTL: DefaultIdempotencyTTL,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))},
		st: newState(), idem: map[string]*idemEntry{}, inflight: map[string]*flight{}, snapIndex: map[uint64]uint64{}}
}

// readSnapshots reads every snapshot file once, checking its checksums,
// its internal consistency and the invariants of the state it holds.
func (v *verifier) readSnapshots() error {
	nums, tmps, err := scanSnapshots(v.dir)
	if err != nil {
		return err
	}
	for _, name := range tmps {
		v.repairable("%s is a temporary file left by an interrupted snapshot write", name)
	}
	for _, n := range nums {
		sr := SnapshotReport{Name: snapName(n), Number: n}
		path := filepath.Join(v.dir, sr.Name)
		if fi, err := os.Stat(path); err == nil {
			sr.Bytes = fi.Size()
		}
		s, st, err := readSnapshot(path)
		switch {
		case err != nil:
			sr.Problem = err.Error()
		case s.num != n:
			sr.Problem = fmt.Sprintf("the file holds snapshot %d", s.num)
		default:
			sr.Head = s.head
			l := offline()
			l.install(s, st)
			if _, bad := l.Audit(); len(bad) > 0 {
				sr.Problem = fmt.Sprintf("breaks %d invariants, first: %s", len(bad), bad[0])
				sr.broken = true
				v.damage("%s passes its checksums but breaks %d invariants, first: %s", sr.Name, len(bad), bad[0])
			} else {
				sr.intact = true
			}
		}
		v.rep.Snapshots = append(v.rep.Snapshots, sr)
	}
	return nil
}

func (v *verifier) snap(num uint64) *SnapshotReport {
	for i := range v.rep.Snapshots {
		if v.rep.Snapshots[i].Number == num {
			return &v.rep.Snapshots[i]
		}
	}
	return nil
}

// candidates returns the intact snapshots, oldest first.
func (v *verifier) candidates() []uint64 {
	var out []uint64
	for i := len(v.rep.Snapshots) - 1; i >= 0; i-- {
		if s := v.rep.Snapshots[i]; s.intact {
			out = append(out, s.Number)
		}
	}
	return out
}

// errNoBase stops a replay that cannot start: the log no longer begins at
// record 1 and no snapshot stands in for the records before it.
var errNoBase = errors.New("the log no longer starts at record 1 and no intact snapshot holds the records before it")

// replayLog replays the log once from the best starting point. If replay
// had to start from a snapshot whose record turns out to be missing, it
// tries the next newer snapshot.
func (v *verifier) replayLog() error {
	cands := v.candidates()
	for c := 0; ; c++ {
		res, err := v.pass(cands, c)
		if err != nil {
			return err
		}
		if res.missingBase != 0 && c+1 < len(cands) {
			continue // that snapshot cannot be placed; start from the next one
		}
		v.finish(res)
		return nil
	}
}

type passResult struct {
	log         *wal.Report
	l           *Ledger
	started     bool   // at least one record was read
	from        uint64 // snapshot replay started from, 0 for the log start
	missingBase uint64 // snapshot replay started from whose record never came
	records     map[uint64]uint64
	matched     map[uint64]bool
	differs     map[uint64]string
}

// pass replays the log. When the log starts at record 1 it replays
// everything; otherwise it starts from cands[c].
func (v *verifier) pass(cands []uint64, c int) (*passResult, error) {
	res := &passResult{records: map[uint64]uint64{}, matched: map[uint64]bool{}, differs: map[uint64]string{}}
	var skipTo uint64
	apply := func(index uint64, p []byte) error {
		if !res.started {
			res.started = true
			res.l = offline()
			if index == 1 {
				return res.l.replay(p, true)
			}
			if c >= len(cands) {
				return errNoBase
			}
			n := cands[c]
			s, st, err := readSnapshot(filepath.Join(v.dir, snapName(n)))
			if err != nil {
				return fmt.Errorf("snapshot %d changed while it was being checked: %v", n, err)
			}
			res.l.install(s, st)
			res.from, skipTo = n, n
		}
		isSnap := bytes.HasPrefix(p, snapRecordPrefix)
		if skipTo != 0 {
			if !isSnap {
				return nil // already in the snapshot replay started from
			}
			r, err := decodeRecord(p)
			if err != nil {
				return err
			}
			if r.Type != recSnapshot {
				return fmt.Errorf("record of type %q looks like a snapshot record", r.Type)
			}
			res.records[r.Snap] = index
			if r.Snap == skipTo {
				if r.Seq != res.l.st.head() {
					return fmt.Errorf("snapshot %d holds head %d but its log record says %d", skipTo, res.l.st.head(), r.Seq)
				}
				res.matched[skipTo] = true
				skipTo = 0
			}
			return nil
		}
		if err := res.l.replay(p, false); err != nil {
			return err
		}
		if isSnap {
			n := res.l.lastSnap
			res.records[n] = index
			if sr := v.snap(n); sr != nil && sr.intact {
				if diff, err := v.compare(res.l, n); err != nil {
					return err
				} else if diff != "" {
					res.differs[n] = diff
				} else {
					res.matched[n] = true
				}
			}
		}
		return nil
	}
	lr, err := wal.Inspect(filepath.Join(v.dir, walDir), filepath.Join(v.dir, legacyLogName), apply)
	if err != nil {
		return nil, err
	}
	res.log = lr
	if skipTo != 0 && len(lr.Damage) == 0 {
		res.missingBase = skipTo
	}
	return res, nil
}

// compare re-reads snapshot n and compares it with the replayed state. It
// returns a description of the first difference, or "".
func (v *verifier) compare(l *Ledger, n uint64) (string, error) {
	s, st, err := readSnapshot(filepath.Join(v.dir, snapName(n)))
	if err != nil {
		return "", fmt.Errorf("snapshot %d changed while it was being checked: %v", n, err)
	}
	return stateDiff(l, s, st), nil
}

// stateDiff describes the first difference between the state l replayed to
// and a snapshot of it, or returns "". The snapshot leaves out idempotency
// keys past their retention, so its keys must be a subset of l's.
func stateDiff(l *Ledger, s *snapshot, st *state) string {
	switch {
	case l.st.head() != s.head:
		return fmt.Sprintf("the log is at journal head %d, the snapshot at %d", l.st.head(), s.head)
	case len(l.st.accounts) != len(st.accounts) || !reflect.DeepEqual(l.st.accounts, st.accounts):
		for id, a := range l.st.accounts {
			if b, ok := st.accounts[id]; !ok || !reflect.DeepEqual(a, b) {
				return fmt.Sprintf("account %s differs", id)
			}
		}
		return fmt.Sprintf("the snapshot holds %d accounts, the log %d", len(st.accounts), len(l.st.accounts))
	case len(l.st.transfers) != len(st.transfers) || !reflect.DeepEqual(l.st.transfers, st.transfers):
		for id, a := range l.st.transfers {
			if b, ok := st.transfers[id]; !ok || !reflect.DeepEqual(a, b) {
				return fmt.Sprintf("transfer %s differs", id)
			}
		}
		return fmt.Sprintf("the snapshot holds %d transfers, the log %d", len(st.transfers), len(l.st.transfers))
	case !reflect.DeepEqual(l.st.entries, st.entries):
		for i := range l.st.entries {
			if !reflect.DeepEqual(l.st.entries[i], st.entries[i]) {
				return fmt.Sprintf("journal entry %d differs", i+1)
			}
		}
		return "the journals differ"
	case l.st.open != st.open:
		return fmt.Sprintf("the snapshot has %d open pending transfers, the log %d", st.open, l.st.open)
	case !reflect.DeepEqual(liveExpiries(l.st), liveExpiries(st)):
		return "the pending transfer timeouts differ"
	}
	for _, q := range s.idem {
		e, ok := l.idem[q.id]
		if !ok || !reflect.DeepEqual(*e, q.e) {
			return fmt.Sprintf("idempotency key %q differs", q.id)
		}
	}
	return ""
}

// liveExpiries lists the scheduled timeouts that still apply, sorted.
func liveExpiries(st *state) []string {
	var out []string
	for _, e := range st.expiries {
		if tr, ok := st.transfers[e.id]; ok && tr.State == StatePending && tr.ExpiresAt.Equal(e.at) {
			out = append(out, e.id+"@"+e.at.String())
		}
	}
	sort.Strings(out)
	return out
}

// finish records what the final pass found.
func (v *verifier) finish(res *passResult) {
	r := v.rep
	r.Log, r.ReplayFrom = res.log, res.from
	r.Damage = append(r.Damage, res.log.Damage...)
	r.Repairable = append(r.Repairable, res.log.Repairable...)
	for n, idx := range res.records {
		if sr := v.snap(n); sr != nil {
			sr.Record = idx
		}
	}
	for n := range res.matched {
		v.snap(n).Matched = true
	}
	for n, diff := range res.differs {
		sr := v.snap(n)
		sr.Problem = "differs from the log: " + diff
		v.damage("%s passes its checksums but differs from the log at record %d: %s", sr.Name, sr.Record, diff)
	}
	if res.missingBase != 0 {
		v.damage("no intact snapshot is marked in the log, so the records before record %d are lost", res.log.FirstIndex)
	}

	switch {
	case res.log.Empty:
		if len(r.Snapshots) > 0 {
			v.damage("the directory holds snapshots but no log")
		}
	case !res.started && res.log.FirstIndex > 1 && len(res.log.Damage) == 0:
		v.damage("the log starts at record %d but holds no records", res.log.FirstIndex)
	case !res.started && len(res.log.Damage) == 0 && len(r.Snapshots) > 0:
		v.damage("the log is empty but the directory holds snapshots")
	}
	// The final state means something only if every record replayed.
	if res.l != nil && len(r.Damage) == 0 {
		l := res.l
		if _, bad := l.Audit(); len(bad) > 0 {
			for _, b := range bad {
				v.damage("replayed state breaks an invariant: %s", b)
			}
		}
		r.Head, r.Accounts, r.Transfers, r.OpenPending = l.st.head(), len(l.st.accounts), len(l.st.transfers), l.st.open
	}
}

// judgeSnapshots classifies snapshots that were never checked against the
// log. Open starts from the newest intact snapshot and refuses to start if
// the log does not mark it; older ones it never reads. Whether a snapshot
// has a record is only known when the whole log was read.
func (v *verifier) judgeSnapshots() {
	complete := v.rep.Log != nil && !v.rep.Log.Empty && len(v.rep.Log.Damage) == 0
	newest := true
	for i := range v.rep.Snapshots {
		sr := &v.rep.Snapshots[i]
		switch {
		case !sr.intact && !sr.broken:
			v.repairable("%s is unreadable and the server skips it (%s); delete it", sr.Name, sr.Problem)
		case !sr.intact:
			// Already reported as damage.
		case sr.Matched || sr.Record != 0 || sr.Problem != "" || !complete:
			newest = false
		case newest:
			sr.Problem = "has no record in the log"
			v.damage("%s is the newest intact snapshot but has no record in the log, so the server would refuse to start", sr.Name)
			newest = false
		default:
			sr.Problem = "has no record in the log"
			v.repairable("%s has no record in the log; the server never reads it, delete it", sr.Name)
		}
	}
}
