package api

import (
	"strings"
	"testing"
)

func camtStatement(acct, entries string) string {
	return `<Stmt><Id>st-` + acct + `</Id><Acct><Id><Othr><Id>` + acct + `</Id></Othr></Id><Ccy>USD</Ccy></Acct>` + entries + `</Stmt>`
}

func camtNtry(ref, amt, ccy, ind, sts, e2e string) string {
	s := `<Ntry><Amt Ccy="` + ccy + `">` + amt + `</Amt><CdtDbtInd>` + ind + `</CdtDbtInd><Sts><Cd>` + sts + `</Cd></Sts>` +
		`<BookgDt><DtTm>2030-03-01T18:00:00Z</DtTm></BookgDt><AcctSvcrRef>` + ref + `</AcctSvcrRef>`
	if e2e != "" {
		s += `<NtryDtls><TxDtls><Refs><EndToEndId>` + e2e + `</EndToEndId></Refs></TxDtls></NtryDtls>`
	}
	return s + "</Ntry>\n"
}

func camtDoc(stmts ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"><BkToCstmrStmt><GrpHdr><MsgId>m</MsgId></GrpHdr>` + "\n" +
		strings.Join(stmts, "\n") + "\n</BkToCstmrStmt></Document>\n"
}

// TestReconcileCamt053 reconciles the fixture's three payments from a
// camt.053 file holding the merchant's statement and another account's.
func TestReconcileCamt053(t *testing.T) {
	f := reconFixture(t)
	merchant := camtStatement("1001-77",
		camtNtry("b1", "10.00", "USD", "CRDT", "BOOK", "ord-1")+
			camtNtry("b2", "2.50", "USD", "CRDT", "BOOK", "NOTPROVIDED")+
			camtNtry("b3", "5.00", "USD", "CRDT", "BOOK", "ord-3")+
			camtNtry("b4", "9.99", "USD", "CRDT", "PDNG", ""))
	other := camtStatement("2002-88", camtNtry("c1", "1.00", "USD", "CRDT", "BOOK", ""))
	doc := camtDoc(merchant, other)
	xml := []string{"Content-Type", "application/xml"}

	r := f.call("POST", "/v1/accounts/merchant/reconcile?statement_account=1001-77", doc, xml...)
	if r.status != 200 {
		t.Fatalf("reconcile: %d %s", r.status, r.body)
	}
	rep := r.json(t)
	sum := rep["summary"].(map[string]any)
	if num(sum["matched"]) != 3 || num(sum["missing_in_ledger"]) != 0 || num(sum["missing_in_statement"]) != 0 {
		t.Fatalf("report %s", r.body)
	}
	camt := rep["camt053"].(map[string]any)
	stmts := camt["statements"].([]any)
	if num(camt["skipped_entries"]) != 1 || len(stmts) != 2 ||
		stmts[0].(map[string]any)["selected"] != true || stmts[1].(map[string]any)["selected"] != false {
		t.Fatalf("camt053 %v", camt)
	}

	// A saved run keeps the lines the file gave, rows numbering entries.
	r = f.call("POST", "/v1/accounts/merchant/reconcile?statement_account=1001-77&save=true&id=camt-1", doc, "Content-Type", "text/xml; charset=utf-8")
	if r.status != 201 {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	run := f.must(200, "GET", "/v1/reconciliations/camt-1", "")
	if ms := run["matches"].([]any); len(ms) != 3 {
		t.Fatalf("run %v", run)
	}

	for _, c := range []struct {
		name, path, body string
		hdr              []string
		status           int
		code, msg        string
	}{
		{"two accounts", "/v1/accounts/merchant/reconcile", doc, xml, 400, "invalid_argument", "statements for 2 accounts"},
		{"unknown account", "/v1/accounts/merchant/reconcile?statement_account=9", doc, xml, 400, "invalid_argument", "no statement for account"},
		{"csv with account", "/v1/accounts/merchant/reconcile?statement_account=1001-77", reconHeader, []string{"Content-Type", "text/csv"}, 400, "invalid_argument", "statement_account"},
		{"json", "/v1/accounts/merchant/reconcile", doc, []string{"Content-Type", "application/json"}, 415, "invalid_argument", "application/xml"},
		{"csv as xml", "/v1/accounts/merchant/reconcile", settlement, xml, 400, "invalid_argument", "statement: line 1: text outside the Document"},
		{"currency", "/v1/accounts/merchant/reconcile", camtDoc(camtStatement("1", camtNtry("e1", "1", "USD", "CRDT", "BOOK", "")+camtNtry("e2", "1", "EUR", "CRDT", "BOOK", ""))), xml, 422, "currency_mismatch", "statement entry 2 is in EUR"},
		{"too large", "/v1/accounts/merchant/reconcile", camtDoc(strings.Repeat(" ", 5<<20)), xml, 413, "invalid_argument", ""},
	} {
		r := f.call("POST", c.path, c.body, c.hdr...)
		if r.status != c.status || r.code() != c.code || !strings.Contains(string(r.body), c.msg) {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
	}
}
