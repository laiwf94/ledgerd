package recon

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

const hdr = "external_id,date,amount,currency,reference\n"

func TestParse(t *testing.T) {
	in := "\xef\xbb\xbfExternal_ID, Date ,amount,currency,reference\n" +
		"bk-1,2030-01-02,12.34,USD,ord-1\n" +
		"bk-2,2030-01-03,-5,USD,\n" +
		`"bk,3",2030-01-03,0.5,USD,"say ""hi"""` + "\n" +
		"bk-4,2030-01-04,500,JPY,x\n"
	lines, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{
		{Row: 2, ExternalID: "bk-1", Date: "2030-01-02", Amount: 1234, Currency: "USD", Reference: "ord-1"},
		{Row: 3, ExternalID: "bk-2", Date: "2030-01-03", Amount: -500, Currency: "USD"},
		{Row: 4, ExternalID: "bk,3", Date: "2030-01-03", Amount: 50, Currency: "USD", Reference: `say "hi"`},
		{Row: 5, ExternalID: "bk-4", Date: "2030-01-04", Amount: 500, Currency: "JPY", Reference: "x"},
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("got %+v", lines)
	}
	if lines, err := Parse(strings.NewReader(hdr)); err != nil || len(lines) != 0 {
		t.Fatalf("header only: %v %v", lines, err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"":                                                            "empty",
		"id,date,amount,currency,ref\n":                               "header",
		hdr + "a,2030-01-02,1.00,USD\n":                               "wrong number of fields",
		hdr + ",2030-01-02,1.00,USD,\n":                               "external_id",
		hdr + "a,02/01/2030,1.00,USD,\n":                              "date",
		hdr + "a,2030-02-30,1.00,USD,\n":                              "date",
		hdr + "a,2030-01-02,1.001,USD,\n":                             "amount",
		hdr + "a,2030-01-02,1.5,JPY,\n":                               "amount",
		hdr + "a,2030-01-02,1;00,USD,\n":                              "amount",
		hdr + "a,2030-01-02,$1,USD,\n":                                "amount",
		hdr + "a,2030-01-02,1,usd,\n":                                 "currency",
		hdr + "a,2030-01-02,1,XAU,\n":                                 "currency",
		hdr + "a,2030-01-02,1,USD,\x01\n":                             "reference",
		hdr + "a\x7f,2030-01-02,1,USD,\n":                             "external_id",
		hdr + `"a,2030-01-02,1,USD,` + "\n":                           "quote",
		hdr + "a,2030-01-02,1,USD,\na,2030-01-03,2,USD,\n":            "already appears on row 2",
		hdr + strings.Repeat("x", 65) + ",2030-01-02,1,USD,\n":        "external_id",
		hdr + "a,2030-01-02,1,USD," + strings.Repeat("r", 129) + "\n": "reference",
		"\xff\xfe": "UTF-8",
		hdr + "a,2030-01-02,99999999999999999999,USD,\n": "too large",
	}
	for in, want := range cases {
		_, err := Parse(strings.NewReader(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
	var pe *ParseError
	if _, err := Parse(strings.NewReader(hdr + "ok,2030-01-02,1,USD,\nbad,x,1,USD,\n")); !errors.As(err, &pe) || pe.Row != 3 {
		t.Fatalf("row number: %v", err)
	}
}

func TestParseRowsAreFileLines(t *testing.T) {
	// Blank lines are skipped by the csv reader but must still count.
	in := hdr + "\nok,2030-01-02,1,USD,\n\n\nbad,x,1,USD,\n"
	var pe *ParseError
	if _, err := Parse(strings.NewReader(in)); !errors.As(err, &pe) || pe.Row != 6 {
		t.Fatalf("row = %v, want 6 (err %v)", pe, err)
	}
	lines, err := Parse(strings.NewReader(hdr + "\nok,2030-01-02,1,USD,\n"))
	if err != nil || len(lines) != 1 || lines[0].Row != 3 {
		t.Fatalf("lines = %+v, %v; want one line on row 3", lines, err)
	}
}

func TestParseLimits(t *testing.T) {
	var b strings.Builder
	b.WriteString(hdr)
	for i := 0; i <= MaxLines; i++ {
		fmt.Fprintf(&b, "id-%d,2030-01-02,1,USD,\n", i)
	}
	if _, err := Parse(strings.NewReader(b.String())); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too many lines: %v", err)
	}
	big := hdr + "a,2030-01-02,1,USD," + strings.Repeat("r", MaxBytes)
	if _, err := Parse(strings.NewReader(big)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too many bytes: %v", err)
	}
	if _, err := Parse(failingReader{}); err == nil {
		t.Fatal("read error ignored")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestFormatRoundTrip(t *testing.T) {
	lines := []Line{
		{Row: 2, ExternalID: "a,b", Date: "2030-01-02", Amount: -1, Currency: "KWD", Reference: `q"uote`},
		{Row: 3, ExternalID: "c", Date: "2030-01-03", Amount: 700, Currency: "JPY"},
	}
	var buf bytes.Buffer
	if err := Format(&buf, lines); err != nil {
		t.Fatal(err)
	}
	back, err := Parse(&buf)
	if err != nil || !reflect.DeepEqual(back, lines) {
		t.Fatalf("round trip: %+v %v\n%s", back, err, buf.String())
	}
	if err := Format(&buf, []Line{{Currency: "ZZZ"}}); err == nil {
		t.Fatal("unknown currency formatted")
	}
}

func at(date string) time.Time {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return d.Add(15 * time.Hour)
}

func day0(date string) time.Time { return at(date).Truncate(24 * time.Hour) }

func TestReconcile(t *testing.T) {
	lines := []Line{
		{Row: 2, ExternalID: "s1", Date: "2030-03-01", Amount: 1000, Currency: "USD", Reference: "ord-1"},  // exact by reference
		{Row: 3, ExternalID: "s2", Date: "2030-03-02", Amount: 2500, Currency: "USD", Reference: "ord-2"},  // reference, wrong amount
		{Row: 4, ExternalID: "s3", Date: "2030-03-04", Amount: 700, Currency: "USD", Reference: "BANK-77"}, // amount+date, ref differs in format
		{Row: 5, ExternalID: "s4", Date: "2030-03-05", Amount: 999, Currency: "USD"},                       // nothing in the ledger
		{Row: 6, ExternalID: "s5", Date: "2030-03-05", Amount: 300, Currency: "USD"},                       // ledger posting too far away
	}
	postings := []Posting{
		{Seq: 1, Time: at("2030-03-01"), TransferID: "t1", Reference: "ord-1", Amount: 1000},
		{Seq: 2, Time: at("2030-03-02"), TransferID: "t2", Reference: "ord-2", Amount: 2400},
		{Seq: 3, Time: at("2030-03-02"), TransferID: "t3", Reference: "ord-3", Amount: 700},
		{Seq: 4, Time: at("2030-03-03"), TransferID: "t4", Amount: 50},  // not on the statement
		{Seq: 5, Time: at("2030-02-20"), TransferID: "t5", Amount: 300}, // outside window and period
		{Seq: 6, Time: at("2030-03-09"), TransferID: "t6", Amount: 1},   // after the period
	}
	from, to, ok := Period(lines)
	if !ok || from.Format("2006-01-02") != "2030-03-01" || to.Format("2006-01-02") != "2030-03-05" {
		t.Fatalf("period %v %v", from, to)
	}
	rep := Reconcile(lines, postings, from, to, 3)
	if len(rep.Matched) != 2 || rep.Matched[0].By != "reference" || rep.Matched[0].TransferID != "t1" ||
		rep.Matched[1].By != "amount_date" || rep.Matched[1].TransferID != "t3" || rep.Matched[1].DaysApart != 2 {
		t.Fatalf("matched %+v", rep.Matched)
	}
	if len(rep.AmountMismatches) != 1 || rep.AmountMismatches[0].StatementAmount != 2500 || rep.AmountMismatches[0].LedgerAmount != 2400 {
		t.Fatalf("mismatches %+v", rep.AmountMismatches)
	}
	if len(rep.MissingInLedger) != 2 || rep.MissingInLedger[0].ExternalID != "s4" || rep.MissingInLedger[1].ExternalID != "s5" {
		t.Fatalf("missing in ledger %+v", rep.MissingInLedger)
	}
	if len(rep.MissingInStatement) != 1 || rep.MissingInStatement[0].TransferID != "t4" || rep.MissingInStatement[0].Date != "2030-03-03" {
		t.Fatalf("missing in statement %+v", rep.MissingInStatement)
	}
	want := Summary{StatementLines: 5, LedgerPostings: 6, Matched: 2, AmountMismatches: 1, MissingInLedger: 2, MissingInStatement: 1}
	if rep.Summary != want || rep.From != "2030-03-01" || rep.WindowDays != 3 {
		t.Fatalf("summary %+v", rep)
	}
	// An exact-amount posting is preferred among several with the reference.
	rep = Reconcile([]Line{{Row: 2, ExternalID: "x", Date: "2030-03-01", Amount: 5, Reference: "r"}},
		[]Posting{{Seq: 1, Time: at("2030-03-01"), Reference: "r", Amount: 4}, {Seq: 2, Time: at("2030-03-01"), Reference: "r", Amount: 5}},
		day0("2030-03-01"), day0("2030-03-01"), 0)
	if len(rep.Matched) != 1 || rep.Matched[0].Seq != 2 || len(rep.MissingInStatement) != 1 {
		t.Fatalf("prefer exact: %+v", rep)
	}
	if _, _, ok := Period(nil); ok {
		t.Fatal("empty statement has a period")
	}
	rep = Reconcile(nil, nil, day0("2030-03-01"), day0("2030-03-01"), 0)
	if rep.Matched == nil || rep.MissingInLedger == nil || rep.MissingInStatement == nil || rep.AmountMismatches == nil {
		t.Fatal("empty sections must be empty lists, not null")
	}
}

func TestPreEpochDates(t *testing.T) {
	ts := time.Date(1969, 12, 31, 23, 0, 0, 0, time.UTC)
	rep := Reconcile([]Line{{Row: 2, ExternalID: "x", Date: "1969-12-31", Amount: 5}},
		[]Posting{{Seq: 1, Time: ts, Amount: 5}}, ts.Truncate(24*time.Hour), ts, 0)
	if len(rep.Matched) != 1 || rep.Matched[0].DaysApart != 0 {
		t.Fatalf("pre-epoch: %+v", rep)
	}
}

// maxMatching is an independent reference for pass 2: the size of a maximum
// bipartite matching between lines and postings with equal amounts and dates
// at most window days apart, by augmenting paths.
func maxMatching(lines []Line, ps []Posting, window int64) int {
	match := make([]int, len(ps))
	for i := range match {
		match[i] = -1
	}
	ok := func(l Line, p Posting) bool {
		d, _ := time.Parse("2006-01-02", l.Date)
		return l.Amount == p.Amount && abs(day(d)-day(p.Time)) <= window
	}
	var try func(li int, seen []bool) bool
	try = func(li int, seen []bool) bool {
		for pi := range ps {
			if !seen[pi] && ok(lines[li], ps[pi]) {
				seen[pi] = true
				if match[pi] < 0 || try(match[pi], seen) {
					match[pi] = li
					return true
				}
			}
		}
		return false
	}
	n := 0
	for li := range lines {
		if try(li, make([]bool, len(ps))) {
			n++
		}
	}
	return n
}

func TestAmountDateMatchingIsMaximal(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	base := day0("2030-01-01")
	for iter := 0; iter < 2000; iter++ {
		var lines []Line
		var ps []Posting
		for i := 0; i < r.Intn(12); i++ {
			lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Currency: "USD",
				Date: base.AddDate(0, 0, r.Intn(10)).Format("2006-01-02"), Amount: int64(1 + r.Intn(3))})
		}
		for i := 0; i < r.Intn(12); i++ {
			ps = append(ps, Posting{Seq: uint64(i + 1), Time: base.AddDate(0, 0, r.Intn(10)).Add(time.Duration(r.Intn(86400)) * time.Second),
				Amount: int64(1 + r.Intn(3))})
		}
		w := r.Intn(3)
		rep := Reconcile(lines, ps, base, base.AddDate(0, 0, 10), w)
		if got, want := len(rep.Matched), maxMatching(lines, ps, int64(w)); got != want {
			t.Fatalf("iteration %d: matched %d, a maximum matching has %d\nlines %+v\npostings %+v", iter, got, want, lines, ps)
		}
		if rep.Summary.Matched+rep.Summary.MissingInLedger != len(lines) {
			t.Fatalf("every line must be matched or missing")
		}
		for _, m := range rep.Matched {
			if m.DaysApart > int64(w) {
				t.Fatalf("matched outside the window: %+v", m)
			}
		}
	}
}

func TestReconcileScales(t *testing.T) {
	// Every line and posting share one amount and one day: the worst case
	// for a naive scan.
	var lines []Line
	var ps []Posting
	for i := 0; i < MaxLines; i++ {
		lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: "2030-01-01", Amount: 100})
	}
	for i := 0; i < 100_000; i++ {
		ps = append(ps, Posting{Seq: uint64(i + 1), Time: at("2030-01-01"), Amount: 100})
	}
	start := time.Now()
	rep := Reconcile(lines, ps, day0("2030-01-01"), day0("2030-01-01"), 3)
	if rep.Summary.Matched != MaxLines || rep.Summary.MissingInStatement != 90_000 {
		t.Fatalf("summary %+v", rep.Summary)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(hdr + "bk-1,2030-01-02,12.34,USD,ord-1\n")
	f.Add(hdr + `"a""b",2030-01-02,-0.5,KWD,"x,y"` + "\n")
	f.Add("\xef\xbb\xbf" + hdr)
	f.Add("garbage")
	f.Fuzz(func(t *testing.T, in string) {
		lines, err := Parse(strings.NewReader(in))
		if err != nil {
			return
		}
		var buf bytes.Buffer
		if err := Format(&buf, lines); err != nil {
			t.Fatal(err)
		}
		back, err := Parse(&buf)
		if err != nil {
			t.Fatalf("formatted output does not parse: %v\n%s", err, buf.String())
		}
		for i := range back {
			back[i].Row = lines[i].Row
		}
		if !reflect.DeepEqual(back, lines) && len(lines) > 0 {
			t.Fatalf("round trip changed the lines:\n%+v\n%+v", lines, back)
		}
	})
}

func BenchmarkReconcile(b *testing.B) {
	r := rand.New(rand.NewSource(1))
	var lines []Line
	var ps []Posting
	for i := 0; i < 5000; i++ {
		d := day0("2030-01-01").AddDate(0, 0, r.Intn(30))
		amt := int64(100 + r.Intn(5000))
		ps = append(ps, Posting{Seq: uint64(i + 1), Time: d.Add(time.Hour), Amount: amt, Reference: fmt.Sprint("r", i)})
		if r.Intn(10) != 0 {
			ref := ""
			if r.Intn(2) == 0 {
				ref = fmt.Sprint("r", i)
			}
			lines = append(lines, Line{Row: i + 2, ExternalID: fmt.Sprint(i), Date: d.AddDate(0, 0, r.Intn(3)).Format("2006-01-02"), Amount: amt, Reference: ref})
		}
	}
	for b.Loop() {
		Reconcile(lines, ps, day0("2030-01-01"), day0("2030-01-30"), 3)
	}
}
