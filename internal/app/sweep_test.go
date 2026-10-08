package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// TestSweepRunsAtStartAndThenKeepsRunning: the pass at start is the one a
// deployment that sleeps depends on, so it must not wait a whole interval.
func TestSweepRunsAtStartAndThenKeepsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var passes atomic.Int32
	first := make(chan struct{})
	third := make(chan struct{})

	expire := func(context.Context) (int64, error) {
		switch passes.Add(1) {
		case 1:
			close(first)
			// A failing pass must not end the sweep.
			return 0, errors.New("connection reset")
		case 3:
			close(third)
		}
		return 1, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sweepProposals(ctx, expire, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("no pass at start: an hour-long interval was waited out first")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep did not stop when its context ended")
	}

	// And with an interval short enough to observe, it keeps going after a
	// failure.
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	passes.Store(0)
	first = make(chan struct{})
	go sweepProposals(ctx, expire, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	select {
	case <-third:
	case <-time.After(2 * time.Second):
		t.Fatalf("made %d passes, want at least 3: a failed pass ended the sweep", passes.Load())
	}
}
