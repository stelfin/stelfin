package settlement

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// readRetries is how many more times a read is attempted after it fails to
// reach Horizon at all.
const readRetries = 2

// readBackoff is the pause before the first retry; it doubles for the next.
const readBackoff = 400 * time.Millisecond

// retryReads repeats a GET that never got an answer.
//
// A read of chain state has no effect, so repeating one is always safe, and on
// a poor link the alternative is a member's command failing because one TLS
// handshake was dropped.
//
// Only GET, and only when no response arrived. The distinction is the whole
// design of Submit: a POST that fails in transit may have landed, and repeating
// it blind is how a payment is made twice. That path has its own handling — it
// looks the hash up before concluding anything — and must never be retried
// here. A response that did arrive, whatever its status, is an answer and is
// returned as one: a 404 means the account is not there, not "ask again".
type retryReads struct {
	next http.RoundTripper
	// pause is how to wait, so a test does not.
	pause func(context.Context, time.Duration) error
}

func (r retryReads) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return r.next.RoundTrip(req)
	}

	backoff := readBackoff
	for attempt := 0; ; attempt++ {
		resp, err := r.next.RoundTrip(req)
		if err == nil || attempt == readRetries {
			return resp, err
		}
		// The caller giving up is not a reason to keep trying on its behalf.
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, err
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if waitErr := r.pause(req.Context(), backoff); waitErr != nil {
			return nil, err
		}
		backoff *= 2
	}
}

func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// horizonHTTP is the client New gives Horizon: bounded, and patient with reads.
func horizonHTTP() *http.Client {
	return &http.Client{
		Timeout:   45 * time.Second,
		Transport: retryReads{next: http.DefaultTransport, pause: sleepFor},
	}
}
