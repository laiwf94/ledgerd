package api

import (
	"strings"
	"testing"
	"time"
)

// Setting, enforcing, removing and listing balance limits over HTTP.
func TestLimitsOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	lim := func(a map[string]any) map[string]any { return a["limits"].(map[string]any) }
	if l := lim(f.must(200, "GET", "/v1/accounts/alice", "")); l["overdraft"].(float64) != 0 || l["credit"] != nil || l["effective_seq"] != nil {
		t.Fatalf("non_negative account starts with %v", l)
	}
	if l := lim(f.must(200, "GET", "/v1/accounts/bank", "")); l["overdraft"] != nil || l["credit"] != nil {
		t.Fatalf("plain account starts with %v", l)
	}
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":100},{"account":"alice","direction":"credit","amount":100}]}`)
	spend := func(n string) resp {
		return f.call("POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":`+n+`},{"account":"merchant","direction":"credit","amount":`+n+`}]}`)
	}
	if r := spend("101"); r.status != 422 || r.code() != "limit_exceeded" {
		t.Fatalf("over the limit: %d %s", r.status, r.body)
	}

	f.clock.Advance(time.Minute)
	a := f.must(200, "POST", "/v1/accounts/alice/limits", `{"kind":"overdraft","amount":50,"reason":"approved"}`, "Idempotency-Key", "lim-1")
	if l := lim(a); l["overdraft"].(float64) != 50 || l["effective_seq"].(float64) != 2 || a["id"] != "alice" || a["version"].(float64) != 1 {
		t.Fatalf("set answer %v", a)
	}
	r := f.call("POST", "/v1/accounts/alice/limits", `{"kind":"overdraft","amount":50,"reason":"approved"}`, "Idempotency-Key", "lim-1")
	if r.status != 200 || r.header.Get(replayedHeader) != "true" {
		t.Fatalf("replay %d %s", r.status, r.body)
	}
	if r := f.call("POST", "/v1/accounts/alice/limits", `{"kind":"overdraft","amount":51}`, "Idempotency-Key", "lim-1"); r.code() != "idempotency_key_reused" {
		t.Fatalf("reused key %d %s", r.status, r.body)
	}
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":150},{"account":"merchant","direction":"credit","amount":150}]}`)
	if r := spend("1"); r.code() != "limit_exceeded" || !strings.Contains(string(r.body), "overdraft limit of 50") {
		t.Fatalf("past the new limit: %d %s", r.status, r.body)
	}
	f.must(200, "POST", "/v1/accounts/merchant/limits", `{"kind":"credit","amount":1000}`)
	if r := spend("1"); r.code() != "limit_exceeded" {
		t.Fatalf("still past: %d %s", r.status, r.body)
	}
	if l := lim(f.must(200, "POST", "/v1/accounts/alice/limits", `{"kind":"overdraft","unlimited":true,"reason":"vip"}`)); l["overdraft"] != nil {
		t.Fatalf("removed %v", l)
	}
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":850},{"account":"merchant","direction":"credit","amount":850}]}`)
	if r := spend("1"); r.code() != "limit_exceeded" || !strings.Contains(string(r.body), "credit limit of 1000") {
		t.Fatalf("merchant credit limit: %d %s", r.status, r.body)
	}

	h := f.must(200, "GET", "/v1/accounts/alice/limits", "")
	changes := h["changes"].([]any)
	if h["account"] != "alice" || lim(h)["overdraft"] != nil || len(changes) != 2 {
		t.Fatalf("history %v", h)
	}
	c0, c1 := changes[0].(map[string]any), changes[1].(map[string]any)
	if c0["kind"] != "overdraft" || c0["amount"].(float64) != 50 || c0["effective_seq"].(float64) != 2 || c0["reason"] != "approved" ||
		c0["time"] != "2030-03-01T09:01:00Z" || c1["amount"] != nil || c1["reason"] != "vip" {
		t.Fatalf("changes %v", changes)
	}
	// Listings and point-in-time reads carry the limits of their time.
	for _, x := range f.must(200, "GET", "/v1/accounts?limit=10", "")["accounts"].([]any) {
		if m := x.(map[string]any); m["id"] == "merchant" && lim(m)["credit"].(float64) != 1000 {
			t.Fatalf("listed %v", m)
		}
	}
	// The change made after entry 1 is in force as of entry 1 and governs
	// entry 2 onwards, like a status change.
	if l := lim(f.must(200, "GET", "/v1/accounts/alice?as_of_seq=1", "")); l["overdraft"].(float64) != 50 || l["effective_seq"].(float64) != 2 {
		t.Fatalf("as of 1: %v", l)
	}
	if l := lim(f.must(200, "GET", "/v1/accounts/merchant?as_of_seq=1", "")); l["credit"] != nil {
		t.Fatalf("merchant as of 1: %v", l)
	}

	// A closed account takes no limit change.
	f.must(201, "POST", "/v1/accounts", `{"id":"gone","currency":"USD","normal_balance":"credit"}`)
	f.must(200, "POST", "/v1/accounts/gone/close", ``)
	if r := f.call("POST", "/v1/accounts/gone/limits", `{"kind":"credit","amount":1}`); r.status != 409 || r.code() != "account_closed" {
		t.Fatalf("closed: %d %s", r.status, r.body)
	}

	// Bad requests.
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/v1/accounts/nope/limits", `{"kind":"credit","amount":1}`, 404},
		{"/v1/accounts/a%20b/limits", `{"kind":"credit","amount":1}`, 400},
		{"/v1/accounts/bank/limits", ``, 415}, // a body is required
		{"/v1/accounts/bank/limits", `{"kind":"credit"}`, 400},
		{"/v1/accounts/bank/limits", `{"kind":"credit","amount":1,"unlimited":true}`, 400},
		{"/v1/accounts/bank/limits", `{"kind":"credit","amount":-1}`, 400},
		{"/v1/accounts/bank/limits", `{"kind":"debit","amount":1}`, 400},
		{"/v1/accounts/bank/limits", `{"kind":"credit","amount":1.5}`, 400},
		{"/v1/accounts/bank/limits", `{"kind":"credit","amount":"1"}`, 400},
		{"/v1/accounts/bank/limits", `{"kind":"credit","amount":1,"limit":2}`, 400},
		{"/v1/accounts/bank/limits", `{"kind":"credit","amount":1,"reason":"` + strings.Repeat("x", 501) + `"}`, 400},
	} {
		if r := f.call("POST", c.path, c.body); r.status != c.want {
			t.Fatalf("%s %s: %d %s", c.path, c.body, r.status, r.body)
		}
	}
	if r := f.call("GET", "/v1/accounts/nope/limits", ""); r.status != 404 {
		t.Fatalf("history of a missing account: %d", r.status)
	}
	if r := f.call("GET", "/v1/accounts/bank/limits", "", "Authorization", ""); r.status != 401 {
		t.Fatalf("no key: %d", r.status)
	}
	if r := f.call("POST", "/v1/accounts/bank/limits", `{"kind":"credit","amount":1}`, "Authorization", ""); r.status != 401 {
		t.Fatalf("no key: %d", r.status)
	}
}
