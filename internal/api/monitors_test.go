package api

import (
	"testing"
)

func TestBalanceMonitorsOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	pay := func(amount string) {
		f.t.Helper()
		f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":`+amount+
			`},{"account":"alice","direction":"credit","amount":`+amount+`}]}`)
	}
	spend := func(amount string) {
		f.t.Helper()
		f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"alice","direction":"debit","amount":`+amount+
			`},{"account":"bank","direction":"credit","amount":`+amount+`}]}`)
	}
	m := f.must(201, "POST", "/v1/balance-monitors", `{"id":"low","account":"alice","condition":{"field":"available","operator":"less_than","value":100},"description":"top up"}`,
		"Idempotency-Key", "mon-1")
	if m["condition_met"] != true || m["from_seq"] != float64(1) || m["description"] != "top up" || m["last_alert_seq"] != float64(0) {
		t.Fatalf("created %v", m)
	}
	again := f.call("POST", "/v1/balance-monitors", `{"id":"low","account":"alice","condition":{"field":"available","operator":"less_than","value":100},"description":"top up"}`,
		"Idempotency-Key", "mon-1")
	if again.status != 201 || again.header.Get(replayedHeader) != "true" {
		t.Fatalf("keyed retry: %d %s", again.status, again.body)
	}
	f.must(201, "POST", "/v1/balance-monitors", `{"id":"rich","account":"alice","condition":{"field":"posted","operator":"greater_than_or_equals","value":1000}}`)
	for _, c := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"id":"low","account":"alice","condition":{"field":"posted","operator":"equals","value":0}}`, 409, "already_exists"},
		{`{"id":"x","account":"nobody","condition":{"field":"posted","operator":"equals","value":0}}`, 404, "not_found"},
		{`{"id":"x","account":"alice","condition":{"field":"settled","operator":"equals","value":0}}`, 400, "invalid_argument"},
		{`{"id":"x","account":"alice","condition":{"field":"posted","operator":"near","value":0}}`, 400, "invalid_argument"},
		{`{"id":"x","account":"alice","condition":{"field":"posted","operator":"equals","value":"1"}}`, 400, "invalid_argument"},
		{`{"id":"x","account":"alice","condition":{"field":"posted","operator":"equals","value":0},"extra":1}`, 400, "invalid_argument"},
	} {
		if r := f.call("POST", "/v1/balance-monitors", c.body); r.status != c.status || r.code() != c.code {
			t.Errorf("%s: %d %s", c.body, r.status, r.body)
		}
	}

	pay("500")   // low clears
	spend("450") // low triggers
	pay("1000")  // low clears, rich triggers
	feed := f.must(200, "GET", "/v1/balance-alerts", "")
	alerts := feed["alerts"].([]any)
	if len(alerts) != 4 || feed["head_seq"] != float64(4) || feed["has_more"] != false || feed["next_after_seq"] != float64(4) {
		t.Fatalf("feed %v", feed)
	}
	first := alerts[0].(map[string]any)
	bal := first["balance"].(map[string]any)
	if first["event"] != "cleared" || first["monitor"] != "low" || first["account"] != "alice" || first["entry_seq"] != float64(1) ||
		first["currency"] != "USD" || bal["available"] != float64(500) || first["transfer_id"] == "" {
		t.Fatalf("first alert %v", first)
	}
	if a := alerts[3].(map[string]any); a["monitor"] != "rich" || a["event"] != "triggered" {
		t.Fatalf("last alert %v", a)
	}
	page := f.must(200, "GET", "/v1/balance-alerts?after_seq=1&limit=1&monitor=low", "")
	if got := page["alerts"].([]any); len(got) != 1 || got[0].(map[string]any)["seq"] != float64(2) || page["has_more"] != true {
		t.Fatalf("page %v", page)
	}
	if got := f.must(200, "GET", "/v1/balance-alerts?account=merchant", "")["alerts"].([]any); len(got) != 0 {
		t.Fatalf("bob %v", got)
	}
	for _, q := range []string{"?after_seq=5", "?after_seq=x", "?limit=0", "?cursor=abc", "?monitor=", "?kind=posted", "?account=a%20b"} {
		if r := f.call("GET", "/v1/balance-alerts"+q, ""); r.status != 400 {
			t.Errorf("%s: %d %s", q, r.status, r.body)
		}
	}

	got := f.must(200, "GET", "/v1/balance-monitors/low", "")
	if got["condition_met"] != false || got["alerts"] != float64(3) || got["last_alert_seq"] != float64(3) {
		t.Fatalf("low %v", got)
	}
	list := f.must(200, "GET", "/v1/balance-monitors", "")
	if ms := list["monitors"].([]any); len(ms) != 2 || ms[0].(map[string]any)["id"] != "low" || list["alert_head_seq"] != float64(4) {
		t.Fatalf("list %v", list)
	}
	if ms := f.must(200, "GET", "/v1/balance-monitors?account=merchant", "")["monitors"].([]any); len(ms) != 0 {
		t.Fatalf("bob %v", ms)
	}
	for _, q := range []string{"?limit=5", "?account=a%20b", "?other=1"} {
		if r := f.call("GET", "/v1/balance-monitors"+q, ""); r.status != 400 {
			t.Errorf("list %s: %d", q, r.status)
		}
	}
	if r := f.call("GET", "/v1/balance-monitors/nope", ""); r.status != 404 {
		t.Fatalf("missing: %d", r.status)
	}
	if d := f.must(200, "DELETE", "/v1/balance-monitors/rich", ""); d["deleted"] != true || d["id"] != "rich" {
		t.Fatalf("delete %v", d)
	}
	if r := f.call("DELETE", "/v1/balance-monitors/rich", ""); r.status != 404 {
		t.Fatalf("second delete: %d", r.status)
	}
	// The alerts of a deleted monitor stay in the feed.
	if got := f.must(200, "GET", "/v1/balance-alerts?monitor=rich", "")["alerts"].([]any); len(got) != 1 {
		t.Fatalf("rich alerts %v", got)
	}
	if inv := f.must(200, "GET", "/v1/admin/invariants", ""); len(inv["violations"].([]any)) != 0 {
		t.Fatalf("invariants %v", inv)
	}
}
