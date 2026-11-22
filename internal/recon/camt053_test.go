package recon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readSample(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "camt053", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCamt053Samples(t *testing.T) {
	cases := []struct {
		file    string
		opt     CamtOptions
		lines   []Line
		stmts   []CamtStatement
		skipped int
	}{
		{
			file: "dk-v02.xml",
			lines: []Line{
				{Row: 2, ExternalID: "2030110212345678", Date: "2030-11-02", Amount: 125000, Currency: "EUR", Reference: "INV-2030-0815"},
				{Row: 3, ExternalID: "2030110212345679", Date: "2030-11-02", Amount: -8990, Currency: "EUR", Reference: "hosting 2030-11"},
				{Row: 5, ExternalID: "2030110212345681", Date: "2030-11-02", Amount: -415, Currency: "EUR"},
				{Row: 6, ExternalID: "2030110212345682", Date: "2030-11-02", Amount: -31100, Currency: "EUR", Reference: "RF18539007547034"},
			},
			stmts:   []CamtStatement{{ID: "2030110200001", Account: "DE89370400440532013000", Currency: "EUR", Entries: 5, Booked: 4, Selected: true}},
			skipped: 1,
		},
		{
			file: "sps-v04.xml",
			lines: []Line{
				{Row: 2, ExternalID: "DD0312A0B3C42C7F", Date: "2030-11-02", Amount: 73550, Currency: "CHF", Reference: "PAYOUT-20301102-A"},
				{Row: 3, ExternalID: "DD0312A0B3C42C80", Date: "2030-11-02", Amount: 12995, Currency: "CHF", Reference: "210000000003139471430009025"},
				{Row: 4, ExternalID: "DD0312A0B3C42C81", Date: "2030-11-02", Amount: -150, Currency: "CHF"},
			},
			stmts: []CamtStatement{{ID: "20301102375204000000002", Account: "CH93 0076 2011 6238 5295 7", Currency: "CHF", Entries: 3, Booked: 3, Selected: true}},
		},
		{
			file: "v08-two-accounts.xml",
			opt:  CamtOptions{Account: " 000123456789"},
			lines: []Line{
				{Row: 2, ExternalID: "FW2030110200017", Date: "2030-11-02", Amount: 4825000, Currency: "USD", Reference: "po_7Hq2xK"},
				{Row: 3, ExternalID: "FW2030110200018", Date: "2030-11-02", Amount: -4825000, Currency: "USD", Reference: "po_7Hq2xK"},
				{Row: 5, ExternalID: "ACH2030110200993", Date: "2030-11-02", Amount: -250000, Currency: "USD", Reference: "payroll-2030-11-a"},
				{Row: 6, ExternalID: "ACH2030110200994", Date: "2030-11-02", Amount: -31040, Currency: "USD", Reference: "vendors-2030-11"},
			},
			stmts: []CamtStatement{
				{ID: "USD-OPS-20301102", Account: "000123456789", Currency: "USD", Entries: 5, Booked: 4, Selected: true},
				{ID: "USD-RES-20301102", Account: "000987654321", Currency: "USD", Entries: 1, Booked: 1},
			},
			skipped: 1,
		},
		{
			file: "v08-two-accounts.xml",
			opt:  CamtOptions{Account: "000987654321"},
			lines: []Line{
				{Row: 7, ExternalID: "SWEEP20301102", Date: "2030-11-02", Amount: 10000000, Currency: "USD"},
			},
			stmts: []CamtStatement{
				{ID: "USD-OPS-20301102", Account: "000123456789", Currency: "USD", Entries: 5, Booked: 4},
				{ID: "USD-RES-20301102", Account: "000987654321", Currency: "USD", Entries: 1, Booked: 1, Selected: true},
			},
		},
		{
			// Spaces and case do not matter when choosing an account.
			file: "sps-v04.xml",
			opt:  CamtOptions{Account: "ch9300762011623852957"},
			lines: []Line{
				{Row: 2, ExternalID: "DD0312A0B3C42C7F", Date: "2030-11-02", Amount: 73550, Currency: "CHF", Reference: "PAYOUT-20301102-A"},
				{Row: 3, ExternalID: "DD0312A0B3C42C80", Date: "2030-11-02", Amount: 12995, Currency: "CHF", Reference: "210000000003139471430009025"},
				{Row: 4, ExternalID: "DD0312A0B3C42C81", Date: "2030-11-02", Amount: -150, Currency: "CHF"},
			},
			stmts: []CamtStatement{{ID: "20301102375204000000002", Account: "CH93 0076 2011 6238 5295 7", Currency: "CHF", Entries: 3, Booked: 3, Selected: true}},
		},
	}
	for _, c := range cases {
		t.Run(c.file+"/"+c.opt.Account, func(t *testing.T) {
			res, err := ParseCamt053(bytes.NewReader(readSample(t, c.file)), c.opt)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Lines, c.lines) {
				t.Errorf("lines:\n got %+v\nwant %+v", res.Lines, c.lines)
			}
			if !reflect.DeepEqual(res.Statements, c.stmts) {
				t.Errorf("statements:\n got %+v\nwant %+v", res.Statements, c.stmts)
			}
			if res.Skipped != c.skipped {
				t.Errorf("skipped %d, want %d", res.Skipped, c.skipped)
			}
			// The lines are a statement the CSV parser takes as they are.
			var buf bytes.Buffer
			if err := Format(&buf, res.Lines); err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(&buf); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCamt053Batch reconciles the Swiss batch booking against the three
// payouts it settles: one line, three movements with its reference.
func TestCamt053Batch(t *testing.T) {
	res, err := ParseCamt053(bytes.NewReader(readSample(t, "sps-v04.xml")), CamtOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d := day0("2030-11-02")
	ps := []Posting{
		{Seq: 1, Time: d, Amount: 40000, Reference: "PAYOUT-20301102-A", TransferID: "p1"},
		{Seq: 2, Time: d, Amount: 25000, Reference: "PAYOUT-20301102-A", TransferID: "p2"},
		{Seq: 3, Time: d, Amount: 8550, Reference: "PAYOUT-20301102-A", TransferID: "p3"},
		{Seq: 4, Time: d, Amount: 12995, Reference: "210000000003139471430009025", TransferID: "q1"},
		{Seq: 5, Time: d, Amount: -150, TransferID: "fee"},
	}
	from, to, _ := Period(res.Lines)
	rep := Reconcile(res.Lines, ps, from, to, 1)
	if rep.Summary.Matched != 3 || len(rep.MissingInLedger) != 0 || len(rep.MissingInStatement) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	for _, m := range rep.Matched {
		if m.ExternalID == "DD0312A0B3C42C7F" && (len(m.Postings) != 3 || m.By != "reference_batch") {
			t.Fatalf("the batch line did not match its three payouts: %+v", m)
		}
	}
}

const camtHead = `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt><Stmt><Id>s</Id><Acct><Id><IBAN>DE89370400440532013000</IBAN></Id></Acct>
`
const camtTail = "</Stmt></BkToCstmrStmt></Document>\n"

func camtEntryXML(amt, ccy, ind, sts, date, ref string) string {
	return `<Ntry><Amt Ccy="` + ccy + `">` + amt + `</Amt><CdtDbtInd>` + ind + `</CdtDbtInd><Sts>` + sts +
		`</Sts><BookgDt><Dt>` + date + `</Dt></BookgDt><AcctSvcrRef>` + ref + "</AcctSvcrRef></Ntry>\n"
}

func TestCamt053Amounts(t *testing.T) {
	for _, c := range []struct {
		amt, ccy string
		want     int64
	}{
		{"500", "JPY", 500},
		{"500.00", "JPY", 500},
		{"0.5", "EUR", 50},
		{"12.340", "EUR", 1234},
		{"1.234", "KWD", 1234},
		{"7.", "EUR", 700},
		{"0", "EUR", 0},
		{"92233720368547758.07", "EUR", 9223372036854775807},
	} {
		res, err := ParseCamt053(strings.NewReader(camtHead+camtEntryXML(c.amt, c.ccy, "CRDT", "BOOK", "2030-01-02", "r")+camtTail), CamtOptions{})
		if err != nil {
			t.Errorf("%s %s: %v", c.amt, c.ccy, err)
			continue
		}
		if got := res.Lines[0].Amount; got != c.want {
			t.Errorf("%s %s: got %d, want %d", c.amt, c.ccy, got, c.want)
		}
	}
}

func TestCamt053Errors(t *testing.T) {
	entry := camtEntryXML("1.00", "EUR", "CRDT", "BOOK", "2030-01-02", "r1")
	deep := strings.Repeat("<X>", MaxCamtDepth) + strings.Repeat("</X>", MaxCamtDepth)
	two := `<Stmt><Id>t</Id><Acct><Id><IBAN>CH9300762011623852957</IBAN></Id></Acct>` + entry + "</Stmt>"
	for _, c := range []struct {
		name, in, want string
		opt            CamtOptions
	}{
		{"empty", "", "no camt.053 Document", CamtOptions{}},
		{"not xml", "external_id,date,amount,currency,reference\n", "outside the Document", CamtOptions{}},
		{"other message", `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.052.001.02"><BkToCstmrAcctRpt/></Document>`, "root element must be Document", CamtOptions{}},
		{"no namespace", `<Document><BkToCstmrStmt/></Document>`, "root element must be Document", CamtOptions{}},
		{"bad version", `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.x2"/>`, "root element must be Document", CamtOptions{}},
		{"wrong body", `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><Other/></Document>`, "must hold BkToCstmrStmt", CamtOptions{}},
		{"no statement", `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt/></Document>`, "no statement", CamtOptions{}},
		{"doctype", `<?xml version="1.0"?><!DOCTYPE d [<!ENTITY a "aaaa">]>` + camtHead[22:] + entry + camtTail, "document type declarations", CamtOptions{}},
		{"entity", camtHead + `<Ntry><NtryRef>&a;</NtryRef></Ntry>` + camtTail, "not well-formed", CamtOptions{}},
		{"truncated", camtHead + entry, "not well-formed", CamtOptions{}},
		{"mismatched", camtHead + "<Ntry></Amt>" + camtTail, "not well-formed", CamtOptions{}},
		{"latin-1", strings.Replace(camtHead, "UTF-8", "ISO-8859-1", 1) + entry + camtTail, "ISO-8859-1", CamtOptions{}},
		{"invalid utf-8", camtHead + "<Ntry><NtryRef>\xff</NtryRef></Ntry>" + camtTail, "not valid UTF-8", CamtOptions{}},
		{"too deep", camtHead + deep + camtTail, "nested more than", CamtOptions{}},
		{"long text", camtHead + "<Ntry><AcctSvcrRef>" + strings.Repeat("x", MaxCamtText+1) + "</AcctSvcrRef></Ntry>" + camtTail, "longer than", CamtOptions{}},
		{"two documents", camtHead + entry + camtTail + `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"/>`, "more than one Document", CamtOptions{}},
		{"processing instruction", camtHead + `<?evil x?>` + entry + camtTail, "processing instruction", CamtOptions{}},
		{"no status", camtHead + `<Ntry><Amt Ccy="EUR">1</Amt><CdtDbtInd>CRDT</CdtDbtInd><BookgDt><Dt>2030-01-01</Dt></BookgDt></Ntry>` + camtTail, "entry 1: the entry has no status", CamtOptions{}},
		{"no amount", camtHead + `<Ntry><Sts>BOOK</Sts></Ntry>` + camtTail, "no amount", CamtOptions{}},
		{"no currency", camtHead + `<Ntry><Amt>1</Amt><Sts>BOOK</Sts></Ntry>` + camtTail, "no Ccy", CamtOptions{}},
		{"unknown currency", camtHead + camtEntryXML("1", "XYZ", "CRDT", "BOOK", "2030-01-02", "r") + camtTail, "currency", CamtOptions{}},
		{"lower-case currency", camtHead + camtEntryXML("1", "eur", "CRDT", "BOOK", "2030-01-02", "r") + camtTail, "currency", CamtOptions{}},
		{"too many decimals", camtHead + camtEntryXML("1.001", "EUR", "CRDT", "BOOK", "2030-01-02", "r") + camtTail, "amount", CamtOptions{}},
		{"negative", camtHead + camtEntryXML("-1.00", "EUR", "CRDT", "BOOK", "2030-01-02", "r") + camtTail, "amount", CamtOptions{}},
		{"overflow", camtHead + camtEntryXML("92233720368547758.08", "EUR", "CRDT", "BOOK", "2030-01-02", "r") + camtTail, "amount", CamtOptions{}},
		{"indicator", camtHead + camtEntryXML("1", "EUR", "CR", "BOOK", "2030-01-02", "r") + camtTail, "CdtDbtInd", CamtOptions{}},
		{"bad date", camtHead + camtEntryXML("1", "EUR", "CRDT", "BOOK", "2030-13-02", "r") + camtTail, "YYYY-MM-DD", CamtOptions{}},
		{"bad date-time", camtHead + `<Ntry><Amt Ccy="EUR">1</Amt><CdtDbtInd>CRDT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><DtTm>2030-01-01 10:00</DtTm></BookgDt></Ntry>` + camtTail, "date-time", CamtOptions{}},
		{"no date", camtHead + `<Ntry><Amt Ccy="EUR">1</Amt><CdtDbtInd>CRDT</CdtDbtInd><Sts>BOOK</Sts></Ntry>` + camtTail, "no booking or value date", CamtOptions{}},
		{"twice", camtHead + `<Ntry><Amt Ccy="EUR">1</Amt><Amt Ccy="EUR">2</Amt></Ntry>` + camtTail, "Amt appears twice", CamtOptions{}},
		{"duplicate reference", camtHead + entry + entry + camtTail, `"r1" already appears on entry 1`, CamtOptions{}},
		{"reference too long", camtHead + camtEntryXML("1", "EUR", "CRDT", "BOOK", "2030-01-02", strings.Repeat("r", MaxExternalID+1)) + camtTail, "printable", CamtOptions{}},
		{"two accounts", camtHead + entry + "</Stmt>" + two + "</BkToCstmrStmt></Document>", "statements for 2 accounts (CH9300762011623852957, DE89370400440532013000)", CamtOptions{}},
		{"account not in file", camtHead + entry + camtTail, "no statement for account", CamtOptions{Account: "GB33BUKB20201555555555"}},
		{"too large", camtHead + strings.Repeat(" ", MaxBytes) + camtTail, "", CamtOptions{}},
		{"too many entries", camtHead + strings.Repeat(`<Ntry><Sts>PDNG</Sts></Ntry>`, MaxLines+1) + camtTail, "", CamtOptions{}},
	} {
		_, err := ParseCamt053(strings.NewReader(c.in), c.opt)
		if c.want == "" {
			if !errors.Is(err, ErrTooLarge) {
				t.Errorf("%s: got %v, want ErrTooLarge", c.name, err)
			}
			continue
		}
		var ce *CamtError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want a CamtError containing %q", c.name, err, c.want)
		}
	}
}

// TestCamt053ErrorLine checks that an error names the line of the file and
// the entry it is in.
func TestCamt053ErrorLine(t *testing.T) {
	in := camtHead + camtEntryXML("1", "EUR", "CRDT", "BOOK", "2030-01-02", "a") + camtEntryXML("1", "EUR", "XX", "BOOK", "2030-01-02", "b") + camtTail
	_, err := ParseCamt053(strings.NewReader(in), CamtOptions{})
	var ce *CamtError
	if !errors.As(err, &ce) || ce.Line != 4 || ce.Entry != 2 {
		t.Fatalf("got %#v", err)
	}
}

// TestCamt053Foreign checks that elements of other namespaces, however
// they are named, are not read as statements or entries.
func TestCamt053Foreign(t *testing.T) {
	in := camtHead + `<x:Ntry xmlns:x="urn:example"><x:Amt Ccy="EUR">5</x:Amt></x:Ntry>` +
		`<Ntry><Amt Ccy="EUR">1</Amt><CdtDbtInd>CRDT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>2030-01-02</Dt></BookgDt>` +
		`<x:AcctSvcrRef xmlns:x="urn:example">no</x:AcctSvcrRef></Ntry>` + camtTail
	res, err := ParseCamt053(strings.NewReader(in), CamtOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{Row: 2, ExternalID: "entry-1", Date: "2030-01-02", Amount: 100, Currency: "EUR"}}
	if !reflect.DeepEqual(res.Lines, want) {
		t.Fatalf("got %+v", res.Lines)
	}
}

// largeCamt is a statement of MaxLines booked entries, each with a
// transaction and its references, as large as the limits allow.
func largeCamt(t testing.TB) string {
	var b strings.Builder
	b.WriteString(camtHead)
	for i := 0; i < MaxLines; i++ {
		b.WriteString(`<Ntry><Amt Ccy="EUR">12.34</Amt><CdtDbtInd>CRDT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>2030-01-02</Dt></BookgDt>`)
		fmt.Fprintf(&b, `<AcctSvcrRef>ref-%06d</AcctSvcrRef>`, i)
		b.WriteString(`<NtryDtls><TxDtls><Refs><EndToEndId>e2e</EndToEndId></Refs><RmtInf><Ustrd>x</Ustrd></RmtInf></TxDtls></NtryDtls></Ntry>`)
	}
	b.WriteString(camtTail)
	if b.Len() > MaxBytes {
		t.Fatalf("the test statement is %d bytes, over the limit", b.Len())
	}
	return b.String()
}

// TestCamt053Large parses a statement of MaxLines entries.
func TestCamt053Large(t *testing.T) {
	res, err := ParseCamt053(strings.NewReader(largeCamt(t)), CamtOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != MaxLines || res.Lines[MaxLines-1].Row != MaxLines+1 {
		t.Fatalf("got %d lines", len(res.Lines))
	}
}

// BenchmarkCamt053 parses a statement of MaxLines entries (2.7 MB).
func BenchmarkCamt053(b *testing.B) {
	in := largeCamt(b)
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseCamt053(strings.NewReader(in), CamtOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzCamt053(f *testing.F) {
	for _, name := range []string{"dk-v02.xml", "sps-v04.xml", "v08-two-accounts.xml"} {
		f.Add(string(readSample(f, name)), "")
	}
	f.Add(string(readSample(f, "v08-two-accounts.xml")), "000987654321")
	f.Add(camtHead+camtEntryXML("500.00", "JPY", "DBIT", "BOOK", "2030-01-02", "r")+camtTail, "")
	f.Add("<Document/>", "")
	f.Fuzz(func(t *testing.T, in, account string) {
		res, err := ParseCamt053(strings.NewReader(in), CamtOptions{Account: account})
		if err != nil {
			return
		}
		ids := map[string]bool{}
		for i, ln := range res.Lines {
			if ln.Row < 2 || (i > 0 && ln.Row <= res.Lines[i-1].Row) {
				t.Fatalf("rows are not increasing from 2: %+v", res.Lines)
			}
			if ids[ln.ExternalID] {
				t.Fatalf("external ID %q twice", ln.ExternalID)
			}
			ids[ln.ExternalID] = true
		}
		// Every statement the parser accepts is one the CSV parser accepts
		// with the same lines.
		var buf bytes.Buffer
		if err := Format(&buf, res.Lines); err != nil {
			t.Fatal(err)
		}
		back, err := Parse(&buf)
		if err != nil {
			t.Fatalf("lines do not make a CSV statement: %v\n%s", err, buf.String())
		}
		for i := range back {
			back[i].Row = res.Lines[i].Row
		}
		if len(back) != len(res.Lines) || (len(back) > 0 && !reflect.DeepEqual(back, res.Lines)) {
			t.Fatalf("CSV round trip changed the lines:\n%+v\n%+v", res.Lines, back)
		}
	})
}

// TestCamt053MixedTransactions checks that an entry whose transactions
// belong to different payments gets no reference: none of theirs names it.
func TestCamt053MixedTransactions(t *testing.T) {
	tx := func(pmt, e2e string) string {
		return `<TxDtls><Refs><PmtInfId>` + pmt + `</PmtInfId><EndToEndId>` + e2e + `</EndToEndId></Refs></TxDtls>`
	}
	ntry := func(ref, txs string) string {
		return `<Ntry><Amt Ccy="EUR">3</Amt><CdtDbtInd>DBIT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>2030-01-02</Dt></BookgDt>` +
			`<AcctSvcrRef>` + ref + `</AcctSvcrRef><NtryDtls>` + txs + `</NtryDtls></Ntry>`
	}
	in := camtHead + ntry("a", tx("p1", "e1")+tx("p2", "e2")+tx("p1", "e3")) + ntry("b", tx("p1", "e1")+tx("p1", "e2")) + camtTail
	res, err := ParseCamt053(strings.NewReader(in), CamtOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Lines[0].Reference != "" || res.Lines[1].Reference != "p1" {
		t.Fatalf("got %+v", res.Lines)
	}
}
