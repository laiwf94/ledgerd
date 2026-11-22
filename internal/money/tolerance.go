package money

import "math/bits"

// MaxToleranceBasisPoints is the largest percentage tolerance, 10%.
const MaxToleranceBasisPoints = 1000

// Tolerance returns how far an amount matched against amount may differ
// from it: the larger of abs (minor units) and bp basis points of |amount|,
// rounded down. abs and bp must not be negative. Reconciliation and the
// ledger's check of a saved run use this one rule, so a near-match the
// matcher proposes is always one the ledger accepts.
func Tolerance(amount, abs, bp int64) int64 {
	if bp <= 0 {
		return abs
	}
	u := uint64(amount)
	if amount < 0 {
		u = -u // also right for the smallest int64
	}
	hi, lo := bits.Mul64(u, uint64(bp))
	q, _ := bits.Div64(hi, lo, 10_000) // hi < 10,000 since bp <= 2^63
	pct := int64(q)
	if q > 1<<63-1 {
		pct = 1<<63 - 1
	}
	return max(abs, pct)
}
