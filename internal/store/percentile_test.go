package store

import "testing"

func TestPercentile(t *testing.T) {
	cases := []struct {
		vals []float64
		p    float64
		want float64
	}{
		{[]float64{7.8, 45.1}, 0.50, 7.8},   // N=2 的中位数取较小者
		{[]float64{7.8, 45.1}, 0.95, 45.1},  // 曾经的 bug：截断后退化成 7.8
		{[]float64{1, 2, 3, 4, 5}, 0.50, 3},
		{[]float64{1, 2, 3, 4, 5}, 0.95, 5},
		{[]float64{42}, 0.95, 42},
		{nil, 0.50, 0},
	}
	for _, c := range cases {
		if got := percentile(c.vals, c.p); got != c.want {
			t.Errorf("percentile(%v, %.2f) = %v, 期望 %v", c.vals, c.p, got, c.want)
		}
	}
}
