// Package money holds the currency table and overflow-checked arithmetic on
// amounts in minor units (cents, pence, yen). Amounts are int64 and never
// floating point: binary floats cannot represent 0.10 exactly, and a ledger
// that rounds is a ledger that does not balance.
package money

import (
	"errors"
	"math"
	"strconv"
)

// Currency is an ISO 4217 currency with its minor-unit exponent: an amount
// of 1234 in a currency with exponent 2 is 12.34.
type Currency struct {
	Code     string
	Exponent int
}

// Lookup returns the currency for an upper-case ISO 4217 alphabetic code.
func Lookup(code string) (Currency, bool) {
	e, ok := exponents[code]
	if !ok {
		return Currency{}, false
	}
	return Currency{Code: code, Exponent: e}, true
}

// Add returns a+b, or false if the sum does not fit in an int64.
func Add(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}

// Sub returns a-b, or false if the difference does not fit in an int64.
func Sub(a, b int64) (int64, bool) {
	d := a - b
	if (b > 0 && d > a) || (b < 0 && d < a) {
		return 0, false
	}
	return d, true
}

// ErrSyntax is returned by ParseDecimal for anything that is not a plain
// decimal number with at most exponent fraction digits.
var ErrSyntax = errors.New("amount must be a decimal number such as 12.34 or -5")

// ErrRange is returned by ParseDecimal when the amount does not fit in an int64.
var ErrRange = errors.New("amount is too large")

// ParseDecimal converts a decimal string in major units ("12.34", "-0.5",
// "+7") to minor units for a currency with the given exponent. It is strict:
// no spaces, thousands separators, currency symbols or exponents, and never
// more fraction digits than the currency has, because silently rounding money
// is worse than rejecting it.
func ParseDecimal(s string, exponent int) (int64, error) {
	if exponent < 0 || exponent > 18 {
		return 0, ErrSyntax
	}
	neg := false
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	whole, frac := s, ""
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			whole, frac = s[:i], s[i+1:]
			if frac == "" {
				return 0, ErrSyntax
			}
			break
		}
	}
	if whole == "" || len(frac) > exponent || !digits(whole) || !digits(frac) {
		return 0, ErrSyntax
	}
	// Accumulate as a negative number so math.MinInt64 is representable.
	var v int64
	step := func(d byte) error {
		if v < math.MinInt64/10 {
			return ErrRange
		}
		v *= 10
		if v < math.MinInt64+int64(d-'0') {
			return ErrRange
		}
		v -= int64(d - '0')
		return nil
	}
	for i := 0; i < len(whole); i++ {
		if err := step(whole[i]); err != nil {
			return 0, err
		}
	}
	for i := 0; i < exponent; i++ {
		d := byte('0')
		if i < len(frac) {
			d = frac[i]
		}
		if err := step(d); err != nil {
			return 0, err
		}
	}
	if neg {
		return v, nil
	}
	if v == math.MinInt64 {
		return 0, ErrRange
	}
	return -v, nil
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// FormatDecimal renders minor units as a decimal string in major units, the
// inverse of ParseDecimal: FormatDecimal(-1250, 2) == "-12.50".
func FormatDecimal(v int64, exponent int) string {
	neg := v < 0
	var u uint64
	if neg {
		u = uint64(-(v + 1)) + 1 // safe for math.MinInt64
	} else {
		u = uint64(v)
	}
	s := strconv.FormatUint(u, 10)
	if exponent > 0 {
		for len(s) <= exponent {
			s = "0" + s
		}
		s = s[:len(s)-exponent] + "." + s[len(s)-exponent:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// exponents maps active ISO 4217 alphabetic codes to their minor units.
// Codes without a minor unit in the standard (precious metals, testing
// codes, SDR) are deliberately absent.
var exponents = map[string]int{
	"AED": 2, "AFN": 2, "ALL": 2, "AMD": 2, "AOA": 2, "ARS": 2, "AUD": 2, "AWG": 2, "AZN": 2,
	"BAM": 2, "BBD": 2, "BDT": 2, "BHD": 3, "BIF": 0, "BMD": 2, "BND": 2, "BOB": 2, "BOV": 2,
	"BRL": 2, "BSD": 2, "BTN": 2, "BWP": 2, "BYN": 2, "BZD": 2,
	"CAD": 2, "CDF": 2, "CHE": 2, "CHF": 2, "CHW": 2, "CLF": 4, "CLP": 0, "CNY": 2, "COP": 2,
	"COU": 2, "CRC": 2, "CUP": 2, "CVE": 2, "CZK": 2,
	"DJF": 0, "DKK": 2, "DOP": 2, "DZD": 2,
	"EGP": 2, "ERN": 2, "ETB": 2, "EUR": 2,
	"FJD": 2, "FKP": 2,
	"GBP": 2, "GEL": 2, "GHS": 2, "GIP": 2, "GMD": 2, "GNF": 0, "GTQ": 2, "GYD": 2,
	"HKD": 2, "HNL": 2, "HTG": 2, "HUF": 2,
	"IDR": 2, "ILS": 2, "INR": 2, "IQD": 3, "IRR": 2, "ISK": 0,
	"JMD": 2, "JOD": 3, "JPY": 0,
	"KES": 2, "KGS": 2, "KHR": 2, "KMF": 0, "KPW": 2, "KRW": 0, "KWD": 3, "KYD": 2, "KZT": 2,
	"LAK": 2, "LBP": 2, "LKR": 2, "LRD": 2, "LSL": 2, "LYD": 3,
	"MAD": 2, "MDL": 2, "MGA": 2, "MKD": 2, "MMK": 2, "MNT": 2, "MOP": 2, "MRU": 2, "MUR": 2,
	"MVR": 2, "MWK": 2, "MXN": 2, "MXV": 2, "MYR": 2, "MZN": 2,
	"NAD": 2, "NGN": 2, "NIO": 2, "NOK": 2, "NPR": 2, "NZD": 2,
	"OMR": 3,
	"PAB": 2, "PEN": 2, "PGK": 2, "PHP": 2, "PKR": 2, "PLN": 2, "PYG": 0,
	"QAR": 2,
	"RON": 2, "RSD": 2, "RUB": 2, "RWF": 0,
	"SAR": 2, "SBD": 2, "SCR": 2, "SDG": 2, "SEK": 2, "SGD": 2, "SHP": 2, "SLE": 2, "SOS": 2,
	"SRD": 2, "SSP": 2, "STN": 2, "SVC": 2, "SYP": 2, "SZL": 2,
	"THB": 2, "TJS": 2, "TMT": 2, "TND": 3, "TOP": 2, "TRY": 2, "TTD": 2, "TWD": 2, "TZS": 2,
	"UAH": 2, "UGX": 0, "USD": 2, "USN": 2, "UYI": 0, "UYU": 2, "UYW": 4, "UZS": 2,
	"VED": 2, "VES": 2, "VND": 0, "VUV": 0,
	"WST": 2,
	"XAF": 0, "XCD": 2, "XCG": 2, "XOF": 0, "XPF": 0,
	"YER": 2,
	"ZAR": 2, "ZMW": 2, "ZWG": 2,
}
