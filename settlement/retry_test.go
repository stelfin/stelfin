package settlement

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// flaky fails a set number of times before answering.
type flaky struct {
	failures int
	calls    int
	status   int
}

func (f *flaky) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("net/http: TLS handshake timeout")
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

func noWait(context.Context, time.Duration) error { return nil }

func get(t *testing.T, method string, rt http.RoundTripper) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, "https://horizon.example/accounts/GABC", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return retryReads{next: rt, pause: noWait}.RoundTrip(req)
}

// A read has no effect, so one dropped handshake should cost a moment and not
// a member's command.
func TestAReadThatNeverArrivedIsTriedAgain(t *testing.T) {
	upstream := &flaky{failures: 2}
	resp, err := get(t, http.MethodGet, upstream)
	if err != nil {
		t.Fatalf("a read that succeeds on the third attempt failed: %v", err)
	}
	resp.Body.Close()
	if upstream.calls != 3 {
		t.Fatalf("made %d attempts, want 3", upstream.calls)
	}

	// And it does give up.
	upstream = &flaky{failures: 99}
	if _, err := get(t, http.MethodGet, upstream); err == nil {
		t.Fatal("a read that never succeeds returned no error")
	}
	if upstream.calls != readRetries+1 {
		t.Fatalf("made %d attempts, want %d", upstream.calls, readRetries+1)
	}
}

// TestASubmissionIsNeverTriedAgain is the property that matters most here. A
// POST that fails in transit may have landed; sending it again is how a payment
// is made twice. Submit resolves that by looking the hash up, and this layer
// must stay out of it entirely.
func TestASubmissionIsNeverTriedAgain(t *testing.T) {
	upstream := &flaky{failures: 1}
	if _, err := get(t, http.MethodPost, upstream); err == nil {
		t.Fatal("a failed submission was reported as a success")
	}
	if upstream.calls != 1 {
		t.Fatalf("a submission was sent %d times", upstream.calls)
	}
}

// An answer is an answer. A 404 says the account is not there, and a 500 is
// Horizon's to explain; neither is "ask again".
func TestAnAnswerIsNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusTooManyRequests} {
		upstream := &flaky{status: status}
		resp, err := get(t, http.MethodGet, upstream)
		if err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		resp.Body.Close()
		if resp.StatusCode != status || upstream.calls != 1 {
			t.Errorf("%d: got %d after %d attempts", status, resp.StatusCode, upstream.calls)
		}
	}
}

// A caller who has stopped waiting is not waited for.
func TestACancelledReadStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://horizon.example/", nil)
	upstream := &flaky{failures: 99}
	if _, err := (retryReads{next: upstream, pause: noWait}).RoundTrip(req); err == nil {
		t.Fatal("expected an error")
	}
	if upstream.calls != 1 {
		t.Fatalf("made %d attempts for a caller who had gone", upstream.calls)
	}
}
