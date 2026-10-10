package store

import "testing"

// roundCents must round half away from zero. The previous int64(v*100+0.5)
// truncated toward zero, so -600 became -599.99 (a lost cent on every
// negative balance/margin).
func TestRoundCents_NegativesAndPositives(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{-600, -600}, {600, 600}, {-12.345, -12.35}, {12.345, 12.35},
		{-0.004, 0}, {0.004, 0}, {-0.005, -0.01}, {0.005, 0.01}, {0, 0},
		{7.2, 7.2}, {-7.2, -7.2}, {19.2, 19.2},
	}
	for _, c := range cases {
		if got := roundCents(c.in); got != c.want {
			t.Fatalf("roundCents(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}