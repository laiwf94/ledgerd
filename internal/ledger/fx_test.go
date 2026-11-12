package ledger

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// quoted is when the test rates were quoted, in a zone other than UTC so the
// tests see it normalized.
var quoted = time.Date(2026, 11, 12, 9, 0, 0, 0, time.FixedZone("CET", 3600))

// in puts a posting in another currency than its transfer's.
func in(cur string, p PostingSpec) PostingSpec { p.Currency = cur; return p }

func eur(p PostingSpec) PostingSpec { return in("EUR", p) }

// convert builds a USD transfer with a rate for its other currency.
func convert(rate string, legs ...PostingSpec) Command {
	return Command{CreateTransfer: &TransferSpec{Currency: "USD", Postings: legs,
		FX: &FXSpec{Rate: rate, Source: "ecb", QuotedAt: quoted}}}
}

func convertHold(rate string, timeout int64, legs ...PostingSpec) Command {
	c := convert(rate, legs...)
	c.CreateTransfer.Pending, c.CreateTransfer.TimeoutSeconds = true, timeout
	return c
}

// usdToEUR is a conversion of usd cents from alice's USD wallet into eurc
// cents in her EUR wallet through the two exchange accounts.
func usdToEUR(rate string, usd, eurc int64) Command {
	return convert(rate, dr("alice", usd), cr("fx_usd", usd), eur(dr("fx_eur", eurc)), eur(cr("alice_eur", eurc)))
}

func exchangeAcct(id, cur string) Command {
	return Command{CreateAccount: &AccountSpec{ID: id, Currency: cur, NormalBalance: Credit, Exchange: true}}
}

// fxSetup opens alice's wallets in three currencies, a second EUR wallet,
// a fee account, an exchange account per currency, and funds alice with
// 1,000.00 USD.
func (e *env) fxSetup() {
	e.standard()
	e.do(openAcct("alice_eur", "EUR", Credit, "nonneg"))
	e.do(openAcct("bob_eur", "EUR", Credit))
	e.do(openAcct("alice_jpy", "JPY", Credit, "nonneg"))
	e.do(openAcct("fees", "USD", Credit))
	e.do(exchangeAcct("fx_usd", "USD"))
	e.do(exchangeAcct("fx_eur", "EUR"))
	e.do(exchangeAcct("fx_jpy", "JPY"))
	e.do(move(dr("equity", 100000), cr("alice", 100000)))
}

func (e *env) head() uint64 {
	e.t.Helper()
	e.l.mu.RLock()
	defer e.l.mu.RUnlock()
	return e.l.st.head()
}

// A USD to EUR conversion is one entry in two currencies that balances in
// each, with its rate on the transfer and the entry.
func TestConversion(t *testing.T) {
	e := newEnv(t, false)
	e.fxSetup()
	if a := e.account("fx_usd"); !a.Exchange {
		t.Fatalf("fx_usd is not an exchange account: %+v", a)
	}
	tr := e.do(usdToEUR("0.9234", 10000, 9234)).Transfer
	want := &FX{Base: "USD", Quote: "EUR", Rate: "0.9234", Source: "ecb", QuotedAt: quoted.UTC()}
	if !reflect.DeepEqual(tr.FX, want) || tr.FX.QuotedAt.Location() != time.UTC {
		t.Fatalf("fx %+v, want %+v", tr.FX, want)
	}
	if tr.Currency != "USD" || tr.Postings[0].Currency != "" || tr.Postings[1].Currency != "" ||
		tr.Postings[2].Currency != "EUR" || tr.Postings[3].Currency != "EUR" {
		t.Fatalf("postings %+v", tr.Postings)
	}
	en := e.entry(tr.Entries[0].Seq)
	if en.FX != tr.FX || en.Currency != "USD" || en.Lines[0].Currency != "" || en.Lines[2].Currency != "EUR" || en.Lines[3].Currency != "EUR" {
		t.Fatalf("entry %+v", en)
	}
	e.wantBalance("alice", 90000, 0, 90000)
	e.wantBalance("fx_usd", 10000, 0, 10000)
	e.wantBalance("fx_eur", -9234, 0, -9234)
	e.wantBalance("alice_eur", 9234, 0, 9234)
	e.invariants()

	lines, _, err := e.l.Statement("alice_eur", 0, 10)
	if err != nil || len(lines) != 1 || lines[0].PostedChange != 9234 || lines[0].TransferID != tr.ID {
		t.Fatalf("statement %+v %v", lines, err)
	}
	// It is listed under both its currencies.
	for cur, n := range map[string]int{"USD": 2, "EUR": 1, "JPY": 0} {
		page, err := e.l.ListTransfers(TransferFilter{Currency: cur}, 0, false, 10)
		if err != nil || len(page.Transfers) != n || n > 0 && page.Transfers[n-1].ID != tr.ID {
			t.Fatalf("%s: %+v %v", cur, page, err)
		}
	}
	if v := e.l.CheckIndexes(); len(v) > 0 {
		t.Fatal(v)
	}
	p, _ := json.Marshal(tr)
	for _, s := range []string{`"fx":{"base":"USD","quote":"EUR","rate":"0.9234","source":"ecb","quoted_at":"2026-11-12T08:00:00Z"}`,
		`"account":"fx_eur","direction":"debit","currency":"EUR","amount":9234`} {
		if !strings.Contains(string(p), s) {
			t.Fatalf("%s lacks %s", p, s)
		}
	}
	plain := e.do(move(dr("alice", 1), cr("merchant", 1))).Transfer
	if p, _ := json.Marshal(plain); strings.Contains(string(p), `"fx"`) || strings.Contains(string(p), `"currency":"USD","amount"`) {
		t.Fatalf("single-currency transfer %s", p)
	}
	// The posting currency may be spelled out when it is the transfer's.
	c := move(dr("alice", 1), in("USD", cr("merchant", 1)))
	if tr := e.do(c).Transfer; tr.FX != nil || tr.Postings[1].Currency != "" {
		t.Fatalf("spelled-out currency: %+v", tr)
	}
}

// The rate is checked against what moves through the exchange accounts.
func TestConversionRates(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  Command
		code Code   // "" when accepted
		msg  string // part of the refusal
	}{
		{"exact", usdToEUR("0.9234", 10000, 9234), "", ""},
		{"rounded up", usdToEUR("0.9234", 10001, 9235), "", ""},
		{"rounded down", usdToEUR("0.9234", 10001, 9234), "", ""},
		{"a unit too much", usdToEUR("0.9234", 10001, 9236), CodeInvalid, "converts to 92.3492 EUR, not 92.36 EUR"},
		{"a unit too little", usdToEUR("0.9234", 10000, 9233), CodeInvalid, "converts to 92.3400 EUR, not 92.33 EUR"},
		{"a fee in the base currency", convert("0.9234", dr("alice", 10100), cr("fx_usd", 10000), cr("fees", 100),
			eur(dr("fx_eur", 9234)), eur(cr("alice_eur", 9234))), "", ""},
		{"a spread in the quote currency", convert("0.9234", dr("alice", 10000), cr("fx_usd", 10000),
			eur(dr("fx_eur", 9234)), eur(cr("alice_eur", 9200)), eur(cr("bob_eur", 34))), "", ""},
		{"the fee counted as converted", convert("0.9234", dr("alice", 10100), cr("fx_usd", 10000), cr("fees", 100),
			eur(dr("fx_eur", 9326)), eur(cr("alice_eur", 9326))), CodeInvalid, "converts to"},
		{"USD to JPY", convert("151.25", dr("alice", 10000), cr("fx_usd", 10000),
			in("JPY", dr("fx_jpy", 15125)), in("JPY", cr("alice_jpy", 15125))), "", ""},
		{"USD to JPY in cents", convert("151.25", dr("alice", 10000), cr("fx_usd", 10000),
			in("JPY", dr("fx_jpy", 1512500)), in("JPY", cr("alice_jpy", 1512500))), CodeInvalid, "converts to 15125.00 JPY"},
		{"no exchange account in EUR", convert("0.9234", dr("alice", 10000), cr("fx_usd", 10000),
			eur(dr("bob_eur", 9234)), eur(cr("alice_eur", 9234))), CodeInvalid, "through exchange accounts in both USD and EUR"},
		{"no exchange account at all", convert("0.9234", dr("alice", 10000), cr("merchant", 10000),
			eur(dr("bob_eur", 9234)), eur(cr("alice_eur", 9234))), CodeInvalid, "through exchange accounts in both"},
		{"exchange accounts take in both", convert("0.9234", dr("alice", 10000), cr("fx_usd", 10000),
			eur(dr("bob_eur", 9234)), eur(cr("fx_eur", 9234))), CodeInvalid, "takes in one and pays out the other"},
		{"exchange accounts on both sides of a currency", convert("0.9234", dr("fx_usd", 10000), cr("fx_usd2", 10000),
			eur(dr("fx_eur", 9234)), eur(cr("alice_eur", 9234))), CodeInvalid, "through exchange accounts in both"},
		{"EUR posting to a USD account", convert("0.9234", dr("alice", 10000), cr("fx_usd", 10000),
			eur(dr("fx_eur", 9234)), eur(cr("merchant", 9234))), CodeCurrencyMismatch, "merchant holds USD, not EUR"},
		{"USD posting to a EUR account", convert("0.9234", dr("alice", 10000), cr("alice_eur", 10000),
			eur(dr("fx_eur", 9234)), eur(cr("bob_eur", 9234))), CodeCurrencyMismatch, "alice_eur holds EUR, not USD"},
		{"missing exchange account", convert("0.9234", dr("alice", 10000), cr("fx_nobody", 10000),
			eur(dr("fx_eur", 9234)), eur(cr("alice_eur", 9234))), CodeNotFound, "fx_nobody"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, false)
			e.fxSetup()
			e.do(exchangeAcct("fx_usd2", "USD"))
			before := e.head()
			out, err := e.l.Do(context.Background(), c.cmd, nil)
			if c.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				if out.Transfer.FX == nil {
					t.Fatalf("no rate on %+v", out.Transfer)
				}
			} else {
				if codeOf(err) != c.code || !strings.Contains(err.Error(), c.msg) {
					t.Fatalf("err %v, want %s containing %q", err, c.code, c.msg)
				}
				if e.head() != before {
					t.Fatal("a refused conversion wrote an entry")
				}
			}
			e.invariants()
		})
	}
}

func TestConversionValidation(t *testing.T) {
	gbp := func(p PostingSpec) PostingSpec { return in("GBP", p) }
	bad := map[string]struct {
		cmd Command
		msg string
	}{
		"three currencies":               {convert("0.9", dr("a", 10), cr("b", 10), eur(dr("c", 9)), eur(cr("d", 9)), gbp(dr("e", 1)), gbp(cr("f", 1))), "at most two currencies"},
		"no rate":                        {move(dr("a", 10), cr("b", 10), eur(dr("c", 9)), eur(cr("d", 9))), "needs fx"},
		"rate without a second currency": {convert("0.9", dr("a", 10), cr("b", 10)), "only to a transfer with postings in two currencies"},
		"unbalanced in each currency":    {convert("0.9", dr("a", 10), eur(cr("d", 10))), "USD debits (10) must equal USD credits (0)"},
		"unbalanced in the quote":        {convert("0.9", dr("a", 10), cr("b", 10), eur(dr("c", 9)), eur(cr("d", 8)), eur(cr("e", 2))), "EUR debits (9) must equal EUR credits (10)"},
		"lower-case currency":            {convert("0.9", dr("a", 10), cr("b", 10), in("eur", dr("c", 9)), in("eur", cr("d", 9))), "postings[2].currency"},
		"unknown currency":               {convert("0.9", dr("a", 10), cr("b", 10), in("XAU", dr("c", 9)), in("XAU", cr("d", 9))), "postings[2].currency"},
	}
	for _, r := range []string{"", "0", "0.000", "-0.9", "+0.9", "1e2", "1/3", ".9", "9.", "0,9", "NaN", strings.Repeat("1", 31)} {
		bad["rate "+r] = struct {
			cmd Command
			msg string
		}{convert(r, dr("a", 10), cr("b", 10), eur(dr("c", 9)), eur(cr("d", 9))), "fx.rate must be a positive decimal"}
	}
	fx := func(f func(*FXSpec)) Command {
		c := convert("0.9", dr("a", 10), cr("b", 10), eur(dr("c", 9)), eur(cr("d", 9)))
		f(c.CreateTransfer.FX)
		return c
	}
	bad["no source"] = struct {
		cmd Command
		msg string
	}{fx(func(f *FXSpec) { f.Source = "" }), "fx.source"}
	bad["long source"] = struct {
		cmd Command
		msg string
	}{fx(func(f *FXSpec) { f.Source = strings.Repeat("s", MaxFXSource+1) }), "fx.source"}
	bad["control character in source"] = struct {
		cmd Command
		msg string
	}{fx(func(f *FXSpec) { f.Source = "ecb\n" }), "fx.source"}
	bad["no quote time"] = struct {
		cmd Command
		msg string
	}{fx(func(f *FXSpec) { f.QuotedAt = time.Time{} }), "fx.quoted_at must be the time"}
	bad["quote time before 1970"] = struct {
		cmd Command
		msg string
	}{fx(func(f *FXSpec) { f.QuotedAt = time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC) }), "between 1970 and 9999"}
	bal := convert("0.9", dr("a", 10), cr("b", 10), eur(dr("c", 9)), eur(cr("d", 9)))
	bal.CreateTransfer.BalanceDebit = true
	bad["balancing"] = struct {
		cmd Command
		msg string
	}{bal, "two-posting transfers"}
	for name, c := range bad {
		err := c.cmd.Validate()
		if err == nil || err.Code != CodeInvalid || !strings.Contains(err.Message, c.msg) {
			t.Errorf("%s: %v, want %q", name, err, c.msg)
		}
	}
	for _, r := range []string{"0.9", "1", "151.25", "0.000001", strings.Repeat("9", 30)} {
		c := convert(r, dr("a", 10), cr("b", 10), eur(dr("c", 9)), eur(cr("d", 9)))
		if err := c.Validate(); err != nil {
			t.Errorf("rate %s: %v", r, err)
		}
	}
	// A batch names the transfer that is wrong.
	b := batchCmd(move(dr("a", 1), cr("b", 1)), convert("0", dr("a", 10), cr("b", 10), eur(dr("c", 9)), eur(cr("d", 9))))
	err := b.Validate()
	if err == nil || err.Item == nil || *err.Item != 1 {
		t.Fatalf("batch: %v", err)
	}
}

// A multi-currency hold reserves in both currencies; explicit amounts for a
// post or an increase must balance in each currency and agree with the
// rate; a void or an expiry releases both sides.
func TestConversionHold(t *testing.T) {
	e := newEnv(t, false)
	e.fxSetup()
	legs := func(usd, eurc int64) []PostingSpec {
		return []PostingSpec{dr("alice", usd), cr("fx_usd", usd), eur(dr("fx_eur", eurc)), eur(cr("alice_eur", eurc))}
	}
	parts := func(usd, eurc int64) []PartialAmount {
		return []PartialAmount{{"alice", usd}, {"fx_usd", usd}, {"fx_eur", eurc}, {"alice_eur", eurc}}
	}
	h := e.do(convertHold("0.9234", 60, legs(20000, 18468)...)).Transfer
	if h.State != StatePending || h.FX == nil || e.entry(h.Entries[0].Seq).FX != h.FX {
		t.Fatalf("hold %+v", h)
	}
	e.wantBalance("alice", 100000, -20000, 80000)
	e.wantBalance("alice_eur", 0, 18468, 0)
	e.wantBalance("fx_eur", 0, -18468, -18468)
	e.invariants()

	if err := e.fail(postCmd(h.ID, 0, parts(10000, 9000)...), CodeInvalid); !strings.Contains(err.Message, "converts to") {
		t.Fatal(err)
	}
	if err := e.fail(postCmd(h.ID, 0, PartialAmount{"alice", 10000}, PartialAmount{"fx_usd", 10000}, PartialAmount{"fx_eur", 9234},
		PartialAmount{"alice_eur", 9000}), CodeInvalid); !strings.Contains(err.Message, "EUR debits (9234) must equal EUR credits (9000)") {
		t.Fatal(err)
	}
	p := e.do(postCmd(h.ID, 0, parts(10000, 9234)...)).Transfer
	if p.State != StatePosted || p.Postings[3].Posted != 9234 || p.Postings[3].Currency != "EUR" {
		t.Fatalf("posted %+v", p)
	}
	if en := e.entry(p.Entries[1].Seq); en.FX != nil || en.Lines[3].Currency != "EUR" || en.Lines[3].Posted != 9234 || en.Lines[3].Pending != -18468 {
		t.Fatalf("post entry %+v", en)
	}
	e.wantBalance("alice", 90000, 0, 90000)
	e.wantBalance("alice_eur", 9234, 0, 9234)
	e.wantBalance("fx_eur", -9234, 0, -9234)
	e.invariants()

	// Posting all of a hold takes what it reserved, whatever the rounding.
	h2 := e.do(convertHold("0.9234", 60, legs(3, 3)...)).Transfer
	e.do(postCmd(h2.ID, 0))
	e.wantBalance("alice_eur", 9237, 0, 9237)

	// An increase is held to the rate; an extension and a void work as on
	// any hold.
	h3 := e.do(convertHold("0.9234", 60, legs(20000, 18468)...)).Transfer
	if err := e.fail(increaseCmd(h3.ID, 0, parts(1000, 500)...), CodeInvalid); !strings.Contains(err.Message, "converts to") {
		t.Fatal(err)
	}
	inc := e.do(increaseCmd(h3.ID, 0, parts(1000, 923)...)).Transfer
	if inc.Postings[0].Amount != 21000 || inc.Postings[3].Amount != 19391 {
		t.Fatalf("increased %+v", inc.Postings)
	}
	if en := e.entry(inc.Entries[1].Seq); en.FX != nil || en.Lines[3].Currency != "EUR" || en.Lines[3].Pending != 923 {
		t.Fatalf("increase entry %+v", en)
	}
	e.do(extendCmd(h3.ID, 600))
	e.wantBalance("alice_eur", 9237, 19391, 9237)
	e.do(voidCmd(h3.ID))
	e.wantBalance("alice_eur", 9237, 0, 9237)
	e.wantBalance("fx_eur", -9237, 0, -9237)

	h4 := e.do(convertHold("0.9234", 5, legs(1000, 923)...)).Transfer
	e.clock.Advance(6 * time.Second)
	if n, err := e.l.ExpireDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired %d %v", n, err)
	}
	if tr, _ := e.l.Transfer(h4.ID); tr.State != StateExpired {
		t.Fatalf("hold %+v", tr)
	}
	e.wantBalance("alice_eur", 9237, 0, 9237)
	e.wantBalance("alice", 99997-10000, 0, 99997-10000)
	e.invariants()
}

// A reversal sends money back at the original rate. Explicit amounts must
// agree with it; what is left is sent back as it is.
func TestConversionReversal(t *testing.T) {
	e := newEnv(t, false)
	e.fxSetup()
	c := e.do(convert("0.9234", dr("alice", 10100), cr("fx_usd", 10000), cr("fees", 100),
		eur(dr("fx_eur", 9234)), eur(cr("alice_eur", 9234)))).Transfer

	// The fee alone is a single-currency reversal.
	fee := e.do(reverseCmd(c.ID, 0, PartialAmount{"alice", 100}, PartialAmount{"fees", 100})).Transfer
	if fee.FX != nil || fee.Currency != "USD" || len(fee.Postings) != 2 || fee.Postings[0].Currency != "" {
		t.Fatalf("fee reversal %+v", fee)
	}
	if e.entry(fee.Entries[0].Seq).FX != nil {
		t.Fatal("fee reversal entry has a rate")
	}
	if err := e.fail(reverseCmd(c.ID, 0, PartialAmount{"alice", 5000}, PartialAmount{"fx_usd", 5000},
		PartialAmount{"fx_eur", 1000}, PartialAmount{"alice_eur", 1000}), CodeInvalid); !strings.Contains(err.Message, "converts to") {
		t.Fatal(err)
	}
	// Half the euro side alone would undo half a conversion.
	if err := e.fail(reverseCmd(c.ID, 0, PartialAmount{"fx_eur", 4617}, PartialAmount{"alice_eur", 4617}), CodeInvalid); !strings.Contains(err.Message, "in both") {
		t.Fatal(err)
	}
	half := e.do(reverseCmd(c.ID, 0, PartialAmount{"alice", 5000}, PartialAmount{"fx_usd", 5000},
		PartialAmount{"fx_eur", 4617}, PartialAmount{"alice_eur", 4617})).Transfer
	if half.FX != c.FX || half.Postings[2].Currency != "EUR" || half.Postings[2].Direction != Credit {
		t.Fatalf("half reversal %+v", half)
	}
	if e.entry(half.Entries[0].Seq).FX != c.FX {
		t.Fatal("reversal entry lacks the rate")
	}
	rest := e.do(reverseCmd(c.ID, 0)).Transfer
	if rest.FX != c.FX || rest.Postings[0].Amount != 5000 || rest.Postings[3].Amount != 4617 {
		t.Fatalf("rest %+v", rest)
	}
	for _, id := range []string{"fx_usd", "fx_eur", "alice_eur", "fees"} {
		e.wantBalance(id, 0, 0, 0)
	}
	e.wantBalance("alice", 100000, 0, 100000)
	orig, _ := e.l.Transfer(c.ID)
	for _, p := range orig.Postings {
		if p.Reversed != p.Posted {
			t.Fatalf("not fully reversed: %+v", orig.Postings)
		}
	}
	e.invariants()
}

// Conversions in a batch, keyed results and refusals, and restarts from the
// log alone and from a snapshot.
func TestConversionSurvivesRestart(t *testing.T) {
	e := newEnv(t, true)
	e.fxSetup()
	ctx := context.Background()
	b, err := e.l.Do(ctx, batchCmd(move(dr("equity", 500), cr("alice", 500)), usdToEUR("0.9234", 10500, 9696),
		convertHold("0.9234", 600, dr("alice", 1000), cr("fx_usd", 1000), eur(dr("fx_eur", 923)), eur(cr("alice_eur", 923)))), key("s", "batch", "batch"))
	if err != nil || len(b.Transfers) != 3 || b.Transfers[1].FX == nil || b.Transfers[2].FX == nil {
		t.Fatalf("batch %+v %v", b, err)
	}
	// One transfer off the rate refuses the batch.
	_, err = e.l.Do(ctx, batchCmd(move(dr("equity", 1), cr("alice", 1)), usdToEUR("0.9234", 100, 50)), key("s", "bad", "bad"))
	if le, _ := err.(*Error); le == nil || le.Code != CodeInvalid || le.Item == nil || *le.Item != 1 {
		t.Fatalf("bad batch: %v", err)
	}
	conv, err := e.l.Do(ctx, usdToEUR("0.9234", 2000, 1847), key("s", "conv", "conv"))
	if err != nil {
		t.Fatal(err)
	}
	e.do(reverseCmd(conv.Transfer.ID, 0, PartialAmount{"alice", 1000}, PartialAmount{"fx_usd", 1000},
		PartialAmount{"fx_eur", 923}, PartialAmount{"alice_eur", 923}))
	e.do(postCmd(b.Transfers[2].ID, 0))
	e.invariants()
	e.restart()
	again, err := e.l.Do(ctx, usdToEUR("0.9234", 2000, 1847), key("s", "conv", "conv"))
	if err != nil || !again.Replayed || !reflect.DeepEqual(again.Transfer, conv.Transfer) {
		t.Fatalf("replay %+v %v", again.Transfer, err)
	}
	if _, err := e.l.Do(ctx, batchCmd(move(dr("equity", 1), cr("alice", 1)), usdToEUR("0.9234", 100, 50)), key("s", "bad", "bad")); codeOf(err) != CodeInvalid {
		t.Fatalf("refusal replay: %v", err)
	}
	e.invariants()
	dir := e.dir
	e.l.Close()
	rep, err := Verify(dir)
	if err != nil || rep.Status != StatusSound {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	fromLog := openAt(t, copyDir(t, dir, false), e.clock, nil)
	fromSnap := openAt(t, copyDir(t, dir, true), e.clock, nil)
	sameState(t, "conversions", fromSnap, fromLog)
	tr, err := fromSnap.Transfer(conv.Transfer.ID)
	if err != nil || !reflect.DeepEqual(tr.FX, conv.Transfer.FX) || len(tr.Reversals) != 1 {
		t.Fatalf("from snapshot %+v %v", tr, err)
	}
}

// The checker refuses currencies and rates that the commands could not have
// written.
func TestCheckerFindsFXDamage(t *testing.T) {
	setTransfer := func(st *state, id string, f func(*Transfer)) {
		c := st.transfers[id].clone()
		f(c)
		st.transfers[id] = c
	}
	entry := func(st *state, id string, k int) *Entry {
		e := &st.entries[st.transfers[id].Entries[k].Seq-1]
		e.Lines = append([]Line(nil), e.Lines...)
		return e
	}
	for _, c := range []struct {
		name  string
		apply func(st *state, conv, hold, plain string)
		want  string
	}{
		{"rate removed from the entry", func(st *state, conv, _, _ string) { entry(st, conv, 0).FX = nil }, "is in USD and EUR but has no rate"},
		{"rate on a post", func(st *state, _, hold, _ string) {
			entry(st, hold, 1).FX = st.transfers[hold].FX
		}, "is a post_pending entry with a rate"},
		{"rate on a single-currency entry", func(st *state, _, _, plain string) {
			entry(st, plain, 0).FX = &FX{Base: "USD", Quote: "EUR", Rate: "1", Source: "x", QuotedAt: quoted}
		}, "has a rate but all its lines are in USD"},
		{"rate changed on the entry", func(st *state, conv, _, _ string) {
			f := *st.transfers[conv].FX
			f.Rate = "0.5"
			entry(st, conv, 0).FX = &f
		}, "converts to 50.0000 EUR, not 92.34 EUR"},
		{"rate for another pair", func(st *state, conv, _, _ string) {
			f := *st.transfers[conv].FX
			f.Base, f.Quote = "EUR", "USD"
			entry(st, conv, 0).FX = &f
		}, "but its rate is for EUR to USD"},
		{"transfer rate differs", func(st *state, conv, _, _ string) {
			setTransfer(st, conv, func(tr *Transfer) { f := *tr.FX; f.Source = "other"; tr.FX = &f })
		}, "but the entry that created it has"},
		{"rate on a single-currency transfer", func(st *state, _, _, plain string) {
			setTransfer(st, plain, func(tr *Transfer) { tr.FX = &FX{Base: "USD", Quote: "EUR", Rate: "1", Source: "x", QuotedAt: quoted} })
		}, "has a rate but all its postings are in USD"},
		{"posting in a third currency", func(st *state, conv, _, _ string) {
			setTransfer(st, conv, func(tr *Transfer) { tr.Postings[2].Currency = "GBP" })
		}, "posting to fx_eur is in GBP, which is not the transfer's other currency"},
		{"posting currency dropped", func(st *state, conv, _, _ string) {
			setTransfer(st, conv, func(tr *Transfer) { tr.Postings[3].Currency = "" })
		}, `posting to alice_eur is in USD, but the account holds "EUR"`},
		{"line in a third currency", func(st *state, conv, _, _ string) { entry(st, conv, 0).Lines[2].Currency = "GBP" }, "has lines in more than two currencies"},
		{"line spelling out its entry's currency", func(st *state, conv, _, _ string) { entry(st, conv, 0).Lines[0].Currency = "USD" }, "spells out its own currency"},
		{"balanced only across currencies", func(st *state, conv, _, _ string) {
			e := entry(st, conv, 0)
			e.Lines[1].Posted++
			e.Lines[2].Posted++
		}, "does not balance"},
		{"exchange flag lost", func(st *state, conv, _, _ string) { st.accounts["fx_usd"].Exchange = false }, "through exchange accounts in both"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, false)
			e.fxSetup()
			conv := e.do(usdToEUR("0.9234", 10000, 9234)).Transfer.ID
			hold := e.do(convertHold("0.9234", 60, dr("alice", 1000), cr("fx_usd", 1000), eur(dr("fx_eur", 923)), eur(cr("alice_eur", 923)))).Transfer.ID
			e.do(postCmd(hold, 0))
			plain := e.do(move(dr("alice", 5), cr("merchant", 5))).Transfer.ID
			e.invariants()
			e.l.mu.Lock()
			c.apply(e.l.st, conv, hold, plain)
			e.l.mu.Unlock()
			if v := strings.Join(e.l.CheckInvariants(), "; "); !strings.Contains(v, c.want) {
				t.Fatalf("violations %q, want %q", v, c.want)
			}
		})
	}
}

// A snapshot round-trips rates and posting currencies, and refuses rates
// and currencies the ledger would not have written.
func TestSnapshotFX(t *testing.T) {
	f := &FX{Base: "USD", Quote: "EUR", Rate: "0.9234", Source: "ecb", QuotedAt: quoted.UTC()}
	tr := &Transfer{ID: "t", State: StatePosted, Currency: "USD", FX: f, Entries: []EntryRef{{Seq: 1, Kind: KindPosted}},
		Postings: []Posting{{Account: "a", Direction: Debit, Amount: 1}, {Account: "b", Direction: Credit, Currency: "EUR", Amount: 1}}}
	read := func(p []byte) (*Transfer, error) {
		r := &snapReader{ver: snapVersion, p: p, ids: map[string]string{"a": "a", "b": "b"}, syms: map[string]string{}}
		got := r.transfer()
		return got, r.err
	}
	enc := func(tr *Transfer) []byte {
		w := &snapWriter{ver: snapVersion}
		w.transfer(tr)
		return w.buf
	}
	got, err := read(enc(tr))
	if err != nil || !reflect.DeepEqual(got, tr) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for name, damage := range map[string]func(*Transfer){
		"zero rate":            func(tr *Transfer) { g := *f; g.Rate = "0"; tr.FX = &g },
		"rate with exponent":   func(tr *Transfer) { g := *f; g.Rate = "1e2"; tr.FX = &g },
		"same currencies":      func(tr *Transfer) { g := *f; g.Quote = "USD"; tr.FX = &g },
		"unknown quote":        func(tr *Transfer) { g := *f; g.Quote = "XAU"; tr.FX = &g },
		"no quote time":        func(tr *Transfer) { g := *f; g.QuotedAt = time.Time{}; tr.FX = &g },
		"no rate":              func(tr *Transfer) { tr.FX = nil },
		"rate, one currency":   func(tr *Transfer) { tr.Postings[1].Currency = "" },
		"unknown currency":     func(tr *Transfer) { tr.Postings[1].Currency = "XAU" },
		"own currency spelled": func(tr *Transfer) { tr.Postings[0].Currency = "USD" },
	} {
		c := tr.clone()
		damage(c)
		if _, err := read(enc(c)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVersionSixSnapshotsLoad(t *testing.T) {
	old := snapWriteVersion
	snapWriteVersion = 6
	t.Cleanup(func() { snapWriteVersion = old })
	dir, clock := writeSmallSnapshot(t, 6)
	snapWriteVersion = old
	fromSnap := openAt(t, copyDir(t, dir, true), clock, nil)
	full := openAt(t, copyDir(t, dir, false), clock, nil)
	sameState(t, "version 6", fromSnap, full)
}
