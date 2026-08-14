package chattest_test

import (
	"testing"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/chat/chattest"
)

// TestFakeSatisfiesTheConformanceSuite runs the suite against the fake.
//
// It checks two things at once: that the fake behaves like a transport, and
// that the suite itself works. A conformance suite nothing has ever run is a
// suite that fails the day a real transport is written against it.
func TestFakeSatisfiesTheConformanceSuite(t *testing.T) {
	f := chattest.NewFake(chat.Telegram)
	h := chattest.FakeHarness(f)
	// The fake authenticates with a shared secret, like Telegram, so the body
	// is not covered. Declared rather than silently skipped.
	h.BodyAuthenticated = false
	chattest.RunTransport(t, h)
}
