package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SegmentReport is what Inspect found in one segment file.
type SegmentReport struct {
	Name    string `json:"name"`
	First   uint64 `json:"first"`   // index of its first record
	Records uint64 `json:"records"` // intact records
	Bytes   int64  `json:"bytes"`   // size of the file
	Sealed  bool   `json:"sealed"`
	// Torn is the number of bytes after the last intact record (or seal):
	// a write that never completed, which Open cuts off.
	Torn int64 `json:"torn,omitempty"`
	// Problem describes damage that Open refuses; empty when the segment
	// reads back as Open expects.
	Problem string `json:"problem,omitempty"`
}

// Report is what Inspect found in a log directory. Damage lists everything
// that stops Open; Repairable lists what Open fixes on its own (a torn
// final write, an interrupted roll, drop or upgrade) without losing a
// record that was ever acknowledged.
type Report struct {
	// Empty is true when there is no log at all: no manifest, no segment
	// files and no legacy file.
	Empty bool `json:"empty,omitempty"`
	// Legacy is true when the log is still the single file of an earlier
	// version, which the next Open converts.
	Legacy     bool            `json:"legacy,omitempty"`
	Segments   []SegmentReport `json:"segments"`
	FirstIndex uint64          `json:"first_index"` // index of the oldest record kept
	NextIndex  uint64          `json:"next_index"`  // index the next record would get
	Damage     []string        `json:"-"`
	Repairable []string        `json:"-"`
}

// Inspect reads the log in dir without locking or changing anything, and
// reports on every file in it. It passes each intact record to apply in
// index order, the way Open would, until the first damage or the first error
// apply returns; after that it keeps checking the checksums of the remaining
// segments so the report covers the whole log. legacy is the path of a
// single-file log written by an earlier version, or empty.
//
// The error is only for failures to read the directory; damage is reported.
func Inspect(dir, legacy string, apply func(index uint64, payload []byte) error) (*Report, error) {
	rep := &Report{}
	stopped := false
	damage := func(format string, args ...any) {
		rep.Damage = append(rep.Damage, fmt.Sprintf(format, args...))
		stopped = true
	}
	repairable := func(format string, args ...any) {
		rep.Repairable = append(rep.Repairable, fmt.Sprintf(format, args...))
	}
	guarded := func(index uint64, p []byte) error {
		if stopped {
			return nil
		}
		if err := apply(index, p); err != nil {
			damage("record %d: %v", index, err)
		}
		return nil
	}

	segs, err := listSegments(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, manifestName+".tmp")); err == nil {
		repairable("a temporary manifest was left by an interrupted write")
	}
	legacyInfo, legacyErr := os.Stat(legacy)
	hasLegacy := legacy != "" && legacyErr == nil
	if legacy != "" && legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist) {
		return nil, legacyErr
	}

	firsts, err := readManifest(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return inspectWithoutManifest(rep, dir, legacy, segs, hasLegacy, guarded, damage, repairable)
	case errors.Is(err, ErrCorrupt):
		damage("MANIFEST: %v", err)
		// Without the manifest the order of the files is still known from
		// their names, so their checksums can be checked anyway.
		next := uint64(0)
		for i, first := range segs {
			s := inspectSegment(dir, first, i == len(segs)-1, &next, guarded, damage)
			rep.Segments = append(rep.Segments, s)
		}
		if len(segs) > 0 {
			rep.FirstIndex, rep.NextIndex = segs[0], next
		}
		return rep, nil
	case err != nil:
		return nil, err
	}

	if hasLegacy {
		sst, serr := os.Stat(filepath.Join(dir, segName(firsts[0])))
		if serr == nil && os.SameFile(legacyInfo, sst) {
			repairable("%s is a second name for the first segment, left by an interrupted upgrade", filepath.Base(legacy))
		} else {
			damage("both %s and a segmented log exist", filepath.Base(legacy))
		}
	}
	listed := map[uint64]bool{}
	for _, f := range firsts {
		listed[f] = true
	}
	for _, first := range segs {
		if listed[first] {
			continue
		}
		switch {
		case first < firsts[0]:
			repairable("segment %s is older than the log and was left by an interrupted drop", segName(first))
		case first > firsts[len(firsts)-1] && fileSize(filepath.Join(dir, segName(first))) == 0:
			repairable("empty segment %s was left by an interrupted roll", segName(first))
		case first > firsts[len(firsts)-1]:
			damage("segment %s is past the end of the log and is not empty", segName(first))
		default:
			damage("segment %s is not in the manifest", segName(first))
		}
	}

	rep.FirstIndex = firsts[0]
	next := firsts[0]
	for i, first := range firsts {
		last := i == len(firsts)-1
		if first != next && next != 0 {
			damage("segment %s should start at record %d", segName(first), next)
		}
		s := inspectSegment(dir, first, last, &next, guarded, damage)
		rep.Segments = append(rep.Segments, s)
		if last && s.Problem == "" && s.Sealed {
			repairable("the newest segment is sealed but the next one was never started (an interrupted roll)")
		}
		if last && s.Torn > 0 {
			repairable("segment %s ends in a torn write of %d bytes that was never acknowledged", s.Name, s.Torn)
		}
	}
	rep.NextIndex = next
	return rep, nil
}

// inspectWithoutManifest covers a directory whose manifest was never
// written: a new log, a log from an earlier version, or debris.
func inspectWithoutManifest(rep *Report, dir, legacy string, segs []uint64, hasLegacy bool,
	apply func(uint64, []byte) error, damage, repairable func(string, ...any)) (*Report, error) {
	if !hasLegacy {
		for _, first := range segs {
			if fileSize(filepath.Join(dir, segName(first))) != 0 {
				damage("segment %s exists without a manifest", segName(first))
			} else {
				repairable("empty segment %s was left by an interrupted start", segName(first))
			}
		}
		rep.Empty = len(segs) == 0
		return rep, nil
	}
	for _, first := range segs {
		repairable("segment %s was left by an interrupted upgrade", segName(first))
	}
	rep.Legacy = true
	f, err := os.Open(legacy)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := SegmentReport{Name: filepath.Base(legacy), First: 1, Bytes: fileSize(legacy)}
	scan, err := scanSegment(f, 1, apply)
	s.Records = scan.count
	switch {
	case err != nil:
		s.Problem = err.Error()
		damage("%s: %v", s.Name, err)
	case scan.sealed:
		s.Problem = "a single-file log cannot be sealed"
		damage("%s: %s", s.Name, s.Problem)
	default:
		s.Torn = s.Bytes - scan.end
		if s.Torn > 0 {
			repairable("%s ends in a torn write of %d bytes that was never acknowledged", s.Name, s.Torn)
		}
	}
	rep.Segments = []SegmentReport{s}
	rep.FirstIndex, rep.NextIndex = 1, 1+scan.count
	return rep, nil
}

// inspectSegment scans one segment, checks what Open checks, and advances
// *next past its records. A *next of zero means the first index is not yet
// known.
func inspectSegment(dir string, first uint64, last bool, next *uint64,
	apply func(uint64, []byte) error, damage func(string, ...any)) SegmentReport {
	name := segName(first)
	s := SegmentReport{Name: name, First: first}
	problem := func(format string, args ...any) {
		if s.Problem == "" {
			s.Problem = fmt.Sprintf(format, args...)
			damage("segment %s: %s", name, s.Problem)
		}
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			problem("missing")
		} else {
			problem("%v", err)
		}
		*next = 0 // nothing after a gap can be placed
		return s
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		s.Bytes = st.Size()
	}
	if *next != 0 && *next != first {
		apply = func(uint64, []byte) error { return nil } // the gap is already reported
	}
	scan, err := scanSegment(f, first, apply)
	s.Records, s.Sealed = scan.count, scan.sealed
	switch {
	case err != nil:
		problem("%v", err)
	case !last && !scan.sealed:
		problem("not sealed but not the newest")
	case !last && s.Bytes != scan.end:
		problem("does not end at its seal")
	case last:
		s.Torn = s.Bytes - scan.end
	}
	*next = first + scan.count
	return s
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}
