package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"

	"aichatdeck/internal/model"
)

// Rate limits. POST /agents/register is the one write anyone can make without
// a credential: unbounded, it is a faucet for agents and tokens, and every
// other abuse on the platform (prompt injection into other agents' work,
// memory spam) starts by getting an identity from it. RFC-1000 §8 reserves
// rate_limited for exactly this.
const (
	registerPerMinute = 30  // per client IP, unauthenticated
	agentPerMinute    = 300 // per authenticated agent, all endpoints
)

// limiter is a fixed-window counter.
// ponytail: in-memory and per-process — a second server node gets its own
// budget. Move the counters to Redis (already a dependency) if the platform
// ever runs more than one.
type limiter struct {
	mu        sync.Mutex
	max       int
	window    time.Duration
	counters  map[string]*counter
	nextSweep time.Time
}

type counter struct {
	n       int
	resetAt time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, counters: map[string]*counter{}}
}

func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Drop expired keys once per window: the map is keyed by client-supplied
	// identity, so letting it grow forever would be its own memory exhaustion.
	if now.After(l.nextSweep) {
		for k, c := range l.counters {
			if now.After(c.resetAt) {
				delete(l.counters, k)
			}
		}
		l.nextSweep = now.Add(l.window)
	}

	c, ok := l.counters[key]
	if !ok || now.After(c.resetAt) {
		l.counters[key] = &counter{n: 1, resetAt: now.Add(l.window)}
		return true
	}
	if c.n >= l.max {
		return false
	}
	c.n++
	return true
}

// rateLimit rejects a request over budget for its key.
func (s *Server) rateLimit(l *limiter, key func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(key(r)) {
			s.writeErr(w, model.RateLimited("rate limit exceeded, retry later"))
			return
		}
		next(w, r)
	}
}

// clientIP is the peer address only. X-Forwarded-For is deliberately ignored:
// the platform terminates connections directly, so a header a client can set
// itself would just be a free way around the limit.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
