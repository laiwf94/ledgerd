package money

import (
	"math"
	"math/big"
	"testing"
)

func TestParseRate(t *testing.T) {
	for _, s := range []string{"0.9234", "1", "151", "1.0", "0.000000001", "123456789012345678901234567890", "0.00000000000000000000000000001"} {
		if _, err := ParseRate(s); err != nil {
			t.Errorf("ParseRate(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", "0", "0.0", "000.000", "-1", "+1", "1e3", "1/3", ".5", "5.", "1.2.3", " 1", "1 ", "1,5", "NaN", "Inf",
		"1234567890123456789012345678901", "0.000000000000000000000000000001"} {
		if r, err := ParseRate(s); err == nil {
			t.Errorf("ParseRate(%q) = %v, want an error", s, r)
		}
	}
	r, _ := ParseRate("0.9234")
	if r.String() != "4617/5000" {
		t.Errorf("0.9234 parsed as %s", r)
	}
}

func TestConverts(t *testing.T) {
	rate := func(s string) *big.Rat {
		r, err := ParseRate(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cases := []struct {
		base     int64
		baseExp  int
		quote    int64
		quoteExp int
		rate     string
		ok       bool
	}{
		{10000, 2, 9234, 2, "0.9234", true},     // 100.00 USD -> 92.34 EUR exactly
		{10001, 2, 9235, 2, "0.9234", true},     // 92.349... rounded up
		{10001, 2, 9234, 2, "0.9234", true},     // ... or down
		{10001, 2, 9236, 2, "0.9234", false},    // two units off
		{10001, 2, 9233, 2, "0.9234", false},    // off the other way
		{10000, 2, 1510000, 0, "151", false},    // USD to JPY needs the exponent
		{10000, 2, 15100, 0, "151", true},       // 100.00 USD -> 15,100 JPY
		{15100, 0, 10000, 2, "0.0066225", true}, // 15,100 JPY -> 100.00 USD (99.99975)
		{1000, 2, 3760, 3, "0.376", true},       // USD (2) to KWD-like exponent 3: 10.00 -> 3.760
		{-500, 2, -460, 2, "0.92", true},        // signs agree
		{500, 2, -460, 2, "0.92", false},        // signs differ
		{0, 2, 0, 2, "0.92", true},
		{0, 2, 1, 2, "0.92", false},
		{1, 2, 0, 2, "0.92", false},
		{math.MaxInt64, 2, math.MaxInt64, 2, "1", true},
		{math.MaxInt64, 2, math.MaxInt64 - 1, 2, "1", false},
	}
	for _, c := range cases {
		if got := Converts(c.base, c.baseExp, c.quote, c.quoteExp, rate(c.rate)); got != c.ok {
			t.Errorf("Converts(%d e%d, %d e%d, %s) = %v, want %v", c.base, c.baseExp, c.quote, c.quoteExp, c.rate, got, c.ok)
		}
	}
}
