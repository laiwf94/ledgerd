package export

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// Status is the outcome of verifying an export.
type Status string

// Verification outcomes.
const (
	// StatusVerified: the file is exactly what the manifest describes and
	// the manifest is signed by the trusted key.
	StatusVerified Status = "verified"
	// StatusIntact: the file matches its manifest, but no trusted key was
	// given, so the signature was not checked and anyone could have written
	// both.
	StatusIntact Status = "intact"
	// StatusDamaged: the file does not match its manifest, is cut short,
	// is not an export, or its signature does not verify.
	StatusDamaged Status = "damaged"
)

// Signature check results.
const (
	SignatureValid     = "valid"
	SignatureInvalid   = "invalid"
	SignatureUnchecked = "unchecked"
	SignatureMissing   = "missing"
)

// MaxLineBytes bounds one line of an export; longer lines are damage.
const MaxLineBytes = 16 << 20

const maxProblems = 32

// Report is what Verify found.
type Report struct {
	Status    Status    `json:"status"`
	Format    Format    `json:"format,omitempty"`
	Manifest  *Manifest `json:"manifest,omitempty"`
	Signature string    `json:"signature"`
	// Entries, Rows and Bytes are what the file holds before its manifest.
	Entries uint64 `json:"entries"`
	Rows    uint64 `json:"rows"`
	Bytes   uint64 `json:"bytes"`
	// IntactEntries is how many leading entries a checkpoint, or the
	// final hash, confirms are as exported, when the file is damaged.
	IntactEntries uint64   `json:"intact_entries"`
	Problems      []string `json:"problems,omitempty"`
	more          int
}

func (r *Report) problem(format string, args ...any) {
	if len(r.Problems) < maxProblems {
		r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
	} else {
		r.more++
	}
}

// verifier holds the state of one pass over a file.
type verifier struct {
	rep     *Report
	h       hash.Hash
	format  Format
	seq     uint64 // the entry being read
	line    int    // CSV: the last line index of the entry being read
	minTime time.Time
	maxTime time.Time
	prefix  []Checkpoint // the running hash at every CheckpointEvery entries
}

// boundary runs before the first row of a new entry and at the manifest:
// the entry count is complete there, so it is where checkpoints fall.
func (v *verifier) boundary() {
	n := v.rep.Entries
	if n > 0 && n%checkpointEvery == 0 && (len(v.prefix) == 0 || v.prefix[len(v.prefix)-1].Entries != n) {
		v.prefix = append(v.prefix, Checkpoint{Entries: n, Seq: v.seq, Bytes: v.rep.Bytes, SHA256: hex.EncodeToString(v.h.Sum(nil))})
	}
}

// Verify reads an export and checks it against its manifest: every byte
// against the running hashes, the counts and sequence range, that entries
// come in sequence order up to the head and inside the period, and, when
// trusted is not nil, the manifest's signature against that key. It
// returns an error only when reading fails; anything wrong with the
// content is in the report.
func Verify(r io.Reader, trusted ed25519.PublicKey) (*Report, error) {
	rep := &Report{Signature: SignatureMissing}
	v := &verifier{rep: rep, h: sha256.New()}
	br := bufio.NewReaderSize(r, 1<<16)
	var rec []byte
	var manifest []byte
	var after int // lines after the manifest
	first := true
	for {
		var err error
		rec, err = readRecord(br, rec[:0], v.format == CSV)
		if len(rec) == 0 && err == io.EOF {
			break
		}
		if err != nil && err != io.EOF {
			if errors.Is(err, errLineTooLong) {
				rep.problem("a line is longer than %d bytes; this is not an export", MaxLineBytes)
				break
			}
			return nil, err
		}
		if manifest != nil {
			after++
			continue
		}
		if first {
			first = false
			switch {
			case bytes.HasPrefix(rec, []byte(ndjsonManifestPrefix)) || bytes.HasPrefix(rec, []byte("{")):
				v.format = NDJSON
			case bytes.HasPrefix(rec, []byte("seq,")):
				v.format = CSV
				v.hash(rec)
				v.header(rec)
				continue
			default:
				rep.problem("the first line is neither a CSV header nor JSON; this is not an export")
				return v.finish(nil, trusted), nil
			}
		}
		if v.isManifest(rec) {
			v.boundary()
			manifest = slices.Clone(rec)
			if err == io.EOF {
				rep.problem("the manifest line has no final newline")
			}
			continue
		}
		if err == io.EOF {
			rep.problem("the last line has no final newline")
		}
		v.row(rec) // first: a checkpoint falls before the row's bytes
		v.hash(rec)
	}
	if after > 0 {
		rep.problem("%d lines follow the manifest", after)
	}
	return v.finish(manifest, trusted), nil
}

var errLineTooLong = errors.New("export: line too long")

// readRecord appends the next line to buf, newline included. For CSV it
// keeps reading while a quoted field is open, so a record whose field holds
// a newline stays whole.
func readRecord(br *bufio.Reader, buf []byte, csvRecord bool) ([]byte, error) {
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxLineBytes {
			return buf, errLineTooLong
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return buf, err
		}
		if csvRecord && bytes.Count(buf, []byte{'"'})%2 == 1 && !bytes.HasPrefix(buf, []byte(csvManifestPrefix)) {
			continue
		}
		return buf, nil
	}
}

func (v *verifier) isManifest(rec []byte) bool {
	if v.format == CSV {
		return bytes.HasPrefix(rec, []byte(csvManifestPrefix))
	}
	return bytes.HasPrefix(rec, []byte(ndjsonManifestPrefix))
}

func (v *verifier) hash(rec []byte) {
	v.h.Write(rec)
	v.rep.Bytes += uint64(len(rec))
}

func (v *verifier) header(rec []byte) {
	cols, err := csv.NewReader(bytes.NewReader(rec)).Read()
	if err != nil || !slices.Equal(cols, CSVHeader) {
		v.rep.problem("the CSV header is not ledgerd's")
	}
}

// row checks one data line and counts it.
func (v *verifier) row(rec []byte) {
	rep := v.rep
	rep.Rows++
	var seq uint64
	var at time.Time
	lineIdx := -1
	if v.format == NDJSON {
		var e ledger.Entry
		if err := json.Unmarshal(rec, &e); err != nil || e.Seq == 0 {
			rep.problem("row %d is not a journal entry", rep.Rows)
			return
		}
		seq, at = e.Seq, e.Time
	} else {
		r := csv.NewReader(bytes.NewReader(rec))
		r.FieldsPerRecord = len(CSVHeader)
		cols, err := r.Read()
		if err != nil {
			rep.problem("row %d is not a CSV row of %d columns", rep.Rows, len(CSVHeader))
			return
		}
		n, err1 := strconv.ParseUint(cols[0], 10, 64)
		t, err2 := time.Parse(time.RFC3339Nano, cols[1])
		if err1 != nil || err2 != nil || n == 0 {
			rep.problem("row %d has no valid seq and time", rep.Rows)
			return
		}
		seq, at = n, t
		if cols[5] != "" {
			if lineIdx, err = strconv.Atoi(cols[5]); err != nil || lineIdx < 0 {
				rep.problem("row %d has an invalid line number", rep.Rows)
				return
			}
		}
	}
	switch {
	case seq > v.seq:
		v.boundary()
		rep.Entries++
		v.seq = seq
		v.line = lineIdx
		v.noteTime(at)
		if lineIdx > 0 {
			rep.problem("entry %d starts at line %d", seq, lineIdx)
		}
	case seq == v.seq && v.format == CSV && lineIdx == v.line+1 && lineIdx > 0:
		v.line = lineIdx
	default:
		rep.problem("row %d (entry %d) is out of order after entry %d", rep.Rows, seq, v.seq)
	}
}

func (v *verifier) noteTime(at time.Time) {
	if v.minTime.IsZero() || at.Before(v.minTime) {
		v.minTime = at
	}
	if at.After(v.maxTime) {
		v.maxTime = at
	}
}

// finish checks the manifest against what was read.
func (v *verifier) finish(line []byte, trusted ed25519.PublicKey) *Report {
	rep := v.rep
	rep.Format = v.format
	defer func() {
		if rep.more > 0 {
			rep.Problems = append(rep.Problems, fmt.Sprintf("and %d more problems", rep.more))
		}
		switch {
		case len(rep.Problems) > 0 || rep.Signature == SignatureInvalid || rep.Signature == SignatureMissing:
			rep.Status = StatusDamaged
		case rep.Signature == SignatureValid:
			rep.Status = StatusVerified
		default:
			rep.Status = StatusIntact
		}
	}()
	if line == nil {
		if len(rep.Problems) == 0 || rep.Rows > 0 {
			rep.problem("there is no manifest: the file was cut short or is not an export")
		}
		return rep
	}
	if v.format == CSV {
		line = line[len(csvManifestPrefix):]
	}
	var ml manifestLine
	var m Manifest
	if err := json.Unmarshal(line, &ml); err != nil || len(ml.Manifest) == 0 || json.Unmarshal(ml.Manifest, &m) != nil {
		rep.problem("the manifest line is not a manifest")
		return rep
	}
	rep.Manifest = &m
	sig, err := decodeBase64(ml.Signature)
	switch {
	case err != nil || len(sig) != ed25519.SignatureSize:
		rep.Signature = SignatureMissing
		rep.problem("the manifest has no well-formed signature")
	case trusted == nil:
		rep.Signature = SignatureUnchecked
	case m.KeyID != KeyID(trusted):
		rep.Signature = SignatureInvalid
		rep.problem("the manifest names key %s, not the trusted key %s", m.KeyID, KeyID(trusted))
	case !ed25519.Verify(trusted, signedBytes(ml.Manifest), sig):
		rep.Signature = SignatureInvalid
		rep.problem("the manifest's signature does not verify with the trusted key")
	default:
		rep.Signature = SignatureValid
	}
	if m.Version != Version {
		rep.problem("manifest version %d is not %d", m.Version, Version)
		return rep
	}
	if m.Format != v.format {
		rep.problem("the manifest says %s but the file is %s", m.Format, v.format)
	}
	sum := hex.EncodeToString(v.h.Sum(nil))
	if m.Bytes != rep.Bytes || m.SHA256 != sum {
		rep.problem("the file holds %d bytes with SHA-256 %s; the manifest says %d bytes with %s", rep.Bytes, sum, m.Bytes, m.SHA256)
	} else {
		rep.IntactEntries = rep.Entries
	}
	if m.Entries != rep.Entries || m.Rows != rep.Rows {
		rep.problem("the file holds %d entries in %d rows; the manifest says %d in %d", rep.Entries, rep.Rows, m.Entries, m.Rows)
	}
	if m.LastSeq > m.HeadSeq || m.FirstSeq > m.LastSeq || (m.Entries == 0) != (m.LastSeq == 0) {
		rep.problem("the manifest's sequence range %d to %d does not fit head %d", m.FirstSeq, m.LastSeq, m.HeadSeq)
	}
	if v.seq > m.HeadSeq {
		rep.problem("entry %d is after the export's head %d", v.seq, m.HeadSeq)
	}
	if v.seq != m.LastSeq {
		rep.problem("the last entry is %d; the manifest says %d", v.seq, m.LastSeq)
	}
	if !m.From.IsZero() && !v.minTime.IsZero() && v.minTime.Before(m.From) || !m.To.IsZero() && !v.maxTime.Before(m.To) && !v.maxTime.IsZero() {
		rep.problem("entries from %s to %s fall outside the period [%s, %s)", formatTime(v.minTime), formatTime(v.maxTime),
			formatTime(m.From), formatTime(m.To))
	}
	if len(m.Checkpoints) != len(v.prefix) {
		rep.problem("the manifest has %d checkpoints; the file's %d entries make %d", len(m.Checkpoints), rep.Entries, len(v.prefix))
	}
	for i := 0; i < min(len(m.Checkpoints), len(v.prefix)); i++ {
		if m.Checkpoints[i] != v.prefix[i] {
			rep.problem("the file differs from the export within entries %d to %d", m.Checkpoints[i].Entries-checkpointEvery+1, m.Checkpoints[i].Entries)
			break
		}
		if rep.IntactEntries < v.prefix[i].Entries {
			rep.IntactEntries = v.prefix[i].Entries
		}
	}
	return rep
}
