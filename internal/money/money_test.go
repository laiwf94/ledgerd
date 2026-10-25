package money

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func TestLookup(t *testing.T) {
	for code, want := range map[string]int{"USD": 2, "JPY": 0, "KWD": 3, "CLF": 4, "EUR": 2} {
		c, ok := Lookup(code)
		if !ok || c.Code != code || c.Exponent != want {
			t.Errorf("Lookup(%s) = %+v, %v", code, c, ok)
		}
	}
	for _, code := range []string{"", "usd", "XAU", "XXX", "US", "USDD"} {
		if _, ok := Lookup(code); ok {
			t.Errorf("Lookup(%q) should fail", code)
		}
	}
	for code, e := range exponents {
		if len(code) != 3 || e < 0 || e > 4 {
			t.Errorf("bad table entry %q: %d", code, e)
		}
	}
}

func TestAddSubMatchBigInt(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	edge := []int64{0, 1, -1, math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1}
	pick := func() int64 {
		if r.Intn(3) == 0 {
			return edge[r.Intn(len(edge))]
		}
		return r.Int63() - r.Int63()
	}
	fits := func(b *big.Int) bool { return b.IsInt64() }
	for i := 0; i < 100_000; i++ {
		a, b := pick(), pick()
		want := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
		got, ok := Add(a, b)
		if ok != fits(want) || (ok && got != want.Int64()) {
			t.Fatalf("Add(%d,%d) = %d,%v want %s", a, b, got, ok, want)
		}
		want = new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
		got, ok = Sub(a, b)
		if ok != fits(want) || (ok && got != want.Int64()) {
			t.Fatalf("Sub(%d,%d) = %d,%v want %s", a, b, got, ok, want)
		}
	}
}

func TestParseDecimal(t *testing.T) {
	ok := []struct {
		s    string
		exp  int
		want int64
	}{
		{"12.34", 2, 1234}, {"12.3", 2, 1230}, {"12", 2, 1200}, {"-0.01", 2, -1}, {"+7", 2, 700},
		{"0", 0, 0}, {"-0", 2, 0}, {"500", 0, 500}, {"1.234", 3, 1234}, {"0.0001", 4, 1},
		{"92233720368547758.07", 2, math.MaxInt64}, {"-92233720368547758.08", 2, math.MinInt64},
		{"9223372036854775807", 0, math.MaxInt64}, {"007.50", 2, 750},
	}
	for _, c := range ok {
		got, err := ParseDecimal(c.s, c.exp)
		if err != nil || got != c.want {
			t.Errorf("ParseDecimal(%q,%d) = %d, %v; want %d", c.s, c.exp, got, err, c.want)
		}
	}
	bad := []struct {
		s   string
		exp int
		err error
	}{
		{"", 2, ErrSyntax}, {"-", 2, ErrSyntax}, {".5", 2, ErrSyntax}, {"5.", 2, ErrSyntax},
		{"1.234", 2, ErrSyntax}, {"1.5", 0, ErrSyntax}, {"1,000", 2, ErrSyntax}, {" 1", 2, ErrSyntax},
		{"1e3", 2, ErrSyntax}, {"$1", 2, ErrSyntax}, {"--1", 2, ErrSyntax}, {"1.2.3", 2, ErrSyntax},
		{"NaN", 2, ErrSyntax}, {"1", -1, ErrSyntax}, {"1", 19, ErrSyntax},
		{"92233720368547758.08", 2, ErrRange}, {"-92233720368547758.09", 2, ErrRange},
		{"99999999999999999999", 0, ErrRange},
	}
	for _, c := range bad {
		if _, err := ParseDecimal(c.s, c.exp); !errors.Is(err, c.err) {
			t.Errorf("ParseDecimal(%q,%d) err = %v, want %v", c.s, c.exp, err, c.err)
		}
	}
}

func TestFormatDecimal(t *testing.T) {
	cases := []struct {
		v    int64
		exp  int
		want string
	}{
		{1234, 2, "12.34"}, {-1250, 2, "-12.50"}, {5, 2, "0.05"}, {0, 2, "0.00"}, {-1, 3, "-0.001"},
		{500, 0, "500"}, {math.MinInt64, 2, "-92233720368547758.08"}, {math.MaxInt64, 0, "9223372036854775807"},
	}
	for _, c := range cases {
		if got := FormatDecimal(c.v, c.exp); got != c.want {
			t.Errorf("FormatDecimal(%d,%d) = %q, want %q", c.v, c.exp, got, c.want)
		}
	}
}

func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"12.34", "-0.01", "+7", "92233720368547758.07", "1.", ".1", "1e5", ""} {
		f.Add(s, 2)
	}
	f.Fuzz(func(t *testing.T, s string, exp int) {
		exp = ((exp % 5) + 5) % 5
		v, err := ParseDecimal(s, exp)
		if err != nil {
			return
		}
		// Anything accepted round-trips through the canonical form.
		back, err := ParseDecimal(FormatDecimal(v, exp), exp)
		if err != nil || back != v {
			t.Fatalf("round trip %q -> %d -> %q -> %d, %v", s, v, FormatDecimal(v, exp), back, err)
		}
	})
}

func BenchmarkParseDecimal(b *testing.B) {
	for b.Loop() {
		if _, err := ParseDecimal("123456.78", 2); err != nil {
			b.Fatal(err)
		}
	}
}
