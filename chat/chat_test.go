package chat_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stelfin/stelfin/chat"
)

// stub is a minimal Transport for registry tests. It is deliberately not the
// chattest fake: chattest imports chat, so using it here would be a cycle.
type stub struct {
	channel chat.Channel
	sent    []chat.Reply
}

func (s *stub) Channel() chat.Channel                                  { return s.channel }
func (s *stub) Verify(r *http.Request) ([]byte, error)                 { return chat.ReadLimited(r) }
func (s *stub) Parse([]byte) (chat.Delivery, error)                    { return chat.Delivery{}, nil }
func (s *stub) RegisterCommands(context.Context, []chat.Command) error { return nil }

func (s *stub) Send(_ context.Context, _ chat.Conversation, _ chat.Actor, r chat.Reply) error {
	s.sent = append(s.sent, r)
	return nil
}

func TestActorRefIsChannelScoped(t *testing.T) {
	// The same numeric id on two platforms is two different people. An
	// unprefixed reference would silently merge them.
	tg := chat.Actor{Channel: chat.Telegram, UserID: "12345"}
	dc := chat.Actor{Channel: chat.Discord, UserID: "12345"}

	if tg.Ref() == dc.Ref() {
		t.Fatalf("two platforms' ids collided: both %q", tg.Ref())
	}
	if got, want := tg.Ref(), "telegram:12345"; got != want {
		t.Fatalf("Ref() = %q, want %q", got, want)
	}
}

func TestActorRefIgnoresTheHandle(t *testing.T) {
	// A handle can be renamed and, once released, taken by someone else. If it
	// leaked into the owner reference, a recycled username would inherit the
	// previous holder's roles and balances.
	before := chat.Actor{Channel: chat.Discord, UserID: "9", Handle: "treasurer"}
	after := chat.Actor{Channel: chat.Discord, UserID: "9", Handle: "someone-else"}

	if before.Ref() != after.Ref() {
		t.Fatalf("renaming the handle changed the owner reference: %q -> %q", before.Ref(), after.Ref())
	}
}

func TestChannelValidRejectsUnknown(t *testing.T) {
	for _, c := range []chat.Channel{chat.Telegram, chat.Discord} {
		if !c.Valid() {
			t.Fatalf("%q should be valid", c)
		}
	}
	for _, c := range []chat.Channel{"", "whatsapp", "TELEGRAM", "../etc"} {
		if c.Valid() {
			t.Fatalf("%q should not be valid", c)
		}
	}
}

func TestNewRegistryRequiresALinkPrefix(t *testing.T) {
	// Without a prefix the authority-link check cannot fire, and a registry that
	// skipped it would fail open.
	if _, err := chat.NewRegistry("  "); err == nil {
		t.Fatal("empty link prefix was accepted")
	}
}

func TestNewRegistryRejectsDuplicateChannels(t *testing.T) {
	a := &stub{channel: chat.Telegram}
	b := &stub{channel: chat.Telegram}
	if _, err := chat.NewRegistry("https://x.test", a, b); err == nil {
		t.Fatal("two transports for one channel were accepted")
	}
}

func TestRegistryRefusesPublicAuthorityLinks(t *testing.T) {
	const prefix = "https://stelfin.test"
	s := &stub{channel: chat.Telegram}
	reg, err := chat.NewRegistry(prefix, s)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	group := chat.Conversation{Channel: chat.Telegram, SpaceID: "-100"}
	actor := chat.Actor{Channel: chat.Telegram, UserID: "1"}

	err = reg.Send(context.Background(), group, actor,
		chat.Reply{Text: "confirm here: " + prefix + "/confirm#tok"})
	if !errors.Is(err, chat.ErrLinkInPublic) {
		t.Fatalf("want ErrLinkInPublic, got %v", err)
	}
	if len(s.sent) != 0 {
		t.Fatal("the refused reply was still delivered")
	}
}

func TestRegistryAllowsAuthorityLinksPrivately(t *testing.T) {
	const prefix = "https://stelfin.test"
	s := &stub{channel: chat.Telegram}
	reg, _ := chat.NewRegistry(prefix, s)
	actor := chat.Actor{Channel: chat.Telegram, UserID: "1"}

	dm := chat.Conversation{Channel: chat.Telegram, SpaceID: "1", IsDM: true}
	if err := reg.Send(context.Background(), dm, actor, chat.Reply{Text: prefix + "/confirm#tok"}); err != nil {
		t.Fatalf("a DM carrying an authority link should be allowed: %v", err)
	}

	group := chat.Conversation{Channel: chat.Telegram, SpaceID: "-100"}
	if err := reg.Send(context.Background(), group, actor,
		chat.Reply{Text: prefix + "/confirm#tok", Ephemeral: true}); err != nil {
		t.Fatalf("an ephemeral authority link should be allowed: %v", err)
	}
	if len(s.sent) != 2 {
		t.Fatalf("delivered %d replies, want 2", len(s.sent))
	}
}

func TestRegistryAllowsOrdinaryPublicMessages(t *testing.T) {
	s := &stub{channel: chat.Telegram}
	reg, _ := chat.NewRegistry("https://stelfin.test", s)
	group := chat.Conversation{Channel: chat.Telegram, SpaceID: "-100"}

	if err := reg.Send(context.Background(), group, chat.Actor{}, chat.Reply{
		Text: "Proposal #4 passed. See https://stellar.expert for the transaction.",
	}); err != nil {
		t.Fatalf("an ordinary message mentioning an unrelated link: %v", err)
	}
}

func TestRegistrySendUnknownChannel(t *testing.T) {
	reg, _ := chat.NewRegistry("https://stelfin.test")
	err := reg.Send(context.Background(), chat.Conversation{Channel: chat.Discord}, chat.Actor{}, chat.Reply{Text: "hi"})
	if err == nil {
		t.Fatal("sending to an unregistered channel should fail")
	}
}

func TestReadLimitedBoundsTheBody(t *testing.T) {
	// The bound applies before authentication, so it must hold for a body no
	// signature has vouched for.
	oversized := bytes.Repeat([]byte("a"), int(chat.MaxWebhookBody)+4096)
	r := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewReader(oversized))

	body, err := chat.ReadLimited(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if int64(len(body)) != chat.MaxWebhookBody {
		t.Fatalf("read %d bytes, want the limit of %d", len(body), chat.MaxWebhookBody)
	}
}

func TestReadLimitedReturnsExactBytes(t *testing.T) {
	want := `{"messages":[{"DedupeID":"telegram:1"}]}`
	r := httptest.NewRequest(http.MethodPost, "/webhook/telegram", strings.NewReader(want))
	got, err := chat.ReadLimited(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRoleOrdering(t *testing.T) {
	if !chat.RoleAdmin.AtLeast(chat.RoleApprover) {
		t.Fatal("admin should satisfy approver")
	}
	if chat.RoleObserver.AtLeast(chat.RoleProposer) {
		t.Fatal("observer should not satisfy proposer")
	}
	if chat.RoleNone.AtLeast(chat.RoleObserver) {
		t.Fatal("an unknown person should not satisfy observer")
	}
}
