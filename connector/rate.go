package connector

import (
	"sync"
	"time"
)

// A rate limit that is a backstop, not the control.
//
// The real limits live on chain, where the DAO set them and can see them. This
// is what stops a connector being called in a loop between those checks: an
// endpoint that answers in a millisecond and a bug that retries is enough to
// exhaust a third party's quota, or a DAO's, before anybody notices.
//
// In-process on purpose, and that is a real limitation worth stating: two
// instances of stelfin each allow the full rate, so the effective limit is per
// instance rather than per deployment. Making it shared means a round trip to
// Postgres on the hot path of every connector call, and the honest trade at
// this size is to say so here rather than to imply a guarantee that does not
// hold.

// rateWindow is the period a limit is counted over.
const rateWindow = time.Hour

type rateLimiter struct {
	mu      sync.Mutex
	windows map[rateKey]*window
}

type rateKey struct {
	org       int64
	connector string
}

type window struct {
	start time.Time
	count int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{windows: make(map[rateKey]*window)}
}

// allow reports whether a call may proceed, and counts it when it may.
//
// A limit of zero means never. That is the default a grant carries before
// anybody sets one, and it fails closed: a connector nobody has given an
// allowance is a connector that does not run.
func (r *rateLimiter) allow(org int64, connector string, perHour int, now time.Time) bool {
	if perHour <= 0 {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := rateKey{org: org, connector: connector}
	w, ok := r.windows[key]
	// A fresh window starts now rather than on a clock boundary — aligning to
	// one would let a caller wait for the tick and spend two windows' worth
	// back to back.
	if !ok || now.Sub(w.start) >= rateWindow {
		r.windows[key] = &window{start: now, count: 1}
		r.sweep(now)
		return true
	}

	if w.count >= perHour {
		return false
	}
	w.count++
	return true
}

// sweep drops windows that have expired.
//
// Called on the cheap path — when a window is being replaced anyway — because a
// map keyed by org and connector grows with tenants, and a limiter that leaks
// one entry per connector per tenant is a slow memory leak in a long-running
// process.
func (r *rateLimiter) sweep(now time.Time) {
	for key, w := range r.windows {
		if now.Sub(w.start) >= rateWindow {
			delete(r.windows, key)
		}
	}
}
