package mcpclient

import (
	"sync"
	"time"
)

// A circuit breaker, so a broken endpoint fails fast.
//
// Every call to a remote server holds something open on stelfin's side: a
// goroutine, a request from a member waiting in a chat, a slot in the
// workspace's hourly rate. A deadline bounds each one. It does not bound the
// sum — an endpoint that takes the full deadline to fail, called by everybody
// who retries, holds a full deadline's worth of work per attempt for as long as
// people keep asking.
//
// So after enough consecutive failures the endpoint is not called at all for a
// while, and the answer is immediate. After the cool-down one call is let
// through to find out whether anything has changed. If it succeeds, the breaker
// closes; if it fails, the cool-down starts again.
//
// In-process and per client, with the limitation connector's rate limiter
// states for itself: two instances of stelfin each keep their own count.

// outcome is what a call told the breaker about the endpoint.
type outcome int

const (
	// succeeded: the endpoint answered as the protocol says it should.
	succeeded outcome = iota
	// failed: it did not — an error, a deadline, a malformed or oversized
	// answer, a changed surface.
	failed
	// inconclusive: the call ended for a reason that says nothing about the
	// endpoint, such as the caller going away.
	inconclusive
)

type breaker struct {
	mu sync.Mutex

	threshold int
	coolDown  time.Duration

	failures int
	open     bool
	openedAt time.Time
	// probing is set while the one call allowed through an open breaker is in
	// flight, so that a cool-down ending does not release every waiting caller
	// onto an endpoint that may still be down.
	probing bool
}

// enter reports whether a call may proceed. When it may, the returned function
// must be called exactly once with what happened.
func (b *breaker) enter(now time.Time) (func(outcome, time.Time), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	probe := false
	if b.open {
		if b.probing || now.Sub(b.openedAt) < b.coolDown {
			return nil, false
		}
		b.probing = true
		probe = true
	}

	return func(result outcome, at time.Time) {
		b.mu.Lock()
		defer b.mu.Unlock()

		if probe {
			b.probing = false
			switch result {
			case succeeded:
				b.open = false
				b.failures = 0
			case failed:
				// Still broken. The cool-down restarts from this failure, not
				// from the first one.
				b.openedAt = at
			case inconclusive:
				// Nothing was learnt, so the next caller may probe.
			}
			return
		}

		if b.open {
			// A call that started before the breaker opened and finished
			// after. Its result is about an earlier moment, and the probe is
			// what decides when to close.
			return
		}

		switch result {
		case succeeded:
			// Consecutive failures, so one success clears the count. An
			// endpoint that fails one call in three is unreliable, and is not
			// the thing a breaker is for.
			b.failures = 0
		case failed:
			b.failures++
			if b.failures >= b.threshold {
				b.open = true
				b.openedAt = at
			}
		case inconclusive:
		}
	}, true
}
