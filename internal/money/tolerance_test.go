package money

import (
	"math"
	"math/big"
	"testing"
)

func TestTolerance(t *testing.T) {
	for _, tc := range []struct {
		amount, abs, bp, want int64
	}{
		{10_000, 0, 0, 0},
		{10_000, 5, 0, 5},
		{10_000, 0, 50, 50},  // 0.5% of 100.00 is 0.50
		{10_000, 80, 50, 80}, // the larger of the two
		{-10_000, 0, 50, 50},
		{199, 0, 50, 0}, // 0.995 rounds down
		{200, 0, 50, 1},
		{math.MinInt64, 0, MaxToleranceBasisPoints, 922337203685477580},
		{math.MaxInt64, 0, MaxToleranceBasisPoints, 922337203685477580},
		{0, 3, 1000, 3},
	} {
		if got := Tolerance(tc.amount, tc.abs, tc.bp); got != tc.want {
			t.Errorf("Tolerance(%d, %d, %d) = %d, want %d", tc.amount, tc.abs, tc.bp, got, tc.want)
		}
	}
}

// The percentage is exact for every amount: it agrees with big.Int.
func TestToleranceIsExact(t *testing.T) {
	amounts := []int64{1, 7, 9999, 10_001, 123_456_789, math.MaxInt64 / 3, math.MaxInt64 - 1, math.MaxInt64, math.MinInt64, math.MinInt64 + 1}
	for _, a := range amounts {
		for _, bp := range []int64{1, 3, 50, 999, MaxToleranceBasisPoints} {
			want := new(big.Int).Abs(big.NewInt(a))
			want.Mul(want, big.NewInt(bp)).Quo(want, big.NewInt(10_000))
			if got := Tolerance(a, 0, bp); !want.IsInt64() || got != want.Int64() {
				t.Errorf("Tolerance(%d, 0, %d) = %d, want %s", a, bp, got, want)
			}
		}
	}
}
