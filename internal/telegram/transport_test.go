package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/chat/chattest"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 32 chars, the minimum

// stubAPI stands in for Telegram's Bot API, recording what was sent to it.
type stubAPI struct {
	server *httptest.Server

	mu       sync.Mutex
	calls    []recordedCall
	response map[string]string // method -> raw JSON response
	status   map[string]int
}

type recordedCall struct {
	Method string
	Body   []byte
	Path   string
}

func newStubAPI(t *testing.T) *stubAPI {
	t.Helper()
	s := &stubAPI{
		response: map[string]string{},
		status:   map[string]int{},
	}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]

		s.mu.Lock()
		s.calls = append(s.calls, recordedCall{Method: method, Body: body, Path: r.URL.Path})
		resp, ok := s.response[method]
		code := s.status[method]
		s.mu.Unlock()

		if code == 0 {
			code = http.StatusOK
		}
		if !ok {
			resp = `{"ok":true,"result":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubAPI) respond(method, body string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.response[method] = body
	s.status[method] = status
}

func (s *stubAPI) callsTo(method string) []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedCall
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func newTransport(t *testing.T, api *stubAPI) *Transport {
	t.Helper()
	tr, err := New(Config{
		Token:         "12345:TEST-TOKEN",
		WebhookSecret: testSecret,
		APIURL:        api.server.URL,
		HTTPClient:    api.server.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr
}

func signedRequest(t testing.TB, body []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhook/telegram", bytes.NewReader(body))
	r.Header.Set(secretHeader, testSecret)
	return r
}

// textUpdate builds a Telegram update carrying one text message.
func textUpdate(updateID, userID, chatID int64, chatType, text string) []byte {
	u := map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": 100,
			"from":       map[string]any{"id": userID, "is_bot": false, "username": "ada"},
			"chat":       map[string]any{"id": chatID, "type": chatType},
			"text":       text,
			"entities":   []any{},
		},
	}
	body, _ := json.Marshal(u)
	return body
}

func TestNewRejectsAWeakSecret(t *testing.T) {
	// Telegram permits a one-character secret. With no signature over the body,
	// that secret is the only thing authenticating a delivery.
	for _, secret := range []string{"", "hunter2", strings.Repeat("a", MinSecretLength-1)} {
		if _, err := New(Config{Token: "t", WebhookSecret: secret}); err == nil {
			t.Errorf("accepted a %d-character webhook secret", len(secret))
		}
	}
	if _, err := New(Config{Token: "", WebhookSecret: testSecret}); err == nil {
		t.Error("accepted an empty bot token")
	}
}

func TestVerify(t *testing.T) {
	tr := newTransport(t, newStubAPI(t))
	body := textUpdate(1, 42, -100, "supergroup", "/balance")

	got, err := tr.Verify(signedRequest(t, body))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Error("Verify returned different bytes than were delivered")
	}

	for name, mutate := range map[string]func(*http.Request){
		"no header":     func(r *http.Request) { r.Header.Del(secretHeader) },
		"wrong secret":  func(r *http.Request) { r.Header.Set(secretHeader, strings.Repeat("b", 32)) },
		"secret prefix": func(r *http.Request) { r.Header.Set(secretHeader, testSecret[:16]) },
		"secret plus":   func(r *http.Request) { r.Header.Set(secretHeader, testSecret+"x") },
	} {
		r := signedRequest(t, body)
		mutate(r)
		if _, err := tr.Verify(r); !errors.Is(err, chat.ErrUnauthenticated) {
			t.Errorf("%s: error = %v, want ErrUnauthenticated", name, err)
		}
	}
}

func TestParseTextMessage(t *testing.T) {
	tr := newTransport(t, newStubAPI(t))
	body := textUpdate(77, 42, -1001234567890, "supergroup", "send 5000 to ada")

	d, err := tr.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(d.Messages))
	}
	m := d.Messages[0]

	if m.DedupeID != "telegram:77" {
		t.Errorf("DedupeID = %q, want telegram:77", m.DedupeID)
	}
	if m.Actor.Ref() != "telegram:42" {
		t.Errorf("Actor.Ref() = %q", m.Actor.Ref())
	}
	if m.Conversation.SpaceID != "-1001234567890" {
		t.Errorf("SpaceID = %q", m.Conversation.SpaceID)
	}
	if m.Conversation.IsDM {
		t.Error("a supergroup was reported as a DM")
	}
	if m.Args != "send 5000 to ada" || m.Command != "" {
		t.Errorf("Command = %q, Args = %q", m.Command, m.Args)
	}
	// The handle is display only. Nothing may look anything up by it.
	if m.Actor.Handle != "ada" {
		t.Errorf("Handle = %q", m.Actor.Handle)
	}
}

// TestParseNeverTrustsThePayloadForPrivilege: Telegram's authentication does not
// cover the body, so anything in it claiming administrative status is worthless.
// Parse must not carry such a claim forward.
func TestParseNeverTrustsThePayloadForPrivilege(t *testing.T) {
	tr := newTransport(t, newStubAPI(t))

	// A forged delivery from someone who obtained the webhook secret, asserting
	// every privilege field a hopeful implementation might read.
	body := []byte(`{"update_id":9,"message":{"message_id":1,
		"from":{"id":42,"is_bot":false,"username":"ada","is_admin":true,"status":"creator"},
		"chat":{"id":-100,"type":"supergroup","all_members_are_administrators":true},
		"text":"/policy enroll on","entities":[{"type":"bot_command","offset":0,"length":7}]}}`)

	d, err := tr.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := d.Messages[0]
	if m.Actor.IsSpaceAdmin {
		t.Error("a privilege claim inside an unauthenticated body was believed")
	}
	if len(m.Actor.Roles) != 0 {
		t.Errorf("roles were taken from the payload: %v", m.Actor.Roles)
	}
}

func TestParseSkipsWhatItCannotActOn(t *testing.T) {
	tr := newTransport(t, newStubAPI(t))

	cases := map[string]string{
		"no message":       `{"update_id":1}`,
		"from another bot": `{"update_id":2,"message":{"message_id":1,"from":{"id":7,"is_bot":true},"chat":{"id":-1,"type":"group"},"text":"hi"}}`,
		"photo, no text":   `{"update_id":3,"message":{"message_id":1,"from":{"id":7},"chat":{"id":-1,"type":"group"},"photo":[{"file_id":"x"}]}}`,
		"whitespace only":  `{"update_id":4,"message":{"message_id":1,"from":{"id":7},"chat":{"id":-1,"type":"group"},"text":"   "}}`,
		"an edit":          `{"update_id":5,"edited_message":{"message_id":1,"from":{"id":7},"chat":{"id":-1,"type":"group"},"text":"/pay 999999"}}`,
		"a join event":     `{"update_id":6,"my_chat_member":{"chat":{"id":-1,"type":"group"}}}`,
		"no update id":     `{"message":{"message_id":1,"from":{"id":7},"chat":{"id":-1,"type":"group"},"text":"hi"}}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := tr.Parse([]byte(body))
			if err != nil {
				t.Fatalf("Parse returned an error for an ordinary delivery: %v", err)
			}
			if len(d.Messages) != 0 {
				t.Fatalf("got %d messages, want 0", len(d.Messages))
			}
			if d.Ack.Status != http.StatusOK {
				t.Errorf("ack status = %d; Telegram will retry anything else", d.Ack.Status)
			}
		})
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	tr := newTransport(t, newStubAPI(t))
	if _, err := tr.Parse([]byte("not json")); err == nil {
		t.Fatal("expected an error for an undecodable body")
	}
}

func TestSendSuppressesPreviewsAndSetsNoParseMode(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, api)

	err := tr.Send(context.Background(),
		chat.Conversation{Channel: chat.Telegram, SpaceID: "-100", IsDM: true},
		chat.Actor{Channel: chat.Telegram, UserID: "42"},
		chat.Reply{Text: "confirm: https://stelfin.example/confirm#tok"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := api.callsTo("sendMessage")
	if len(calls) != 1 {
		t.Fatalf("made %d sendMessage calls, want 1", len(calls))
	}

	var body map[string]any
	if err := json.Unmarshal(calls[0].Body, &body); err != nil {
		t.Fatalf("decode outbound body: %v", err)
	}

	opts, ok := body["link_preview_options"].(map[string]any)
	if !ok || opts["is_disabled"] != true {
		t.Errorf("link previews are not disabled: %s", calls[0].Body)
	}
	if _, present := body["parse_mode"]; present {
		t.Error("parse_mode was set; a user-controlled label could then render " +
			"as a link whose visible text and target differ")
	}
}

// TestSendDMsAnEphemeralReply: Telegram groups have no ephemeral messages, so
// the only way to keep an authority link away from bystanders is a DM.
func TestSendDMsAnEphemeralReply(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, api)

	group := chat.Conversation{Channel: chat.Telegram, SpaceID: "-1001234567890", MessageID: "55", ThreadID: "9"}
	actor := chat.Actor{Channel: chat.Telegram, UserID: "42"}

	if err := tr.Send(context.Background(), group, actor,
		chat.Reply{Text: "https://stelfin.example/confirm#tok", Ephemeral: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(api.callsTo("sendMessage")[0].Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["chat_id"] != "42" {
		t.Errorf("ephemeral reply went to %v, want the actor's own chat", body["chat_id"])
	}
	if _, present := body["reply_parameters"]; present {
		t.Error("a DM replied to a message id from a different conversation")
	}
	if body["message_thread_id"] != nil && body["message_thread_id"] != "" {
		t.Errorf("a DM carried the group's thread id: %v", body["message_thread_id"])
	}
}

// TestSendExplainsAClosedDM: a member who has never started a chat with the bot
// cannot be messaged privately. That is a person-shaped problem, so the error
// has to say so rather than look like a transient failure worth retrying.
func TestSendExplainsAClosedDM(t *testing.T) {
	api := newStubAPI(t)
	api.respond("sendMessage",
		`{"ok":false,"error_code":403,"description":"Forbidden: bot can't initiate conversation with a user"}`,
		http.StatusForbidden)
	tr := newTransport(t, api)

	err := tr.Send(context.Background(),
		chat.Conversation{Channel: chat.Telegram, SpaceID: "-100"},
		chat.Actor{Channel: chat.Telegram, UserID: "42"},
		chat.Reply{Text: "confirm here", Ephemeral: true})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "never") {
		t.Errorf("error does not explain the cause: %v", err)
	}
}

// TestErrorsNeverCarryTheBotToken: the token is in the API URL path, and net/http
// puts the URL in its error strings. An error that reaches a log would disclose
// the credential.
func TestErrorsNeverCarryTheBotToken(t *testing.T) {
	api := newStubAPI(t)
	api.respond("sendMessage", `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, http.StatusOK)
	tr := newTransport(t, api)

	err := tr.Send(context.Background(),
		chat.Conversation{Channel: chat.Telegram, SpaceID: "-100", IsDM: true},
		chat.Actor{}, chat.Reply{Text: "hi"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "TEST-TOKEN") {
		t.Fatalf("error discloses the bot token: %v", err)
	}
}

func TestSendRefusesAnEmptyMessage(t *testing.T) {
	tr := newTransport(t, newStubAPI(t))
	if err := tr.Send(context.Background(),
		chat.Conversation{SpaceID: "1", IsDM: true}, chat.Actor{}, chat.Reply{Text: "  "}); err == nil {
		t.Fatal("an empty message was accepted")
	}
}

func TestRegisterCommandsScopesTheAdminSubset(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, api)

	cmds := []chat.Command{
		{Name: "balance", Description: "show balances", MinRole: chat.RoleObserver},
		{Name: "policy", Description: "change org policy", MinRole: chat.RoleAdmin},
	}
	if err := tr.RegisterCommands(context.Background(), cmds); err != nil {
		t.Fatalf("RegisterCommands: %v", err)
	}

	calls := api.callsTo("setMyCommands")
	if len(calls) != 2 {
		t.Fatalf("made %d setMyCommands calls, want 2", len(calls))
	}

	groups := string(calls[0].Body)
	if !strings.Contains(groups, `"all_group_chats"`) {
		t.Errorf("first call is not scoped to group chats: %s", groups)
	}
	if strings.Contains(groups, `"policy"`) {
		t.Error("an admin-only command was advertised to every member; " +
			"this is a menu hint, but there is no reason to advertise it")
	}

	admins := string(calls[1].Body)
	if !strings.Contains(admins, `"all_chat_administrators"`) {
		t.Errorf("second call is not scoped to administrators: %s", admins)
	}
	for _, want := range []string{`"policy"`, `"balance"`} {
		if !strings.Contains(admins, want) {
			t.Errorf("administrators are not offered %s: %s", want, admins)
		}
	}
}

func TestIsSpaceAdmin(t *testing.T) {
	api := newStubAPI(t)
	api.respond("getChatMember", `{"ok":true,"result":{"status":"administrator"}}`, http.StatusOK)
	tr := newTransport(t, api)

	got, err := tr.IsSpaceAdmin(context.Background(), "-100", "42")
	if err != nil {
		t.Fatalf("IsSpaceAdmin: %v", err)
	}
	if !got {
		t.Error("an administrator was not recognised")
	}

	// Cached: a second ask inside the TTL must not cost another round trip.
	if _, err := tr.IsSpaceAdmin(context.Background(), "-100", "42"); err != nil {
		t.Fatalf("IsSpaceAdmin (cached): %v", err)
	}
	if n := len(api.callsTo("getChatMember")); n != 1 {
		t.Errorf("made %d getChatMember calls, want 1", n)
	}
}

func TestIsSpaceAdminRejectsOrdinaryMembers(t *testing.T) {
	for _, status := range []string{"member", "restricted", "left", "kicked"} {
		api := newStubAPI(t)
		api.respond("getChatMember", `{"ok":true,"result":{"status":"`+status+`"}}`, http.StatusOK)
		tr := newTransport(t, api)

		got, err := tr.IsSpaceAdmin(context.Background(), "-100", "42")
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if got {
			t.Errorf("%s was treated as an administrator", status)
		}
	}
}

// TestIsSpaceAdminDoesNotCacheFailures: a failed lookup must not pin a "no" for
// a minute, and must never be mistaken for an authoritative denial.
func TestIsSpaceAdminDoesNotCacheFailures(t *testing.T) {
	api := newStubAPI(t)
	api.respond("getChatMember", `{"ok":false,"error_code":400,"description":"Bad Request"}`, http.StatusOK)
	tr := newTransport(t, api)

	if _, err := tr.IsSpaceAdmin(context.Background(), "-100", "42"); err == nil {
		t.Fatal("expected an error")
	}
	api.respond("getChatMember", `{"ok":true,"result":{"status":"creator"}}`, http.StatusOK)

	got, err := tr.IsSpaceAdmin(context.Background(), "-100", "42")
	if err != nil {
		t.Fatalf("IsSpaceAdmin: %v", err)
	}
	if !got {
		t.Error("a failed lookup was cached as a denial")
	}
}

// TestTelegramSatisfiesTheConformanceSuite runs the shared rules.
func TestTelegramSatisfiesTheConformanceSuite(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, api)

	chattest.RunTransport(t, chattest.Harness{
		Transport:     tr,
		SignedRequest: func(tb testing.TB, body []byte) *http.Request { return signedRequest(tb, body) },
		SampleBody:    textUpdate(1, 42, -1001234567890, "supergroup", "send 5000 to ada"),
		// Telegram echoes a shared secret; it does not sign the body. Declared
		// so the consequence stays visible rather than being assumed away.
		BodyAuthenticated: false,
		LastOutbound: func() ([]byte, bool) {
			calls := api.callsTo("sendMessage")
			if len(calls) == 0 {
				return nil, false
			}
			return calls[len(calls)-1].Body, true
		},
		PreviewSuppressed: func(body []byte) bool {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				return false
			}
			opts, ok := m["link_preview_options"].(map[string]any)
			return ok && opts["is_disabled"] == true
		},
	})
}
