package discord

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/chat/chattest"
)

const (
	testAppID    = "111111111111111111"
	testBotToken = "bot-token-not-a-real-one"
)

// signer holds the keypair a test signs deliveries with, standing in for
// Discord's application key.
type signer struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
	now     func() time.Time
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	return &signer{public: pub, private: priv, now: func() time.Time { return fixed }}
}

// request builds a delivery signed the way Discord signs one: over the
// timestamp header concatenated with the exact body bytes.
func (s *signer) request(t testing.TB, body []byte) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(s.now().Unix(), 10)
	sig := ed25519.Sign(s.private, append([]byte(ts), body...))

	r := httptest.NewRequest(http.MethodPost, "/webhook/discord", bytes.NewReader(body))
	r.Header.Set(timestampHeader, ts)
	r.Header.Set(signatureHeader, hex.EncodeToString(sig))
	return r
}

// stubAPI stands in for Discord's REST API.
type stubAPI struct {
	server *httptest.Server

	mu     sync.Mutex
	calls  []recordedCall
	status map[string]int
}

type recordedCall struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

func newStubAPI(t *testing.T) *stubAPI {
	t.Helper()
	s := &stubAPI{status: map[string]int{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

		s.mu.Lock()
		s.calls = append(s.calls, recordedCall{
			Method: r.Method, Path: r.URL.Path,
			Auth: r.Header.Get("Authorization"), Body: body,
		})
		code := s.status[r.Method+" "+r.URL.Path]
		if code == 0 {
			code = s.status["*"]
		}
		s.mu.Unlock()

		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubAPI) failAll(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status["*"] = code
}

func (s *stubAPI) recorded() []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedCall(nil), s.calls...)
}

func newTransport(t *testing.T, s *signer, api *stubAPI) *Transport {
	t.Helper()
	tr, err := New(Config{
		PublicKey:     hex.EncodeToString(s.public),
		BotToken:      testBotToken,
		ApplicationID: testAppID,
		APIURL:        api.server.URL,
		HTTPClient:    api.server.Client(),
		Now:           s.now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr
}

// commandInteraction builds a guild slash-command interaction.
func commandInteraction(id, name, userID, permissions string, options ...map[string]any) []byte {
	data := map[string]any{"id": "cmd", "name": name}
	if len(options) > 0 {
		data["options"] = options
	}
	body, _ := json.Marshal(map[string]any{
		"id":         id,
		"type":       interactionCommand,
		"token":      "interaction-token",
		"guild_id":   "222222222222222222",
		"channel_id": "333333333333333333",
		"data":       data,
		"member": map[string]any{
			"user":        map[string]any{"id": userID, "username": "ada"},
			"roles":       []string{"444444444444444444"},
			"permissions": permissions,
		},
	})
	return body
}

func stringOption(name, value string) map[string]any {
	return map[string]any{"name": name, "type": optionTypeString, "value": value}
}

func TestNewValidatesCredentials(t *testing.T) {
	s := newSigner(t)
	valid := hex.EncodeToString(s.public)

	for name, cfg := range map[string]Config{
		"not hex":       {PublicKey: "zzzz", BotToken: "t", ApplicationID: "a"},
		"wrong length":  {PublicKey: "aabb", BotToken: "t", ApplicationID: "a"},
		"no bot token":  {PublicKey: valid, ApplicationID: "a"},
		"no app id":     {PublicKey: valid, BotToken: "t"},
		"no public key": {BotToken: "t", ApplicationID: "a"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVerify(t *testing.T) {
	s := newSigner(t)
	tr := newTransport(t, s, newStubAPI(t))
	body := commandInteraction("1", "balance", "42", "0")

	got, err := tr.Verify(s.request(t, body))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Error("Verify returned different bytes than were delivered")
	}
}

// TestVerifyRejectsATamperedBody is the property Telegram cannot offer and
// Discord can: the signature covers the body, so changing one byte breaks it.
func TestVerifyRejectsATamperedBody(t *testing.T) {
	s := newSigner(t)
	tr := newTransport(t, s, newStubAPI(t))
	body := commandInteraction("1", "balance", "42", "0")

	authentic := s.request(t, body)

	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-1] ^= 0x01
	req := httptest.NewRequest(http.MethodPost, "/webhook/discord", bytes.NewReader(tampered))
	req.Header = authentic.Header.Clone()

	if _, err := tr.Verify(req); !errors.Is(err, chat.ErrUnauthenticated) {
		t.Fatalf("a body that differs from the signed one was accepted: %v", err)
	}
}

// TestVerifyRejectsReserializedJSON pins the mistake that breaks most webhook
// implementations: verifying re-encoded JSON rather than the bytes received.
// These two bodies are the same object and different bytes.
func TestVerifyRejectsReserializedJSON(t *testing.T) {
	s := newSigner(t)
	tr := newTransport(t, s, newStubAPI(t))

	received := []byte(`{"b":1,"a":2}`)
	reserialized := []byte(`{"a":2,"b":1}`)

	signed := s.request(t, received)
	req := httptest.NewRequest(http.MethodPost, "/webhook/discord", bytes.NewReader(reserialized))
	req.Header = signed.Header.Clone()

	if _, err := tr.Verify(req); !errors.Is(err, chat.ErrUnauthenticated) {
		t.Error("re-serialized JSON verified against the raw-body signature; " +
			"an implementation that parses before verifying would appear to work " +
			"and would be checking nothing")
	}
}

func TestVerifyRejectsBadSignatures(t *testing.T) {
	s := newSigner(t)
	tr := newTransport(t, s, newStubAPI(t))
	body := commandInteraction("1", "balance", "42", "0")

	// Discord deliberately sends invalid signatures when an endpoint is
	// registered, and refuses the endpoint if any of them gets a 2xx.
	other := newSigner(t)

	for name, mutate := range map[string]func(*http.Request){
		"no signature":    func(r *http.Request) { r.Header.Del(signatureHeader) },
		"no timestamp":    func(r *http.Request) { r.Header.Del(timestampHeader) },
		"not hex":         func(r *http.Request) { r.Header.Set(signatureHeader, "zzzz") },
		"short signature": func(r *http.Request) { r.Header.Set(signatureHeader, "aabb") },
		"another key":     func(r *http.Request) { *r = *other.request(t, body) },
		"timestamp swap":  func(r *http.Request) { r.Header.Set(timestampHeader, "1700000001") },
		"garbage timestamp": func(r *http.Request) {
			r.Header.Set(timestampHeader, "not-a-number")
		},
	} {
		req := s.request(t, body)
		mutate(req)
		if name == "another key" {
			// The whole request was replaced with one signed by a different
			// application; it must not verify against ours.
			if _, err := tr.Verify(req); !errors.Is(err, chat.ErrUnauthenticated) {
				t.Errorf("%s: error = %v, want ErrUnauthenticated", name, err)
			}
			continue
		}
		if _, err := tr.Verify(req); !errors.Is(err, chat.ErrUnauthenticated) {
			t.Errorf("%s: error = %v, want ErrUnauthenticated", name, err)
		}
	}
}

// TestVerifyRejectsAStaleDelivery: without a freshness bound, a captured
// interaction replays forever, because its signature never stops being valid.
func TestVerifyRejectsAStaleDelivery(t *testing.T) {
	s := newSigner(t)
	tr := newTransport(t, s, newStubAPI(t))
	body := commandInteraction("1", "balance", "42", "0")
	req := s.request(t, body)

	// Same signed delivery, replayed an hour later.
	later := s.now().Add(time.Hour)
	tr.now = func() time.Time { return later }

	if _, err := tr.Verify(req); !errors.Is(err, chat.ErrUnauthenticated) {
		t.Fatalf("a delivery an hour old was accepted: %v", err)
	}
}

// TestParsePingAnswersPong: the liveness check Discord runs against a
// registered endpoint. Anything else and the endpoint is refused.
func TestParsePingAnswersPong(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))

	d, err := tr.Parse([]byte(`{"id":"1","type":1,"token":"t"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Messages) != 0 {
		t.Fatalf("a ping produced %d messages", len(d.Messages))
	}
	if got := string(d.Ack.Body); got != `{"type":1}` {
		t.Errorf("ack = %s, want a PONG", got)
	}
}

// TestParseDefersEphemerally is the three-second budget and the privacy rule at
// once: the acknowledgement promises a reply within fifteen minutes, and fixes
// it as visible only to the invoker. The flag cannot be changed afterwards, so
// getting it wrong here makes every later reply public.
func TestParseDefersEphemerally(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))

	d, err := tr.Parse(commandInteraction("55", "balance", "42", "0"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var resp struct {
		Type int `json:"type"`
		Data struct {
			Flags int `json:"flags"`
		} `json:"data"`
	}
	if err := json.Unmarshal(d.Ack.Body, &resp); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if resp.Type != responseDeferredEphemeral {
		t.Errorf("ack type = %d, want %d (deferred)", resp.Type, responseDeferredEphemeral)
	}
	if resp.Data.Flags&flagEphemeral == 0 {
		t.Error("the deferred response is not ephemeral, so every later reply is public")
	}
}

func TestParseCommandInteraction(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))

	d, err := tr.Parse(commandInteraction("55", "ASK", "42", "0", stringOption("text", "send 5000 to ada")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(d.Messages))
	}
	m := d.Messages[0]

	if m.DedupeID != "discord:55" {
		t.Errorf("DedupeID = %q", m.DedupeID)
	}
	if m.Actor.Ref() != "discord:42" {
		t.Errorf("Actor.Ref() = %q", m.Actor.Ref())
	}
	if m.Command != "ask" {
		t.Errorf("Command = %q, want the lowercased name", m.Command)
	}
	if m.Args != "send 5000 to ada" {
		t.Errorf("Args = %q", m.Args)
	}
	if m.Options["text"] != "send 5000 to ada" {
		t.Errorf("Options = %v", m.Options)
	}
	if m.Conversation.SpaceID != "222222222222222222" {
		t.Errorf("SpaceID = %q, want the guild", m.Conversation.SpaceID)
	}
	if m.Conversation.ThreadID != "333333333333333333" {
		t.Errorf("ThreadID = %q, want the channel", m.Conversation.ThreadID)
	}
	if m.Conversation.InteractionToken != "interaction-token" {
		t.Errorf("InteractionToken = %q", m.Conversation.InteractionToken)
	}
	if m.Conversation.IsDM {
		t.Error("a guild interaction was reported as a DM")
	}
	if len(m.Actor.Roles) != 1 {
		t.Errorf("Roles = %v", m.Actor.Roles)
	}
}

// TestArgsIsEmptyForMultipleOptions: Args is defined as the exact string that
// gets tokenized and shown back as what the user said. A command with several
// typed options has no such string, and inventing one would hand the tokenizer
// a sentence nobody typed.
func TestArgsIsEmptyForMultipleOptions(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))

	d, err := tr.Parse(commandInteraction("55", "pay", "42", "0",
		stringOption("amount", "5,000"), stringOption("to", "ada")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := d.Messages[0]

	if m.Args != "" {
		t.Errorf("Args = %q, want empty for a multi-option command", m.Args)
	}
	if m.Options["amount"] != "5,000" || m.Options["to"] != "ada" {
		t.Errorf("Options = %v", m.Options)
	}
}

// TestNumericOptionsKeepTheirWrittenForm: an amount that renders differently
// than it was typed is exactly what the verification scheme exists to prevent,
// so a value never passes through a Go numeric type on the way in.
func TestNumericOptionsKeepTheirWrittenForm(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))

	body, _ := json.Marshal(map[string]any{
		"id": "55", "type": interactionCommand, "token": "t",
		"guild_id": "2", "channel_id": "3",
		"data": map[string]any{"name": "pay", "options": []map[string]any{
			{"name": "amount", "type": 10, "value": json.RawMessage("5000.0000001")},
		}},
		"member": map[string]any{"user": map[string]any{"id": "42"}, "permissions": "0"},
	})

	d, err := tr.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := d.Messages[0].Options["amount"]; got != "5000.0000001" {
		t.Errorf("amount = %q, want the digits as written", got)
	}
}

// TestParseTrustsPermissionsBecauseTheBodyIsSigned is the difference from
// Telegram, stated as a test. Discord's Ed25519 signature covers the body, so
// the permission bits inside it are authenticated and need no round trip.
func TestParseTrustsPermissionsBecauseTheBodyIsSigned(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))

	for permissions, wantAdmin := range map[string]bool{
		"0":               false,
		"8":               true,  // ADMINISTRATOR
		"32":              true,  // MANAGE_GUILD
		"2147483647":      true,  // everything
		"1024":            false, // VIEW_CHANNEL only
		"not-a-number":    false,
		"562949953421312": false, // a high bit that is neither
	} {
		d, err := tr.Parse(commandInteraction("55", "policy", "42", permissions))
		if err != nil {
			t.Fatalf("%s: %v", permissions, err)
		}
		if got := d.Messages[0].Actor.IsSpaceAdmin; got != wantAdmin {
			t.Errorf("permissions %q: IsSpaceAdmin = %v, want %v", permissions, got, wantAdmin)
		}
	}
}

func TestParseDirectMessage(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))

	body, _ := json.Marshal(map[string]any{
		"id": "55", "type": interactionCommand, "token": "t",
		"channel_id": "999",
		"data":       map[string]any{"name": "balance"},
		"user":       map[string]any{"id": "42", "username": "ada"},
	})

	d, err := tr.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := d.Messages[0]
	if !m.Conversation.IsDM {
		t.Error("a direct message was not recognised as one")
	}
	if m.Actor.UserID != "42" {
		t.Errorf("UserID = %q; a DM carries the user at the top level", m.Actor.UserID)
	}
	if m.Actor.IsSpaceAdmin {
		t.Error("a DM conferred administrative authority")
	}
}

func TestParseRefusesBotsAndUnreadableInteractions(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))

	cases := map[string]string{
		"a bot":           `{"id":"1","type":2,"token":"t","channel_id":"9","data":{"name":"pay"},"user":{"id":"7","bot":true}}`,
		"no data":         `{"id":"1","type":2,"token":"t","channel_id":"9","user":{"id":"7"}}`,
		"no user":         `{"id":"1","type":2,"token":"t","channel_id":"9","data":{"name":"pay"}}`,
		"no id":           `{"type":2,"token":"t","channel_id":"9","data":{"name":"pay"},"user":{"id":"7"}}`,
		"a modal submit":  `{"id":"1","type":5,"token":"t","channel_id":"9","data":{}}`,
		"an autocomplete": `{"id":"1","type":4,"token":"t","channel_id":"9","data":{}}`,
		"a component":     `{"id":"1","type":3,"token":"t","channel_id":"9","data":{}}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := tr.Parse([]byte(body))
			if err != nil {
				t.Fatalf("Parse returned an error for an ordinary interaction: %v", err)
			}
			if len(d.Messages) != 0 {
				t.Fatalf("got %d messages, want 0", len(d.Messages))
			}
			// It must close the interaction rather than defer: a deferred
			// response promises a reply, and nothing here will send one.
			var resp struct {
				Type int `json:"type"`
			}
			if err := json.Unmarshal(d.Ack.Body, &resp); err != nil {
				t.Fatalf("decode ack: %v", err)
			}
			if resp.Type == responseDeferredEphemeral {
				t.Error("deferred a reply that will never arrive; " +
					"the invoker watches a spinner until it times out")
			}
		})
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))
	if _, err := tr.Parse([]byte("not json")); err == nil {
		t.Fatal("expected an error for an undecodable body")
	}
}

func TestSendEditsTheDeferredMessage(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, newSigner(t), api)

	err := tr.Send(context.Background(), chat.Conversation{
		Channel: chat.Discord, SpaceID: "2", ThreadID: "3", InteractionToken: "tok",
	}, chat.Actor{UserID: "42"}, chat.Reply{Text: "confirm: https://x.test/confirm#tok", Ephemeral: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := api.recorded()
	if len(calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(calls))
	}
	c := calls[0]
	if c.Method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", c.Method)
	}
	if want := "/webhooks/" + testAppID + "/tok/messages/@original"; c.Path != want {
		t.Errorf("path = %s, want %s", c.Path, want)
	}
	// The interaction token in the path is the credential. Discord rejects a
	// bot-authenticated followup.
	if c.Auth != "" {
		t.Errorf("followup carried an Authorization header: %q", c.Auth)
	}

	var body map[string]any
	if err := json.Unmarshal(c.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	flags, _ := body["flags"].(float64)
	if int(flags)&flagSuppressEmbeds == 0 {
		t.Error("embeds are not suppressed; Discord's crawler would spend the token")
	}
	mentions, ok := body["allowed_mentions"].(map[string]any)
	if !ok || len(mentions["parse"].([]any)) != 0 {
		t.Errorf("allowed_mentions is not empty: %s", c.Body)
	}
}

// TestSendRefusesToFallBackPublicly: fifteen minutes after the interaction the
// followup route is gone, and the only thing left is an ordinary channel post.
// Doing that with a confirmation link hands payment authority to everyone who
// can read the channel.
func TestSendRefusesToFallBackPublicly(t *testing.T) {
	api := newStubAPI(t)
	api.failAll(http.StatusNotFound) // an expired interaction token
	tr := newTransport(t, newSigner(t), api)

	err := tr.Send(context.Background(), chat.Conversation{
		Channel: chat.Discord, SpaceID: "2", ThreadID: "3", InteractionToken: "expired",
	}, chat.Actor{UserID: "42"}, chat.Reply{Text: "https://x.test/confirm#tok", Ephemeral: true})

	if !errors.Is(err, chat.ErrLinkInPublic) {
		t.Fatalf("error = %v, want ErrLinkInPublic", err)
	}
	for _, c := range api.recorded() {
		if c.Method == http.MethodPost {
			t.Fatal("a private reply was posted to the channel anyway")
		}
	}
}

// TestSendRefusesAPrivateReplyWithNoInteraction: without an interaction there
// is no private route in a guild at all.
func TestSendRefusesAPrivateReplyWithNoInteraction(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, newSigner(t), api)

	err := tr.Send(context.Background(),
		chat.Conversation{Channel: chat.Discord, SpaceID: "2", ThreadID: "3"},
		chat.Actor{UserID: "42"}, chat.Reply{Text: "secret", Ephemeral: true})

	if !errors.Is(err, chat.ErrLinkInPublic) {
		t.Fatalf("error = %v, want ErrLinkInPublic", err)
	}
	if len(api.recorded()) != 0 {
		t.Fatal("the refused reply was still sent")
	}
}

// TestSendFallsBackInADirectMessage: a DM has no bystanders, so an expired
// interaction can be answered by posting to the channel.
func TestSendFallsBackInADirectMessage(t *testing.T) {
	api := newStubAPI(t)
	api.status["PATCH /webhooks/"+testAppID+"/expired/messages/@original"] = http.StatusNotFound
	tr := newTransport(t, newSigner(t), api)

	err := tr.Send(context.Background(), chat.Conversation{
		Channel: chat.Discord, SpaceID: "999", ThreadID: "999",
		InteractionToken: "expired", IsDM: true,
	}, chat.Actor{UserID: "42"}, chat.Reply{Text: "your balance is 12 USDC", Ephemeral: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var posted bool
	for _, c := range api.recorded() {
		if c.Method == http.MethodPost && c.Path == "/channels/999/messages" {
			posted = true
			if c.Auth != "Bot "+testBotToken {
				t.Errorf("a channel post is not bot-authenticated: %q", c.Auth)
			}
		}
	}
	if !posted {
		t.Error("no fallback channel post was made")
	}
}

func TestSendRefusesAnEmptyMessage(t *testing.T) {
	tr := newTransport(t, newSigner(t), newStubAPI(t))
	if err := tr.Send(context.Background(),
		chat.Conversation{SpaceID: "1", IsDM: true}, chat.Actor{}, chat.Reply{Text: " "}); err == nil {
		t.Fatal("an empty message was accepted")
	}
}

func TestRegisterCommandsOverwritesInBulk(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, newSigner(t), api)

	err := tr.RegisterCommands(context.Background(), []chat.Command{
		{Name: "balance", Description: "show balances", MinRole: chat.RoleObserver},
		{
			Name: "policy", Description: "change org policy", MinRole: chat.RoleAdmin,
			Options: []chat.Option{{Name: "key", Description: "setting", Type: chat.OptString, Required: true}},
		},
	})
	if err != nil {
		t.Fatalf("RegisterCommands: %v", err)
	}

	calls := api.recorded()
	if len(calls) != 1 {
		t.Fatalf("made %d calls, want 1 bulk overwrite", len(calls))
	}
	if calls[0].Method != http.MethodPut {
		t.Errorf("method = %s, want PUT — a command removed from the code must "+
			"disappear from the guild rather than linger", calls[0].Method)
	}
	if want := "/applications/" + testAppID + "/commands"; calls[0].Path != want {
		t.Errorf("path = %s, want %s", calls[0].Path, want)
	}

	var registered []map[string]any
	if err := json.Unmarshal(calls[0].Body, &registered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(registered) != 2 {
		t.Fatalf("registered %d commands, want 2", len(registered))
	}
	for _, c := range registered {
		switch c["name"] {
		case "balance":
			if c["default_member_permissions"] != nil {
				t.Errorf("an observer command was hidden: %v", c)
			}
		case "policy":
			if c["default_member_permissions"] != "0" {
				t.Errorf("an admin command is visible by default: %v", c)
			}
			if len(c["options"].([]any)) != 1 {
				t.Errorf("options were lost: %v", c)
			}
		}
	}
}

// TestErrorsNeverCarryTheBotToken: the token authenticates every REST call, and
// an error that reaches a log must not disclose it.
func TestErrorsNeverCarryTheBotToken(t *testing.T) {
	api := newStubAPI(t)
	api.failAll(http.StatusInternalServerError)
	tr := newTransport(t, newSigner(t), api)

	err := tr.RegisterCommands(context.Background(), []chat.Command{{Name: "x", Description: "y"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Fatalf("error discloses the bot token: %v", err)
	}
}

// TestDiscordSatisfiesTheConformanceSuite runs the shared rules, this time with
// body authentication actually available.
func TestDiscordSatisfiesTheConformanceSuite(t *testing.T) {
	s := newSigner(t)
	api := newStubAPI(t)
	tr := newTransport(t, s, api)

	chattest.RunTransport(t, chattest.Harness{
		Transport:         tr,
		SignedRequest:     func(tb testing.TB, body []byte) *http.Request { return s.request(tb, body) },
		SampleBody:        commandInteraction("55", "ask", "42", "0", stringOption("text", "send 5000 to ada")),
		BodyAuthenticated: true,
		PrivateConversation: func(c chat.Channel) chat.Conversation {
			// A live interaction is the only private route Discord offers in a
			// guild, and it is the one every reply actually travels on.
			return chat.Conversation{Channel: c, SpaceID: "2", ThreadID: "3", InteractionToken: "tok"}
		},
		LastOutbound: func() ([]byte, bool) {
			calls := api.recorded()
			for i := len(calls) - 1; i >= 0; i-- {
				if len(calls[i].Body) > 0 && calls[i].Path != "" &&
					(calls[i].Method == http.MethodPatch || calls[i].Method == http.MethodPost) {
					return calls[i].Body, true
				}
			}
			return nil, false
		},
		PreviewSuppressed: func(body []byte) bool {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				return false
			}
			flags, _ := m["flags"].(float64)
			return int(flags)&flagSuppressEmbeds != 0
		},
	})
}
