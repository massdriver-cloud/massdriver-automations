package automation

import (
	"sync"
	"time"
)

// dedupe remembers event ids in memory. It does not survive a restart or span replicas.
type dedupe struct {
	ttl  time.Duration
	mu   sync.Mutex
	seen map[string]time.Time
}

func newDedupe(ttl time.Duration) *dedupe { return &dedupe{ttl: ttl, seen: map[string]time.Time{}} }

func (d *dedupe) has(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.seen[id]
	return ok && time.Since(t) < d.ttl
}

func (d *dedupe) add(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, t := range d.seen {
		if now.Sub(t) >= d.ttl {
			delete(d.seen, k)
		}
	}
	d.seen[id] = now
}
