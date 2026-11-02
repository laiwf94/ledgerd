package wal

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
	"sync"
)

// A segment is one file of the log, named after the index of its first
// record (records are numbered from 1 across the whole log):
//
//	00000000000000048211.seg
//
// It holds record lines, and once full it is sealed with a footer line:
//
//	#seal <first> <records> <bytes> <crc32c of those bytes> <crc32c of the line so far>
//
// where <bytes> is the length of the segment before the footer and both
// checksums are 8 lower-case hex digits. A record line starts with a hex
// digit, so the footer can never be mistaken for one. A sealed segment
// describes itself completely: it can be verified, copied or archived on its
// own, and nothing is ever written to it again.
const (
	segSuffix  = ".seg"
	sealPrefix = "#seal "
)

// DefaultSegmentBytes is the size at which a segment is sealed and a new one
// started when Options.SegmentBytes is zero.
const DefaultSegmentBytes = 64 << 20

func segName(first uint64) string {
	return fmt.Sprintf("%020d%s", first, segSuffix)
}

// parseSegName returns the first index a segment file name stands for.
func parseSegName(name string) (uint64, bool) {
	s, ok := strings.CutSuffix(name, segSuffix)
	if !ok || len(s) != 20 {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 || segName(n) != name {
		return 0, false
	}
	return n, true
}

type footer struct {
	first, count uint64
	bytes        int64
	crc          uint32
}

func encodeFooter(f footer) []byte {
	b := []byte(sealPrefix)
	b = strconv.AppendUint(b, f.first, 10)
	b = append(b, ' ')
	b = strconv.AppendUint(b, f.count, 10)
	b = append(b, ' ')
	b = strconv.AppendInt(b, f.bytes, 10)
	b = append(b, ' ')
	b = appendHex32(b, f.crc)
	b = append(b, ' ')
	b = appendHex32(b, crc32.Checksum(b, crcTable))
	return append(b, '\n')
}

// decodeFooter parses a footer line including its newline. Only the
// canonical encoding is accepted.
func decodeFooter(line []byte) (footer, error) {
	fields := strings.Fields(string(line[len(sealPrefix):]))
	if len(fields) != 5 {
		return footer{}, errors.New("malformed seal")
	}
	var f footer
	var err1, err2, err3 error
	f.first, err1 = strconv.ParseUint(fields[0], 10, 64)
	f.count, err2 = strconv.ParseUint(fields[1], 10, 64)
	f.bytes, err3 = strconv.ParseInt(fields[2], 10, 64)
	crc, ok := parseHex32([]byte(fields[3]))
	if err1 != nil || err2 != nil || err3 != nil || !ok {
		return footer{}, errors.New("malformed seal")
	}
	f.crc = crc
	if !bytes.Equal(encodeFooter(f), line) {
		return footer{}, errors.New("seal checksum mismatch")
	}
	return f, nil
}

// segScan is what reading a segment found.
type segScan struct {
	count  uint64 // intact records
	good   int64  // bytes of intact records, which is where a seal starts
	crc    uint32 // crc32c of those bytes
	sealed bool
	end    int64 // good plus the seal, if any: the bytes to keep
}

// scanSegment reads one segment whose first record has index first, calls
// apply for each intact record and checks the seal if there is one. A torn
// final line (a record or a seal) is not an error; scan.end says where the
// intact part ends. Damage anywhere else, data after a seal, or a seal that
// disagrees with the records before it is ErrCorrupt, as is an error
// returned by apply.
func scanSegment(r io.Reader, first uint64, apply func(index uint64, payload []byte) error) (segScan, error) {
	br := getReader(r)
	defer putReader(br)
	var s segScan
	atEOF := func() bool { _, err := br.Peek(1); return errors.Is(err, io.EOF) }
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, io.EOF) {
			if s.sealed && len(line) > 0 {
				return s, fmt.Errorf("%w: data after the seal at offset %d", ErrCorrupt, s.good)
			}
			return s, nil // no trailing newline: a torn final write
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			return s, fmt.Errorf("%w: record at offset %d is longer than %d bytes", ErrCorrupt, s.end, MaxRecord)
		}
		if err != nil {
			return s, err
		}
		if s.sealed {
			return s, fmt.Errorf("%w: data after the seal at offset %d", ErrCorrupt, s.good)
		}
		if bytes.HasPrefix(line, []byte(sealPrefix)) {
			f, ferr := decodeFooter(line)
			if ferr != nil {
				if atEOF() {
					return s, nil // a torn seal: the segment was never sealed
				}
				return s, fmt.Errorf("%w: seal at offset %d: %v", ErrCorrupt, s.good, ferr)
			}
			if f.first != first || f.count != s.count || f.bytes != s.good || f.crc != s.crc {
				return s, fmt.Errorf("%w: seal says records %d+%d in %d bytes (crc %08x), segment holds %d+%d in %d bytes (crc %08x)",
					ErrCorrupt, f.first, f.count, f.bytes, f.crc, first, s.count, s.good, s.crc)
			}
			s.sealed = true
			s.end = s.good + int64(len(line))
			continue
		}
		payload, derr := decode(line[:len(line)-1])
		if derr != nil {
			if atEOF() {
				return s, nil // a damaged record is only acceptable as the very last line
			}
			return s, fmt.Errorf("%w: record at offset %d: %v", ErrCorrupt, s.good, derr)
		}
		if err := apply(first+s.count, payload); err != nil {
			return s, fmt.Errorf("%w: record %d at offset %d: %v", ErrCorrupt, first+s.count, s.good, err)
		}
		s.count++
		s.good += int64(len(line))
		s.crc = crc32.Update(s.crc, crcTable, line)
		s.end = s.good
	}
}

// Readers big enough for the longest record are pooled: replaying a log of
// many small segments would otherwise allocate and clear a megabyte per
// segment.
var readers = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, MaxRecord+16) }}

func getReader(r io.Reader) *bufio.Reader {
	br := readers.Get().(*bufio.Reader)
	br.Reset(r)
	return br
}

func putReader(br *bufio.Reader) {
	br.Reset(nil)
	readers.Put(br)
}

func crc32Checksum(b []byte) uint32 { return crc32.Checksum(b, crcTable) }
