package api

import (
	"strings"
	"testing"
)

// A balancing sweep over HTTP: the response carries the amount that moved,
// a keyed retry returns it unchanged, a sweep with nothing to take is 422,
// and a balancing transfer with more than two postings is 400.
func TestBalancingTransferOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	f.must(201, "POST", "/v1/transfers", xfer("bank", "alice", 640, ""))

	sweep := xfer("alice", "merchant", 1000, `,"balance_debit":true,"reference":"sweep-1"`)
	r := f.call("POST", "/v1/transfers", sweep, "Idempotency-Key", "sweep-1")
	if r.status != 201 {
		t.Fatalf("sweep: %d %s", r.status, r.body)
	}
	tr := r.json(t)
	b, _ := tr["balancing"].(map[string]any)
	if b == nil || b["balance_debit"] != true || b["balance_credit"] != nil || num(b["requested_amount"]) != 1000 || num(b["amount"]) != 640 {
		t.Fatalf("balancing %v", tr["balancing"])
	}
	for _, p := range tr["postings"].([]any) {
		if p := p.(map[string]any); num(p["amount"]) != 640 || num(p["posted"]) != 640 {
			t.Fatalf("posting %v", p)
		}
	}
	if got := f.must(200, "GET", "/v1/transfers/"+tr["id"].(string), ""); num(got["balancing"].(map[string]any)["amount"]) != 640 {
		t.Fatalf("read back %v", got)
	}
	if b := f.balance("alice"); num(b["posted"]) != 0 {
		t.Fatalf("alice %v", b)
	}

	// Money arrives; the keyed retry still answers with the original 640.
	f.must(201, "POST", "/v1/transfers", xfer("bank", "alice", 50, ""))
	again := f.call("POST", "/v1/transfers", sweep, "Idempotency-Key", "sweep-1")
	if again.status != 201 || again.header.Get(replayedHeader) != "true" || string(again.body) != string(r.body) {
		t.Fatalf("replay %d %s", again.status, again.body)
	}
	// A new sweep takes the 50, then there is nothing left.
	if got := f.must(201, "POST", "/v1/transfers", sweep); num(got["balancing"].(map[string]any)["amount"]) != 50 {
		t.Fatalf("second sweep %v", got)
	}
	if x := f.call("POST", "/v1/transfers", sweep); x.status != 422 || x.code() != "limit_exceeded" {
		t.Fatalf("empty sweep: %d %s", x.status, x.body)
	}

	three := `{"currency":"USD","balance_credit":true,"postings":[` + leg("bank", "debit", 2) + "," + leg("alice", "credit", 1) + "," + leg("merchant", "credit", 1) + `]}`
	if x := f.call("POST", "/v1/transfers", three); x.status != 400 || !strings.Contains(string(x.body), "two-posting") {
		t.Fatalf("three postings: %d %s", x.status, x.body)
	}

	// In a batch: fund and sweep in one record.
	r = f.call("POST", "/v1/transfers/batch", batchBody(xfer("bank", "alice", 70, ""), sweep))
	if r.status != 201 {
		t.Fatalf("batch: %d %s", r.status, r.body)
	}
	item := r.json(t)["results"].([]any)[1].(map[string]any)["transfer"].(map[string]any)
	if num(item["balancing"].(map[string]any)["amount"]) != 70 {
		t.Fatalf("batch sweep %v", item)
	}
	// A plain transfer has no balancing field.
	if got := f.must(201, "POST", "/v1/transfers", xfer("bank", "alice", 1, "")); got["balancing"] != nil {
		t.Fatalf("plain transfer %v", got)
	}
}
