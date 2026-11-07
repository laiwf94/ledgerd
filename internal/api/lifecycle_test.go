package api

import (
	"strings"
	"testing"
	"time"
)

// Freeze, unfreeze and close over HTTP: each answers with the account, keeps
// its reason, refuses money movement with its own code and replays under an
// Idempotency-Key.
func TestAccountLifecycleOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":500},{"account":"alice","direction":"credit","amount":500}]}`)
	hold := f.must(201, "POST", "/v1/transfers", `{"currency":"USD","pending":true,"postings":[{"account":"alice","direction":"debit","amount":100},{"account":"merchant","direction":"credit","amount":100}]}`)["id"].(string)

	if a := f.must(200, "GET", "/v1/accounts/alice", ""); a["status"] != "open" || a["status_reason"] != nil || a["status_changed_at"] != nil {
		t.Fatalf("new account %v", a)
	}
	f.clock.Advance(time.Minute)
	a := f.must(200, "POST", "/v1/accounts/alice/freeze", `{"reason":"chargeback review"}`, "Idempotency-Key", "fr-1")
	if a["status"] != "frozen" || a["status_reason"] != "chargeback review" || a["status_changed_at"] != "2030-03-01T09:01:00Z" || a["id"] != "alice" {
		t.Fatalf("freeze answer %v", a)
	}
	r := f.call("POST", "/v1/accounts/alice/freeze", `{"reason":"chargeback review"}`, "Idempotency-Key", "fr-1")
	if r.status != 200 || r.header.Get(replayedHeader) != "true" {
		t.Fatalf("replay %d %s", r.status, r.body)
	}
	if r := f.call("POST", "/v1/accounts/alice/freeze", `{"reason":"other"}`, "Idempotency-Key", "fr-1"); r.code() != "idempotency_key_reused" {
		t.Fatalf("reused key %d %s", r.status, r.body)
	}
	// The same key on another operation is another request.
	if r := f.call("POST", "/v1/accounts/alice/close", `{"reason":"chargeback review"}`, "Idempotency-Key", "fr-1"); r.code() != "idempotency_key_reused" {
		t.Fatalf("key across operations %d %s", r.status, r.body)
	}
	for _, c := range []struct{ path, body string }{
		{"/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":1},{"account":"merchant","direction":"credit","amount":1}]}`},
		{"/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":1},{"account":"alice","direction":"credit","amount":1}]}`},
		{"/v1/transfers/" + hold + "/post", ``},
	} {
		if r := f.call("POST", c.path, c.body); r.status != 409 || r.code() != "account_frozen" {
			t.Fatalf("%s while frozen: %d %s", c.path, r.status, r.body)
		}
	}
	if r := f.call("POST", "/v1/accounts/alice/freeze", ``); r.status != 409 || r.code() != "invalid_state" {
		t.Fatalf("freeze twice %d %s", r.status, r.body)
	}
	if r := f.call("POST", "/v1/accounts/alice/close", ``); r.status != 412 || r.code() != "failed_precondition" {
		t.Fatalf("close with a balance %d %s", r.status, r.body)
	}
	f.must(200, "POST", "/v1/transfers/"+hold+"/void", ``)
	if a := f.must(200, "POST", "/v1/accounts/alice/unfreeze", ``); a["status"] != "open" || a["status_reason"] != nil {
		t.Fatalf("unfreeze %v", a)
	}
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":500},{"account":"bank","direction":"credit","amount":500}]}`)
	a = f.must(200, "POST", "/v1/accounts/alice/close", `{"reason":"customer left"}`)
	if a["status"] != "closed" || a["balance"].(map[string]any)["posted"].(float64) != 0 {
		t.Fatalf("close %v", a)
	}
	for _, p := range []string{"freeze", "unfreeze", "close"} {
		if r := f.call("POST", "/v1/accounts/alice/"+p, ``); r.status != 409 || r.code() != "account_closed" {
			t.Fatalf("%s a closed account: %d %s", p, r.status, r.body)
		}
	}
	if r := f.call("POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":1},{"account":"alice","direction":"credit","amount":1}]}`); r.code() != "account_closed" {
		t.Fatalf("transfer to a closed account: %d %s", r.status, r.body)
	}
	// Listings show the status; a point-in-time read shows the old one.
	list := f.must(200, "GET", "/v1/accounts?limit=10", "")["accounts"].([]any)
	for _, x := range list {
		if m := x.(map[string]any); m["id"] == "alice" && m["status"] != "closed" {
			t.Fatalf("listed %v", m)
		}
	}
	if a := f.must(200, "GET", "/v1/accounts/alice?as_of_seq=1", ""); a["status"] != "open" {
		t.Fatalf("as of 1: %v", a)
	}

	// Bad requests.
	for _, c := range []struct{ path, body, ct string }{
		{"/v1/accounts/nope/freeze", ``, ""},
		{"/v1/accounts/a%20b/freeze", ``, ""},
		{"/v1/accounts/merchant/freeze", `{"reason":"` + strings.Repeat("x", 501) + `"}`, ""},
		{"/v1/accounts/merchant/freeze", `{"why":"x"}`, ""},
		{"/v1/accounts/merchant/freeze", `{"reason":"x"}{}`, ""},
		{"/v1/accounts/merchant/freeze", `{"reason":"x"}`, "text/plain"},
	} {
		h := []string{}
		if c.ct != "" {
			h = []string{"Content-Type", c.ct}
		}
		r := f.call("POST", c.path, c.body, h...)
		want := 400
		if strings.Contains(c.path, "nope") {
			want = 404
		}
		if c.ct != "" {
			want = 415
		}
		if r.status != want {
			t.Fatalf("%s %s: %d %s", c.path, c.body, r.status, r.body)
		}
	}
	if r := f.call("GET", "/v1/accounts/merchant/freeze", ``); r.status != 405 && r.status != 404 {
		t.Fatalf("GET freeze: %d", r.status)
	}
	if r := f.call("POST", "/v1/accounts/merchant/freeze", ``, "Authorization", ""); r.status != 401 {
		t.Fatalf("no key: %d", r.status)
	}
}
