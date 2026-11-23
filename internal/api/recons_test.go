package api

import (
	"fmt"
	"reflect"
	"strings"
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

// Tolerances turn fees and rounding into labelled near-matches with the
// difference recorded, in the report and in a saved run.
func TestReconcileWithTolerancesOverHTTP(t *testing.T) {
	f := reconFixture(t)
	csv := []string{"Content-Type", "text/csv"}
	stmt := reconHeader +
		"s1,2030-03-01,10.00,USD,ord-1\n" + // exact
		"s2,2030-03-02,2.48,USD,\n" + // 0.02 less than the 2.50 payment
		"s3,2030-03-01,4.99,USD,ord-3\n" // 0.01 less than the 5.00 payment
	r := f.call("POST", "/v1/accounts/merchant/reconcile", stmt, csv...)
	if sum := r.json(t)["summary"].(map[string]any); r.status != 200 || sum["near_matches"] != float64(0) ||
		sum["amount_mismatches"] != float64(1) || sum["missing_in_ledger"] != float64(1) {
		t.Fatalf("exact: %d %s", r.status, r.body)
	}
	for _, q := range []string{"tolerance=0.05", "tolerance_pct=1", "tolerance=0.01&tolerance_pct=0.81"} {
		r = f.call("POST", "/v1/accounts/merchant/reconcile?"+q, stmt, csv...)
		rep := r.json(t)
		sum := rep["summary"].(map[string]any)
		if r.status != 200 || sum["matched"] != float64(3) || sum["near_matches"] != float64(2) || sum["amount_mismatches"] != float64(0) {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
		got := map[string]string{}
		for _, m := range rep["matched"].([]any) {
			m := m.(map[string]any)
			got[m["external_id"].(string)] = fmt.Sprint(m["matched_by"], " ", m["difference"])
		}
		if want := map[string]string{"s1": "reference <nil>", "s2": "amount_date_near -2", "s3": "reference_near -1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: matches %v", q, got)
		}
	}
	// 0.81% of 2.48 rounds down to 0.02 and of 4.99 to 0.04; 0.80% of 2.48
	// is 0.01, so with a 0.01 floor s2 stays open.
	r = f.call("POST", "/v1/accounts/merchant/reconcile?tolerance=0.01&tolerance_pct=0.80", stmt, csv...)
	if sum := r.json(t)["summary"].(map[string]any); sum["near_matches"] != float64(1) || sum["missing_in_ledger"] != float64(1) {
		t.Fatalf("0.80%%: %s", r.body)
	}

	// A saved run keeps its tolerance and its near-matches.
	r = f.call("POST", "/v1/accounts/merchant/reconcile?save=true&id=fees&tolerance=0.05&tolerance_pct=0.5", stmt, csv...)
	if r.status != 201 {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	run := f.must(200, "GET", "/v1/reconciliations/fees", "")
	if run["tolerance"] != float64(5) || run["tolerance_bp"] != float64(50) || run["summary"].(map[string]any)["near_matches"] != float64(2) {
		t.Fatalf("run %v", run)
	}
	for _, m := range run["matches"].([]any) {
		m := m.(map[string]any)
		line := m["line"].(map[string]any)
		switch line["external_id"] {
		case "s1":
			if m["matched_by"] != "reference" || m["difference"] != nil {
				t.Fatalf("exact match %v", m)
			}
		case "s2":
			if m["matched_by"] != "amount_date_near" || m["difference"] != float64(-2) {
				t.Fatalf("near-match %v", m)
			}
		case "s3":
			if m["matched_by"] != "reference_near" || m["difference"] != float64(-1) {
				t.Fatalf("near-match %v", m)
			}
		}
	}
	if list := f.must(200, "GET", "/v1/reconciliations", "")["reconciliations"].([]any); list[0].(map[string]any)["tolerance"] != float64(5) {
		t.Fatalf("listing %v", list)
	}

	for _, q := range []string{"tolerance=-0.01", "tolerance=0.001", "tolerance=abc", "tolerance=1e3",
		"tolerance_pct=10.01", "tolerance_pct=0.001", "tolerance_pct=-1", "tolerance_pct=%25"} {
		if r := f.call("POST", "/v1/accounts/merchant/reconcile?"+q, stmt, csv...); r.status != 400 || r.code() != "invalid_argument" {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
	}
	if r := f.call("POST", "/v1/accounts/merchant/reconcile?tolerance_pct=10", stmt, csv...); r.status != 200 {
		t.Fatalf("10%%: %d %s", r.status, r.body)
	}
}

// Open breaks age from their dates; the report and the gauges agree.
func TestReconAgingOverHTTP(t *testing.T) {
	f := reconFixture(t)
	csv := []string{"Content-Type", "text/csv"}
	empty := f.must(200, "GET", "/v1/reconciliations/breaks/aging", "")
	if empty["as_of"] != "2030-03-01" || empty["open_breaks"] != float64(0) || len(empty["buckets"].([]any)) != 4 {
		t.Fatalf("empty report %v", empty)
	}
	// s3 (an amount mismatch, off by 0.01) is dated 2030-03-01, s4 (missing
	// in the ledger, 7.00) 2030-03-02.
	if r := f.call("POST", "/v1/accounts/merchant/reconcile?save=true&id=mar&window_days=1", settlement, csv...); r.status != 201 {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	rep := f.must(200, "GET", "/v1/reconciliations/breaks/aging?as_of=2030-03-10&account=merchant", "")
	if rep["as_of"] != "2030-03-10" || rep["account"] != "merchant" || rep["open_breaks"] != float64(2) {
		t.Fatalf("report %v", rep)
	}
	buckets := rep["buckets"].([]any)
	b37, b830, last := buckets[1].(map[string]any), buckets[2].(map[string]any), buckets[3].(map[string]any)
	if b37["bucket"] != "3-7" || b37["min_days"] != float64(3) || b37["max_days"] != float64(7) || b37["open_breaks"] != float64(0) ||
		len(b37["amounts"].([]any)) != 0 {
		t.Fatalf("3-7 %v", b37)
	}
	// 8 and 9 days old.
	if b830["open_breaks"] != float64(2) || b830["amount_mismatches"] != float64(1) || b830["missing_in_ledger"] != float64(1) ||
		b830["oldest"] != "2030-03-01" || fmt.Sprint(b830["amounts"]) != "[map[amount:7.01 currency:USD]]" {
		t.Fatalf("8-30 %v", b830)
	}
	if last["bucket"] != "30+" || last["min_days"] != float64(31) || last["max_days"] != nil {
		t.Fatalf("30+ %v", last)
	}
	// Reopening the reference match adds it, from its date, for 10.00.
	f.must(200, "POST", "/v1/reconciliations/mar/reopen", `{"matches":[1]}`)
	if rep := f.must(200, "GET", "/v1/reconciliations/breaks/aging", ""); rep["open_breaks"] != float64(3) ||
		fmt.Sprint(rep["buckets"].([]any)[0].(map[string]any)["amounts"]) != "[map[amount:17.01 currency:USD]]" {
		t.Fatalf("after reopen %v", rep)
	}
	if got := f.must(200, "GET", "/v1/reconciliations/breaks/aging?account=alice", "")["open_breaks"]; got != float64(0) {
		t.Fatalf("alice %v", got)
	}
	f.clock.Advance(40 * 24 * time.Hour)
	m := f.call("GET", "/metrics", "").String()
	for _, want := range []string{"# TYPE ledgerd_recon_open_breaks gauge", `ledgerd_recon_open_breaks{bucket="0-2"} 0`,
		`ledgerd_recon_open_breaks{bucket="8-30"} 0`, `ledgerd_recon_open_breaks{bucket="30+"} 3`} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q in metrics", want)
		}
	}
	for _, c := range []struct {
		q      string
		status int
	}{
		{"?as_of=2030-02-30", 400},
		{"?as_of=", 400},
		{"?as_of=1969-12-31", 400},
		{"?account=a%20b", 400},
		{"?account=ghost", 404},
		{"?limit=5", 400},
		{"?as_of=2030-03-01&as_of=2030-03-02", 400},
		{"?as_of=" + strings.Repeat("9", 100), 400},
	} {
		if r := f.call("GET", "/v1/reconciliations/breaks/aging"+c.q, ""); r.status != c.status {
			t.Errorf("%s: %d %s", c.q, r.status, r.body)
		}
	}
	// A run may still be called breaks.
	if r := f.call("POST", "/v1/accounts/merchant/reconcile?save=true&id=breaks", settlement, csv...); r.status != 201 {
		t.Fatalf("save breaks: %d %s", r.status, r.body)
	}
	if got := f.must(200, "GET", "/v1/reconciliations/breaks", "")["id"]; got != "breaks" {
		t.Fatalf("run named breaks: %v", got)
	}
}
