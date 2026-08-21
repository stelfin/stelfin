package chattest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stelfin/stelfin/chat"
)

// Harness is what a transport package supplies so its implementation can be run
// through RunTransport.
type Harness struct {
	Transport chat.Transport

	// SignedRequest returns a request the transport authenticates successfully,
	// carrying exactly body.
	SignedRequest func(tb testing.TB, body []byte) *http.Request

	// EncodeBody renders messages into this platform's wire shape.
	EncodeBody func(tb testing.TB, msgs []chat.Inbound) []byte

	// SampleMessages is a small, valid delivery. It must contain at least one
	// message, so the round trip through Parse is observable.
	SampleMessages []chat.Inbound

	// SampleBody, if set, overrides EncodeBody(SampleMessages).
	SampleBody []byte

	// BodyAuthenticated reports whether this platform's authentication actually
	// covers the request body.
	//
	// Discord signs timestamp||body with Ed25519, so it does. Telegram echoes a
	// shared secret in a header, which proves only that the caller knew the
	// secret — anyone able to present it can send any body at all. That is a
	// real and permanent difference between the two, not a detail, and it is
	// declared here rather than assumed so the consequence stays visible: on a
	// platform where this is false, nothing inside the body may be trusted to
	// establish privilege. Admin status in particular has to be fetched from the
	// platform's API rather than read out of the update.
	BodyAuthenticated bool

	// PrivateConversation returns a conversation on which this transport can
	// actually deliver an ephemeral reply.
	//
	// The platforms differ here in a way that is not incidental. A Telegram bot
	// can direct-message anyone who has opened a chat with it, so almost any
	// conversation has a private route. Discord has one only while an
	// interaction is live: fifteen minutes after a command, the sole remaining
	// route is a public channel post. A suite that assumed the Telegram shape
	// would be asserting a capability Discord does not have.
	//
	// Optional; defaults to a direct message, where privacy is free.
	PrivateConversation func(chat.Channel) chat.Conversation

	// LastOutbound returns the most recent wire body the transport wrote.
	LastOutbound func() ([]byte, bool)

	// PreviewSuppressed reports whether an outbound wire body disables link
	// previews on this platform.
	PreviewSuppressed func(body []byte) bool
}

// RunTransport asserts the properties every transport must hold.
//
// These are not stylistic preferences. Each one corresponds to a way a
// messaging transport has been observed to leak payment authority or to run a
// payment twice, and each is cheap to break during a refactor and expensive to
// notice afterwards.
func RunTransport(t *testing.T, h Harness) {
	t.Helper()
	if h.Transport == nil || h.SignedRequest == nil || h.LastOutbound == nil || h.PreviewSuppressed == nil {
		t.Fatal("chattest: harness is incomplete")
	}

	body := h.SampleBody
	if body == nil {
		if h.EncodeBody == nil || len(h.SampleMessages) == 0 {
			t.Fatal("chattest: harness needs SampleBody, or EncodeBody plus SampleMessages")
		}
		body = h.EncodeBody(t, h.SampleMessages)
	}
	if len(body) == 0 {
		t.Fatal("chattest: sample body is empty")
	}

	t.Run("ChannelIsKnown", func(t *testing.T) {
		if c := h.Transport.Channel(); !c.Valid() {
			t.Fatalf("transport reports unknown channel %q", c)
		}
	})

	t.Run("VerifyAcceptsAGoodRequest", func(t *testing.T) {
		got, err := h.Transport.Verify(h.SignedRequest(t, body))
		if err != nil {
			t.Fatalf("verify a well-formed request: %v", err)
		}
		if string(got) != string(body) {
			// Verify must hand back the bytes it authenticated. Returning a
			// re-encoded body would mean the signature covered something other
			// than what Parse goes on to read.
			t.Fatalf("verify returned different bytes than were sent\n got: %q\nwant: %q", got, body)
		}
	})

	t.Run("VerifyRejectsAMutatedBody", func(t *testing.T) {
		if !h.BodyAuthenticated {
			t.Skip("this platform's authentication does not cover the body; " +
				"nothing inside it may be trusted for privilege")
		}
		mutated := append([]byte(nil), body...)
		mutated[len(mutated)-1] ^= 0x01

		// Authenticate for the original body, then deliver a different one —
		// the exact shape of a tampered delivery.
		authentic := h.SignedRequest(t, body)
		tampered := h.SignedRequest(t, mutated)
		tampered.Header = authentic.Header.Clone()

		if _, err := h.Transport.Verify(tampered); err == nil {
			t.Fatal("verify accepted a body that differs from the authenticated one")
		}
	})

	t.Run("VerifyRejectsAnUnauthenticatedRequest", func(t *testing.T) {
		r := h.SignedRequest(t, body)
		r.Header = http.Header{}
		_, err := h.Transport.Verify(r)
		if err == nil {
			t.Fatal("verify accepted a request carrying no authentication")
		}
		if !errors.Is(err, chat.ErrUnauthenticated) {
			t.Fatalf("want ErrUnauthenticated, got %v", err)
		}
	})

	t.Run("ParseProducesAnAck", func(t *testing.T) {
		d, err := h.Transport.Parse(body)
		if err != nil {
			t.Fatalf("parse a well-formed body: %v", err)
		}
		if d.Ack.Status < 200 || d.Ack.Status > 299 {
			t.Fatalf("ack status %d is not a success; the platform will retry", d.Ack.Status)
		}
		for i, m := range d.Messages {
			if m.DedupeID == "" {
				t.Fatalf("message %d has no DedupeID; a retried delivery would be processed twice", i)
			}
			if m.Actor.UserID == "" {
				t.Fatalf("message %d has no Actor.UserID", i)
			}
			if !m.Actor.Channel.Valid() || !m.Conversation.Channel.Valid() {
				t.Fatalf("message %d carries an unknown channel", i)
			}
		}
	})

	t.Run("SendSuppressesLinkPreviews", func(t *testing.T) {
		ctx := context.Background()
		to := chat.Conversation{Channel: h.Transport.Channel(), SpaceID: "space-1", IsDM: true}
		actor := chat.Actor{Channel: h.Transport.Channel(), UserID: "42"}
		if err := h.Transport.Send(ctx, to, actor, chat.Reply{Text: "https://example.test/confirm#tok"}); err != nil {
			t.Fatalf("send: %v", err)
		}
		out, ok := h.LastOutbound()
		if !ok {
			t.Fatal("no outbound body was recorded")
		}
		if !h.PreviewSuppressed(out) {
			// A preview crawler fetching a confirmation link leaks the URL to
			// the platform and consumes the single-use token behind it.
			t.Fatal("outbound message does not suppress link previews")
		}
	})

	t.Run("RegistryRefusesAuthorityLinksInPublic", func(t *testing.T) {
		const prefix = "https://stelfin.test"
		reg, err := chat.NewRegistry(prefix, h.Transport)
		if err != nil {
			t.Fatalf("new registry: %v", err)
		}
		channel := h.Transport.Channel()
		public := chat.Conversation{Channel: channel, SpaceID: "space-1"}
		actor := chat.Actor{Channel: channel, UserID: "42"}

		// The registry refuses before the transport is reached, so this holds
		// on every platform regardless of what its private routes look like.
		err = reg.Send(context.Background(), public, actor,
			chat.Reply{Text: "tap to confirm: " + prefix + "/confirm#tok"})
		if !errors.Is(err, chat.ErrLinkInPublic) {
			t.Fatalf("posting an authority link to a group: want ErrLinkInPublic, got %v", err)
		}

		private := chat.Conversation{Channel: channel, SpaceID: "space-1", IsDM: true}
		if h.PrivateConversation != nil {
			private = h.PrivateConversation(channel)
		}
		if err := reg.Send(context.Background(), private, actor,
			chat.Reply{Text: prefix + "/confirm#tok", Ephemeral: true}); err != nil {
			t.Fatalf("an ephemeral authority link must be deliverable privately: %v", err)
		}
	})

	t.Run("RegisterCommandsIsIdempotent", func(t *testing.T) {
		cmds := []chat.Command{{Name: "balance", Description: "show balances", MinRole: chat.RoleObserver}}
		ctx := context.Background()
		if err := h.Transport.RegisterCommands(ctx, cmds); err != nil {
			t.Fatalf("register commands: %v", err)
		}
		if err := h.Transport.RegisterCommands(ctx, cmds); err != nil {
			t.Fatalf("register the same commands twice: %v", err)
		}
	})
}
