package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/export"
	"github.com/laiwf94/ledgerd/internal/ledger"
)

func (f *fixture) exportKey() ([]byte, string) {
	f.t.Helper()
	k := f.must(200, "GET", "/v1/export/key", "")
	if k["algorithm"] != "ed25519" {
		f.t.Fatalf("key %v", k)
	}
	pub, err := export.ParsePublicKey(k["public_key"].(string))
	if err != nil || export.KeyID(pub) != k["key_id"] {
		f.t.Fatalf("key %v: %v", k, err)
	}
	return pub, k["key_id"].(string)
}

func (f *fixture) verifyExport(b []byte) *export.Report {
	f.t.Helper()
	pub, _ := f.exportKey()
	r, err := export.Verify(bytes.NewReader(b), pub)
	if err != nil {
		f.t.Fatal(err)
	}
	if r.Status != export.StatusVerified {
		f.t.Fatalf("export did not verify: %+v\n%s", r, b)
	}
	return r
}

// An export over HTTP holds every entry of the period, verifies against
// the key the server publishes, and equals the journal feed.
func TestExportOverHTTP(t *testing.T) {
	signer, _ := export.NewSigner(bytes.Repeat([]byte{3}, export.SeedSize))
	f := newFixture(t, func(_ *ledger.Options, c *Config) { c.ExportSigner = signer })
	f.accounts()
	day0 := f.clock.Now().Truncate(24 * time.Hour)
	for d := 0; d < 3; d++ {
		for i := 0; i < 4; i++ {
			f.must(201, "POST", "/v1/transfers", xfer("bank", "alice", 10+i, ""))
		}
		f.clock.Advance(24 * time.Hour)
	}
	if _, id := f.exportKey(); id != signer.KeyID() {
		t.Fatalf("the server publishes key %s, not the configured %s", id, signer.KeyID())
	}

	r := f.call("GET", "/v1/export", "")
	if r.status != 200 || r.header.Get("Content-Type") != "application/x-ndjson" || r.header.Get(exportHeadHeader) != "12" ||
		r.header.Get("Content-Disposition") != `attachment; filename="ledgerd-export-12.ndjson"` {
		t.Fatalf("%d %v", r.status, r.header)
	}
	rep := f.verifyExport(r.body)
	if rep.Entries != 12 || rep.Manifest.HeadSeq != 12 || !rep.Manifest.From.IsZero() || !rep.Manifest.To.IsZero() {
		t.Fatalf("%+v", rep.Manifest)
	}
	var page struct{ Entries []json.RawMessage }
	if err := json.Unmarshal(f.call("GET", "/v1/entries?limit=1000", "").body, &page); err != nil {
		t.Fatal(err)
	}
	feed := page.Entries
	lines := bytes.Split(bytes.TrimSuffix(r.body, []byte("\n")), []byte("\n"))
	if len(lines) != 13 {
		t.Fatalf("%d lines", len(lines))
	}
	for i, ln := range lines[:12] {
		if !bytes.Equal(ln, feed[i]) {
			t.Fatalf("line %d differs from the feed:\n%s\n%s", i, ln, feed[i])
		}
	}

	// The second day, as dates and as RFC 3339 times.
	day1, day2 := day0.Add(24*time.Hour), day0.Add(48*time.Hour)
	for _, q := range []string{"from=" + day1.Format("2006-01-02") + "&to=" + day2.Format("2006-01-02"),
		"from=" + day1.Format(time.RFC3339) + "&to=" + strings.Replace(day2.Add(-time.Nanosecond).Format(time.RFC3339Nano), "Z", "%2B00:00", 1)} {
		r := f.call("GET", "/v1/export?format=csv&"+q, "")
		if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "text/csv") {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
		rep := f.verifyExport(r.body)
		m := rep.Manifest
		if m.Entries != 4 || m.Rows != 8 || m.FirstSeq != 5 || m.LastSeq != 8 || !m.From.Equal(day1) || m.HeadSeq != 12 {
			t.Fatalf("%s: %+v", q, m)
		}
		cr := csv.NewReader(bytes.NewReader(r.body))
		cr.Comment = '#' // readers that skip comments skip the manifest
		rows, err := cr.ReadAll()
		if err != nil || len(rows) != 9 || rows[1][0] != "5" || rows[8][0] != "8" {
			t.Fatalf("%s: %v %v", q, rows, err)
		}
	}
	// A period with nothing in it is still a signed export.
	r = f.call("GET", "/v1/export?from=2001-01-01&to=2001-01-02", "")
	if rep := f.verifyExport(r.body); rep.Entries != 0 || rep.Manifest.HeadSeq != 12 {
		t.Fatalf("%+v", rep.Manifest)
	}

	var metrics bytes.Buffer
	f.api.m.Write(&metrics)
	for _, want := range []string{"ledgerd_exports_total 4", "ledgerd_export_entries_total 20", "ledgerd_exports_failed_total 0"} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestExportQueryErrors(t *testing.T) {
	f := newFixture(t, nil)
	for _, q := range []string{
		"format=xml", "format=", "from=yesterday", "to=2030-13-01", "from=2030-03-02&to=2030-03-01",
		"from=2030-03-01&to=2030-03-01", "from=2030-03-01&from=2030-03-02", "limit=10", "after_seq=1", "%zz",
		"from=" + strings.Repeat("9", 70),
	} {
		if r := f.call("GET", "/v1/export?"+q, ""); r.status != 400 || r.code() != "invalid_argument" {
			t.Errorf("%s: %d %s", q, r.status, r.body)
		}
	}
	for _, path := range []string{"/v1/export", "/v1/export/key"} {
		if r := f.call("GET", path, "", "Authorization", ""); r.status != 401 {
			t.Errorf("%s without a key: %d", path, r.status)
		}
	}
}

func TestExportConcurrencyLimit(t *testing.T) {
	f := newFixture(t, nil)
	for i := 0; i < MaxConcurrentExports; i++ {
		f.api.exports <- struct{}{}
	}
	r := f.call("GET", "/v1/export", "")
	if r.status != 429 || r.code() != "resource_exhausted" || r.header.Get("Retry-After") != "1" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	<-f.api.exports
	if r := f.call("GET", "/v1/export", ""); r.status != 200 {
		t.Fatalf("%d once a slot is free", r.status)
	}
}

// Without a configured key the server signs with a random one, and a
// handler behind a writer that has no deadlines still streams.
func TestExportWithoutDeadlinesOrKey(t *testing.T) {
	l, err := ledger.Open(ledger.Options{SweepInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s, err := New(Config{Ledger: l, APIKeys: []string{apiKey}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/export?format=csv", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	r, err := export.Verify(rec.Body, s.signer.PublicKey())
	if err != nil || rec.Code != 200 || r.Status != export.StatusVerified {
		t.Fatalf("%d %+v %v", rec.Code, r, err)
	}
}

// A client that stops reading is disconnected once a write misses its
// deadline, its file has no manifest, and commits never wait for it.
func TestExportSlowClientIsCutOff(t *testing.T) {
	f := newFixture(t, func(o *ledger.Options, c *Config) {
		o.Dir = "" // in memory: the test needs many entries quickly
		c.StreamWriteTimeout = 200 * time.Millisecond
	})
	f.accounts()
	items := make([]string, 4000)
	for i := range items {
		items[i] = xfer("bank", "alice", 1+i%7, "")
	}
	body := batchBody(items...)
	for i := 0; i < 12; i++ {
		f.must(201, "POST", "/v1/transfers/batch", body)
	}

	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
	}}
	conn, err := d.Dial("tcp", strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /v1/export HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", apiKey)
	res, err := http.ReadResponse(bufio.NewReaderSize(conn, 4096), nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("%v %v", res, err)
	}
	// The client reads nothing more. A commit goes through meanwhile.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.l.Do(ctx, ledger.Command{CreateTransfer: &ledger.TransferSpec{Currency: "USD", Postings: []ledger.PostingSpec{
		{Account: "bank", Direction: ledger.Debit, Amount: 1}, {Account: "alice", Direction: ledger.Credit, Amount: 1}}}}, nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a commit took %s during a stalled export", d)
	}
	for f.api.mExportFailed.Value() == 0 {
		if time.Since(start) > 30*time.Second {
			t.Fatal("the stalled export was never cut off")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// What the client got is a file without its manifest.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, _ := io.ReadAll(res.Body)
	if r, err := export.Verify(bytes.NewReader(got), nil); err != nil || r.Status != export.StatusDamaged || r.Manifest != nil {
		t.Fatalf("%d bytes read: %+v %v", len(got), r, err)
	}
	conn.Close()
	f.srv.Close() // waits for handlers, so the log is complete
	if !strings.Contains(f.logs.String(), "export ended early") {
		t.Fatal("the cut-off was not logged")
	}
}
