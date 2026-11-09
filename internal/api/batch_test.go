package api

import (
	"fmt"
	"strings"
	"testing"
)

func leg(account, dir string, amount int) string {
	return fmt.Sprintf(`{"account":%q,"direction":%q,"amount":%d}`, account, dir, amount)
}

func xfer(from, to string, amount int, extra string) string {
	return `{"currency":"USD","postings":[` + leg(from, "debit", amount) + "," + leg(to, "credit", amount) + `]` + extra + `}`
}

func batchBody(items ...string) string { return `{"transfers":[` + strings.Join(items, ",") + `]}` }

// An atomic batch over HTTP: per-item results in order, all or nothing,
// replay with the same Idempotency-Key, and the refusal shape.
func TestBatchOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/accounts", `{"id":"bob","currency":"USD","normal_balance":"credit","non_negative":true}`)

	// A split payout: the platform is funded and pays two sellers in one go.
	body := batchBody(xfer("bank", "merchant", 1000, `,"reference":"payout-7"`),
		xfer("merchant", "alice", 700, `,"metadata":{"split":"seller"}`),
		xfer("merchant", "bob", 250, `,"pending":true,"timeout_seconds":60`))
	r := f.call("POST", "/v1/transfers/batch", body, "Idempotency-Key", "payout-7")
	if r.status != 201 {
		t.Fatalf("batch: %d %s", r.status, r.body)
	}
	results := r.json(t)["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results %v", results)
	}
	for i, x := range results {
		m := x.(map[string]any)
		tr := m["transfer"].(map[string]any)
		if m["status"] != "created" || m["error"] != nil {
			t.Fatalf("item %d: %v", i, m)
		}
		if got := f.must(200, "GET", "/v1/transfers/"+tr["id"].(string), ""); got["state"] != tr["state"] {
			t.Fatalf("item %d reads back as %v", i, got)
		}
	}
	if s := results[2].(map[string]any)["transfer"].(map[string]any)["state"]; s != "pending" {
		t.Fatalf("held item is %v", s)
	}
	if b := f.balance("merchant"); num(b["posted"]) != 300 || num(b["available"]) != 50 {
		t.Fatalf("merchant %v", b)
	}

	// The same key replays the same transfers; another body is refused.
	again := f.call("POST", "/v1/transfers/batch", body, "Idempotency-Key", "payout-7")
	if again.status != 201 || again.header.Get(replayedHeader) != "true" || string(again.body) != string(r.body) {
		t.Fatalf("replay %d %s", again.status, again.body)
	}
	if x := f.call("POST", "/v1/transfers/batch", batchBody(xfer("bank", "merchant", 1, "")), "Idempotency-Key", "payout-7"); x.code() != "idempotency_key_reused" {
		t.Fatalf("reused key: %d %s", x.status, x.body)
	}

	// The third transfer overdraws alice, so none of them happen. The
	// status and error are the failing transfer's.
	head := f.must(200, "GET", "/readyz", "")["head"]
	fail := batchBody(xfer("bank", "alice", 5, ""), xfer("alice", "bob", 5, ""), xfer("alice", "bob", 10_000, ""), xfer("bank", "bob", 1, ""))
	r = f.call("POST", "/v1/transfers/batch", fail, "Idempotency-Key", "fail-1")
	m := r.json(t)
	e := m["error"].(map[string]any)
	if r.status != 422 || e["code"] != "limit_exceeded" || e["item"].(float64) != 2 || !strings.HasPrefix(e["message"].(string), "transfers[2]: ") || e["request_id"] == "" {
		t.Fatalf("refused batch: %d %s", r.status, r.body)
	}
	want := []string{"not_applied", "not_applied", "failed", "not_applied"}
	for i, x := range m["results"].([]any) {
		it := x.(map[string]any)
		if it["status"] != want[i] || it["transfer"] != nil {
			t.Fatalf("item %d: %v", i, it)
		}
		if i == 2 && it["error"].(map[string]any)["code"] != "limit_exceeded" {
			t.Fatalf("failed item %v", it)
		}
		if i != 2 && it["error"] != nil {
			t.Fatalf("item %d has an error: %v", i, it)
		}
	}
	if f.must(200, "GET", "/readyz", "")["head"] != head {
		t.Fatal("the refused batch wrote entries")
	}
	if b := f.balance("alice"); num(b["posted"]) != 700 {
		t.Fatalf("alice %v", b)
	}
	// The refusal replays as it was, even once it would succeed.
	f.must(201, "POST", "/v1/transfers", xfer("bank", "alice", 20_000, ""))
	rr := f.call("POST", "/v1/transfers/batch", fail, "Idempotency-Key", "fail-1")
	if rr.status != 422 || rr.header.Get(replayedHeader) != "true" || rr.json(t)["error"].(map[string]any)["item"].(float64) != 2 {
		t.Fatalf("replayed refusal %d %s", rr.status, rr.body)
	}

	// Validation errors name the transfer when one is at fault.
	r = f.call("POST", "/v1/transfers/batch", batchBody(xfer("bank", "alice", 1, ""), xfer("bank", "alice", 0, "")))
	if r.status != 400 || r.json(t)["error"].(map[string]any)["item"].(float64) != 1 {
		t.Fatalf("bad item: %d %s", r.status, r.body)
	}
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"transfers":[]}`, 400},
		{`{}`, 400},
		{`{"transfers":{}}`, 400},
		{`{"transfers":[` + xfer("bank", "alice", 1, "") + `],"atomic":true}`, 400},
		{`{"transfers":[` + xfer("bank", "alice", 1, `,"color":"red"`) + `]}`, 400},
		{batchBody(strings.Repeat(xfer("bank", "alice", 1, "")+",", 8190) + xfer("bank", "alice", 1, "")), 400},
		{batchBody(xfer("bank", "nobody", 1, "")), 404},
	} {
		if r := f.call("POST", "/v1/transfers/batch", c.body); r.status != c.want {
			t.Fatalf("%.80s: %d %s", c.body, r.status, r.body)
		}
	}
	if r := f.call("POST", "/v1/transfers/batch", batchBody(xfer("bank", "alice", 1, "")), "Content-Type", "text/plain"); r.status != 415 {
		t.Fatalf("content type: %d", r.status)
	}
	if r := f.call("POST", "/v1/transfers/batch", batchBody(xfer("bank", "alice", 1, "")), "Authorization", ""); r.status != 401 {
		t.Fatalf("no key: %d", r.status)
	}
	big := `{"transfers":[` + xfer("bank", "alice", 1, `,"reference":"`+strings.Repeat(" ", MaxBatchBodyBytes)+`"`) + `]}`
	if r := f.call("POST", "/v1/transfers/batch", big); r.status != 413 {
		t.Fatalf("oversized body: %d %.200s", r.status, r.body)
	}
	if r := f.call("GET", "/v1/transfers/batch", ""); r.status != 400 && r.status != 404 {
		t.Fatalf("GET batch: %d", r.status)
	}

	// The largest batch works over HTTP, with a body far over the 1 MiB
	// limit of other endpoints.
	items := make([]string, 8190)
	for i := range items {
		items[i] = xfer("bank", "bob", 1, fmt.Sprintf(`,"reference":"p-%d"`, i))
	}
	r = f.call("POST", "/v1/transfers/batch", batchBody(items...))
	if r.status != 201 || len(r.json(t)["results"].([]any)) != 8190 {
		t.Fatalf("largest batch: %d %.200s", r.status, r.body)
	}
	if v := f.must(200, "GET", "/v1/admin/invariants", "")["ok"]; v != true {
		t.Fatalf("invariants: %v", v)
	}
}
