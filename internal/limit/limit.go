// Package limit is a shared, keyed token-bucket rate limiter used by loft.db, loft.upload, and
// loft.ai. A token bucket (vs a fixed window) gives smooth limiting with a small controlled burst
// and no boundary-doubling at window edges. Buckets are created per key (e.g. a site, or "site|user")
// and idle ones are evicted so the map can't grow without bound in a long-lived process.
package limit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	evictThreshold = 10_000           // sweep idle buckets once the map exceeds this
	idleTTL        = 10 * time.Minute // a bucket unused this long is evictable
)

// Limiter is a set of per-key token buckets. Safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*entry
	rate    rate.Limit
	burst   int
}

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// New builds a limiter allowing perMinute sustained requests per key, with the given burst.
func New(perMinute, burst int) *Limiter {
	return &Limiter{
		buckets: map[string]*entry{},
		rate:    rate.Limit(float64(perMinute) / 60.0),
		burst:   burst,
	}
}

// Allow reports whether the key may proceed now, consuming one token if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.buckets) > evictThreshold {
		for k, e := range l.buckets {
			if now.Sub(e.seen) > idleTTL {
				delete(l.buckets, k)
			}
		}
	}
	e := l.buckets[key]
	if e == nil {
		e = &entry{lim: rate.NewLimiter(l.rate, l.burst)}
		l.buckets[key] = e
	}
	e.seen = now
	return e.lim.Allow()
}
