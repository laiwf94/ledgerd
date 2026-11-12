package api

import (
	"strings"
	"testing"
	"time"
)

// A hotel hold over HTTP: it grows, its timeout moves later, keyed retries
// answer byte for byte, and it posts for the grown amount.
func TestHoldChangesOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/transfers", xfer("bank", "alice", 500, ""))
	h := f.must(201, "POST", "/v1/transfers", xfer("alice", "merchant", 200, `,"pending":true,"timeout_seconds":3600`))
	id := h["id"].(string)

	r := f.call("POST", "/v1/transfers/"+id+"/increase", `{"amount":80}`, "Idempotency-Key", "night-2")
	if r.status != 200 {
		t.Fatalf("increase: %d %s", r.status, r.body)
	}
	up := r.json(t)
	for _, p := range up["postings"].([]any) {
		if p := p.(map[string]any); num(p["amount"]) != 280 || num(p["posted"]) != 0 {
			t.Fatalf("posting %v", p)
		}
	}
	if es := up["entries"].([]any); len(es) != 2 || es[1].(map[string]any)["kind"] != "increase_pending" {
		t.Fatalf("entries %v", up["entries"])
	}
	if b := f.balance("alice"); num(b["available"]) != 220 || num(b["pending"]) != -280 {
		t.Fatalf("alice %v", b)
	}
	again := f.call("POST", "/v1/transfers/"+id+"/increase", `{"amount":80}`, "Idempotency-Key", "night-2")
	if again.status != 200 || again.header.Get(replayedHeader) != "true" || string(again.body) != string(r.body) {
		t.Fatalf("replay %d %s", again.status, again.body)
	}
	if x := f.call("POST", "/v1/transfers/"+id+"/increase", `{"amount":81}`, "Idempotency-Key", "night-2"); x.status != 422 || x.code() != "idempotency_key_reused" {
		t.Fatalf("key reuse: %d %s", x.status, x.body)
	}
	// More than alice has.
	if x := f.call("POST", "/v1/transfers/"+id+"/increase", `{"amount":221}`); x.status != 422 || x.code() != "limit_exceeded" {
		t.Fatalf("over the limit: %d %s", x.status, x.body)
	}
	// Named postings work too.
	f.must(200, "POST", "/v1/transfers/"+id+"/increase", `{"postings":[{"account":"alice","amount":20},{"account":"merchant","amount":20}]}`)

	f.clock.Advance(30 * time.Minute)
	ext := f.call("POST", "/v1/transfers/"+id+"/extend", `{"timeout_seconds":7200}`, "Idempotency-Key", "late-checkout")
	if ext.status != 200 {
		t.Fatalf("extend: %d %s", ext.status, ext.body)
	}
	got := ext.json(t)
	want := f.clock.Now().Add(2 * time.Hour).Format(time.RFC3339)
	if got["expires_at"] != want {
		t.Fatalf("expires_at %v, want %s", got["expires_at"], want)
	}
	if es := got["entries"].([]any); len(es) != 4 || es[3].(map[string]any)["kind"] != "extend_pending" {
		t.Fatalf("entries %v", got["entries"])
	}
	if again := f.call("POST", "/v1/transfers/"+id+"/extend", `{"timeout_seconds":7200}`, "Idempotency-Key", "late-checkout"); string(again.body) != string(ext.body) {
		t.Fatalf("extend replay %s", again.body)
	}
	if x := f.call("POST", "/v1/transfers/"+id+"/extend", `{"timeout_seconds":60}`); x.status != 409 || x.code() != "invalid_state" {
		t.Fatalf("shorter timeout: %d %s", x.status, x.body)
	}
	// The old timeout passes and the hold is still open.
	f.clock.Advance(31 * time.Minute)
	if _, err := f.api.l.ExpireDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if tr := f.must(200, "GET", "/v1/transfers/"+id, ""); tr["state"] != "pending" {
		t.Fatalf("state %v", tr["state"])
	}

	done := f.must(200, "POST", "/v1/transfers/"+id+"/post", "")
	if done["state"] != "posted" || num(done["postings"].([]any)[1].(map[string]any)["posted"]) != 300 {
		t.Fatalf("posted %v", done)
	}
	if b := f.balance("merchant"); num(b["posted"]) != 300 {
		t.Fatalf("merchant %v", b)
	}
	// Neither change applies to a posted transfer.
	if x := f.call("POST", "/v1/transfers/"+id+"/increase", `{"amount":1}`); x.status != 409 || x.code() != "invalid_state" {
		t.Fatalf("increase posted: %d %s", x.status, x.body)
	}
	if x := f.call("POST", "/v1/transfers/"+id+"/extend", `{"timeout_seconds":60}`); x.status != 409 || x.code() != "invalid_state" {
		t.Fatalf("extend posted: %d %s", x.status, x.body)
	}
	if inv := f.must(200, "GET", "/v1/admin/invariants", ""); inv["ok"] != true {
		t.Fatalf("invariants %v", inv)
	}
}

func TestHoldChangeRequestsAreValidated(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/transfers", xfer("bank", "alice", 500, ""))
	id := f.must(201, "POST", "/v1/transfers", xfer("alice", "merchant", 10, `,"pending":true`))["id"].(string)
	for _, c := range []struct{ path, body, want string }{
		{"/increase", `{}`, "how much more"},
		{"/increase", `{"amount":-1}`, "positive"},
		{"/increase", `{"amount":1,"postings":[{"account":"alice","amount":1}]}`, "not both"},
		{"/increase", `{"amount":1,"timeout_seconds":5}`, "unknown field"},
		{"/increase", `{"postings":[{"account":"alice","amount":1}]}`, "must equal"},
		{"/extend", `{}`, "timeout_seconds must be between"},
		{"/extend", `{"timeout_seconds":2592001}`, "timeout_seconds must be between"},
		{"/extend", `{"timeout_seconds":60,"amount":1}`, "unknown field"},
	} {
		x := f.call("POST", "/v1/transfers/"+id+c.path, c.body)
		if x.status != 400 || !strings.Contains(string(x.body), c.want) {
			t.Fatalf("%s %s: %d %s", c.path, c.body, x.status, x.body)
		}
	}
	// Both need a body.
	for _, p := range []string{"/increase", "/extend"} {
		if x := f.call("POST", "/v1/transfers/"+id+p, ""); x.status != 415 {
			t.Fatalf("%s without a body: %d %s", p, x.status, x.body)
		}
	}
	if x := f.call("POST", "/v1/transfers/tr_missing/increase", `{"amount":1}`); x.status != 404 {
		t.Fatalf("missing: %d %s", x.status, x.body)
	}
	if x := f.call("POST", "/v1/transfers/tr_missing/extend", `{"timeout_seconds":60}`); x.status != 404 {
		t.Fatalf("missing: %d %s", x.status, x.body)
	}
	if x := f.call("POST", "/v1/transfers/"+id+"/extend", `{"timeout_seconds":60}`, "Authorization", ""); x.status != 401 {
		t.Fatalf("no key: %d %s", x.status, x.body)
	}
}
