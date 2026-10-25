package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/metrics"
	"github.com/laiwf94/ledgerd/internal/wal"
)

const (
	apiKey   = "test-api-key-0123456789"
	otherKey = "other-api-key-0123456789"
	adminTok = "admin-token-0123456789"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	t     *testing.T
	srv   *httptest.Server
	api   *Server
	l     *ledger.Ledger
	clock *clock
	logs  *bytes.Buffer
}

func newFixture(t *testing.T, mutate func(*ledger.Options, *Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, clock: &clock{t: time.Date(2030, 3, 1, 9, 0, 0, 0, time.UTC)}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	reg := metrics.New()
	opts := ledger.Options{Dir: t.TempDir(), Clock: f.clock.Now, SweepInterval: -1, Logger: log, Metrics: reg}
	cfg := Config{APIKeys: []string{apiKey, otherKey}, Logger: log, Metrics: reg}
	if mutate != nil {
		mutate(&opts, &cfg)
	}
	l, err := ledger.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	cfg.Ledger = l
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.l, f.api = l, s
	f.srv = httptest.NewServer(s)
	t.Cleanup(f.srv.Close)
	return f
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("response is not JSON: %s", r.body)
	}
	return m
}

func (r resp) code() string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	json.Unmarshal(r.body, &e)
	return e.Error.Code
}

func (f *fixture) call(method, path, body string, headers ...string) resp {
	f.t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

func (f *fixture) must(status int, method, path, body string, headers ...string) map[string]any {
	f.t.Helper()
	r := f.call(method, path, body, headers...)
	if r.status != status {
		f.t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.status, status, r.body)
	}
	return r.json(f.t)
}

func (f *fixture) balance(id string) map[string]any {
	f.t.Helper()
	return f.must(200, "GET", "/v1/accounts/"+id, "")["balance"].(map[string]any)
}

func num(v any) int64 { return int64(v.(float64)) }

func (f *fixture) accounts() {
	f.t.Helper()
	for _, a := range []string{
		`{"id":"bank","currency":"USD","normal_balance":"debit"}`,
		`{"id":"alice","currency":"USD","normal_balance":"credit","non_negative":true}`,
		`{"id":"merchant","currency":"USD","normal_balance":"credit","non_negative":true}`,
	} {
		f.must(201, "POST", "/v1/accounts", a)
	}
}

func TestEndToEndCardPayment(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	got := f.must(201, "POST", "/v1/accounts", `{"id":"fees","currency":"USD","normal_balance":"credit","metadata":{"kind":"revenue"}}`)
	if got["exponent"].(float64) != 2 || got["metadata"].(map[string]any)["kind"] != "revenue" {
		t.Fatalf("account %v", got)
	}

	fund := f.must(201, "POST", "/v1/transfers", `{"currency":"USD","reference":"dep-1","postings":[
		{"account":"bank","direction":"debit","amount":10000},{"account":"alice","direction":"credit","amount":10000}]}`)
	if fund["state"] != "posted" {
		t.Fatalf("fund %v", fund)
	}

	// Authorize 50.00, capture 42.00 with a fee, release the rest.
	auth := f.must(201, "POST", "/v1/transfers", `{"currency":"USD","pending":true,"timeout_seconds":604800,"reference":"ord-1","postings":[
		{"account":"alice","direction":"debit","amount":5000},{"account":"merchant","direction":"credit","amount":4850},
		{"account":"fees","direction":"credit","amount":150}]}`)
	id := auth["id"].(string)
	if b := f.balance("alice"); num(b["available"]) != 5000 || num(b["posted"]) != 10000 || num(b["pending"]) != -5000 {
		t.Fatalf("alice during auth: %v", b)
	}
	capt := f.must(200, "POST", "/v1/transfers/"+id+"/post", `{"postings":[{"account":"alice","amount":4200},
		{"account":"merchant","amount":4074},{"account":"fees","amount":126}]}`)
	if capt["state"] != "posted" {
		t.Fatalf("capture %v", capt)
	}
	if b := f.balance("alice"); num(b["available"]) != 5800 || num(b["pending"]) != 0 {
		t.Fatalf("alice after capture: %v", b)
	}

	// Refund 10.00 of the merchant's part as a linked reversal.
	ref := f.must(201, "POST", "/v1/transfers/"+id+"/reverse", `{"postings":[{"account":"alice","amount":1000},{"account":"merchant","amount":1000}],"reference":"rf-1"}`)
	if ref["reverses"] != id {
		t.Fatalf("refund %v", ref)
	}
	if b := f.balance("merchant"); num(b["posted"]) != 3074 {
		t.Fatalf("merchant %v", b)
	}
	orig := f.must(200, "GET", "/v1/transfers/"+id, "")
	if len(orig["reversals"].([]any)) != 1 || len(orig["entries"].([]any)) != 2 {
		t.Fatalf("original %v", orig)
	}

	// An overdraft is refused and leaves nothing behind.
	r := f.call("POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":999999},{"account":"merchant","direction":"credit","amount":999999}]}`)
	if r.status != 422 || r.code() != "limit_exceeded" {
		t.Fatalf("overdraft: %d %s", r.status, r.body)
	}

	// Statement with running balances, paged.
	st := f.must(200, "GET", "/v1/accounts/alice/statement?limit=2", "")
	lines := st["lines"].([]any)
	if len(lines) != 2 || st["has_more"] != true || num(lines[0].(map[string]any)["balance"].(map[string]any)["posted"]) != 10000 {
		t.Fatalf("statement %v", st)
	}
	st = f.must(200, "GET", fmt.Sprintf("/v1/accounts/alice/statement?after_seq=%d", num(st["next_after_seq"])), "")
	if len(st["lines"].([]any)) != 2 || st["has_more"] != false {
		t.Fatalf("statement page 2 %v", st)
	}
	// Balance as of the authorization.
	asOf := f.must(200, "GET", "/v1/accounts/alice?as_of_seq=2", "")
	if num(asOf["balance"].(map[string]any)["available"]) != 5000 || num(asOf["as_of_seq"]) != 2 {
		t.Fatalf("as of %v", asOf)
	}

	// Reconcile the merchant's settlement file.
	csv := "external_id,date,amount,currency,reference\n" +
		"stl-1,2030-03-01,40.74,USD,ord-1\n" + // the capture, by reference
		"stl-2,2030-03-02,-10.00,USD,BANK-9\n" + // the refund, by amount and date
		"stl-3,2030-03-02,5.00,USD,\n" // unknown to the ledger
	r = f.call("POST", "/v1/accounts/merchant/reconcile?window_days=2", csv, "Content-Type", "text/csv")
	if r.status != 200 {
		t.Fatalf("reconcile: %d %s", r.status, r.body)
	}
	rep := r.json(t)
	sum := rep["summary"].(map[string]any)
	if num(sum["matched"]) != 2 || num(sum["missing_in_ledger"]) != 1 || num(sum["missing_in_statement"]) != 0 || rep["currency"] != "USD" {
		t.Fatalf("report %s", r.body)
	}

	inv := f.must(200, "GET", "/v1/admin/invariants", "")
	if inv["ok"] != true || len(inv["violations"].([]any)) != 0 {
		t.Fatalf("invariants %v", inv)
	}
}

func TestIdempotencyOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	body := `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":100},{"account":"alice","direction":"credit","amount":100}]}`
	first := f.call("POST", "/v1/transfers", body, "Idempotency-Key", "pay-1")
	if first.status != 201 || first.header.Get(replayedHeader) != "" {
		t.Fatalf("first: %d %s", first.status, first.body)
	}
	// Same request, different formatting and field order: replayed, byte for byte.
	reordered := `{ "postings":[{"amount":100,"direction":"debit","account":"bank"},{"account":"alice","direction":"credit","amount":100}],
		"currency":"USD" }`
	again := f.call("POST", "/v1/transfers", reordered, "Idempotency-Key", "pay-1")
	if again.status != 201 || again.header.Get(replayedHeader) != "true" || !bytes.Equal(again.body, first.body) {
		t.Fatalf("retry: %d %s %s", again.status, again.header, again.body)
	}
	// Same key, different request.
	r := f.call("POST", "/v1/transfers", strings.Replace(body, "100", "200", 2), "Idempotency-Key", "pay-1")
	if r.status != 422 || r.code() != "idempotency_key_reused" {
		t.Fatalf("reuse: %d %s", r.status, r.body)
	}
	// The same key on another endpoint is a different request too.
	r = f.call("POST", "/v1/accounts", `{"id":"x","currency":"USD","normal_balance":"debit"}`, "Idempotency-Key", "pay-1")
	if r.status != 422 {
		t.Fatalf("cross-endpoint reuse: %d %s", r.status, r.body)
	}
	// Another API key has its own key space.
	r = f.call("POST", "/v1/transfers", body, "Idempotency-Key", "pay-1", "Authorization", "Bearer "+otherKey)
	if r.status != 201 || r.header.Get(replayedHeader) != "" {
		t.Fatalf("other caller: %d %s", r.status, r.body)
	}
	// Errors replay with their status.
	over := `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":5000},{"account":"merchant","direction":"credit","amount":5000}]}`
	r1 := f.call("POST", "/v1/transfers", over, "Idempotency-Key", "over-1")
	f.must(201, "POST", "/v1/transfers", strings.Replace(body, "100", "9000", 2))
	r2 := f.call("POST", "/v1/transfers", over, "Idempotency-Key", "over-1")
	if r1.status != 422 || r2.status != 422 || r2.header.Get(replayedHeader) != "true" || r1.code() != r2.code() {
		t.Fatalf("error replay: %d %d %s", r1.status, r2.status, r2.body)
	}
	if r := f.call("POST", "/v1/transfers", body, "Idempotency-Key", strings.Repeat("k", 300)); r.status != 400 {
		t.Fatalf("long key: %d", r.status)
	}

	// Concurrent identical requests: one executes, all get its result.
	var wg sync.WaitGroup
	ids := make([]string, 20)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := f.call("POST", "/v1/transfers", body, "Idempotency-Key", "burst")
			if r.status != 201 {
				t.Errorf("burst %d: %d %s", i, r.status, r.body)
				return
			}
			ids[i] = r.json(t)["id"].(string)
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("burst produced different transfers: %v", ids)
		}
	}
	if b := f.balance("alice"); num(b["posted"]) != 100+100+9000+100 { // first, other caller, top-up, burst
		t.Fatalf("alice %v", b)
	}
}

func TestPendingExpiryOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":100},{"account":"alice","direction":"credit","amount":100}]}`)
	h := f.must(201, "POST", "/v1/transfers", `{"currency":"USD","pending":true,"timeout_seconds":60,"postings":[{"account":"alice","direction":"debit","amount":100},{"account":"merchant","direction":"credit","amount":100}]}`)
	f.clock.Advance(time.Minute)
	r := f.call("POST", "/v1/transfers/"+h["id"].(string)+"/post", "")
	if r.status != 409 || r.code() != "transfer_expired" {
		t.Fatalf("post after expiry: %d %s", r.status, r.body)
	}
	if b := f.balance("alice"); num(b["available"]) != 100 {
		t.Fatalf("reservation not released: %v", b)
	}
	v := f.must(201, "POST", "/v1/transfers", `{"currency":"USD","pending":true,"postings":[{"account":"alice","direction":"debit","amount":10},{"account":"merchant","direction":"credit","amount":10}]}`)
	if v["expires_at"] == nil {
		t.Fatal("no default timeout")
	}
	f.must(200, "POST", "/v1/transfers/"+v["id"].(string)+"/void", "")
	if r := f.call("POST", "/v1/transfers/"+v["id"].(string)+"/void", "{}"); r.status != 409 || r.code() != "invalid_state" {
		t.Fatalf("double void: %d %s", r.status, r.body)
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/accounts", `{"id":"euro","currency":"EUR","normal_balance":"credit"}`)
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":9223372036854775807},{"account":"alice","direction":"credit","amount":9223372036854775807}]}`)
	p := f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":10},{"account":"merchant","direction":"credit","amount":10}]}`)
	posted := p["id"].(string)
	t2 := `{"currency":"USD","postings":[{"account":"%s","direction":"debit","amount":%s},{"account":"%s","direction":"credit","amount":%s}]%s}`
	big := strings.Repeat("x", MaxBodyBytes+10)
	cases := []struct {
		method, path, body string
		headers            []string
		status             int
		code               string
	}{
		{"POST", "/v1/accounts", `{"id":"a",`, nil, 400, "invalid_argument"},
		{"POST", "/v1/accounts", `{"id":"a","currency":"USD","normal_balance":"debit","color":"red"}`, nil, 400, "invalid_argument"},
		{"POST", "/v1/accounts", `{"id":"a","currency":"USD","normal_balance":"debit"} {}`, nil, 400, "invalid_argument"},
		{"POST", "/v1/accounts", `{"id":"a","currency":"USD","normal_balance":"debit"}`, []string{"Content-Type", "text/plain"}, 415, "invalid_argument"},
		{"POST", "/v1/accounts", `{"id":"` + big + `"}`, nil, 413, "invalid_argument"},
		{"POST", "/v1/accounts", `{"id":"bank","currency":"USD","normal_balance":"debit"}`, nil, 409, "already_exists"},
		{"POST", "/v1/accounts", `{"id":"b","currency":"BTC","normal_balance":"debit"}`, nil, 400, "invalid_argument"},
		{"POST", "/v1/accounts", ``, nil, 415, "invalid_argument"},
		{"GET", "/v1/accounts/nobody", "", nil, 404, "not_found"},
		{"GET", "/v1/accounts/bad%20id", "", nil, 400, "invalid_argument"},
		{"GET", "/v1/accounts/alice?as_of_seq=999", "", nil, 400, "invalid_argument"},
		{"GET", "/v1/accounts/alice?as_of_seq=-1", "", nil, 400, "invalid_argument"},
		{"GET", "/v1/accounts/alice/statement?limit=0", "", nil, 400, "invalid_argument"},
		{"GET", "/v1/accounts/alice/statement?limit=1001", "", nil, 400, "invalid_argument"},
		{"GET", "/v1/accounts/nobody/statement", "", nil, 404, "not_found"},
		{"GET", "/v1/transfers/tr_none", "", nil, 404, "not_found"},
		{"POST", "/v1/transfers", fmt.Sprintf(t2, "bank", "10", "alice", "9", ""), nil, 400, "invalid_argument"},
		{"POST", "/v1/transfers", fmt.Sprintf(t2, "bank", "1.5", "alice", "1.5", ""), nil, 400, "invalid_argument"},
		{"POST", "/v1/transfers", fmt.Sprintf(t2, "bank", `"10"`, "alice", `"10"`, ""), nil, 400, "invalid_argument"},
		{"POST", "/v1/transfers", fmt.Sprintf(t2, "bank", "99999999999999999999", "alice", "1", ""), nil, 400, "invalid_argument"},
		{"POST", "/v1/transfers", fmt.Sprintf(t2, "alice", "1", "euro", "1", ""), nil, 422, "currency_mismatch"},
		{"POST", "/v1/transfers", fmt.Sprintf(t2, "alice", "1", "ghost", "1", ""), nil, 404, "not_found"},
		{"POST", "/v1/transfers", fmt.Sprintf(t2, "bank", "10", "alice", "10", ""), nil, 422, "overflow"},
		{"POST", "/v1/transfers", fmt.Sprintf(t2, "alice", "10", "merchant", "10", `,"preconditions":[{"account":"alice","version":0}]`), nil, 412, "failed_precondition"},
		{"POST", "/v1/transfers/" + posted + "/post", "", nil, 409, "invalid_state"},
		{"POST", "/v1/transfers/" + posted + "/post", `{"amount":-1}`, nil, 400, "invalid_argument"},
		{"POST", "/v1/transfers/" + posted + "/void", `{"why":"x"}`, nil, 400, "invalid_argument"},
		{"POST", "/v1/transfers/" + posted + "/reverse", `{"amount":11}`, nil, 400, "invalid_argument"},
		{"POST", "/v1/transfers/tr_none/reverse", ``, nil, 404, "not_found"},
		{"POST", "/v1/transfers/-x/void", ``, nil, 400, "invalid_argument"},
		{"POST", "/v1/accounts/alice/reconcile", "external_id,date,amount,currency,reference\n", nil, 415, "invalid_argument"},
		{"POST", "/v1/accounts/alice/reconcile", "nope\n", []string{"Content-Type", "text/csv"}, 400, "invalid_argument"},
		{"POST", "/v1/accounts/alice/reconcile", "external_id,date,amount,currency,reference\n", []string{"Content-Type", "text/csv"}, 400, "invalid_argument"},
		{"POST", "/v1/accounts/alice/reconcile", "external_id,date,amount,currency,reference\na,2030-01-01,1,EUR,\n", []string{"Content-Type", "text/csv"}, 422, "currency_mismatch"},
		{"POST", "/v1/accounts/alice/reconcile?from=2030-02-01&to=2030-01-01", "external_id,date,amount,currency,reference\n", []string{"Content-Type", "text/csv"}, 400, "invalid_argument"},
		{"POST", "/v1/accounts/alice/reconcile?from=2030-01-01&to=2032-01-01", "external_id,date,amount,currency,reference\n", []string{"Content-Type", "text/csv"}, 400, "invalid_argument"},
		{"POST", "/v1/accounts/alice/reconcile?from=yesterday", "x", []string{"Content-Type", "text/csv"}, 400, "invalid_argument"},
		{"POST", "/v1/accounts/alice/reconcile?to=tomorrow", "x", []string{"Content-Type", "text/csv"}, 400, "invalid_argument"},
		{"POST", "/v1/accounts/alice/reconcile?window_days=32", "x", []string{"Content-Type", "text/csv"}, 400, "invalid_argument"},
		{"POST", "/v1/accounts/ghost/reconcile", "x", []string{"Content-Type", "text/csv"}, 404, "not_found"},
		{"POST", "/v1/accounts/alice/reconcile", "external_id,date,amount,currency,reference\n" + strings.Repeat("a", 5<<20), []string{"Content-Type", "text/csv"}, 413, "invalid_argument"},
		{"DELETE", "/v1/transfers/" + posted, "", nil, 404, "not_found"},
		{"GET", "/v2/anything", "", nil, 404, "not_found"},
	}
	for _, c := range cases {
		r := f.call(c.method, c.path, c.body, c.headers...)
		if r.status != c.status || r.code() != c.code {
			t.Errorf("%s %s %.80s: %d %s, want %d %s", c.method, c.path, c.body, r.status, r.body, c.status, c.code)
		}
		if id := r.json(t)["error"].(map[string]any)["request_id"]; id == "" || id != r.header.Get(requestIDHeader) {
			t.Errorf("%s %s: request id %v / %q", c.method, c.path, id, r.header.Get(requestIDHeader))
		}
	}
	// Reconciliations beyond the concurrency limit are turned away, not queued.
	for i := 0; i < MaxConcurrentReconciles; i++ {
		f.api.recon <- struct{}{}
	}
	r := f.call("POST", "/v1/accounts/merchant/reconcile?from=2030-03-01&to=2030-03-01", "external_id,date,amount,currency,reference\n", "Content-Type", "text/csv")
	if r.status != 429 || r.code() != "resource_exhausted" || r.header.Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d %s", r.status, r.body)
	}
	for i := 0; i < MaxConcurrentReconciles; i++ {
		<-f.api.recon
	}
	// An empty statement with a period lists the ledger's unmatched movements.
	r = f.call("POST", "/v1/accounts/merchant/reconcile?from=2030-03-01&to=2030-03-01", "external_id,date,amount,currency,reference\n", "Content-Type", "text/csv")
	if r.status != 200 || num(r.json(t)["summary"].(map[string]any)["missing_in_statement"]) != 1 {
		t.Fatalf("empty statement: %d %s", r.status, r.body)
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t, nil)
	for _, h := range []string{"", "Bearer ", "Bearer wrong-key-0123456789", "Basic " + apiKey, apiKey} {
		r := f.call("GET", "/v1/accounts/x", "", "Authorization", h)
		if r.status != 401 || r.code() != "unauthenticated" || r.header.Get("WWW-Authenticate") == "" {
			t.Errorf("auth %q: %d", h, r.status)
		}
	}
	if r := f.call("GET", "/metrics", "", "Authorization", ""); r.status != 401 {
		t.Errorf("metrics without auth: %d", r.status)
	}
	if r := f.call("GET", "/healthz", "", "Authorization", ""); r.status != 200 {
		t.Errorf("healthz: %d", r.status)
	}
	// Admin on loopback with an API key: allowed (httptest listens on loopback).
	if r := f.call("GET", "/v1/admin/invariants", ""); r.status != 200 {
		t.Errorf("admin on loopback: %d", r.status)
	}
	if r := f.call("GET", "/v1/admin/invariants", "", "Authorization", ""); r.status != 401 {
		t.Errorf("admin without key: %d", r.status)
	}
	// From another address it is refused without an admin token.
	req := httptest.NewRequest("GET", "/v1/admin/invariants", nil)
	req.RemoteAddr = "203.0.113.9:4000"
	req.Header.Set("Authorization", "Bearer "+apiKey)
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("admin from a remote address: %d", w.Code)
	}

	// With an admin token, only the token works.
	g := newFixture(t, func(_ *ledger.Options, c *Config) { c.AdminToken = adminTok })
	if r := g.call("GET", "/v1/admin/invariants", ""); r.status != 401 {
		t.Errorf("API key as admin token: %d", r.status)
	}
	if r := g.call("GET", "/v1/admin/invariants", "", "Authorization", "Bearer "+adminTok); r.status != 200 {
		t.Errorf("admin token: %d %s", r.status, r.body)
	}
	if r := g.call("GET", "/v1/accounts/x", "", "Authorization", "Bearer "+adminTok); r.status != 401 {
		t.Errorf("admin token as API key: %d", r.status)
	}
	if strings.Contains(f.logs.String(), apiKey) || strings.Contains(g.logs.String(), adminTok) {
		t.Fatal("a secret reached the logs")
	}
}

func TestConfigValidation(t *testing.T) {
	l, err := ledger.Open(ledger.Options{SweepInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cases := map[string]Config{
		"no ledger":   {APIKeys: []string{apiKey}},
		"no keys":     {Ledger: l},
		"short key":   {Ledger: l, APIKeys: []string{"short"}},
		"short admin": {Ledger: l, APIKeys: []string{apiKey}, AdminToken: "short"},
		"admin reuse": {Ledger: l, APIKeys: []string{apiKey}, AdminToken: apiKey},
	}
	for name, c := range cases {
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(Config{Ledger: l, APIKeys: []string{apiKey}}); err != nil {
		t.Fatal(err)
	}
}

func TestRequestIDs(t *testing.T) {
	f := newFixture(t, nil)
	if r := f.call("GET", "/healthz", "", "X-Request-Id", "abc-123"); r.header.Get(requestIDHeader) != "abc-123" {
		t.Fatalf("echo: %q", r.header.Get(requestIDHeader))
	}
	if r := f.call("GET", "/healthz", "", "X-Request-Id", "bad id with spaces"); r.header.Get(requestIDHeader) == "bad id with spaces" || len(r.header.Get(requestIDHeader)) != 24 {
		t.Fatalf("unsafe id kept: %q", r.header.Get(requestIDHeader))
	}
	if !strings.Contains(f.logs.String(), `"request_id":"abc-123"`) {
		t.Fatal("request id not logged")
	}
}

func TestMetricsAndHealth(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(200, "GET", "/readyz", "")
	r := f.call("GET", "/metrics", "")
	for _, want := range []string{"ledgerd_http_requests_total", "ledgerd_commands_total", "ledgerd_log_batches_total",
		"ledgerd_journal_head", "ledgerd_pending_transfers", "ledgerd_commit_duration_seconds_bucket"} {
		if !strings.Contains(string(r.body), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

type failSync struct {
	wal.File
	on *atomic.Bool
}

func (f failSync) Sync() error {
	if f.on.Load() {
		return errors.New("input/output error")
	}
	return f.File.Sync()
}

func TestStorageFailure(t *testing.T) {
	var on atomic.Bool
	f := newFixture(t, func(o *ledger.Options, _ *Config) {
		o.WrapFile = func(file wal.File) wal.File { return failSync{file, &on} }
	})
	f.accounts()
	on.Store(true)
	r := f.call("POST", "/v1/accounts", `{"id":"z","currency":"USD","normal_balance":"debit"}`)
	if r.status != 503 || r.code() != "unavailable" {
		t.Fatalf("write during failure: %d %s", r.status, r.body)
	}
	if r := f.call("GET", "/readyz", ""); r.status != 503 {
		t.Fatalf("readyz: %d", r.status)
	}
	if r := f.call("GET", "/v1/accounts/alice", ""); r.status != 200 {
		t.Fatalf("reads must keep working: %d", r.status)
	}
	if !strings.Contains(f.logs.String(), "input/output error") {
		t.Fatal("cause not logged")
	}
}

func TestFailMapping(t *testing.T) {
	f := newFixture(t, nil)
	for code, status := range map[ledger.Code]int{
		ledger.CodeInvalid: 400, ledger.CodeNotFound: 404, ledger.CodeAlreadyExists: 409, ledger.CodeInvalidState: 409,
		ledger.CodeExpired: 409, ledger.CodeInProgress: 409, ledger.CodePrecondition: 412, ledger.CodeCurrencyMismatch: 422,
		ledger.CodeLimitExceeded: 422, ledger.CodeOverflow: 422, ledger.CodeKeyReused: 422, ledger.CodeUnavailable: 503, "mystery": 500,
	} {
		if got := statusOf(code); got != status {
			t.Errorf("%s -> %d, want %d", code, got, status)
		}
	}
	for err, want := range map[error]int{
		context.DeadlineExceeded:                     504,
		context.Canceled:                             499,
		errors.New("secret detail"):                  500,
		&ledger.Error{Code: "mystery", Message: "m"}: 500,
	} {
		w := httptest.NewRecorder()
		f.api.fail(w, httptest.NewRequest("GET", "/", nil), err)
		if w.Code != want || strings.Contains(w.Body.String(), "secret detail") {
			t.Errorf("%v -> %d %s", err, w.Code, w.Body.String())
		}
	}
}

func TestPanicsBecome500(t *testing.T) {
	f := newFixture(t, nil)
	f.api.route("GET /boom", func(http.ResponseWriter, *http.Request) error { panic("kaboom") }, authNone)
	r := f.call("GET", "/boom", "")
	if r.status != 500 || r.code() != "internal" || strings.Contains(string(r.body), "kaboom") {
		t.Fatalf("panic: %d %s", r.status, r.body)
	}
	if !strings.Contains(f.logs.String(), "kaboom") {
		t.Fatal("panic not logged")
	}
}

func TestRequestTimeout(t *testing.T) {
	gate := make(chan struct{})
	var armed atomic.Bool
	f := newFixture(t, func(o *ledger.Options, c *Config) {
		c.Timeout = 50 * time.Millisecond
		o.WrapFile = func(file wal.File) wal.File { return gatedSync{file, gate, &armed} }
	})
	f.accounts()
	armed.Store(true)
	r := f.call("POST", "/v1/accounts", `{"id":"slow","currency":"USD","normal_balance":"debit"}`, "Idempotency-Key", "slow-1")
	if r.status != 504 || r.code() != "deadline_exceeded" {
		t.Fatalf("slow write: %d %s", r.status, r.body)
	}
	// While the first is still in flight, a retry reports "in progress".
	r = f.call("POST", "/v1/accounts", `{"id":"slow","currency":"USD","normal_balance":"debit"}`, "Idempotency-Key", "slow-1")
	if r.status != 409 || r.code() != "idempotency_in_progress" {
		t.Fatalf("retry in flight: %d %s", r.status, r.body)
	}
	armed.Store(false)
	close(gate)
	// Once it lands, the retry returns the original result.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r = f.call("POST", "/v1/accounts", `{"id":"slow","currency":"USD","normal_balance":"debit"}`, "Idempotency-Key", "slow-1")
		if r.status == 201 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.status != 201 || r.header.Get(replayedHeader) != "true" {
		t.Fatalf("retry after landing: %d %s", r.status, r.body)
	}
}

type gatedSync struct {
	wal.File
	gate  chan struct{}
	armed *atomic.Bool
}

func (g gatedSync) Sync() error {
	if g.armed.Load() {
		<-g.gate
	}
	return g.File.Sync()
}

func FuzzCreateTransfer(f *testing.F) {
	f.Add(`{"currency":"USD","postings":[{"account":"a","direction":"debit","amount":1},{"account":"b","direction":"credit","amount":1}]}`)
	f.Add(`{"currency":"USD","pending":true,"timeout_seconds":5,"postings":[]}`)
	f.Add(`{"postings":[{"amount":1e400}]}`)
	f.Add(`[]`)
	l, err := ledger.Open(ledger.Options{SweepInterval: -1})
	if err != nil {
		f.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	l.Do(ctx, ledger.Command{CreateAccount: &ledger.AccountSpec{ID: "a", Currency: "USD", NormalBalance: ledger.Debit}}, nil)
	l.Do(ctx, ledger.Command{CreateAccount: &ledger.AccountSpec{ID: "b", Currency: "USD", NormalBalance: ledger.Credit, NonNegative: true}}, nil)
	s, err := New(Config{Ledger: l, APIKeys: []string{apiKey}})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, body string) {
		for _, path := range []string{"/v1/transfers", "/v1/accounts", "/v1/transfers/tr_x/reverse"} {
			req := httptest.NewRequest("POST", path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if w.Code >= 500 {
				t.Fatalf("%s %q: %d %s", path, body, w.Code, w.Body.String())
			}
			if !json.Valid(w.Body.Bytes()) {
				t.Fatalf("%s %q: response is not JSON", path, body)
			}
		}
		if v := l.CheckInvariants(); len(v) > 0 {
			t.Fatalf("invariants broken after %q: %v", body, v)
		}
	})
}
