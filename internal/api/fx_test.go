package api

import (
	"fmt"
	"strings"
	"testing"
)

func fxLeg(account, dir string, amount int, cur string) string {
	return fmt.Sprintf(`{"account":%q,"direction":%q,"amount":%d,"currency":%q}`, account, dir, amount, cur)
}

// usdToEUR converts usd cents from alice into eurc cents in alice_eur at
// rate.
func usdToEUR(usd, eurc int, rate string) string {
	return `{"currency":"USD","postings":[` + leg("alice", "debit", usd) + "," + leg("fx_usd", "credit", usd) + "," +
		fxLeg("fx_eur", "debit", eurc, "EUR") + "," + fxLeg("alice_eur", "credit", eurc, "EUR") +
		`],"fx":{"rate":"` + rate + `","source":"ecb","quoted_at":"2026-11-12T09:00:00+01:00"}}`
}

// A USD to EUR conversion over HTTP: exchange accounts, the rate in the
// response with its time in UTC, a keyed replay, rate and validation
// refusals, and listing by either currency.
func TestConversionOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	for _, a := range []string{
		`{"id":"alice_eur","currency":"EUR","normal_balance":"credit","non_negative":true}`,
		`{"id":"fx_usd","currency":"USD","normal_balance":"credit","exchange":true}`,
		`{"id":"fx_eur","currency":"EUR","normal_balance":"credit","exchange":true}`,
	} {
		f.must(201, "POST", "/v1/accounts", a)
	}
	if a := f.must(200, "GET", "/v1/accounts/fx_eur", ""); a["exchange"] != true {
		t.Fatalf("exchange account %v", a)
	}
	if a := f.must(200, "GET", "/v1/accounts/alice", ""); a["exchange"] != nil {
		t.Fatalf("plain account %v", a)
	}
	f.must(201, "POST", "/v1/transfers", xfer("bank", "alice", 50000, ""))

	r := f.call("POST", "/v1/transfers", usdToEUR(10000, 9234, "0.9234"), "Idempotency-Key", "conv-1")
	if r.status != 201 {
		t.Fatalf("conversion: %d %s", r.status, r.body)
	}
	tr := r.json(t)
	fx, _ := tr["fx"].(map[string]any)
	if fx == nil || fx["base"] != "USD" || fx["quote"] != "EUR" || fx["rate"] != "0.9234" || fx["source"] != "ecb" || fx["quoted_at"] != "2026-11-12T08:00:00Z" {
		t.Fatalf("fx %v", tr["fx"])
	}
	ps := tr["postings"].([]any)
	if ps[0].(map[string]any)["currency"] != nil || ps[3].(map[string]any)["currency"] != "EUR" {
		t.Fatalf("postings %v", ps)
	}
	if b := f.balance("alice_eur"); num(b["posted"]) != 9234 {
		t.Fatalf("alice_eur %v", b)
	}
	if b := f.balance("alice"); num(b["posted"]) != 40000 {
		t.Fatalf("alice %v", b)
	}
	again := f.call("POST", "/v1/transfers", usdToEUR(10000, 9234, "0.9234"), "Idempotency-Key", "conv-1")
	if again.status != 201 || again.header.Get(replayedHeader) != "true" || string(again.body) != string(r.body) {
		t.Fatalf("replay %d %s", again.status, again.body)
	}
	for _, cur := range []string{"USD", "EUR"} {
		page := f.must(200, "GET", "/v1/transfers?currency="+cur, "")
		found := false
		for _, x := range page["transfers"].([]any) {
			found = found || x.(map[string]any)["id"] == tr["id"]
		}
		if !found {
			t.Fatalf("%s listing %v", cur, page)
		}
	}

	for _, c := range []struct {
		name, body string
		status     int
		msg        string
	}{
		{"off the rate", usdToEUR(10000, 9000, "0.9234"), 400, "converts to 92.3400 EUR, not 90.00 EUR"},
		{"bad rate", usdToEUR(10000, 9234, "1e-1"), 400, "fx.rate must be a positive decimal"},
		{"no rate", `{"currency":"USD","postings":[` + leg("alice", "debit", 1) + "," + leg("fx_usd", "credit", 1) + "," +
			fxLeg("fx_eur", "debit", 1, "EUR") + "," + fxLeg("alice_eur", "credit", 1, "EUR") + `]}`, 400, "needs fx"},
		{"rate is a number", strings.Replace(usdToEUR(10000, 9234, "0.9234"), `"0.9234"`, `0.9234`, 1), 400, ""},
		{"wrong account currency", `{"currency":"USD","postings":[` + leg("alice", "debit", 100) + "," + leg("fx_usd", "credit", 100) + "," +
			fxLeg("fx_eur", "debit", 92, "EUR") + "," + fxLeg("merchant", "credit", 92, "EUR") + `],"fx":{"rate":"0.92","source":"ecb","quoted_at":"2026-11-12T09:00:00Z"}}`,
			422, "currency_mismatch"},
	} {
		x := f.call("POST", "/v1/transfers", c.body)
		if x.status != c.status || !strings.Contains(string(x.body), c.msg) {
			t.Errorf("%s: %d %s", c.name, x.status, x.body)
		}
	}
	// In a batch, one conversion off the rate refuses everything.
	x := f.call("POST", "/v1/transfers/batch", batchBody(usdToEUR(100, 92, "0.9234"), usdToEUR(100, 1, "0.9234")))
	if x.status != 400 || !strings.Contains(string(x.body), "transfers[1]: at 0.9234 EUR per USD") {
		t.Fatalf("batch: %d %s", x.status, x.body)
	}
	if v := f.must(200, "GET", "/v1/admin/invariants", ""); v["ok"] != true {
		t.Fatalf("invariants %v", v)
	}
}
