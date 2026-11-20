package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/webhook"
)

const secret = "whsec_example_0123456789abcdef"

var t0 = time.Unix(1_763_190_000, 0)

// The example verifies with the standard library alone; it must accept
// exactly what the server's signer produces and refuse what the server's
// own verifier refuses.
func TestVerifyMatchesServer(t *testing.T) {
	body := []byte(`{"id":"orders.1.2","endpoint":"orders","entries":[{"seq":1},{"seq":2}]}`)
	good := webhook.Sign(secret, t0, body)
	other := webhook.Sign("whsec_another_secret_0123456789", t0, body)
	_, mac, _ := strings.Cut(good, ",v1=")
	_, otherMAC, _ := strings.Cut(other, ",v1=")
	cases := []struct {
		name   string
		header string
		body   []byte
		now    time.Time
		want   error
	}{
		{"valid", good, body, t0, nil},
		{"valid within tolerance", good, body, t0.Add(4 * time.Minute), nil},
		{"clock behind the sender", good, body, t0.Add(-4 * time.Minute), nil},
		{"rotation: second v1 matches", other + ",v1=" + mac, body, t0, nil},
		{"tampered body", good, bytes.Replace(body, []byte(`"seq":2`), []byte(`"seq":3`), 1), t0, errBadSignature},
		{"other secret", other, body, t0, errBadSignature},
		{"timestamp changed", "t=1763190001,v1=" + mac, body, t0, errBadSignature},
		{"played back later", good, body, t0.Add(6 * time.Minute), errStale},
		{"from the future", good, body, t0.Add(-6 * time.Minute), errStale},
		{"empty", "", body, t0, errNoSignature},
		{"no v1", "t=1763190000", body, t0, errNoSignature},
		{"no t", "v1=" + mac, body, t0, errNoSignature},
		{"two t", "t=1,t=1763190000,v1=" + mac, body, t0, errNoSignature},
		{"short v1", "t=1763190000,v1=abcd", body, t0, errNoSignature},
		{"v1 not hex", "t=1763190000,v1=" + strings.Repeat("zz", 32), body, t0, errNoSignature},
		{"no equals", "t=1763190000,v1", body, t0, errNoSignature},
		{"huge t", "t=99999999999999999999,v1=" + mac, body, t0, errNoSignature},
		{"other scheme ignored", "t=1763190000,v0=" + otherMAC + ",v1=" + mac, body, t0, nil},
	}
	for _, c := range cases {
		got := verify([]string{secret}, c.header, c.body, c.now, 5*time.Minute)
		if got != c.want {
			t.Errorf("%s: verify = %v, want %v", c.name, got, c.want)
		}
		server := webhook.Verify(secret, c.header, c.body, c.now, 5*time.Minute)
		if (got == nil) != (server == nil) {
			t.Errorf("%s: the example says %v but webhook.Verify says %v", c.name, got, server)
		}
	}
	// With both secrets configured, a delivery signed with either verifies.
	if err := verify([]string{"whsec_another_secret_0123456789", secret}, good, body, t0, time.Minute); err != nil {
		t.Errorf("rotation: %v", err)
	}
}

type testReceiver struct {
	*receiver
	path string
	srv  *httptest.Server
	logs *bytes.Buffer
	now  time.Time
}

func newTestReceiver(t *testing.T, path string) *testReceiver {
	t.Helper()
	tr := &testReceiver{path: path, logs: &bytes.Buffer{}, now: t0}
	r, err := openReceiver(path, []string{secret}, 5*time.Minute, 1<<20, func() time.Time { return tr.now },
		slog.New(slog.NewJSONHandler(tr.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	tr.receiver = r
	tr.srv = httptest.NewServer(r)
	t.Cleanup(func() { tr.srv.Close(); r.Close() })
	return tr
}

func entry(seq uint64) string {
	return fmt.Sprintf(`{"seq":%d, "kind":"posted", "lines":[{"account":"a","amount":%d}]}`, seq, seq*100)
}

func deliveryBody(endpoint string, replay bool, seqs ...uint64) (id string, body []byte) {
	es := make([]string, len(seqs))
	for i, s := range seqs {
		es[i] = entry(s)
	}
	id = fmt.Sprintf("%s.%d.%d", endpoint, seqs[0], seqs[len(seqs)-1])
	r := ""
	if replay {
		r = `"replay":true,`
	}
	return id, []byte(fmt.Sprintf(`{"id":%q,"endpoint":%q,%s"entries":[%s]}`, id, endpoint, r, strings.Join(es, ",")))
}

// post sends a delivery signed at the receiver's clock and returns the status.
func (tr *testReceiver) post(t *testing.T, id string, body []byte, sign func([]byte) string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", tr.srv.URL, bytes.NewReader(body))
	if sign == nil {
		sign = func(b []byte) string { return webhook.Sign(secret, tr.now, b) }
	}
	req.Header.Set(signatureHeader, sign(body))
	req.Header.Set(idHeader, id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func (tr *testReceiver) deliver(t *testing.T, replay bool, seqs ...uint64) {
	t.Helper()
	id, body := deliveryBody("orders", replay, seqs...)
	if code := tr.post(t, id, body, nil); code != 200 {
		t.Fatalf("delivery %s: %d\n%s", id, code, tr.logs)
	}
}

// fileSeqs reads the file's sequences in order, failing on any bad line.
func fileSeqs(t *testing.T, path string) []uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var seqs []uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e struct {
			Seq   uint64 `json:"seq"`
			Lines []struct {
				Amount uint64 `json:"amount"`
			} `json:"lines"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		if len(e.Lines) != 1 || e.Lines[0].Amount != e.Seq*100 {
			t.Fatalf("line %q is not entry %d as sent", sc.Text(), e.Seq)
		}
		seqs = append(seqs, e.Seq)
	}
	return seqs
}

func TestDuplicatesAndReplaysApplyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entries.ndjson")
	tr := newTestReceiver(t, path)
	tr.deliver(t, false, 1, 2, 3)
	tr.deliver(t, false, 1, 2, 3)    // the answer was lost: sent again
	tr.deliver(t, false, 3, 4, 5)    // overlaps what is held
	tr.deliver(t, false, 9, 10)      // 6 to 8 became a dead letter
	tr.deliver(t, true, 6, 7, 8)     // replayed after later entries
	tr.deliver(t, true, 6, 7, 8)     // replayed twice
	tr.deliver(t, false, 1, 2, 3, 4) // the position was moved back
	tr.deliver(t, false, 11, 11)     // a sequence twice in one delivery
	want := []uint64{1, 2, 3, 4, 5, 9, 10, 6, 7, 8, 11}
	if got := fileSeqs(t, path); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("file holds %v, want %v", got, want)
	}
	if tr.Len() != 11 || tr.Highest() != 11 {
		t.Fatalf("Len %d Highest %d", tr.Len(), tr.Highest())
	}
	if !strings.Contains(tr.logs.String(), `"replay":true,"entries":3,"applied":0,"duplicates":3`) {
		t.Fatalf("the second replay was not logged as all duplicates:\n%s", tr.logs)
	}

	// After a restart the file is read back and nothing is applied twice.
	tr.srv.Close()
	tr.Close()
	tr2 := newTestReceiver(t, path)
	if tr2.Len() != 11 || tr2.Highest() != 11 || tr2.truncated != 0 {
		t.Fatalf("reopened: Len %d Highest %d truncated %d", tr2.Len(), tr2.Highest(), tr2.truncated)
	}
	tr2.deliver(t, false, 10, 11, 12)
	if got := fileSeqs(t, path); fmt.Sprint(got) != fmt.Sprint(append(want, 12)) {
		t.Fatalf("after restart the file holds %v", got)
	}
}

// A crash during the append leaves part of a line. That delivery was never
// answered 200, so the server sends it again; the receiver must cut the
// torn line at start and apply the entries when they come back.
func TestTornLastLineIsCutAtStart(t *testing.T) {
	for _, tail := range []string{
		`{"seq":4,"kind":"pos`,          // cut mid-line
		`{"seq":4,"kind":"posted"`,      // cut before the closing brace
		"{\"seq\":4,\"kind\"\x00\x00\n", // garbage that ends in a newline
		`{"kind":"posted"}` + "\n",      // a complete line that is not an entry
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "entries.ndjson")
		var held strings.Builder
		for s := uint64(1); s <= 3; s++ {
			held.WriteString(entry(s))
			held.WriteByte('\n')
		}
		// entry() has spaces; a received entry is compacted, so build the
		// held lines the way the receiver writes them.
		var compact bytes.Buffer
		for _, l := range strings.Split(strings.TrimSpace(held.String()), "\n") {
			json.Compact(&compact, []byte(l))
			compact.WriteByte('\n')
		}
		good := compact.Len()
		os.WriteFile(path, append(compact.Bytes(), tail...), 0o600)
		tr := newTestReceiver(t, path)
		if tr.Len() != 3 || tr.Highest() != 3 || tr.truncated != int64(len(tail)) {
			t.Fatalf("tail %q: Len %d Highest %d truncated %d", tail, tr.Len(), tr.Highest(), tr.truncated)
		}
		if fi, _ := os.Stat(path); fi.Size() != int64(good) {
			t.Fatalf("tail %q: file is %d bytes, want %d", tail, fi.Size(), good)
		}
		tr.deliver(t, false, 3, 4, 5)
		if got := fileSeqs(t, path); fmt.Sprint(got) != "[1 2 3 4 5]" {
			t.Fatalf("tail %q: file holds %v", tail, got)
		}
	}
}

// Damage before the last line is not a torn write; the receiver refuses
// to start rather than guess, and a sequence held twice is refused too.
func TestCorruptFileRefusesToStart(t *testing.T) {
	for name, content := range map[string]string{
		"bad middle line": `{"seq":1}` + "\n" + `not json` + "\n" + `{"seq":3}` + "\n",
		"duplicate":       `{"seq":1}` + "\n" + `{"seq":1}` + "\n",
		"zero seq":        `{"seq":0}` + "\n" + `{"seq":1}` + "\n",
	} {
		path := filepath.Join(t.TempDir(), "entries.ndjson")
		os.WriteFile(path, []byte(content), 0o600)
		if _, err := openReceiver(path, []string{secret}, time.Minute, 1<<20, time.Now, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

func TestRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entries.ndjson")
	tr := newTestReceiver(t, path)
	tr.deliver(t, false, 1)
	id, body := deliveryBody("orders", false, 2)
	stale := func(b []byte) string { return webhook.Sign(secret, t0.Add(-10*time.Minute), b) }
	forged := func(b []byte) string { return webhook.Sign("whsec_not_the_secret_0123456789", t0, b) }
	unsigned := func([]byte) string { return "" }
	tampered := func([]byte) string { return webhook.Sign(secret, t0, body) }
	for name, c := range map[string]struct {
		id   string
		body []byte
		sign func([]byte) string
		want int
	}{
		"stale":      {id, body, stale, 401},
		"forged":     {id, body, forged, 401},
		"unsigned":   {id, body, unsigned, 401},
		"tampered":   {id, bytes.Replace(body, []byte(`"seq":2`), []byte(`"seq":7`), 1), tampered, 401},
		"id header":  {"orders.9.9", body, nil, 400},
		"not json":   {"x", []byte(`{"id":"x",`), nil, 400},
		"no seq":     {"x", []byte(`{"id":"x","entries":[{"kind":"posted"}]}`), nil, 400},
		"bad entry":  {"x", []byte(`{"id":"x","entries":[7]}`), nil, 400},
		"too big":    {"x", bytes.Repeat([]byte(" "), 2<<20), nil, 413},
		"empty body": {"", nil, nil, 400},
	} {
		if code := tr.post(t, c.id, c.body, c.sign); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
	if got := fileSeqs(t, path); fmt.Sprint(got) != "[1]" {
		t.Fatalf("a refused delivery reached the file: %v", got)
	}
	// Only POST to the path is served.
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", addr, "-secret", secret, "-out", filepath.Join(t.TempDir(), "e.ndjson")}, io.Discard)
	}()
	var resp *http.Response
	for i := 0; ; i++ {
		resp, err = http.Get("http://" + addr + "/hook")
		if err == nil {
			break
		}
		if i == 200 {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /hook: %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestRunNeedsSecret(t *testing.T) {
	t.Setenv("LEDGERD_WEBHOOK_SECRET", "")
	if err := run(context.Background(), []string{"-out", filepath.Join(t.TempDir(), "e")}, io.Discard); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("run without a secret: %v", err)
	}
}
