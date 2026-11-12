package ledger

import (
	"fmt"

	"github.com/laiwf94/ledgerd/internal/money"
)

// Multi-currency transfers. A transfer may have postings in two currencies:
// its own (the FX block's Base) and one other (Quote). It balances in each
// currency on its own, so no currency's totals ever move because of another's,
// and the conversion itself happens on exchange accounts: the ledger's
// position in each currency. A USD to EUR conversion debits the customer's
// USD account and credits the USD exchange account, then debits the EUR
// exchange account and credits the customer's EUR account, all in one entry.
//
// The rate is checked against what moves through the exchange accounts, not
// against every posting, so a fee or a spread can be taken in either currency
// on another account in the same entry. In each currency the exchange
// accounts' net movement (credits minus debits) must be non-zero, the two
// nets must have opposite signs (the ledger takes in one currency and pays
// out the other), and the quote net must be the base net converted at the
// rate, rounded either way to a whole minor unit.

// conversionAmount picks the amount a line moves for the rate check: what
// it reserves for a hold or an increase, what it posts otherwise.
func conversionAmount(kind EntryKind) func(*Line) int64 {
	if kind == KindPending || kind == KindIncreasePending {
		return func(ln *Line) int64 { return ln.Pending }
	}
	return func(ln *Line) int64 { return ln.Posted }
}

// conversionProblem checks the lines of an entry in currency against the
// rate in fx; exchange says whether lines[i] is on an exchange account. When
// required is false, lines that move nothing through exchange accounts in
// either currency pass: a partial post or reversal that leaves the
// conversion alone. It returns "" when the lines agree with the rate.
func conversionProblem(fx *FX, currency string, lines []Line, amount func(*Line) int64, exchange func(int) bool, required bool) string {
	var nets [2]int64
	for i := range lines {
		ln := &lines[i]
		k := 0
		switch lineCurrency(currency, ln) {
		case fx.Base:
		case fx.Quote:
			k = 1
		default:
			return fmt.Sprintf("account %s is in %s, which is neither %s nor %s", ln.Account, lineCurrency(currency, ln), fx.Base, fx.Quote)
		}
		if !exchange(i) {
			continue
		}
		a := amount(ln)
		if ln.Direction == Debit {
			a = -a
		}
		n, ok := money.Add(nets[k], a)
		if !ok {
			return fmt.Sprintf("the amounts through exchange accounts in %s add up to more than an int64", []string{fx.Base, fx.Quote}[k])
		}
		nets[k] = n
	}
	switch {
	case nets[0] == 0 && nets[1] == 0 && !required:
		return ""
	case nets[0] == 0 || nets[1] == 0:
		return fmt.Sprintf("a conversion moves money through exchange accounts in both %s and %s; this moves %d and %d",
			fx.Base, fx.Quote, nets[0], nets[1])
	case (nets[0] > 0) == (nets[1] > 0):
		return fmt.Sprintf("the exchange accounts take in both %s and %s; a conversion takes in one and pays out the other", fx.Base, fx.Quote)
	}
	rate, err := money.ParseRate(fx.Rate)
	if err != nil {
		return fmt.Sprintf("fx rate %q: %v", fx.Rate, err)
	}
	base, quote := absInt(nets[0]), absInt(nets[1])
	bc, _ := money.Lookup(fx.Base)
	qc, _ := money.Lookup(fx.Quote)
	if !money.Converts(base, bc.Exponent, quote, qc.Exponent, rate) {
		exact := money.Convert(base, bc.Exponent, 0, rate) // in major units
		return fmt.Sprintf("at %s %s per %s, %s %s through exchange accounts converts to %s %s, not %s %s",
			fx.Rate, fx.Quote, fx.Base, money.FormatDecimal(base, bc.Exponent), fx.Base,
			exact.FloatString(qc.Exponent+2), fx.Quote, money.FormatDecimal(quote, qc.Exponent), fx.Quote)
	}
	return ""
}

// absInt is |x|; nets never reach math.MinInt64, since each is a sum of
// amounts on one side minus amounts on the other that money.Add kept in
// range, but if one did the result stays negative and fails Converts.
func absInt(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
