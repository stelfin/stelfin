package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
	"github.com/stelfin/stelfin/settlement"
)

// fakeReplier records replies instead of sending them.
type fakeReplier struct {
	mu   sync.Mutex
	sent []chat.Reply
	err  error
}

func (r *fakeReplier) Send(_ context.Context, _ chat.Conversation, _ chat.Actor, m chat.Reply) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, m)
	return nil
}

func (r *fakeReplier) replies() []chat.Reply {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]chat.Reply(nil), r.sent...)
}

// only returns the single reply a test expects, failing if there was any other
// number of them.
func (r *fakeReplier) only(t *testing.T) chat.Reply {
	t.Helper()
	sent := r.replies()
	if len(sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(sent))
	}
	return sent[0]
}

// stubLinker mints predictable links so replies can be asserted on.
type stubLinker struct{}

func (stubLinker) IssueConfirmLink(_ Scope, hash string, _ time.Time) (string, error) {
	return "https://stelfin.example/confirm#token-for-" + hash, nil
}

func (stubLinker) IssueEnrollLink(scope Scope, _ time.Time) (string, error) {
	return "https://stelfin.example/enroll#token-for-" + scope.OwnerRef, nil
}

func (stubLinker) IssueLinkLink(_ Scope, hash string, _ time.Time) (string, error) {
	return "https://stelfin.example/link#token-for-" + hash, nil
}

func (stubLinker) IssueApproveLink(_ Scope, id store.ProposalID, _ time.Time) (string, error) {
	return fmt.Sprintf("https://stelfin.example/approve#token-for-%d", id), nil
}

// actorFor derives a stable actor for a test. Its Ref is the owner reference
// the fixture is keyed on, so a test's messages and its ledger rows agree.
func actorFor(t *testing.T) chat.Actor {
	t.Helper()
	return chat.Actor{Channel: chat.Telegram, UserID: t.Name(), Handle: "tester"}
}

// inboundIn builds a message from an actor, arriving in a registered space, in
// a group rather than a DM: the harder case, and the one every reply-visibility
// rule exists for.
//
// The space matters now. HandleInbound resolves the tenant from it before it
// looks at anything else, so a message from a space no org has claimed is
// ignored — which is correct, and would make every test here silently pass by
// doing nothing if the space were wrong.
func inboundIn(a chat.Actor, space, dedupeID, text string) chat.Inbound {
	return chat.Inbound{
		DedupeID: dedupeID,
		Actor:    a,
		Conversation: chat.Conversation{
			Channel: chat.Telegram,
			SpaceID: space,
		},
		Args:       text,
		ReceivedAt: time.Now(),
	}
}

func TestHandleSendRepliesWithAConfirmLink(t *testing.T) {
	actor := actorFor(t)
	f := newFixtureFor(t, sendDecoded(), actor.Ref())
	out := &fakeReplier{}

	msg := inboundIn(actor, f.space, "telegram:1", sendMessage)
	if err := f.svc.HandleSend(t.Context(), f.scope, msg, out, stubLinker{}); err != nil {
		t.Fatalf("HandleSend: %v", err)
	}

	body := out.only(t).Text
	for _, want := range []string{"5,000.00", "USDC", "Brother", "confirm#token-for-"} {
		if !strings.Contains(body, want) {
			t.Errorf("reply does not mention %q:\n%s", want, body)
		}
	}
	// The user's own words come back, so a decode that drifted is visible in
	// the message and not only on the confirmation page.
	if !strings.Contains(body, `"5,000"`) || !strings.Contains(body, `"brother"`) {
		t.Errorf("reply does not echo the user's words:\n%s", body)
	}
}

// TestRepliesAreAlwaysEphemeral is the group-chat rule.
//
// A confirmation link is single-use payment authority, and the messages that do
// not carry one still quote the user's payment instruction back at them.
// Neither belongs in a room several hundred people can read. The registry
// refuses an authority link that reaches it without this flag, but the flag has
// to be set here for that refusal never to fire in production.
func TestRepliesAreAlwaysEphemeral(t *testing.T) {
	actor := actorFor(t)
	f := newFixtureFor(t, sendDecoded(), actor.Ref())
	out := &fakeReplier{}

	// One success and one failure, so both reply paths are covered.
	if err := f.svc.HandleSend(t.Context(), f.scope, inboundIn(actor, f.space, "telegram:ok", sendMessage), out, stubLinker{}); err != nil {
		t.Fatalf("HandleSend: %v", err)
	}
	if err := f.svc.replyWithProblem(t.Context(), out, inboundIn(actor, f.space, "telegram:bad", "x"),
		intent.ErrDestinationNotFound); err != nil {
		t.Fatalf("replyWithProblem: %v", err)
	}

	for i, m := range out.replies() {
		if !m.Ephemeral {
			t.Errorf("reply %d was not ephemeral:\n%s", i, m.Text)
		}
	}
}

// TestHandleInboundExplainsFailures: every failure the user can act on becomes
// a specific question, and everything else becomes a generic apology that does
// not describe what broke.
func TestHandleSendExplainsFailures(t *testing.T) {
	cases := map[string]struct {
		message string
		want    string
	}{
		"unknown recipient": {
			message: "send 5,000 to landlord",
			want:    "don't have that person saved",
		},
		"undecodable": {
			message: "hello there",
			want:    "didn't catch that",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			decoded := sendDecoded()
			switch name {
			case "unknown recipient":
				decoded.Destination.Text = "landlord"
			case "undecodable":
				decoded.Action.Text = "hello"
			}

			actor := actorFor(t)
			f := newFixtureFor(t, decoded, actor.Ref())
			out := &fakeReplier{}

			msg := inboundIn(actor, f.space, "telegram:"+name, c.message)
			if err := f.svc.HandleSend(t.Context(), f.scope, msg, out, stubLinker{}); err != nil {
				t.Fatalf("HandleSend: %v", err)
			}
			if body := out.only(t).Text; !strings.Contains(body, c.want) {
				t.Errorf("reply %q does not contain %q", body, c.want)
			}
		})
	}
}

// TestHandleSendOffersEnrollmentBeforeAnAccountExists: an owner with no
// account gets a wallet-creation link instead of stelfin trying — and failing —
// to decode a payment it has no "from" account to build.
func TestHandleSendOffersEnrollmentBeforeAnAccountExists(t *testing.T) {
	ctx := context.Background()
	actor := actorFor(t)

	settle, err := settlement.NewWith(&fakeHorizon{sequence: 1}, settlement.Config{
		HorizonURL:        "https://horizon-testnet.stellar.org",
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("settlement client: %v", err)
	}
	svc, err := NewService(testPool, fixedDecoder{decoded: sendDecoded()},
		intent.NewResolver(testPool), settle,
		Config{Asset: txnbuild.CreditAsset{Code: "USDC", Issuer: testIssuer}, AssetCode: "USDC"})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	// A registered space with no member account behind it.
	org := orgFor(t, "/unenrolled")

	out := &fakeReplier{}
	msg := inboundIn(actor, spaceFor(org), "telegram:enroll", sendMessage)
	if err := svc.HandleSend(ctx, Scope{Org: org.ID, OwnerRef: actor.Ref()}, msg, out, stubLinker{}); err != nil {
		t.Fatalf("HandleSend: %v", err)
	}

	body := out.only(t).Text
	if !strings.Contains(body, "/enroll#") {
		t.Errorf("reply does not carry an enroll link:\n%s", body)
	}
	if strings.Contains(body, "confirm#") {
		t.Errorf("an owner with no account was offered a payment confirmation:\n%s", body)
	}
}

// TestFailureRepliesNeverLeakInternals: a user must not be shown an internal
// error string. Address details, SQL, and Go error text all belong in logs.
func TestFailureRepliesNeverLeakInternals(t *testing.T) {
	decoded := sendDecoded()
	decoded.Destination.Text = "landlord"

	actor := actorFor(t)
	f := newFixtureFor(t, decoded, actor.Ref())
	out := &fakeReplier{}

	msg := inboundIn(actor, f.space, "telegram:leak", "send 5,000 to landlord")
	if err := f.svc.HandleSend(t.Context(), f.scope, msg, out, stubLinker{}); err != nil {
		t.Fatalf("HandleSend: %v", err)
	}
	body := out.only(t).Text
	for _, leak := range []string{"intent:", "api:", "ledger:", "SQLSTATE", "pgx", "G" + strings.Repeat("A", 55)} {
		if strings.Contains(body, leak) {
			t.Errorf("reply leaks %q:\n%s", leak, body)
		}
	}
}
