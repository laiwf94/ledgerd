package api

import (
	"testing"
	"time"

	"github.com/laiwf94/ledgerd/internal/ledger"
	"github.com/laiwf94/ledgerd/internal/recon"
)

const reconHeader = "external_id,date,amount,currency,reference\n"

// reconFixture pays the merchant three times on 2030-03-01: two payments
// with order references and one without.
func reconFixture(t *testing.T) *fixture {
	f := newFixture(t, nil)
	f.accounts()
	pay := func(amount, ref string) {
		f.t.Helper()
		f.must(201, "POST", "/v1/transfers", `{"currency":"USD","reference":"`+ref+`","postings":[{"account":"bank","direction":"debit","amount":`+amount+
			`},{"account":"merchant","direction":"credit","amount":`+amount+`}]}`)
	}
	pay("1000", "ord-1")
	pay("250", "")
	pay("500", "ord-3")
	return f
}

var settlement = reconHeader +
	"s1,2030-03-01,10.00,USD,ord-1\n" + // by reference
	"s2,2030-03-02,2.50,USD,\n" + // by amount and date
	"s3,2030-03-01,4.99,USD,ord-3\n" + // amount mismatch
	"s4,2030-03-02,7.00,USD,\n" // missing in the ledger

func TestSavedReconciliationsOverHTTP(t *testing.T) {
	f := reconFixture(t)
	csv := []string{"Content-Type", "text/csv"}

	// Without save nothing is stored.
	r := f.call("POST", "/v1/accounts/merchant/reconcile", settlement, csv...)
	if r.status != 200 || r.json(t)["reconciliation"] != nil {
		t.Fatalf("plain reconcile: %d %s", r.status, r.body)
	}
	if got := f.must(200, "GET", "/v1/reconciliations", "")["reconciliations"].([]any); len(got) != 0 {
		t.Fatalf("stored without save: %v", got)
	}

	// save=true stores the run under the given id and returns its summary
	// with the report.
	keyed := append([]string{"Idempotency-Key", "stl-1"}, csv...)
	r = f.call("POST", "/v1/accounts/merchant/reconcile?save=true&id=mar-01&window_days=1", settlement, keyed...)
	if r.status != 201 {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	rep := r.json(t)
	run := rep["reconciliation"].(map[string]any)
	sum := run["summary"].(map[string]any)
	if run["id"] != "mar-01" || run["number"] != float64(1) || run["account"] != "merchant" || run["currency"] != "USD" ||
		run["window_days"] != float64(1) || run["from"] != "2030-03-01" || run["to"] != "2030-03-02" || run["matches"] != nil ||
		sum["matches"] != float64(2) || sum["proposed"] != float64(2) || sum["breaks"] != float64(2) ||
		sum["amount_mismatches"] != float64(1) || sum["missing_in_ledger"] != float64(1) || sum["missing_in_statement"] != float64(0) ||
		rep["summary"].(map[string]any)["matched"] != float64(2) {
		t.Fatalf("saved run %s", r.body)
	}
	// A keyed retry gets the same run back, even after the journal moved.
	f.must(201, "POST", "/v1/transfers", `{"currency":"USD","postings":[{"account":"bank","direction":"debit","amount":1},{"account":"alice","direction":"credit","amount":1}]}`)
	again := f.call("POST", "/v1/accounts/merchant/reconcile?save=true&id=mar-01&window_days=1", settlement, keyed...)
	if again.status != 201 || again.header.Get(replayedHeader) != "true" || again.json(t)["reconciliation"].(map[string]any)["number"] != float64(1) {
		t.Fatalf("keyed retry: %d %s", again.status, again.body)
	}
	if r := f.call("POST", "/v1/accounts/merchant/reconcile?save=true&id=mar-01", settlement, csv...); r.status != 409 || r.code() != "already_exists" {
		t.Fatalf("same id: %d %s", r.status, r.body)
	}
	// Without an id the server names the run.
	r = f.call("POST", "/v1/accounts/merchant/reconcile?save=true", settlement, csv...)
	if r.status != 201 {
		t.Fatalf("unnamed save: %d %s", r.status, r.body)
	}
	unnamed := r.json(t)["reconciliation"].(map[string]any)["id"].(string)
	if len(unnamed) != len("rec_")+20 || unnamed[:4] != "rec_" {
		t.Fatalf("generated id %q", unnamed)
	}
	// An empty statement over a period saves the movements as breaks.
	r = f.call("POST", "/v1/accounts/merchant/reconcile?save=true&id=empty&from=2030-03-01&to=2030-03-01", reconHeader, csv...)
	if r.status != 201 || r.json(t)["reconciliation"].(map[string]any)["summary"].(map[string]any)["missing_in_statement"] != float64(3) {
		t.Fatalf("empty statement: %d %s", r.status, r.body)
	}

	for _, c := range []struct {
		path   string
		status int
	}{
		{"/v1/accounts/merchant/reconcile?save=yes", 400},
		{"/v1/accounts/merchant/reconcile?id=x", 400},
		{"/v1/accounts/merchant/reconcile?save=true&id=-x", 400},
		{"/v1/accounts/ghost/reconcile?save=true", 404},
	} {
		if r := f.call("POST", c.path, settlement, csv...); r.status != c.status {
			t.Errorf("%s: %d %s", c.path, r.status, r.body)
		}
	}

	// The detail has every match and break.
	d := f.must(200, "GET", "/v1/reconciliations/mar-01", "")
	matches, breaks := d["matches"].([]any), d["breaks"].([]any)
	if len(matches) != 2 || len(breaks) != 2 || d["as_of_seq"] != float64(3) {
		t.Fatalf("detail %v", d)
	}
	m1 := matches[0].(map[string]any)
	if m1["id"] != float64(1) || m1["matched_by"] != "reference" || m1["status"] != "proposed" || m1["line"].(map[string]any)["external_id"] != "s1" ||
		m1["postings"].([]any)[0].(map[string]any)["amount"] != float64(1000) || m1["status_changed_at"] != nil {
		t.Fatalf("match 1 %v", m1)
	}
	if m2 := matches[1].(map[string]any); m2["matched_by"] != "amount_date" || m2["days_apart"] != float64(1) {
		t.Fatalf("match 2 %v", m2)
	}
	if b := breaks[0].(map[string]any); b["kind"] != "amount_mismatch" || b["line"].(map[string]any)["amount"] != float64(499) ||
		b["posting"].(map[string]any)["amount"] != float64(500) {
		t.Fatalf("break 1 %v", b)
	}
	if b := breaks[1].(map[string]any); b["kind"] != "missing_in_ledger" || b["posting"] != nil {
		t.Fatalf("break 2 %v", b)
	}
	if r := f.call("GET", "/v1/reconciliations/nope", ""); r.status != 404 {
		t.Fatalf("missing run: %d", r.status)
	}

	// Listing pages oldest first and filters by account.
	l := f.must(200, "GET", "/v1/reconciliations?limit=2", "")
	runs := l["reconciliations"].([]any)
	if len(runs) != 2 || runs[0].(map[string]any)["id"] != "mar-01" || l["has_more"] != true || l["next_after"] != float64(2) ||
		runs[0].(map[string]any)["matches"] != nil {
		t.Fatalf("list %v", l)
	}
	l = f.must(200, "GET", "/v1/reconciliations?limit=2&after=2", "")
	if runs := l["reconciliations"].([]any); len(runs) != 1 || runs[0].(map[string]any)["id"] != "empty" || l["has_more"] != false {
		t.Fatalf("list page 2 %v", l)
	}
	if got := f.must(200, "GET", "/v1/reconciliations?account=alice", "")["reconciliations"].([]any); len(got) != 0 {
		t.Fatalf("alice %v", got)
	}
	for _, q := range []string{"?cursor=x", "?after=x", "?account=a%20b", "?limit=0", "?status=open"} {
		if r := f.call("GET", "/v1/reconciliations"+q, ""); r.status != 400 {
			t.Errorf("%s: %d %s", q, r.status, r.body)
		}
	}

	// Confirm and reopen.
	f.clock.Advance(time.Hour)
	c := f.must(200, "POST", "/v1/reconciliations/mar-01/confirm", `{"matches":[1],"note":"bank portal"}`, "Idempotency-Key", "c-1")
	if c["changed"] != float64(1) || c["reconciliation"].(map[string]any)["summary"].(map[string]any)["confirmed"] != float64(1) {
		t.Fatalf("confirm %v", c)
	}
	again = f.call("POST", "/v1/reconciliations/mar-01/confirm", `{"matches":[1],"note":"bank portal"}`, "Idempotency-Key", "c-1")
	if again.status != 200 || again.header.Get(replayedHeader) != "true" {
		t.Fatalf("keyed confirm retry: %d %s", again.status, again.body)
	}
	m1 = f.must(200, "GET", "/v1/reconciliations/mar-01", "")["matches"].([]any)[0].(map[string]any)
	if m1["status"] != "confirmed" || m1["note"] != "bank portal" || m1["status_changed_at"] != "2030-03-01T10:00:00Z" {
		t.Fatalf("confirmed match %v", m1)
	}
	for _, c := range []struct {
		path, body string
		status     int
		code       string
	}{
		{"/v1/reconciliations/mar-01/confirm", `{"matches":[1]}`, 412, "failed_precondition"},
		{"/v1/reconciliations/mar-01/confirm", `{"matches":[9]}`, 404, "not_found"},
		{"/v1/reconciliations/mar-01/confirm", `{}`, 400, "invalid_argument"},
		{"/v1/reconciliations/mar-01/confirm", `{"matches":[0]}`, 400, "invalid_argument"},
		{"/v1/reconciliations/mar-01/confirm", `{"matches":[1],"all":true}`, 400, "invalid_argument"},
		{"/v1/reconciliations/mar-01/confirm", `{"matches":"1"}`, 400, "invalid_argument"},
		{"/v1/reconciliations/ghost/reopen", `{"all":true}`, 404, "not_found"},
	} {
		if r := f.call("POST", c.path, c.body); r.status != c.status || r.code() != c.code {
			t.Errorf("%s %s: %d %s", c.path, c.body, r.status, r.body)
		}
	}
	re := f.must(200, "POST", "/v1/reconciliations/mar-01/reopen", `{"all":true,"note":"statement corrected"}`)
	s := re["reconciliation"].(map[string]any)["summary"].(map[string]any)
	if re["changed"] != float64(2) || s["reopened"] != float64(2) || s["confirmed"] != float64(0) || s["proposed"] != float64(0) {
		t.Fatalf("reopen %v", re)
	}
	if c := f.must(200, "POST", "/v1/reconciliations/mar-01/confirm", `{"all":true}`); c["changed"] != float64(2) {
		t.Fatalf("confirm all %v", c)
	}

	inv := f.must(200, "GET", "/v1/admin/invariants", "")
	if inv["ok"] != true {
		t.Fatalf("invariants %v", inv)
	}
}

// The ledger's limits on a run agree with what a statement may hold.
func TestReconLimitsAgree(t *testing.T) {
	if ledger.MaxReconLines != recon.MaxLines || ledger.MaxReconWindow != recon.MaxWindowDays || ledger.MaxReconDays != MaxReconcileDays ||
		ledger.MaxReference != recon.MaxReference || ledger.MaxReconExternalID != recon.MaxExternalID || ledger.MaxMovements < 1 ||
		ledger.MaxMatchPostings != recon.MaxBatchPostings || string(ledger.ByReferenceBatch) != recon.ByReferenceBatch ||
		string(ledger.ByAmountDateBatch) != recon.ByAmountDateBatch || string(ledger.ByReference) != recon.ByReference ||
		string(ledger.ByAmountDate) != recon.ByAmountDate {
		t.Fatal("reconciliation limits disagree")
	}
}

// A payout settled as one bank line matches the transfers it pays: by the
// reference they share, or by amount within the window, and is saved with
// every movement.
func TestBatchReconciliationOverHTTP(t *testing.T) {
	f := newFixture(t, nil)
	f.accounts()
	pay := func(amount, ref string) {
		f.t.Helper()
		f.must(201, "POST", "/v1/transfers", `{"currency":"USD","reference":"`+ref+`","postings":[{"account":"bank","direction":"debit","amount":`+amount+
			`},{"account":"merchant","direction":"credit","amount":`+amount+`}]}`)
	}
	pay("1000", "po-7")
	pay("2500", "po-7")
	pay("400", "po-7")
	pay("120", "")
	pay("80", "")
	pay("999", "")
	statement := reconHeader +
		"bk-1,2030-03-02,39.00,USD,po-7\n" + // 10.00 + 25.00 + 4.00
		"bk-2,2030-03-02,2.00,USD,\n" // 1.20 + 0.80
	r := f.call("POST", "/v1/accounts/merchant/reconcile?save=true&id=po&window_days=1&from=2030-03-01", statement, "Content-Type", "text/csv")
	if r.status != 201 {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	rep := r.json(t)
	matched := rep["matched"].([]any)
	if len(matched) != 2 || rep["summary"].(map[string]any)["batch_matches"] != float64(2) {
		t.Fatalf("report %s", r.body)
	}
	want := []struct {
		by    string
		parts int
	}{{recon.ByReferenceBatch, 3}, {recon.ByAmountDateBatch, 2}}
	for i, w := range want {
		m := matched[i].(map[string]any)
		if m["matched_by"] != w.by || len(m["postings"].([]any)) != w.parts || m["transfer_id"] != nil || m["seq"] != nil {
			t.Fatalf("match %d: %v", i, m)
		}
	}
	if ms := rep["missing_in_statement"].([]any); len(ms) != 1 || ms[0].(map[string]any)["amount"] != float64(999) {
		t.Fatalf("missing in statement %v", ms)
	}
	run := f.must(200, "GET", "/v1/reconciliations/po", "")
	if run["summary"].(map[string]any)["ledger_postings"] != float64(6) {
		t.Fatalf("run %v", run)
	}
	for i, w := range want {
		m := run["matches"].([]any)[i].(map[string]any)
		if m["matched_by"] != w.by || len(m["postings"].([]any)) != w.parts || m["status"] != "proposed" {
			t.Fatalf("saved match %d: %v", i, m)
		}
	}
	if ok := f.must(200, "GET", "/v1/admin/invariants", "")["ok"]; ok != true {
		t.Fatal("invariants")
	}
}
