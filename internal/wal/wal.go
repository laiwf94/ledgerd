// Package wal is an append-only log of checksummed records, the durable
// source of truth for the ledger.
//
// A record is one line:
//
//	<crc32c of payload, 8 hex digits> <payload>\n
//
// Records are numbered from 1 and stored in a directory of fixed-size
// segments (segment.go) listed by a manifest (manifest.go). Append writes to
// the newest segment; once it would grow past Options.SegmentBytes the
// segment is sealed with a footer that checksums all of it, and a new one is
// started. DropBefore removes whole sealed segments whose records are no
// longer needed, which is how the log is compacted.
//
// Append writes a whole batch of records with one write and one fsync, which
// is what makes group commit cheap: many concurrent transfers share a single
// disk flush. A record is durable once Append returns. A batch never spans
// two segments.
//
// A crash can leave a torn final line in the newest segment. Open drops it,
// because the batch it belonged to was never acknowledged. A bad record
// anywhere else, a missing segment, or a seal that disagrees with its
// segment means the log was damaged after it was written, and Open stops
// with ErrCorrupt instead of guessing which acknowledged writes to throw
// away.
package wal

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// MaxRecord is the largest payload a record may carry.
const MaxRecord = 1 << 20

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	// ErrCorrupt means the log is damaged somewhere other than its final record.
	ErrCorrupt = errors.New("log is corrupt")
	// ErrBroken means an earlier write or fsync failed. The process must
	// restart and recover from what is actually on disk.
	ErrBroken = errors.New("log is unavailable after an I/O error")
)

// File is the part of *os.File the log writes through.
type File interface {
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

// Options configures a Log.
type Options struct {
	// SegmentBytes is the size past which the newest segment is sealed and
	// a new one started. A batch larger than this still goes into one
	// segment. Default DefaultSegmentBytes.
	SegmentBytes int64
	// Legacy is the path of a single-file log written by an earlier version.
	// If it exists and the directory holds no log yet, Open adopts it as the
	// first segment.
	Legacy string
	// WrapFile, when set, wraps every file the log opens for writing:
	// segments and manifests. The failure-injection tests use it to make
	// writes and fsyncs fail.
	WrapFile func(File) File
}

type segment struct {
	first, count uint64
	size         int64 // bytes, including the seal once sealed
}

// Log is an open log. It is safe for concurrent use.
type Log struct {
	mu     sync.Mutex
	dir    string
	opts   Options
	lockf  *os.File
	segs   []segment // oldest first; the last takes appends
	f      File      // the last segment
	crc    uint32    // crc32c of the last segment's bytes
	size   int64     // bytes in all segments
	syncs  uint64
	broken error
}

// Open replays the log in dir through apply, in index order, drops a torn
// tail, and opens the newest segment for appending. The directory and an
// empty log are created if needed. apply must not retain the payload slice.
func Open(dir string, opts Options, apply func(index uint64, payload []byte) error) (*Log, error) {
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = DefaultSegmentBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockf, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(lockf); err != nil {
		lockf.Close()
		return nil, err
	}
	l := &Log{dir: dir, opts: opts, lockf: lockf}
	if err := l.open(apply); err != nil {
		if l.f != nil {
			l.f.Close()
		}
		lockf.Close()
		return nil, err
	}
	return l, nil
}

func (l *Log) wrap(f *os.File) File {
	if l.opts.WrapFile != nil {
		return l.opts.WrapFile(f)
	}
	return f
}

func (l *Log) open(apply func(uint64, []byte) error) error {
	os.Remove(filepath.Join(l.dir, manifestName+".tmp"))
	firsts, err := readManifest(l.dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if firsts, err = l.create(); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if err := l.retireLegacy(firsts[0]); err != nil {
			return err
		}
	}
	if err := l.removeOrphans(firsts); err != nil {
		return err
	}
	next := firsts[0]
	for i, first := range firsts {
		if first != next {
			return fmt.Errorf("%w: segment %s should start at record %d", ErrCorrupt, segName(first), next)
		}
		last := i == len(firsts)-1
		path := filepath.Join(l.dir, segName(first))
		flag := os.O_RDONLY
		if last {
			flag = os.O_RDWR
		}
		f, err := os.OpenFile(path, flag, 0)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: segment %s is missing", ErrCorrupt, segName(first))
		}
		if err != nil {
			return err
		}
		s, err := scanSegment(f, first, apply)
		if err == nil && !last && !s.sealed {
			err = fmt.Errorf("%w: segment %s is not sealed but is not the newest", ErrCorrupt, segName(first))
		}
		if err == nil && !last {
			if st, serr := f.Stat(); serr != nil || st.Size() != s.end {
				err = fmt.Errorf("%w: segment %s does not end at its seal", ErrCorrupt, segName(first))
			}
		}
		if err != nil {
			f.Close()
			return fmt.Errorf("segment %s: %w", segName(first), err)
		}
		l.segs = append(l.segs, segment{first: first, count: s.count, size: s.end})
		l.size += s.end
		next = first + s.count
		if !last {
			f.Close()
			continue
		}
		if err := truncateTo(f, s.end); err != nil {
			f.Close()
			return err
		}
		l.f, l.crc = l.wrap(f), s.crc
		if s.sealed {
			// A crash between sealing this segment and listing the next one.
			return l.startSegment(next)
		}
	}
	return nil
}

// truncateTo cuts a torn tail off f and leaves it positioned for appends.
func truncateTo(f *os.File, end int64) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != end {
		if err := f.Truncate(end); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
	}
	_, err = f.Seek(end, io.SeekStart)
	return err
}

// create starts a log in a directory without a manifest: from the legacy
// single-file log if there is one, otherwise empty.
func (l *Log) create() ([]uint64, error) {
	segs, err := listSegments(l.dir)
	if err != nil {
		return nil, err
	}
	legacy, err := os.Open(l.opts.Legacy)
	if l.opts.Legacy == "" || errors.Is(err, os.ErrNotExist) {
		// Segment files without a manifest are left by a crash while the log
		// was being created, before any record could be appended.
		for _, first := range segs {
			path := filepath.Join(l.dir, segName(first))
			if st, err := os.Stat(path); err != nil || st.Size() != 0 {
				return nil, fmt.Errorf("%w: segment %s exists without a manifest", ErrCorrupt, segName(first))
			}
			if err := os.Remove(path); err != nil {
				return nil, err
			}
		}
		osf, err := os.OpenFile(filepath.Join(l.dir, segName(1)), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		f := l.wrap(osf)
		err = f.Sync()
		if err == nil {
			err = syncDir(l.dir)
		}
		if err == nil {
			err = writeManifest(l.dir, []uint64{1}, l.opts.WrapFile)
		}
		f.Close()
		if err != nil {
			return nil, err
		}
		return []uint64{1}, nil
	}
	if err != nil {
		return nil, err
	}
	defer legacy.Close()
	// An earlier version locked the file itself; hold that lock too while
	// moving it, so an old binary cannot be appending to it.
	if err := lock(legacy); err != nil {
		return nil, err
	}
	// Any segment here is a link made by an interrupted migration.
	for _, first := range segs {
		if err := os.Remove(filepath.Join(l.dir, segName(first))); err != nil {
			return nil, err
		}
	}
	if err := os.Link(l.opts.Legacy, filepath.Join(l.dir, segName(1))); err != nil {
		return nil, err
	}
	if err := syncDir(l.dir); err != nil {
		return nil, err
	}
	if err := writeManifest(l.dir, []uint64{1}, l.opts.WrapFile); err != nil {
		return nil, err
	}
	if err := l.retireLegacy(1); err != nil {
		return nil, err
	}
	return []uint64{1}, nil
}

// retireLegacy removes the legacy file once the manifest lists the segment it
// became. It is a second link to the first segment, unless something wrote
// a separate log there after the migration.
func (l *Log) retireLegacy(first uint64) error {
	if l.opts.Legacy == "" {
		return nil
	}
	lst, err := os.Stat(l.opts.Legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sst, err := os.Stat(filepath.Join(l.dir, segName(first)))
	if err != nil || !os.SameFile(lst, sst) {
		return fmt.Errorf("%w: both %s and a segmented log in %s exist", ErrCorrupt, l.opts.Legacy, l.dir)
	}
	if err := os.Remove(l.opts.Legacy); err != nil {
		return err
	}
	return syncDir(filepath.Dir(l.opts.Legacy))
}

// removeOrphans deletes segment files the manifest does not list: older
// ones left by an interrupted drop, and an empty newer one left by an
// interrupted roll. Anything else is damage.
func (l *Log) removeOrphans(firsts []uint64) error {
	segs, err := listSegments(l.dir)
	if err != nil {
		return err
	}
	listed := map[uint64]bool{}
	for _, f := range firsts {
		listed[f] = true
	}
	removed := false
	for _, first := range segs {
		if listed[first] {
			continue
		}
		path := filepath.Join(l.dir, segName(first))
		switch {
		case first < firsts[0]:
		case first > firsts[len(firsts)-1]:
			if st, err := os.Stat(path); err != nil || st.Size() != 0 {
				return fmt.Errorf("%w: segment %s is past the end of the log", ErrCorrupt, segName(first))
			}
		default:
			return fmt.Errorf("%w: segment %s is not in the manifest", ErrCorrupt, segName(first))
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDir(l.dir)
	}
	return nil
}

// listSegments returns the first index of every segment file in dir, in order.
func listSegments(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, e := range entries {
		if n, ok := parseSegName(e.Name()); ok {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Read replays the log in dir without locking or changing it, and returns
// the index the next record would get. A torn tail in the newest segment is
// skipped, as Open would drop it. It is meant for tools and tests that look
// at a log no process is appending to.
func Read(dir string, apply func(index uint64, payload []byte) error) (uint64, error) {
	firsts, err := readManifest(dir)
	if err != nil {
		return 0, err
	}
	next := firsts[0]
	for i, first := range firsts {
		if first != next {
			return 0, fmt.Errorf("%w: segment %s should start at record %d", ErrCorrupt, segName(first), next)
		}
		f, err := os.Open(filepath.Join(dir, segName(first)))
		if err != nil {
			return 0, fmt.Errorf("%w: segment %s: %v", ErrCorrupt, segName(first), err)
		}
		s, err := scanSegment(f, first, apply)
		f.Close()
		if err == nil && i < len(firsts)-1 && !s.sealed {
			err = fmt.Errorf("%w: segment is not sealed but is not the newest", ErrCorrupt)
		}
		if err != nil {
			return 0, fmt.Errorf("segment %s: %w", segName(first), err)
		}
		next = first + s.count
	}
	return next, nil
}

// Replay applies every intact record in a stream of record lines, such as a
// log written by an earlier, single-file version, and returns the offset
// where the intact prefix ends. It knows nothing of seals. A torn final record is not an error; any other damage is
// ErrCorrupt, as is an error returned by apply.
func Replay(r io.Reader, apply func(payload []byte) error) (int64, error) {
	br := bufio.NewReaderSize(r, MaxRecord+16)
	var offset int64
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, io.EOF) {
			return offset, nil // no trailing newline: a torn final write
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			return 0, fmt.Errorf("%w: record at offset %d is longer than %d bytes", ErrCorrupt, offset, MaxRecord)
		}
		if err != nil {
			return 0, err
		}
		payload, derr := decode(line[:len(line)-1])
		if derr != nil {
			// A damaged record is only acceptable as the very last thing in the file.
			if _, perr := br.Peek(1); errors.Is(perr, io.EOF) {
				return offset, nil
			}
			return 0, fmt.Errorf("%w: record at offset %d: %v", ErrCorrupt, offset, derr)
		}
		n := int64(len(line))
		if err := apply(payload); err != nil {
			return 0, fmt.Errorf("%w: record at offset %d: %v", ErrCorrupt, offset, err)
		}
		offset += n
	}
}

// Encode frames one payload as a log line.
func Encode(dst, payload []byte) []byte {
	var sum [4]byte
	c := crc32.Checksum(payload, crcTable)
	sum[0], sum[1], sum[2], sum[3] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)
	dst = hex.AppendEncode(dst, sum[:])
	dst = append(dst, ' ')
	dst = append(dst, payload...)
	return append(dst, '\n')
}

func decode(line []byte) ([]byte, error) {
	if len(line) < 9 || line[8] != ' ' {
		return nil, errors.New("malformed record")
	}
	for _, c := range line[:8] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, errors.New("malformed checksum") // lower-case hex only, so each record has one encoding
		}
	}
	var sum [4]byte
	hex.Decode(sum[:], line[:8])
	payload := line[9:]
	want := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
	if crc32.Checksum(payload, crcTable) != want {
		return nil, errors.New("checksum mismatch")
	}
	return payload, nil
}

func validPayload(p []byte) error {
	if len(p) > MaxRecord {
		return fmt.Errorf("record of %d bytes is larger than %d", len(p), MaxRecord)
	}
	if bytes.IndexByte(p, '\n') >= 0 {
		return errors.New("record payload contains a newline")
	}
	return nil
}

// Append durably writes a batch of records: one write, one fsync. Either the
// whole call succeeds, or the log is marked broken and every later call fails
// with ErrBroken. If the batch would take the newest segment past
// SegmentBytes, that segment is sealed first and the batch starts a new one.
//
// The log does not try to carry on after a failed write or fsync. After a
// failed fsync the kernel may already have dropped the dirty pages, so neither
// the file contents nor a retry can be trusted; the only safe state is the one
// recovered from disk after a restart. Clients learn the outcome of an
// ambiguous request by retrying it with its idempotency key.
func (l *Log) Append(payloads ...[]byte) error {
	n := 0
	for _, p := range payloads {
		if err := validPayload(p); err != nil {
			return err
		}
		n += len(p) + 10
	}
	buf := make([]byte, 0, n)
	for _, p := range payloads {
		buf = Encode(buf, p)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.broken != nil {
		return fmt.Errorf("%w: %v", ErrBroken, l.broken)
	}
	active := &l.segs[len(l.segs)-1]
	if active.count > 0 && active.size+int64(len(buf)) > l.opts.SegmentBytes {
		if err := l.roll(); err != nil {
			l.broken = fmt.Errorf("starting a new segment: %w", err)
			return fmt.Errorf("%w: %v", ErrBroken, l.broken)
		}
		active = &l.segs[len(l.segs)-1]
	}
	if _, err := l.f.Write(buf); err != nil {
		l.broken = fmt.Errorf("write: %w", err)
		return fmt.Errorf("%w: %v", ErrBroken, l.broken)
	}
	if err := l.f.Sync(); err != nil {
		l.broken = fmt.Errorf("fsync: %w", err)
		return fmt.Errorf("%w: %v", ErrBroken, l.broken)
	}
	active.count += uint64(len(payloads))
	active.size += int64(len(buf))
	l.crc = crc32.Update(l.crc, crcTable, buf)
	l.size += int64(len(buf))
	l.syncs++
	return nil
}

// roll seals the newest segment and starts the next one. The order makes
// every crash point recoverable: a sealed newest segment means the next one
// was never listed, and Open finishes the roll; an unlisted newer file is
// empty and removed.
func (l *Log) roll() error {
	active := &l.segs[len(l.segs)-1]
	seal := encodeFooter(footer{first: active.first, count: active.count, bytes: active.size, crc: l.crc})
	if _, err := l.f.Write(seal); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	active.size += int64(len(seal))
	l.size += int64(len(seal))
	return l.startSegment(active.first + active.count)
}

// startSegment closes the newest segment, which must be sealed, and creates
// and lists an empty one starting at record first.
func (l *Log) startSegment(first uint64) error {
	if err := l.f.Close(); err != nil {
		return err
	}
	l.f = nil
	osf, err := os.OpenFile(filepath.Join(l.dir, segName(first)), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	l.f, l.crc = l.wrap(osf), 0
	if err := l.f.Sync(); err != nil {
		return err
	}
	if err := syncDir(l.dir); err != nil {
		return err
	}
	firsts := make([]uint64, 0, len(l.segs)+1)
	for _, s := range l.segs {
		firsts = append(firsts, s.first)
	}
	if err := writeManifest(l.dir, append(firsts, first), l.opts.WrapFile); err != nil {
		return err
	}
	l.segs = append(l.segs, segment{first: first})
	return nil
}

// DropBefore deletes the sealed segments whose records all have an index
// below index, and returns how many it deleted. The newest segment is never
// dropped. The manifest is replaced first, so a crash leaves at worst
// unlisted files that the next Open removes. A failure before the manifest
// is replaced changes nothing; a failed directory fsync after it breaks the
// log, since the directory can no longer be trusted.
func (l *Log) DropBefore(index uint64) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.broken != nil {
		return 0, fmt.Errorf("%w: %v", ErrBroken, l.broken)
	}
	k := 0
	for k < len(l.segs)-1 && l.segs[k].first+l.segs[k].count <= index {
		k++
	}
	if k == 0 {
		return 0, nil
	}
	firsts := make([]uint64, 0, len(l.segs)-k)
	for _, s := range l.segs[k:] {
		firsts = append(firsts, s.first)
	}
	if err := writeManifest(l.dir, firsts, l.opts.WrapFile); err != nil {
		if errors.Is(err, errAfterRename) {
			l.broken = err
			return 0, fmt.Errorf("%w: %v", ErrBroken, err)
		}
		return 0, err
	}
	dropped := l.segs[:k]
	l.segs = append([]segment(nil), l.segs[k:]...)
	var firstErr error
	for _, s := range dropped {
		l.size -= s.size
		if err := os.Remove(filepath.Join(l.dir, segName(s.first))); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := syncDir(l.dir); err != nil && firstErr == nil {
		firstErr = err
	}
	// The manifest no longer lists them, so a file left behind is only space.
	return k, firstErr
}

// Size returns the number of bytes in all segments.
func (l *Log) Size() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.size
}

// Syncs returns the number of successful Append fsyncs.
func (l *Log) Syncs() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.syncs
}

// Segments returns the number of segments, sealed and active.
func (l *Log) Segments() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.segs)
}

// FirstIndex returns the index of the oldest record still in the log.
func (l *Log) FirstIndex() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.segs[0].first
}

// NextIndex returns the index the next appended record will get.
func (l *Log) NextIndex() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.segs[len(l.segs)-1]
	return s.first + s.count
}

// Err returns the error that broke the log, or nil.
func (l *Log) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.broken
}

// Close closes the newest segment and releases the lock.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	if l.f != nil {
		err = l.f.Close()
		l.f = nil
	}
	if l.lockf != nil {
		l.lockf.Close()
		l.lockf = nil
	}
	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
