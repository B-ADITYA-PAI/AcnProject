// Package pool holds the set of backends the load balancer may route to.
//
// Backend i (1-based) listens on BasePort+10*i (TCP), +1 (UDP) and +2 (HTTP),
// on one host. The same convention is used by cmd/backend.
package pool

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

type Backend struct {
	ID       string
	Index    int
	Host     string
	TCPPort  int
	UDPPort  int
	HTTPPort int

	active atomic.Int64
}

func NewBackend(index int, host string, basePort int) *Backend {
	p := basePort + 10*index
	return &Backend{
		ID:       fmt.Sprintf("backend-%d", index),
		Index:    index,
		Host:     host,
		TCPPort:  p,
		UDPPort:  p + 1,
		HTTPPort: p + 2,
	}
}

// Active is the number of connections the load balancer currently has open
// to this backend.
func (b *Backend) Active() int64 { return b.active.Load() }
func (b *Backend) Inc()          { b.active.Add(1) }
func (b *Backend) Dec()          { b.active.Add(-1) }

// Pool is the set of known backends and the subset currently "in rotation".
type Pool struct {
	mu      sync.RWMutex
	known   map[string]*Backend
	up      []*Backend // sorted by Index, so hashing is stable
	version uint64
}

func New() *Pool {
	return &Pool{known: make(map[string]*Backend)}
}

// Register makes a backend known without putting it in rotation.
func (p *Pool) Register(b *Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.known[b.ID] = b
}

// Add puts a known backend in rotation. It reports whether anything changed.
func (p *Pool) Add(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.known[id]
	if !ok {
		return false
	}
	for _, u := range p.up {
		if u.ID == id {
			return false
		}
	}
	p.up = append(p.up, b)
	sort.Slice(p.up, func(i, j int) bool { return p.up[i].Index < p.up[j].Index })
	p.version++
	return true
}

// Remove takes a backend out of rotation. It reports whether anything changed.
func (p *Pool) Remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, u := range p.up {
		if u.ID == id {
			p.up = append(append([]*Backend{}, p.up[:i]...), p.up[i+1:]...)
			p.version++
			return true
		}
	}
	return false
}

// Snapshot returns the backends in rotation and the pool version. The slice
// must not be modified by the caller.
func (p *Pool) Snapshot() ([]*Backend, uint64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.up, p.version
}

func (p *Pool) IDs() []string {
	up, _ := p.Snapshot()
	ids := make([]string, len(up))
	for i, b := range up {
		ids[i] = b.ID
	}
	return ids
}
