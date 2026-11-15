package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/webhook"
)

func TestWebhookEndpointsOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":5},{"account":"alice","direction":"credit","amount":5}]}`)

	// A generated secret, shown on creation and when read by ID.
	h := f.must(201, "POST", "/v1/webhooks", `{"id":"orders","url":"https://hooks.example.test/ledger","account":"alice","kinds":["posted","pending"],"currency":"USD","max_batch":50}`)
	secret, _ := h["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") || h["delivered_through_seq"] != float64(1) || h["head_seq"] != float64(1) || num(h["version"]) != 1 ||
		num(h["max_batch"]) != 50 || h["account"] != "alice" || len(h["kinds"].([]any)) != 2 || len(h["dead_letters"].([]any)) != 0 {
		t.Fatalf("created %v", h)
	}
	if _, ok := h["NextDeadLetter"]; ok {
		t.Fatal("internal field in the response")
	}
	got := f.must(200, "GET", "/v1/webhooks/orders", "")
	if got["secret"] != secret || got["url"] != "https://hooks.example.test/ledger" {
		t.Fatalf("read %v", got)
	}
	// A secret of the client's own, delivery from the start of the journal.
	f.must(201, "POST", "/v1/webhooks", `{"id":"all","url":"http://127.0.0.1:1/x","secret":"my-own-secret-0123456789abcdef","after_seq":0}`)
	list := f.must(200, "GET", "/v1/webhooks", "")
	hooks := list["webhooks"].([]any)
	if len(hooks) != 2 || hooks[0].(map[string]any)["id"] != "all" || list["head_seq"] != float64(1) {
		t.Fatalf("listing %v", list)
	}
	for _, x := range hooks {
		m := x.(map[string]any)
		if _, ok := m["secret"]; ok {
			t.Fatal("a listing shows secrets")
		}
		if _, ok := m["dead_letter_count"]; !ok {
			t.Fatal("no dead_letter_count")
		}
	}
	if s := f.must(200, "GET", "/v1/webhooks/all", "")["secret"]; s != "my-own-secret-0123456789abcdef" {
		t.Fatalf("own secret %v", s)
	}

	// Replays.
	r := f.must(200, "POST", "/v1/webhooks/orders/replay", `{"after_seq":0}`)
	if r["delivered_through_seq"] != float64(0) || num(r["version"]) <= num(h["version"]) {
		t.Fatalf("replayed %v", r)
	}
	f.must(200, "POST", "/v1/webhooks/orders/replay", `{"all_dead_letters":true}`)
	for _, c := range []struct{ body, code string }{
		{`{"after_seq":5}`, "invalid_argument"},
		{`{}`, "invalid_argument"},
		{`{"after_seq":0,"all_dead_letters":true}`, "invalid_argument"},
		{`{"dead_letters":[7]}`, "not_found"},
		{`{"bogus":1}`, "invalid_argument"},
	} {
		if res := f.call("POST", "/v1/webhooks/orders/replay", c.body); res.code() != c.code {
			t.Errorf("replay %s: %d %s", c.body, res.status, res.body)
		}
	}
	if res := f.call("POST", "/v1/webhooks/nope/replay", `{"after_seq":0}`); res.status != 404 {
		t.Errorf("replay of a missing endpoint: %d", res.status)
	}

	// Errors on create.
	for _, c := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"id":"orders","url":"https://x.example.test/"}`, 409, "already_exists"},
		{`{"id":"x","url":"ftp://x.example.test/"}`, 400, "invalid_argument"},
		{`{"id":"x","url":"https://x.example.test/","secret":"short"}`, 400, "invalid_argument"},
		{`{"id":"x","url":"https://x.example.test/","kinds":["nope"]}`, 400, "invalid_argument"},
		{`{"id":"x","url":"https://x.example.test/","after_seq":2}`, 400, "invalid_argument"},
		{`{"id":"x","url":"https://x.example.test/","max_batch":1001}`, 400, "invalid_argument"},
		{`{"id":"x","url":"https://x.example.test/","unknown":true}`, 400, "invalid_argument"},
		{`{"id":"x"`, 400, "invalid_argument"},
	} {
		if res := f.call("POST", "/v1/webhooks", c.body); res.status != c.status || res.code() != c.code {
			t.Errorf("create %s: %d %s", c.body, res.status, res.body)
		}
	}
	if res := f.call("GET", "/v1/webhooks?limit=1", ""); res.status != 400 {
		t.Errorf("listing with parameters: %d", res.status)
	}
	if res := f.call("GET", "/v1/webhooks/-bad", ""); res.status != 400 {
		t.Errorf("bad id: %d", res.status)
	}

	// Idempotency keys work as for every other write: the retry gets the
	// first answer, generated secret included.
	first := f.must(201, "POST", "/v1/webhooks", `{"id":"keyed","url":"https://k.example.test/"}`, "Idempotency-Key", "hook-1")
	again := f.call("POST", "/v1/webhooks", `{"id":"keyed","url":"https://k.example.test/"}`, "Idempotency-Key", "hook-1")
	if again.status != 201 || again.header.Get(replayedHeader) != "true" || again.json(t)["secret"] != first["secret"] {
		t.Fatalf("keyed retry: %d %s", again.status, again.body)
	}

	// Delete.
	if d := f.must(200, "DELETE", "/v1/webhooks/orders", ""); d["deleted"] != true || d["id"] != "orders" {
		t.Fatalf("delete %v", d)
	}
	if res := f.call("DELETE", "/v1/webhooks/orders", ""); res.status != 404 {
		t.Fatalf("second delete: %d", res.status)
	}
	if res := f.call("GET", "/v1/webhooks/orders", ""); res.status != 404 {
		t.Fatalf("read after delete: %d", res.status)
	}

	// Auth.
	for _, c := range [][2]string{{"GET", "/v1/webhooks"}, {"GET", "/v1/webhooks/all"}, {"POST", "/v1/webhooks"}, {"DELETE", "/v1/webhooks/all"}, {"POST", "/v1/webhooks/all/replay"}} {
		if res := f.call(c[0], c[1], `{}`, "Authorization", ""); res.status != 401 {
			t.Errorf("%s %s without a key: %d", c[0], c[1], res.status)
		}
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Fatal("a webhook secret reached the logs")
	}
}

// End to end in one process: an endpoint registered over HTTP receives
// signed deliveries of transfers made over HTTP, and a replay requested
// over HTTP sends them again.
func TestWebhookDeliveryOverHTTP(t *testing.T) {
	var mu sync.Mutex
	var got []webhook.Delivery
	var secret string
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if err := webhook.Verify(secret, r.Header.Get(webhook.SignatureHeader), body, time.Now(), 0); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		var d webhook.Delivery
		if err := json.Unmarshal(body, &d); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = append(got, d)
	}))
	defer recv.Close()
	f := newFixture(t, nil)
	d, err := webhook.Start(webhook.Options{Ledger: f.l, Metrics: f.api.m, BackoffBase: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f.accounts()
	mu.Lock()
	secret = f.must(201, "POST", "/v1/webhooks", `{"id":"alice","url":"`+recv.URL+`","account":"alice"}`)["secret"].(string)
	mu.Unlock()
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":5},{"account":"alice","direction":"credit","amount":5}]}`)
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":7},{"account":"merchant","direction":"credit","amount":7}]}`)
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":2},{"account":"merchant","direction":"credit","amount":2}]}`)
	seqs := func() []uint64 {
		mu.Lock()
		defer mu.Unlock()
		var out []uint64
		for _, d := range got {
			for _, e := range d.Entries {
				out = append(out, e.Seq)
			}
		}
		return out
	}
	wait := func(n int) {
		deadline := time.Now().Add(10 * time.Second)
		for len(seqs()) < n {
			if time.Now().After(deadline) {
				t.Fatalf("received %v, want %d entries", seqs(), n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	wait(2)
	if s := seqs(); len(s) != 2 || s[0] != 1 || s[1] != 3 {
		t.Fatalf("received %v", s)
	}
	for num(f.must(200, "GET", "/v1/webhooks/alice", "")["delivered_through_seq"]) != 3 {
		time.Sleep(2 * time.Millisecond)
	}
	f.must(200, "POST", "/v1/webhooks/alice/replay", `{"after_seq":0}`)
	wait(4)
	if s := seqs(); len(s) != 4 || s[2] != 1 || s[3] != 3 {
		t.Fatalf("after replay %v", s)
	}
	if !strings.Contains(f.call("GET", "/metrics", "").String(), "ledgerd_webhook_endpoints 1") {
		t.Fatal("no webhook metrics")
	}
}

func (r resp) String() string { return string(r.body) }
