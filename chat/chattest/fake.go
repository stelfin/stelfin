// Package chattest provides a fake transport and the conformance suite every
// real transport must pass.
//
// The suite exists because two transports written months apart drift. Each one
// re-derives "verify before parse", "suppress previews", "never post authority
// in public" from its own platform's documentation, and one of them gets it
// subtly wrong. A shared executable statement of the rules is the only thing
// that keeps them honest as a third is added.
package chattest

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stelfin/stelfin/chat"
)

// FakeSecret is the shared secret Fake authenticates requests with.
const FakeSecret = "fake-transport-secret"

// FakeAuthHeader carries FakeSecret on an inbound request.
const FakeAuthHeader = "X-Fake-Auth"

// Sent is one recorded outbound reply.
type Sent struct {
	To    chat.Conversation
	Actor chat.Actor
	Reply chat.Reply
}

// Fake is an in-memory Transport.
//
// It models the shape of a real one rather than a specific platform: a shared
// secret on an inbound request, a JSON envelope, and an outbound wire body
// carrying an explicit preview-suppression flag. That is enough for the core to
// be tested without a network, and enough for the conformance suite to be
// exercised against something before a real transport exists.
type Fake struct {
	// FailParse makes Parse return an error, for testing the "verified but
	// unreadable" path.
	FailParse bool

	channel chat.Channel

	mu       sync.Mutex
	sent     []Sent
	outbound [][]byte
	commands []chat.Command
	sendErr  error
}

// NewFake returns a Fake for the given channel.
func NewFake(c chat.Channel) *Fake {
	if c == "" {
		c = chat.Telegram
	}
	return &Fake{channel: c}
}

func (f *Fake) Channel() chat.Channel { return f.channel }

// Verify checks the shared secret and returns the exact bytes read.
func (f *Fake) Verify(r *http.Request) ([]byte, error) {
	got := r.Header.Get(FakeAuthHeader)
	if subtle.ConstantTimeCompare([]byte(got), []byte(FakeSecret)) != 1 {
		return nil, chat.ErrUnauthenticated
	}
	return chat.ReadLimited(r)
}

// fakeEnvelope is the wire shape Fake parses.
type fakeEnvelope struct {
	Messages []chat.Inbound `json:"messages"`
}

// Parse decodes the envelope.
func (f *Fake) Parse(body []byte) (chat.Delivery, error) {
	if f.FailParse {
		return chat.Delivery{}, errors.New("chattest: parse failed on purpose")
	}
	var env fakeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return chat.Delivery{}, fmt.Errorf("chattest: decode envelope: %w", err)
	}
	return chat.Delivery{
		Ack:      chat.Ack{Status: http.StatusOK, ContentType: "application/json", Body: []byte(`{"ok":true}`)},
		Messages: env.Messages,
	}, nil
}

// fakeOutbound is the wire body Fake "writes to the platform". The preview flag
// is present so the conformance suite can assert suppression against a real
// serialised body rather than against an in-memory struct field.
type fakeOutbound struct {
	ChatID          string `json:"chat_id"`
	Text            string `json:"text"`
	DisablePreview  bool   `json:"disable_preview"`
	EphemeralToUser string `json:"ephemeral_to_user,omitempty"`
}

// Send records the reply.
func (f *Fake) Send(_ context.Context, to chat.Conversation, actor chat.Actor, r chat.Reply) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	body, err := json.Marshal(fakeOutbound{
		ChatID:         to.SpaceID,
		Text:           r.Text,
		DisablePreview: true, // unconditional, exactly as a real transport must be
		EphemeralToUser: func() string {
			if r.Ephemeral {
				return actor.UserID
			}
			return ""
		}(),
	})
	if err != nil {
		return err
	}
	f.sent = append(f.sent, Sent{To: to, Actor: actor, Reply: r})
	f.outbound = append(f.outbound, body)
	return nil
}

// RegisterCommands records the command set.
func (f *Fake) RegisterCommands(_ context.Context, cmds []chat.Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append([]chat.Command(nil), cmds...)
	return nil
}

// SetSendErr makes subsequent sends fail.
func (f *Fake) SetSendErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

// Sent returns every recorded reply.
func (f *Fake) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent(nil), f.sent...)
}

// Outbound returns every recorded wire body.
func (f *Fake) Outbound() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.outbound...)
}

// Commands returns the last registered command set.
func (f *Fake) Commands() []chat.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chat.Command(nil), f.commands...)
}

// Reset clears recorded state.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent, f.outbound, f.commands, f.sendErr = nil, nil, nil, nil
}

// Envelope builds a wire body carrying msgs, for use with SignedRequest.
func Envelope(tb testing.TB, msgs ...chat.Inbound) []byte {
	tb.Helper()
	body, err := json.Marshal(fakeEnvelope{Messages: msgs})
	if err != nil {
		tb.Fatalf("chattest: marshal envelope: %v", err)
	}
	return body
}

// SignedRequest returns a request Fake.Verify accepts.
func SignedRequest(tb testing.TB, body []byte) *http.Request {
	tb.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewReader(body))
	r.Header.Set(FakeAuthHeader, FakeSecret)
	return r
}

// Message returns a plausible Inbound for tests that do not care about detail.
func Message(dedupeID, userID, text string) chat.Inbound {
	return chat.Inbound{
		DedupeID: dedupeID,
		Actor: chat.Actor{
			Channel: chat.Telegram,
			UserID:  userID,
			Handle:  "tester",
		},
		Conversation: chat.Conversation{
			Channel: chat.Telegram,
			SpaceID: "space-1",
			IsDM:    true,
		},
		Args: text,
	}
}

// FakeHarness returns the conformance Harness for Fake, which also serves as
// the worked example a real transport's harness is written against.
func FakeHarness(f *Fake) Harness {
	return Harness{
		Transport:     f,
		SignedRequest: func(tb testing.TB, body []byte) *http.Request { return SignedRequest(tb, body) },
		SampleBody:    nil, // filled in by RunTransport via SampleMessages
		SampleMessages: []chat.Inbound{
			Message("fake:1", "42", "hello"),
		},
		EncodeBody: func(tb testing.TB, msgs []chat.Inbound) []byte { return Envelope(tb, msgs...) },
		PreviewSuppressed: func(body []byte) bool {
			var out fakeOutbound
			if err := json.Unmarshal(body, &out); err != nil {
				return false
			}
			return out.DisablePreview
		},
		LastOutbound: func() ([]byte, bool) {
			bodies := f.Outbound()
			if len(bodies) == 0 {
				return nil, false
			}
			return bodies[len(bodies)-1], true
		},
	}
}
