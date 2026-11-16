// Package export writes the journal as a file a downstream system can load
// and check: NDJSON or CSV, ending in a manifest of counts and running
// SHA-256 hashes that is signed with Ed25519.
//
// A file is written in one pass and never held in memory: each entry is
// encoded into a small buffer, hashed and handed to the underlying writer.
// The manifest comes last, so it can count what was written; a file cut
// short anywhere has no manifest and fails verification.
//
// The running hash is SHA-256 over every byte of the file before the
// manifest line. Every CheckpointEvery entries the manifest records the
// hash of the prefix written so far, so a damaged file is narrowed to the
// span between two checkpoints and a reader can check a prefix it has
// already loaded.
package export

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

// Format is the encoding of an export's entries.
type Format string

// Export formats.
const (
	// NDJSON writes one entry per line, as the journal feed returns it.
	NDJSON Format = "ndjson"
	// CSV writes a header and then one row per entry line, so an entry
	// with two lines takes two rows with the same seq. An entry without
	// lines (extend_pending) takes one row with the line columns empty.
	CSV Format = "csv"
)

// ParseFormat reads a format name.
func ParseFormat(s string) (Format, error) {
	switch f := Format(s); f {
	case NDJSON, CSV:
		return f, nil
	}
	return "", fmt.Errorf("format must be %s or %s", NDJSON, CSV)
}

// ContentType is the media type of a file in the format.
func (f Format) ContentType() string {
	if f == CSV {
		return "text/csv; charset=utf-8"
	}
	return "application/x-ndjson"
}

// Version is the manifest version this package writes and reads.
const Version = 1

// CheckpointEvery is how many entries lie between two checkpoints.
const CheckpointEvery = 65536

var checkpointEvery uint64 = CheckpointEvery // tests shorten it

// CSVHeader is the first row of a CSV export.
var CSVHeader = []string{"seq", "time", "kind", "transfer_id", "entry_currency", "line", "account", "direction",
	"currency", "posted", "pending", "expires_at", "fx_base", "fx_quote", "fx_rate", "fx_source", "fx_quoted_at"}

// Markers that open the manifest line. The NDJSON one is a JSON object
// with a manifest key, which no entry has; the CSV one is a comment line,
// which CSV readers that honour # comments skip.
const (
	ndjsonManifestPrefix = `{"manifest":`
	csvManifestPrefix    = "#ledgerd-export "
	signatureContext     = "ledgerd export manifest v1\x00"
)

// Manifest describes an export. It is signed as the exact bytes it was
// written with.
type Manifest struct {
	Version int    `json:"version"`
	Format  Format `json:"format"`
	// From and To are the period asked for, [From, To) by entry time; a
	// zero end is open.
	From time.Time `json:"from,omitzero"`
	To   time.Time `json:"to,omitzero"`
	// HeadSeq is the newest journal entry when the export began. The file
	// holds every entry up to it whose time is in the period.
	HeadSeq uint64 `json:"head_seq"`
	// Entries counts journal entries; Rows counts data lines, which differs
	// from Entries in CSV. Neither counts the CSV header.
	Entries  uint64 `json:"entries"`
	Rows     uint64 `json:"rows"`
	FirstSeq uint64 `json:"first_seq,omitempty"`
	LastSeq  uint64 `json:"last_seq,omitempty"`
	// Bytes and SHA256 cover every byte before the manifest line.
	Bytes       uint64       `json:"bytes"`
	SHA256      string       `json:"sha256"`
	Checkpoints []Checkpoint `json:"checkpoints"`
	CreatedAt   time.Time    `json:"created_at"`
	// KeyID names the signing key: the first 8 bytes of the SHA-256 of
	// the Ed25519 public key, in hex.
	KeyID string `json:"key_id"`
}

// Checkpoint is the running hash after an entry.
type Checkpoint struct {
	Entries uint64 `json:"entries"`
	Seq     uint64 `json:"seq"`
	Bytes   uint64 `json:"bytes"`
	SHA256  string `json:"sha256"`
}

// manifestLine is the JSON object on the manifest line.
type manifestLine struct {
	Manifest  json.RawMessage `json:"manifest"`
	Signature string          `json:"signature"`
}

// Signer signs manifests with an Ed25519 key.
type Signer struct {
	priv  ed25519.PrivateKey
	keyID string
}

// SeedSize is the length of a signing key's seed.
const SeedSize = ed25519.SeedSize

// NewSigner makes a signer from a 32-byte seed.
func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != SeedSize {
		return nil, fmt.Errorf("export: a signing key seed is %d bytes, not %d", SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, keyID: KeyID(priv.Public().(ed25519.PublicKey))}, nil
}

// PublicKey is the key that verifies this signer's manifests.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// KeyID names the signer's key.
func (s *Signer) KeyID() string { return s.keyID }

// KeyID names a public key: the first 8 bytes of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// EncodeKey writes a key or seed as standard base64.
func EncodeKey(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ParsePublicKey reads a public key in standard or URL-safe base64, with or
// without padding.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeBase64(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("export: a public key is %d bytes of base64", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("export: not base64")
}

// Writer writes one export. Call Write for each entry in sequence order,
// then Close to write the manifest.
type Writer struct {
	w      io.Writer
	h      hash.Hash
	m      Manifest
	buf    bytes.Buffer
	enc    *json.Encoder
	csv    *csv.Writer
	row    []string
	err    error
	closed bool
}

// NewWriter starts an export of the journal up to head, restricted to
// entries with times in [from, to). It writes the CSV header straight away.
func NewWriter(w io.Writer, format Format, from, to time.Time, head uint64) (*Writer, error) {
	if _, err := ParseFormat(string(format)); err != nil {
		return nil, err
	}
	x := &Writer{w: w, h: sha256.New(), m: Manifest{Version: Version, Format: format, From: from.UTC(), To: to.UTC(),
		HeadSeq: head, Checkpoints: []Checkpoint{}}}
	if from.IsZero() {
		x.m.From = time.Time{}
	}
	if to.IsZero() {
		x.m.To = time.Time{}
	}
	if format == CSV {
		x.csv = csv.NewWriter(&x.buf)
		x.row = make([]string, len(CSVHeader))
		x.csv.Write(CSVHeader)
		x.csv.Flush()
		if err := x.emit(); err != nil {
			return nil, err
		}
	} else {
		x.enc = json.NewEncoder(&x.buf)
	}
	return x, nil
}

// emit hashes the buffer and writes it out.
func (x *Writer) emit() error {
	x.h.Write(x.buf.Bytes())
	x.m.Bytes += uint64(x.buf.Len())
	_, err := x.w.Write(x.buf.Bytes())
	x.buf.Reset()
	if err != nil {
		x.err = err
	}
	return err
}

// Write adds an entry. Entries must come in increasing sequence order, at
// or below the head the export was started with.
func (x *Writer) Write(e *ledger.Entry) error {
	if x.err != nil {
		return x.err
	}
	if x.closed {
		return errors.New("export: write after close")
	}
	if e.Seq <= x.m.LastSeq || e.Seq > x.m.HeadSeq {
		return fmt.Errorf("export: entry %d out of order after %d (head %d)", e.Seq, x.m.LastSeq, x.m.HeadSeq)
	}
	if x.csv != nil {
		x.m.Rows += x.encodeCSV(e)
	} else {
		if err := x.enc.Encode(e); err != nil {
			x.err = err
			return err
		}
		x.m.Rows++
	}
	if err := x.emit(); err != nil {
		return err
	}
	if x.m.Entries == 0 {
		x.m.FirstSeq = e.Seq
	}
	x.m.Entries++
	x.m.LastSeq = e.Seq
	if x.m.Entries%checkpointEvery == 0 {
		x.m.Checkpoints = append(x.m.Checkpoints, Checkpoint{Entries: x.m.Entries, Seq: e.Seq, Bytes: x.m.Bytes,
			SHA256: hex.EncodeToString(x.h.Sum(nil))})
	}
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// encodeCSV writes an entry's rows into the buffer and returns how many.
func (x *Writer) encodeCSV(e *ledger.Entry) uint64 {
	r := x.row
	clear(r)
	r[0] = strconv.FormatUint(e.Seq, 10)
	r[1] = formatTime(e.Time)
	r[2] = string(e.Kind)
	r[3] = e.TransferID
	r[4] = e.Currency
	r[11] = formatTime(e.ExpiresAt)
	if e.FX != nil {
		r[12], r[13], r[14], r[15], r[16] = e.FX.Base, e.FX.Quote, e.FX.Rate, e.FX.Source, formatTime(e.FX.QuotedAt)
	}
	if len(e.Lines) == 0 {
		x.csv.Write(r)
		x.csv.Flush()
		return 1
	}
	for i := range e.Lines {
		ln := &e.Lines[i]
		cur := ln.Currency
		if cur == "" {
			cur = e.Currency
		}
		r[5] = strconv.Itoa(i)
		r[6] = ln.Account
		r[7] = string(ln.Direction)
		r[8] = cur
		r[9] = strconv.FormatInt(ln.Posted, 10)
		r[10] = strconv.FormatInt(ln.Pending, 10)
		x.csv.Write(r)
	}
	x.csv.Flush()
	return uint64(len(e.Lines))
}

// Close writes the signed manifest and returns it. The export is complete
// only once Close returns nil.
func (x *Writer) Close(s *Signer, now time.Time) (Manifest, error) {
	if x.err != nil {
		return Manifest{}, x.err
	}
	if x.closed {
		return Manifest{}, errors.New("export: closed twice")
	}
	x.closed = true
	x.m.SHA256 = hex.EncodeToString(x.h.Sum(nil))
	x.m.CreatedAt = now.UTC()
	x.m.KeyID = s.KeyID()
	raw, err := json.Marshal(x.m)
	if err != nil {
		return Manifest{}, err
	}
	line, err := json.Marshal(manifestLine{Manifest: raw, Signature: EncodeKey(ed25519.Sign(s.priv, signedBytes(raw)))})
	if err != nil {
		return Manifest{}, err
	}
	if x.m.Format == CSV {
		x.buf.WriteString(csvManifestPrefix)
	}
	x.buf.Write(line)
	x.buf.WriteByte('\n')
	_, err = x.w.Write(x.buf.Bytes())
	x.buf.Reset()
	if err != nil {
		x.err = err
		return Manifest{}, err
	}
	return x.m, nil
}

func signedBytes(raw []byte) []byte {
	return append([]byte(signatureContext), raw...)
}
