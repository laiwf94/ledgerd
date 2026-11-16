package export

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
)

var t0 = time.Date(2030, 3, 1, 9, 0, 0, 0, time.UTC)

func testSigner(t testing.TB, b byte) *Signer {
	t.Helper()
	s, err := NewSigner(bytes.Repeat([]byte{b}, SeedSize))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func withCheckpointEvery(t testing.TB, n uint64) {
	old := checkpointEvery
	checkpointEvery = n
	t.Cleanup(func() { checkpointEvery = old })
}

// sampleEntries has every shape an entry takes: one line pair, several
// lines, no lines (extend_pending), a conversion with an FX rate whose
// source needs CSV quoting, and the lines in another currency.
func sampleEntries(n int) []ledger.Entry {
	out := make([]ledger.Entry, 0, n)
	for i := 1; i <= n; i++ {
		e := ledger.Entry{Seq: uint64(i), Time: t0.Add(time.Duration(i) * time.Minute), Kind: ledger.KindPosted,
			TransferID: "tr_" + strconv.Itoa(i), Currency: "USD",
			Lines: []ledger.Line{{Account: "alice", Direction: ledger.Debit, Posted: int64(i)}, {Account: "bob", Direction: ledger.Credit, Posted: int64(i)}}}
		switch i % 5 {
		case 1:
			e.Kind = ledger.KindPending
			e.Lines = append(e.Lines, ledger.Line{Account: "fees", Direction: ledger.Credit, Pending: 3})
			e.Lines[0].Posted, e.Lines[0].Pending = 0, int64(i)+3
			e.Lines[1].Posted, e.Lines[1].Pending = 0, int64(i)
		case 2:
			e.Kind, e.Lines, e.ExpiresAt = ledger.KindExtendPending, nil, t0.Add(time.Hour*24)
		case 3:
			e.FX = &ledger.FX{Base: "USD", Quote: "EUR", Rate: "0.9234", Source: "desk \"A\", line 1\nline 2", QuotedAt: t0}
			e.Lines = append(e.Lines,
				ledger.Line{Account: "fx_eur", Direction: ledger.Debit, Currency: "EUR", Posted: 92},
				ledger.Line{Account: "alice_eur", Direction: ledger.Credit, Currency: "EUR", Posted: 92})
		}
		out = append(out, e)
	}
	return out
}

func writeExport(t testing.TB, format Format, entries []ledger.Entry, head uint64, s *Signer) ([]byte, Manifest) {
	t.Helper()
	var buf bytes.Buffer
	x, err := NewWriter(&buf, format, time.Time{}, time.Time{}, head)
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if err := x.Write(&entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	m, err := x.Close(s, t0)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), m
}

func verify(t testing.TB, b []byte, key ed25519.PublicKey) *Report {
	t.Helper()
	r, err := Verify(bytes.NewReader(b), key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// readBack decodes an export's entries again, so the test can compare them
// with what was written.
func readBack(t *testing.T, format Format, b []byte) []ledger.Entry {
	t.Helper()
	lines := strings.SplitAfter(string(b), "\n")
	body := strings.Join(lines[:len(lines)-2], "") // drop the manifest and the empty tail
	var out []ledger.Entry
	if format == NDJSON {
		dec := json.NewDecoder(strings.NewReader(body))
		for dec.More() {
			var e ledger.Entry
			if err := dec.Decode(&e); err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
		return out
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows[0], CSVHeader) {
		t.Fatalf("header %v", rows[0])
	}
	parseTime := func(s string) time.Time {
		if s == "" {
			return time.Time{}
		}
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, r := range rows[1:] {
		seq, _ := strconv.ParseUint(r[0], 10, 64)
		if len(out) == 0 || out[len(out)-1].Seq != seq {
			e := ledger.Entry{Seq: seq, Time: parseTime(r[1]), Kind: ledger.EntryKind(r[2]), TransferID: r[3], Currency: r[4], ExpiresAt: parseTime(r[11])}
			if r[12] != "" {
				e.FX = &ledger.FX{Base: r[12], Quote: r[13], Rate: r[14], Source: r[15], QuotedAt: parseTime(r[16])}
			}
			out = append(out, e)
		}
		if r[5] == "" {
			continue
		}
		e := &out[len(out)-1]
		if r[5] != strconv.Itoa(len(e.Lines)) {
			t.Fatalf("entry %d: line %s after %d lines", seq, r[5], len(e.Lines))
		}
		posted, _ := strconv.ParseInt(r[9], 10, 64)
		pending, _ := strconv.ParseInt(r[10], 10, 64)
		ln := ledger.Line{Account: r[6], Direction: ledger.Direction(r[7]), Posted: posted, Pending: pending}
		if r[8] != e.Currency {
			ln.Currency = r[8]
		}
		e.Lines = append(e.Lines, ln)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	withCheckpointEvery(t, 4)
	s := testSigner(t, 1)
	entries := sampleEntries(23)
	for _, format := range []Format{NDJSON, CSV} {
		t.Run(string(format), func(t *testing.T) {
			b, m := writeExport(t, format, entries, 30, s)
			if got := readBack(t, format, b); !reflect.DeepEqual(got, entries) {
				t.Fatalf("read back\n%+v\nwant\n%+v", got, entries)
			}
			wantRows := uint64(len(entries))
			if format == CSV {
				wantRows = 0
				for _, e := range entries {
					wantRows += uint64(max(len(e.Lines), 1))
				}
			}
			if m.Entries != 23 || m.Rows != wantRows || m.FirstSeq != 1 || m.LastSeq != 23 || m.HeadSeq != 30 ||
				len(m.Checkpoints) != 5 || m.KeyID != s.KeyID() || m.Version != Version || m.Format != format {
				t.Fatalf("manifest %+v", m)
			}
			// Every checkpoint is the hash of the file's prefix.
			for _, c := range m.Checkpoints {
				sum := sha256.Sum256(b[:c.Bytes])
				if c.SHA256 != hex.EncodeToString(sum[:]) || c.Seq != c.Entries {
					t.Fatalf("checkpoint %+v", c)
				}
			}
			sum := sha256.Sum256(b[:m.Bytes])
			if m.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatal("final hash is not the hash of the body")
			}
			r := verify(t, b, s.PublicKey())
			if r.Status != StatusVerified || r.Signature != SignatureValid || len(r.Problems) != 0 ||
				r.Entries != 23 || r.Rows != wantRows || r.IntactEntries != 23 || !reflect.DeepEqual(*r.Manifest, m) {
				t.Fatalf("report %+v", r)
			}
			if r := verify(t, b, nil); r.Status != StatusIntact || r.Signature != SignatureUnchecked {
				t.Fatalf("without a key: %+v", r)
			}
			other := testSigner(t, 2)
			if r := verify(t, b, other.PublicKey()); r.Status != StatusDamaged || r.Signature != SignatureInvalid {
				t.Fatalf("another key: %+v", r)
			}
		})
	}
}

func TestEmptyExport(t *testing.T) {
	s := testSigner(t, 1)
	for _, format := range []Format{NDJSON, CSV} {
		b, m := writeExport(t, format, nil, 0, s)
		if m.Entries != 0 || m.FirstSeq != 0 || m.LastSeq != 0 || len(m.Checkpoints) != 0 {
			t.Fatalf("%s: %+v", format, m)
		}
		if r := verify(t, b, s.PublicKey()); r.Status != StatusVerified {
			t.Fatalf("%s: %+v", format, r)
		}
	}
	if r := verify(t, nil, s.PublicKey()); r.Status != StatusDamaged {
		t.Fatalf("an empty file: %+v", r)
	}
}

// Every cut and every flipped bit makes a signed file fail verification.
func TestEveryCutAndFlipIsCaught(t *testing.T) {
	withCheckpointEvery(t, 3)
	s := testSigner(t, 1)
	for _, format := range []Format{NDJSON, CSV} {
		b, _ := writeExport(t, format, sampleEntries(8), 8, s)
		for n := 0; n < len(b); n++ {
			if r := verify(t, b[:n], s.PublicKey()); r.Status != StatusDamaged {
				t.Fatalf("%s cut at %d of %d: %+v", format, n, len(b), r)
			}
		}
		for i := range b {
			for _, bit := range []byte{0x01, 0x80} {
				c := bytes.Clone(b)
				c[i] ^= bit
				if r := verify(t, c, s.PublicKey()); r.Status != StatusDamaged {
					t.Fatalf("%s byte %d ^ %#x: %+v", format, i, bit, r)
				}
			}
		}
	}
}

// resign rebuilds the manifest for an edited body, as someone who has the
// signing key would, so the checks beyond the hashes can be tested.
func resign(t *testing.T, format Format, body []byte, s *Signer, edit func(*Manifest)) []byte {
	t.Helper()
	m := Manifest{Version: Version, Format: format, HeadSeq: 100, Checkpoints: []Checkpoint{}, CreatedAt: t0, KeyID: s.KeyID()}
	rep := verify(t, body, nil) // count what is there
	m.Entries, m.Rows, m.Bytes = rep.Entries, rep.Rows, uint64(len(body))
	sum := sha256.Sum256(body)
	m.SHA256 = hex.EncodeToString(sum[:])
	if edit != nil {
		edit(&m)
	}
	raw, _ := json.Marshal(m)
	line, _ := json.Marshal(manifestLine{Manifest: raw, Signature: EncodeKey(ed25519.Sign(s.priv, signedBytes(raw)))})
	out := bytes.Clone(body)
	if format == CSV {
		out = append(out, csvManifestPrefix...)
	}
	return append(append(out, line...), '\n')
}

func body(b []byte) []byte {
	i := bytes.LastIndexByte(b[:len(b)-1], '\n')
	return bytes.Clone(b[:i+1])
}

func TestStructuralChecks(t *testing.T) {
	s := testSigner(t, 1)
	entries := sampleEntries(6)
	nd, _ := writeExport(t, NDJSON, entries, 6, s)
	ndLines := strings.SplitAfter(string(body(nd)), "\n")
	swapped := ndLines[0] + ndLines[2] + ndLines[1] + strings.Join(ndLines[3:], "")
	cs, _ := writeExport(t, CSV, entries, 6, s)
	csLines := strings.SplitAfter(string(body(cs)), "\n")
	ok := func(m *Manifest) { m.FirstSeq, m.LastSeq = 1, 6 }

	cases := []struct {
		name   string
		file   []byte
		expect string
	}{
		{"entries out of order", resign(t, NDJSON, []byte(swapped), s, ok), "out of order"},
		{"entry repeated", resign(t, NDJSON, []byte(ndLines[0]+ndLines[0]), s, func(m *Manifest) { m.FirstSeq, m.LastSeq = 1, 1 }), "out of order"},
		{"head below the entries", resign(t, NDJSON, body(nd), s, func(m *Manifest) { ok(m); m.HeadSeq = 4 }), "after the export's head"},
		{"wrong counts", resign(t, NDJSON, body(nd), s, func(m *Manifest) { ok(m); m.Entries = 5 }), "the manifest says 5"},
		{"wrong last seq", resign(t, NDJSON, body(nd), s, func(m *Manifest) { m.FirstSeq, m.LastSeq = 1, 5 }), "the last entry is 6"},
		{"bad range", resign(t, NDJSON, body(nd), s, func(m *Manifest) { m.FirstSeq, m.LastSeq = 7, 6 }), "does not fit head"},
		{"outside the period", resign(t, NDJSON, body(nd), s, func(m *Manifest) { ok(m); m.From = t0.Add(3 * time.Minute) }), "outside the period"},
		{"at the period's end", resign(t, NDJSON, body(nd), s, func(m *Manifest) { ok(m); m.To = t0.Add(6 * time.Minute) }), "outside the period"},
		{"format mismatch", resign(t, NDJSON, body(nd), s, func(m *Manifest) { ok(m); m.Format = CSV }), "says csv"},
		{"unknown version", resign(t, NDJSON, body(nd), s, func(m *Manifest) { m.Version = 2 }), "version 2"},
		{"missing checkpoint", resign(t, NDJSON, body(nd), s, func(m *Manifest) {
			ok(m)
			m.Checkpoints = []Checkpoint{{Entries: 1}}
		}), "1 checkpoints"},
		{"not a journal entry", resign(t, NDJSON, []byte(ndLines[0]+"{\"x\":1}\n"), s, ok), "not a journal entry"},
		{"CSV header changed", resign(t, CSV, []byte(strings.Replace(string(body(cs)), "seq,time", "seq,when", 1)), s, ok), "header"},
		{"CSV row short", resign(t, CSV, []byte(csLines[0]+"1,x\n"), s, nil), "not a CSV row"},
		{"CSV row without time", resign(t, CSV, []byte(csLines[0]+strings.Replace(csLines[1], "2030", "x", 1)), s, nil), "no valid seq and time"},
		{"CSV line skipped", resign(t, CSV, []byte(csLines[0]+csLines[1]+csLines[3]), s, nil), "out of order"},
		{"CSV entry starts mid-way", resign(t, CSV, []byte(csLines[0]+csLines[2]), s, nil), "starts at line 1"},
		{"CSV bad line number", resign(t, CSV, []byte(csLines[0]+strings.Replace(csLines[1], ",USD,0,", ",USD,-1,", 1)), s, nil), "invalid line number"},
		{"lines after the manifest", append(bytes.Clone(nd), nd...), "follow the manifest"},
		{"manifest not JSON", append(body(nd), "{\"manifest\":nope}\n"...), "not a manifest"},
		{"manifest unsigned", append(body(nd), "{\"manifest\":{\"version\":1},\"signature\":\"\"}\n"...), "no well-formed signature"},
		{"not an export", []byte("hello\n"), "not an export"},
		{"no final newline", nd[:len(nd)-1], "no final newline"},
	}
	for _, c := range cases {
		r := verify(t, c.file, s.PublicKey())
		if r.Status != StatusDamaged || !strings.Contains(strings.Join(r.Problems, "\n"), c.expect) {
			t.Errorf("%s: %s %q, want a problem containing %q", c.name, r.Status, r.Problems, c.expect)
		}
	}
	// The resigning helper itself produces files that verify.
	if r := verify(t, resign(t, NDJSON, body(nd), s, ok), s.PublicKey()); r.Status != StatusVerified {
		t.Fatalf("resigned original: %+v", r)
	}
}

// Someone without the key who edits a row and writes a matching manifest
// signed with their own key is caught by the key ID or the signature.
func TestForgedManifests(t *testing.T) {
	s, forger := testSigner(t, 1), testSigner(t, 9)
	nd, _ := writeExport(t, NDJSON, sampleEntries(4), 4, s)
	edited := bytes.Replace(body(nd), []byte(`"posted":4`), []byte(`"posted":5`), 1)
	ok := func(m *Manifest) { m.FirstSeq, m.LastSeq = 1, 4 }
	own := resign(t, NDJSON, edited, forger, ok)
	if r := verify(t, own, s.PublicKey()); r.Status != StatusDamaged || r.Signature != SignatureInvalid ||
		!strings.Contains(r.Problems[0], "not the trusted key") {
		t.Fatalf("forger's own key: %+v", r)
	}
	claim := resign(t, NDJSON, edited, forger, func(m *Manifest) { ok(m); m.KeyID = s.KeyID() })
	if r := verify(t, claim, s.PublicKey()); r.Status != StatusDamaged || r.Signature != SignatureInvalid ||
		!strings.Contains(r.Problems[0], "does not verify") {
		t.Fatalf("forger claiming the key: %+v", r)
	}
	// Without a trusted key the forgery is only "intact": the exit code
	// tells the user the signature was not checked.
	if r := verify(t, own, nil); r.Status != StatusIntact {
		t.Fatalf("without a key: %+v", r)
	}
}

// Damage is narrowed to the span between two checkpoints.
func TestDamageIsLocalisedByCheckpoints(t *testing.T) {
	withCheckpointEvery(t, 10)
	s := testSigner(t, 1)
	b, m := writeExport(t, NDJSON, sampleEntries(45), 45, s)
	// Change a transfer ID in entry 25, in the span of checkpoint 30.
	i := bytes.Index(b, []byte(`"tr_25"`))
	if i < 0 || uint64(i) < m.Checkpoints[1].Bytes || uint64(i) > m.Checkpoints[2].Bytes {
		t.Fatal("entry 25 is not between checkpoints 20 and 30")
	}
	b[i+4] = 'X'
	r := verify(t, b, s.PublicKey())
	if r.Status != StatusDamaged || r.IntactEntries != 20 || !strings.Contains(strings.Join(r.Problems, "\n"), "within entries 21 to 30") {
		t.Fatalf("%+v", r)
	}
}

func TestWriterErrors(t *testing.T) {
	s := testSigner(t, 1)
	if _, err := NewWriter(io.Discard, "xml", time.Time{}, time.Time{}, 0); err == nil {
		t.Fatal("unknown format accepted")
	}
	if _, err := ParseFormat("NDJSON"); err == nil {
		t.Fatal("format names are case sensitive")
	}
	if NDJSON.ContentType() != "application/x-ndjson" || !strings.HasPrefix(CSV.ContentType(), "text/csv") {
		t.Fatal("content types")
	}
	es := sampleEntries(3)
	x, _ := NewWriter(io.Discard, NDJSON, time.Time{}, time.Time{}, 2)
	if err := x.Write(&es[0]); err != nil {
		t.Fatal(err)
	}
	if err := x.Write(&es[0]); err == nil {
		t.Fatal("a repeated entry was accepted")
	}
	if err := x.Write(&es[2]); err == nil {
		t.Fatal("an entry past the head was accepted")
	}
	if _, err := x.Close(s, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Close(s, t0); err == nil {
		t.Fatal("closed twice")
	}
	if err := x.Write(&es[1]); err == nil {
		t.Fatal("write after close")
	}

	boom := errors.New("disk full")
	if _, err := NewWriter(&failWriter{after: 0, err: boom}, CSV, time.Time{}, time.Time{}, 3); !errors.Is(err, boom) {
		t.Fatalf("header write: %v", err)
	}
	x, _ = NewWriter(&failWriter{after: 1, err: boom}, NDJSON, time.Time{}, time.Time{}, 3)
	if err := x.Write(&es[0]); err != nil {
		t.Fatal(err)
	}
	if err := x.Write(&es[1]); !errors.Is(err, boom) {
		t.Fatalf("entry write: %v", err)
	}
	if err := x.Write(&es[2]); !errors.Is(err, boom) {
		t.Fatalf("the error sticks: %v", err)
	}
	if _, err := x.Close(s, t0); !errors.Is(err, boom) {
		t.Fatalf("close after a failure: %v", err)
	}
	x, _ = NewWriter(&failWriter{after: 1, err: boom}, NDJSON, time.Time{}, time.Time{}, 3)
	x.Write(&es[0])
	if _, err := x.Close(s, t0); !errors.Is(err, boom) {
		t.Fatalf("manifest write: %v", err)
	}
}

type failWriter struct {
	after int
	err   error
}

func (w *failWriter) Write(b []byte) (int, error) {
	if w.after == 0 {
		return 0, w.err
	}
	w.after--
	return len(b), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestVerifyReadErrorsAndLongLines(t *testing.T) {
	if _, err := Verify(errReader{}, nil); err == nil {
		t.Fatal("a read error was not returned")
	}
	long := append([]byte("{"), bytes.Repeat([]byte("x"), MaxLineBytes+1)...)
	if r := verify(t, long, nil); r.Status != StatusDamaged || !strings.Contains(r.Problems[0], "longer than") {
		t.Fatalf("%+v", r)
	}
	// Problems are capped.
	var junk bytes.Buffer
	junk.WriteString("{\"seq\":1}\n")
	for i := 0; i < 100; i++ {
		junk.WriteString("{}\n")
	}
	r := verify(t, junk.Bytes(), nil)
	if len(r.Problems) != maxProblems+1 || !strings.HasPrefix(r.Problems[maxProblems], "and ") {
		t.Fatalf("%d problems, last %q", len(r.Problems), r.Problems[len(r.Problems)-1])
	}
}

func TestKeys(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Fatal("short seed accepted")
	}
	s := testSigner(t, 7)
	for _, enc := range []string{EncodeKey(s.PublicKey()), strings.TrimRight(EncodeKey(s.PublicKey()), "="),
		strings.NewReplacer("+", "-", "/", "_").Replace(EncodeKey(s.PublicKey()))} {
		k, err := ParsePublicKey(enc)
		if err != nil || !k.Equal(s.PublicKey()) {
			t.Fatalf("%s: %v", enc, err)
		}
	}
	for _, bad := range []string{"", "!!!", EncodeKey([]byte("short"))} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
		if _, err := ParseSeed(bad); err == nil {
			t.Fatalf("seed %q accepted", bad)
		}
	}
	if KeyID(s.PublicKey()) != s.KeyID() || len(s.KeyID()) != 16 {
		t.Fatal("key id")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, KeyFile)
	a, created, err := LoadOrCreateKey(path)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v: %v", fi.Mode(), err)
	}
	b, created, err := LoadOrCreateKey(path)
	if err != nil || created || b.KeyID() != a.KeyID() {
		t.Fatalf("reload: %v %v", created, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	os.WriteFile(path, []byte("not a key\n"), 0o600)
	if _, _, err := LoadOrCreateKey(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("damaged key file: %v", err)
	}
	if _, _, err := LoadOrCreateKey(filepath.Join(dir, "missing", KeyFile)); err == nil {
		t.Fatal("a key in a missing directory")
	}
	if _, _, err := LoadOrCreateKey(dir); err == nil {
		t.Fatal("a directory read as a key")
	}
}

// countingSink measures the live heap while an export streams through it.
type countingSink struct {
	n, next   int
	base      uint64
	maxGrowth uint64
}

func (c *countingSink) Write(b []byte) (int, error) {
	c.n += len(b)
	if c.n >= c.next {
		c.next += 16 << 20
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > c.base && ms.HeapAlloc-c.base > c.maxGrowth {
			c.maxGrowth = ms.HeapAlloc - c.base
		}
	}
	return len(b), nil
}

// An export's live memory stays flat however much it writes: entries are
// encoded one at a time and nothing is kept per entry.
func TestExportMemoryIsFlat(t *testing.T) {
	const n = 400_000
	s := testSigner(t, 1)
	for _, format := range []Format{NDJSON, CSV} {
		shapes := sampleEntries(5)
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		sink := &countingSink{base: ms.HeapAlloc}
		x, err := NewWriter(sink, format, time.Time{}, time.Time{}, n)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= n; i++ {
			e := shapes[i%5]
			e.Seq = uint64(i)
			if err := x.Write(&e); err != nil {
				t.Fatal(err)
			}
		}
		m, err := x.Close(s, t0)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d MiB written, live heap grew at most %d KiB", format, m.Bytes>>20, sink.maxGrowth>>10)
		if m.Bytes < 64<<20 {
			t.Fatalf("only %d bytes written", m.Bytes)
		}
		if sink.maxGrowth > 4<<20 {
			t.Fatalf("%s: live heap grew by %d bytes while exporting %d bytes", format, sink.maxGrowth, m.Bytes)
		}
	}
}

func BenchmarkExport(b *testing.B) {
	s := testSigner(b, 1)
	shapes := sampleEntries(5)
	for _, format := range []Format{NDJSON, CSV} {
		b.Run(string(format), func(b *testing.B) {
			var size uint64
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				x, _ := NewWriter(io.Discard, format, time.Time{}, time.Time{}, 1<<62)
				for j := 1; j <= 100_000; j++ {
					e := shapes[j%5]
					e.Seq = uint64(j)
					x.Write(&e)
				}
				m, _ := x.Close(s, t0)
				size = m.Bytes
			}
			b.SetBytes(int64(size))
			b.ReportMetric(float64(b.N)*100_000/b.Elapsed().Seconds(), "entries/s")
		})
	}
}

func BenchmarkVerify(b *testing.B) {
	s := testSigner(b, 1)
	for _, format := range []Format{NDJSON, CSV} {
		b.Run(string(format), func(b *testing.B) {
			shapes := sampleEntries(5)
			entries := make([]ledger.Entry, 100_000)
			for j := range entries {
				entries[j] = shapes[(j+1)%5]
				entries[j].Seq = uint64(j + 1)
			}
			file, _ := writeExport(b, format, entries, 100_000, s)
			b.SetBytes(int64(len(file)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if r, _ := Verify(bytes.NewReader(file), s.PublicKey()); r.Status != StatusVerified {
					b.Fatal(r.Problems)
				}
			}
			b.ReportMetric(float64(b.N)*100_000/b.Elapsed().Seconds(), "entries/s")
		})
	}
}

func FuzzVerify(f *testing.F) {
	s := testSigner(f, 1)
	for _, format := range []Format{NDJSON, CSV} {
		b, _ := writeExport(f, format, sampleEntries(4), 4, s)
		f.Add(b)
		f.Add(b[:len(b)/2])
	}
	f.Add([]byte("seq,\n\"\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := Verify(bytes.NewReader(b), s.PublicKey())
		if err != nil {
			t.Fatal(err)
		}
		damaged := len(r.Problems) > 0 || r.Signature != SignatureValid
		if (r.Status == StatusDamaged) != damaged || r.Status == StatusIntact {
			t.Fatalf("status %s with signature %s and problems %q", r.Status, r.Signature, r.Problems)
		}
		if r.Status == StatusVerified && (r.Manifest == nil || r.Manifest.Bytes != r.Bytes || r.IntactEntries != r.Entries) {
			t.Fatalf("verified %+v", r)
		}
		_ = fmt.Sprint(r)
	})
}
