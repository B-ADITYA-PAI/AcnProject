package zipf

import (
	"math"
	"math/rand"
	"slices"
)

type Zipf struct {
	cdf []float64
}

func New(n int, alpha float64) *Zipf {
	cdf := make([]float64, n)
	sum := 0.0
	for k := 1; k <= n; k++ {
		sum += math.Pow(float64(k), -alpha)
		cdf[k-1] = sum
	}
	for i := range cdf {
		cdf[i] /= sum
	}
	// ensure the last value is exactly 1.0 to avoid precision issues
	cdf[n-1] = 1.0
	return &Zipf{cdf: cdf}
}

// Sample returns a value between 1 and n
func (z *Zipf) Sample(r *rand.Rand) int {
	val := r.Float64()
	i, _ := slices.BinarySearch(z.cdf, val)
	if i >= len(z.cdf) {
		i = len(z.cdf) - 1
	}
	return i + 1
}
