// Package wal is an append-only log of checksummed records, the durable
// source of truth for the ledger.
//
// The file is a sequence of lines:
//
//	<crc32c of payload, 8 hex digits> <payload>\n
//
// Append writes a whole batch of records with one write and one fsync, which
// is what makes group commit cheap: many concurrent transfers share a single
// disk flush. A record is durable once Append returns.
//
// A crash can leave a torn final line. Replay drops it, because the batch it
// belonged to was never acknowledged. A bad record anywhere else means the
// file was damaged after it was written, and replay stops with ErrCorrupt
// instead of guessing which acknowledged writes to throw away.
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
	// WrapFile, when set, wraps every file the log opens for writing. The
	// failure-injection tests use it to make writes and fsyncs fail.
	WrapFile func(File) File
}

// Log is an open log file. It is safe for concurrent use.
type Log struct {
	mu     sync.Mutex
	path   string
	opts   Options
	f      File
	size   int64
	syncs  uint64
	broken error
}

// Open replays the log at path through apply, truncates a torn tail, and
// opens the file for appending. The file is created if it does not exist.
// apply must not retain the payload slice.
func Open(path string, opts Options, apply func(payload []byte) error) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	good, err := Replay(f, apply)
	if err != nil {
		f.Close()
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() != good {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	l := &Log{path: path, opts: opts, f: f, size: good}
	if opts.WrapFile != nil {
		l.f = opts.WrapFile(f)
	}
	return l, nil
}

// Replay applies every intact record from r and returns the offset where the
// intact prefix ends. A torn final record is not an error; any other damage is
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
// with ErrBroken.
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
	if _, err := l.f.Write(buf); err != nil {
		l.broken = fmt.Errorf("write: %w", err)
		return fmt.Errorf("%w: %v", ErrBroken, l.broken)
	}
	if err := l.f.Sync(); err != nil {
		l.broken = fmt.Errorf("fsync: %w", err)
		return fmt.Errorf("%w: %v", ErrBroken, l.broken)
	}
	l.size += int64(len(buf))
	l.syncs++
	return nil
}

// Compact rewrites the log through filter: each record is kept as returned,
// or dropped when keep is false. The new file is written beside the old one,
// fsynced, and atomically renamed over it, so a crash at any point leaves
// either the old log or the new one, never a mix.
func (l *Log) Compact(filter func(payload []byte) (out []byte, keep bool, err error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.broken != nil {
		return fmt.Errorf("%w: %v", ErrBroken, l.broken)
	}
	src, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer src.Close()
	tmpPath := l.path + ".tmp"
	osTmp, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var tmp File = osTmp
	if l.opts.WrapFile != nil {
		tmp = l.opts.WrapFile(osTmp)
	}
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	w := bufio.NewWriterSize(tmp, 1<<20)
	var size int64
	end, err := Replay(io.LimitReader(src, l.size), func(p []byte) error {
		out, keep, err := filter(p)
		if err != nil || !keep {
			return err
		}
		if err := validPayload(out); err != nil {
			return err
		}
		line := Encode(nil, out)
		size += int64(len(line))
		_, err = w.Write(line)
		return err
	})
	if err != nil {
		return fail(err)
	}
	if end != l.size {
		return fail(fmt.Errorf("%w: log changed during compaction", ErrCorrupt))
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpPath, l.path); err != nil {
		return fail(err)
	}
	// From here the new file is the log, whatever happens next.
	l.f.Close()
	l.f, l.size = tmp, size
	if err := syncDir(filepath.Dir(l.path)); err != nil {
		l.broken = fmt.Errorf("fsync directory after compaction: %w", err)
		return fmt.Errorf("%w: %v", ErrBroken, l.broken)
	}
	return nil
}

// Size returns the number of bytes in the log.
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

// Err returns the error that broke the log, or nil.
func (l *Log) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.broken
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
