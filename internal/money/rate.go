package money

import (
	"errors"
	"math/big"
)

// MaxRateDigits is how many digits an exchange rate may have, before and
// after the point together.
const MaxRateDigits = 30

// ErrRate is returned by ParseRate for anything that is not a positive plain
// decimal number.
var ErrRate = errors.New("rate must be a positive decimal number such as 0.9234, with at most 30 digits")

// ParseRate reads an exchange rate: how many major units of the quote
// currency one major unit of the base currency buys. It is a plain decimal
// ("0.9234", "151", "1.0"), never a fraction, an exponent or a float, so the
// rate a ledger records is exactly the one the client sent.
func ParseRate(s string) (*big.Rat, error) {
	n, point, zero := 0, false, true
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			n++
			zero = zero && c == '0'
		case c == '.' && !point && i > 0 && i < len(s)-1:
			point = true
		default:
			return nil, ErrRate
		}
	}
	if n == 0 || n > MaxRateDigits || zero {
		return nil, ErrRate
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, ErrRate
	}
	return r, nil
}

// Converts reports whether quote minor units of a currency with exponent
// quoteExp are base minor units of a currency with exponent baseExp converted
// at rate, rounded either way: quote is base × rate rescaled to the quote
// currency's minor unit, cut down or rounded up to a whole unit. Any rounding
// rule a client uses (half up, half even, down, up) passes; anything further
// off does not. base and quote must have the same sign.
func Converts(base int64, baseExp int, quote int64, quoteExp int, rate *big.Rat) bool {
	if base == 0 || quote == 0 {
		return base == quote
	}
	if (base < 0) != (quote < 0) {
		return false
	}
	x := Convert(base, baseExp, quoteExp, rate)
	d := x.Sub(x, new(big.Rat).SetInt64(quote))
	return d.Abs(d).Cmp(big.NewRat(1, 1)) < 0
}

// Convert returns base minor units of a currency with exponent baseExp
// converted at rate, exactly, in minor units of a currency with exponent
// quoteExp.
func Convert(base int64, baseExp, quoteExp int, rate *big.Rat) *big.Rat {
	x := new(big.Rat).Mul(new(big.Rat).SetInt64(base), rate)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(quoteExp-baseExp))), nil)
	if quoteExp >= baseExp {
		return x.Mul(x, new(big.Rat).SetInt(scale))
	}
	return x.Quo(x, new(big.Rat).SetInt(scale))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
