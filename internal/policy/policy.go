// Package policy implements the four L4 routing policies compared in the
// project behind one interface, so only the policy changes between runs.
package policy

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"

	"acnlb/internal/pool"
)

// Policy chooses a backend for a flow. key is the flow identifier an L4 load
// balancer can see (the client source IP). up is never empty.
type Policy interface {
	Name() string
	Pick(key string, up []*pool.Backend, version uint64) *pool.Backend
}

func New(name string) (Policy, error) {
	switch name {
	case "rr":
		return &roundRobin{}, nil
	case "hash":
		return &basicHash{}, nil
	case "least":
		return &leastLoaded{}, nil
	case "consistent":
		return &consistent{vnodes: 160, ver: ^uint64(0)}, nil
	}
	return nil, fmt.Errorf("unknown policy %q (want rr, hash, least or consistent)", name)
}

func Names() []string { return []string{"rr", "hash", "least", "consistent"} }

// ---- Round Robin: fair under uniform traffic, no affinity ----

type roundRobin struct{ n atomic.Uint64 }

func (*roundRobin) Name() string { return "rr" }
func (r *roundRobin) Pick(_ string, up []*pool.Backend, _ uint64) *pool.Backend {
	return up[(r.n.Add(1)-1)%uint64(len(up))]
}

// ---- Basic hashing: hash(key) mod N, ignores load, reshuffles on churn ----

type basicHash struct{}

func (*basicHash) Name() string { return "hash" }
func (*basicHash) Pick(key string, up []*pool.Backend, _ uint64) *pool.Backend {
	h := fnv.New32a()
	h.Write([]byte(key))
	return up[h.Sum32()%uint32(len(up))]
}

// ---- Least-loaded: fewest active connections ----

type leastLoaded struct{ start atomic.Uint64 }

func (*leastLoaded) Name() string { return "least" }
func (l *leastLoaded) Pick(_ string, up []*pool.Backend, _ uint64) *pool.Backend {
	// Rotate the starting point so ties are spread instead of always
	// favouring the first backend.
	s := l.start.Add(1)
	var best *pool.Backend
	for i := range up {
		b := up[(s+uint64(i))%uint64(len(up))]
		if best == nil || b.Active() < best.Active() {
			best = b
		}
	}
	return best
}

// ---- Consistent hashing: hash ring with virtual nodes ----

type ringPoint struct {
	h uint64
	b *pool.Backend
}

type consistent struct {
	vnodes int

	mu   sync.Mutex
	ver  uint64
	ring []ringPoint
}

func (*consistent) Name() string { return "consistent" }

func hash64(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	// splitmix64 finaliser: FNV alone disperses short similar strings poorly.
	x := h.Sum64()
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func (c *consistent) ringFor(up []*pool.Backend, version uint64) []ringPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ver != version || c.ring == nil {
		ring := make([]ringPoint, 0, len(up)*c.vnodes)
		for _, b := range up {
			for v := 0; v < c.vnodes; v++ {
				ring = append(ring, ringPoint{hash64(fmt.Sprintf("%s#%d", b.ID, v)), b})
			}
		}
		sort.Slice(ring, func(i, j int) bool { return ring[i].h < ring[j].h })
		c.ring, c.ver = ring, version
	}
	return c.ring
}

func (c *consistent) Pick(key string, up []*pool.Backend, version uint64) *pool.Backend {
	ring := c.ringFor(up, version)
	h := hash64(key)
	i := sort.Search(len(ring), func(i int) bool { return ring[i].h >= h })
	if i == len(ring) {
		i = 0
	}
	return ring[i].b
}
