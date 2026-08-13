package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/settlement"
)

// fakeMessenger records replies instead of sending them.
type fakeMessenger struct {
	mu   sync.Mutex
	sent []struct{ To, Body string }
	err  error
}

func (m *fakeMessenger) Send(_ context.Context, to, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, struct{ To, Body string }{to, body})
	return nil
}

func (m *fakeMessenger) messages() []struct{ To, Body string } {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]struct{ To, Body string }, len(m.sent))
	copy(out, m.sent)
	return out
}

// stubLinker mints predictable links so replies can be asserted on.
type stubLinker struct{}

func (stubLinker) IssueConfirmLink(_, hash string, _ time.Time) (string, error) {
	return "https://stelfin.example/confirm#token-for-" + hash, nil
}

func (stubLinker) IssueEnrollLink(ownerRef string, _ time.Time) (string, error) {
	return "https://stelfin.example/enroll#token-for-" + ownerRef, nil
}

const metaTextPayload = `{
  "object": "whatsapp_business_account",
  "entry": [{
    "id": "123",
    "changes": [{
      "field": "messages",
      "value": {
        "messaging_product": "whatsapp",
        "messages": [{
          "id": "wamid.ABC",
          "from": "2348012345678",
          "timestamp": "1700000000",
          "type": "text",
          "text": {"body": "send 5,000 to brother"}
        }]
      }
    }]
  }]
}`

func TestParseInbound(t *testing.T) {
	got, err := ParseInbound([]byte(metaTextPayload))
	if err != nil {
		t.Fatalf("ParseInbound: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if got[0].ID != "wamid.ABC" || got[0].From != "2348012345678" {
		t.Errorf("message = %+v", got[0])
	}
	if got[0].Text != "send 5,000 to brother" {
		t.Errorf("text = %q", got[0].Text)
	}
}

// TestParseInboundSkipsWhatWeCannotAct: status callbacks and non-text messages
// are ordinary deliveries, not failures. Erroring on them would make the
// webhook look broken during normal operation.
func TestParseInboundSkipsWhatWeCannotAct(t *testing.T) {
	cases := map[string]string{
		"status callback": `{"entry":[{"changes":[{"field":"statuses","value":{
			"statuses":[{"id":"wamid.X","status":"delivered"}]}}]}]}`,
		"audio message": `{"entry":[{"changes":[{"field":"messages","value":{
			"messages":[{"id":"wamid.Y","from":"234801","type":"audio","audio":{"id":"a"}}]}}]}]}`,
		"no message id": `{"entry":[{"changes":[{"field":"messages","value":{
			"messages":[{"from":"234801","type":"text","text":{"body":"hi"}}]}}]}]}`,
		"empty entry": `{"entry":[]}`,
		"no entry":    `{"object":"whatsapp_business_account"}`,
	}
	for name, body := range cases {
		got, err := ParseInbound([]byte(body))
		if err != nil {
			t.Errorf("%s: ParseInbound returned an error: %v", name, err)
			continue
		}
		if len(got) != 0 {
			t.Errorf("%s: got %d messages, want 0", name, len(got))
		}
	}
}

func TestParseInboundRejectsGarbage(t *testing.T) {
	if _, err := ParseInbound([]byte("not json")); err == nil {
		t.Fatal("expected an error for an unparseable payload")
	}
}

func TestHandleInboundRepliesWithAConfirmLink(t *testing.T) {
	// Meta reports the sender without a leading '+', and HandleInbound
	// normalizes to E.164 — so the fixture's user must be keyed the same way.
	phone := phoneFor(t)
	f := newFixtureFor(t, sendDecoded(), phone)
	msgr := &fakeMessenger{}

	msg := InboundMessage{ID: "wamid.1", From: strings.TrimPrefix(phone, "+"), Text: sendMessage}

	if err := f.svc.HandleInbound(t.Context(), msg, msgr, stubLinker{}); err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}

	sent := msgr.messages()
	if len(sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(sent))
	}
	body := sent[0].Body
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

// TestHandleInboundIsIdempotent: Meta retries deliveries, and one instruction
// must not produce two confirmations.
func TestHandleInboundIsIdempotent(t *testing.T) {
	phone := phoneFor(t)
	f := newFixtureFor(t, sendDecoded(), phone)
	msgr := &fakeMessenger{}
	msg := InboundMessage{ID: "wamid.dup", From: strings.TrimPrefix(phone, "+"), Text: sendMessage}

	for i := 0; i < 3; i++ {
		if err := f.svc.HandleInbound(t.Context(), msg, msgr, stubLinker{}); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if got := len(msgr.messages()); got != 1 {
		t.Errorf("sent %d replies for 3 deliveries of one message, want 1", got)
	}
}

// TestConcurrentDeliveriesReplyOnce: two retries arriving together must not
// both win the claim.
func TestConcurrentDeliveriesReplyOnce(t *testing.T) {
	phone := phoneFor(t)
	f := newFixtureFor(t, sendDecoded(), phone)
	msgr := &fakeMessenger{}
	msg := InboundMessage{ID: "wamid.race", From: strings.TrimPrefix(phone, "+"), Text: sendMessage}

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.svc.HandleInbound(context.Background(), msg, msgr, stubLinker{}); err != nil {
				t.Errorf("HandleInbound: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(msgr.messages()); got != 1 {
		t.Errorf("sent %d replies for %d concurrent deliveries, want 1", got, workers)
	}
}

// TestHandleInboundExplainsFailures: every failure the user can act on becomes
// a specific question, and everything else becomes a generic apology that does
// not describe what broke.
func TestHandleInboundExplainsFailures(t *testing.T) {
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

			phone := phoneFor(t)
			f := newFixtureFor(t, decoded, phone)
			msgr := &fakeMessenger{}
			msg := InboundMessage{ID: "wamid." + name, From: strings.TrimPrefix(phone, "+"), Text: c.message}

			if err := f.svc.HandleInbound(t.Context(), msg, msgr, stubLinker{}); err != nil {
				t.Fatalf("HandleInbound: %v", err)
			}
			sent := msgr.messages()
			if len(sent) != 1 {
				t.Fatalf("sent %d replies, want 1", len(sent))
			}
			if !strings.Contains(sent[0].Body, c.want) {
				t.Errorf("reply %q does not contain %q", sent[0].Body, c.want)
			}
		})
	}
}

// TestHandleInboundOffersEnrollmentBeforeAnAccountExists: an unenrolled phone
// number gets a wallet-creation link instead of stelfin trying — and failing
// — to decode a payment it has no "from" account to build.
func TestHandleInboundOffersEnrollmentBeforeAnAccountExists(t *testing.T) {
	ctx := context.Background()
	owner := t.Name()

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

	msgr := &fakeMessenger{}
	msg := InboundMessage{ID: "wamid.enroll", From: strings.TrimPrefix(owner, "+"), Text: sendMessage}
	if err := svc.HandleInbound(ctx, msg, msgr, stubLinker{}); err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}

	sent := msgr.messages()
	if len(sent) != 1 {
		t.Fatalf("sent %d replies, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Body, "/enroll#") {
		t.Errorf("reply does not carry an enroll link:\n%s", sent[0].Body)
	}
	if strings.Contains(sent[0].Body, "confirm#") {
		t.Errorf("an unenrolled user was offered a payment confirmation:\n%s", sent[0].Body)
	}
}

// TestFailureRepliesNeverLeakInternals: a user must not be shown an internal
// error string. Address details, SQL, and Go error text all belong in logs.
func TestFailureRepliesNeverLeakInternals(t *testing.T) {
	decoded := sendDecoded()
	decoded.Destination.Text = "landlord"

	phone := phoneFor(t)
	f := newFixtureFor(t, decoded, phone)
	msgr := &fakeMessenger{}
	msg := InboundMessage{ID: "wamid.leak", From: strings.TrimPrefix(phone, "+"), Text: "send 5,000 to landlord"}

	if err := f.svc.HandleInbound(t.Context(), msg, msgr, stubLinker{}); err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}
	body := msgr.messages()[0].Body
	for _, leak := range []string{"intent:", "api:", "ledger:", "SQLSTATE", "pgx", "G" + strings.Repeat("A", 55)} {
		if strings.Contains(body, leak) {
			t.Errorf("reply leaks %q:\n%s", leak, body)
		}
	}
}
