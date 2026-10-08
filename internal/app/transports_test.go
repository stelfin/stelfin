package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stelfin/stelfin/chat"
)

// flaky fails each call a set number of times before it succeeds.
type flaky struct {
	mu                         sync.Mutex
	startFails, publishFails   int
	starts, publishes          int
	startedAfter, publishedFor int
}

func (f *flaky) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.starts <= f.startFails {
		return errors.New("telegram: getMe: request failed")
	}
	f.startedAfter = f.starts
	return nil
}

func (f *flaky) RegisterCommands(_ context.Context, cmds []chat.Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes++
	if f.publishes <= f.publishFails {
		return errors.New("discord: 503")
	}
	f.publishedFor = len(cmds)
	return nil
}

// TestPreparingRetriesUntilBothHaveHappened: a platform having a bad minute at
// boot should cost a minute, not a restart. And each half is retried only until
// it has succeeded — setting a webhook that is already set, on every pass of a
// loop waiting for something else, would be a way to get rate limited.
func TestPreparingRetriesUntilBothHaveHappened(t *testing.T) {
	f := &flaky{startFails: 2, publishFails: 1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		prepareTransports(t.Context(), f, make([]chat.Command, 3), time.Millisecond, quiet)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("never finished preparing")
	}
	if f.starts != 3 || f.startedAfter != 3 {
		t.Errorf("started %d times, want 3: twice failing, once not, and no more", f.starts)
	}
	if f.publishes != 2 || f.publishedFor != 3 {
		t.Errorf("published %d times for %d commands, want 2 and 3", f.publishes, f.publishedFor)
	}
}

func TestPreparingStopsWithTheServer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	f := &flaky{startFails: 1 << 30, publishFails: 1 << 30}
	done := make(chan struct{})
	go func() {
		defer close(done)
		prepareTransports(ctx, f, nil, time.Hour, quiet)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("kept waiting to retry after the server was told to stop")
	}
}

// TestTelegramIsOnlyPointedAtAnAddressItCanReach: a laptop sharing a bot token
// with a deployment must not take the deployment's deliveries away by starting.
func TestTelegramIsOnlyPointedAtAnAddressItCanReach(t *testing.T) {
	for base, want := range map[string]string{
		"https://stelfin.onrender.com":  "https://stelfin.onrender.com/webhook/telegram",
		"https://stelfin.onrender.com/": "https://stelfin.onrender.com/webhook/telegram",
		"http://localhost:8080":         "",
		"http://127.0.0.1:8080":         "",
		"":                              "",
	} {
		if got := telegramWebhookURL(base); got != want {
			t.Errorf("telegramWebhookURL(%q) = %q, want %q", base, got, want)
		}
	}
}
