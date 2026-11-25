package policy

import (
	"fmt"
	"testing"

	"acnlb/internal/pool"
)

func backends(n int) []*pool.Backend {
	out := make([]*pool.Backend, n)
	for i := range out {
		out[i] = pool.NewBackend(i+1, "h", 10000)
	}
	return out
}

func keys(n int) []string {
	ks := make([]string, n)
	for i := range ks {
		ks[i] = fmt.Sprintf("10.10.%d.%d", i%4+1, i/4%250+1)
	}
	return ks
}

func TestNewRejectsUnknown(t *testing.T) {
	if _, err := New("nope"); err == nil {
		t.Fatal("expected error")
	}
	for _, n := range Names() {
		if _, err := New(n); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
}

func TestRoundRobinIsEven(t *testing.T) {
	p, _ := New("rr")
	up := backends(5)
	count := map[string]int{}
	for i := 0; i < 1000; i++ {
		count[p.Pick("x", up, 1).ID]++
	}
	for id, c := range count {
		if c != 200 {
			t.Fatalf("%s got %d, want 200", id, c)
		}
	}
}

func TestHashIsDeterministic(t *testing.T) {
	p, _ := New("hash")
	up := backends(5)
	for _, k := range keys(100) {
		if p.Pick(k, up, 1) != p.Pick(k, up, 1) {
			t.Fatalf("hash not deterministic for %s", k)
		}
	}
}

func TestLeastPicksLowestActive(t *testing.T) {
	p, _ := New("least")
	up := backends(4)
	up[0].Inc()
	up[0].Inc()
	up[1].Inc()
	up[3].Inc()
	up[3].Inc()
	up[3].Inc()
	for i := 0; i < 20; i++ {
		if got := p.Pick("k", up, 1); got.ID != "backend-3" {
			t.Fatalf("got %s, want backend-3", got.ID)
		}
	}
}

// Removing one backend should move only that backend's keys under consistent
// hashing, but most keys under hash-mod-N.
func remapFraction(t *testing.T, name string) float64 {
	p, _ := New(name)
	up := backends(5)
	ks := keys(2000)
	before := map[string]string{}
	for _, k := range ks {
		before[k] = p.Pick(k, up, 1).ID
	}
	// remove backend-3
	smaller := append(append([]*pool.Backend{}, up[:2]...), up[3:]...)
	moved, onRemoved := 0, 0
	for _, k := range ks {
		if before[k] == "backend-3" {
			onRemoved++
			continue
		}
		if p.Pick(k, smaller, 2).ID != before[k] {
			moved++
		}
	}
	_ = onRemoved
	return float64(moved) / float64(len(ks)-onRemoved)
}

func TestConsistentHashingMovesOnlyRemovedKeys(t *testing.T) {
	if f := remapFraction(t, "consistent"); f != 0 {
		t.Fatalf("consistent hashing moved %.1f%% of surviving keys, want 0", f*100)
	}
}

func TestBasicHashReshufflesMostKeys(t *testing.T) {
	if f := remapFraction(t, "hash"); f < 0.4 {
		t.Fatalf("hash mod N moved only %.1f%% of surviving keys, expected a large share", f*100)
	}
}

func TestConsistentIsRoughlyBalanced(t *testing.T) {
	p, _ := New("consistent")
	up := backends(5)
	count := map[string]int{}
	ks := keys(5000)
	for _, k := range ks {
		count[p.Pick(k, up, 1).ID]++
	}
	for id, c := range count {
		if c < 600 || c > 1500 { // ideal is 1000
			t.Fatalf("%s got %d of %d keys, ring is badly skewed", id, c, len(ks))
		}
	}
}
