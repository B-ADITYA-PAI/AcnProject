package zipf

import (
	"math"
	"math/rand"
	"testing"
)

func TestZipf(t *testing.T) {
	n := 1000
	for _, alpha := range []float64{0.8, 1.0, 1.2} {
		z := New(n, alpha)
		counts := make(map[int]int)
		iters := 1000000
		
		r := rand.New(rand.NewSource(42))
		for i := 0; i < iters; i++ {
			counts[z.Sample(r)]++
		}
		
		// Expected share for rank 1 is 1 / (Sum(1/k^alpha))
		sum := 0.0
		for k := 1; k <= n; k++ {
			sum += math.Pow(float64(k), -alpha)
		}
		
		for rank := 1; rank <= 3; rank++ {
			expectedShare := math.Pow(float64(rank), -alpha) / sum
			actualShare := float64(counts[rank]) / float64(iters)
			diff := math.Abs(expectedShare - actualShare) / expectedShare
			
			if diff > 0.02 {
				t.Errorf("alpha=%.1f rank=%d: expected %.4f, got %.4f (diff=%.2f%%)", alpha, rank, expectedShare, actualShare, diff*100)
			}
		}
	}
}
