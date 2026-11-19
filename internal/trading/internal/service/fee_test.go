package service

import (
	"math"
	"testing"
)

func TestFeeNoOverflow(t *testing.T) {
	if got := fee(math.MaxInt64, 10_000); got != math.MaxInt64 {
		t.Fatalf("fee(max, 100%%) = %d", got)
	}
	if got := fee(1_000_000, 25); got != 2_500 {
		t.Fatalf("fee = %d", got)
	}
}
