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
	"unicode/utf16"

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

// The bot the stub API says this is.
const (
	botID   = 9000
	botName = "stelfin_test_bot"
)

// started returns a transport that has asked who it is and been told.
func started(t *testing.T, api *stubAPI) *Transport {
	t.Helper()
	api.respond("getMe", `{"ok":true,"result":{"id":9000,"is_bot":true,"username":"stelfin_test_bot"}}`, http.StatusOK)
	tr := newTransport(t, api)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return tr
}

// groupUpdate is a message in a supergroup, with whatever entities Telegram
// would have annotated and, when replyTo is not zero, as a reply to a message
// from that user.
func groupUpdate(updateID int64, text string, entities []map[string]any, replyTo int64) []byte {
	if entities == nil {
		entities = []map[string]any{}
	}
	message := map[string]any{
		"message_id": 100,
		"from":       map[string]any{"id": 42, "is_bot": false, "username": "ada"},
		"chat":       map[string]any{"id": -1001234567890, "type": "supergroup"},
		"text":       text,
		"entities":   entities,
	}
	if replyTo != 0 {
		message["reply_to_message"] = map[string]any{
			"from": map[string]any{"id": replyTo, "is_bot": replyTo == botID},
		}
	}
	body, _ := json.Marshal(map[string]any{"update_id": updateID, "message": message})
	return body
}

// mention annotates text[offset:offset+length] the way Telegram does an @name.
// The offsets are UTF-16 code units, which for the ASCII these tests use are
// the same as bytes.
func mention(offset, length int) map[string]any {
	return map[string]any{"type": "mention", "offset": offset, "length": length}
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
	tr := started(t, newStubAPI(t))
	// Words in a group are an instruction only when said to the bot; see
	// TestGroupTextIsReadOnlyWhenAddressed. A reply to one of its messages is.
	body := groupUpdate(77, "send 5000 to ada", nil, botID)

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

	// The error reaches a log. The person who can fix it is in the group, and
	// has to be told there — in words that repeat nothing of the reply.
	calls := api.callsTo("sendMessage")
	if len(calls) != 2 {
		t.Fatalf("made %d sendMessage calls, want the private attempt and a notice", len(calls))
	}
	notice := string(calls[1].Body)
	if !strings.Contains(notice, `"chat_id":"-100"`) {
		t.Errorf("the notice did not go to the group: %s", notice)
	}
	if !strings.Contains(notice, "Start") {
		t.Errorf("the notice does not say what to do: %s", notice)
	}
	if strings.Contains(notice, "confirm here") {
		t.Errorf("the notice repeats the private reply where the room can read it: %s", notice)
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
	if len(calls) != 3 {
		t.Fatalf("made %d setMyCommands calls, want 3", len(calls))
	}

	// A private chat is where the bot sends people and where only an
	// introduction happens. Its menu offers what gets one, and none of the
	// workspace commands, which would all answer "not here".
	private := string(calls[2].Body)
	if !strings.Contains(private, `"all_private_chats"`) {
		t.Errorf("third call is not scoped to private chats: %s", private)
	}
	for _, want := range []string{`"start"`, `"help"`} {
		if !strings.Contains(private, want) {
			t.Errorf("a private chat is not offered %s: %s", want, private)
		}
	}
	for _, not := range []string{`"balance"`, `"policy"`} {
		if strings.Contains(private, not) {
			t.Errorf("a private chat is offered %s, which does nothing there: %s", not, private)
		}
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

// A hyphen is not legal in a Telegram command, and one illegal name fails the
// whole setMyCommands call — so a single hyphenated command would take the menu
// away for every other one.
func TestRegisterCommandsPublishesNamesTelegramAccepts(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, api)

	cmds := []chat.Command{
		{Name: "link-code", Description: "get a code", MinRole: chat.RoleNone},
		{Name: "link-treasury", Description: "prove an account", MinRole: chat.RoleAdmin},
	}
	if err := tr.RegisterCommands(context.Background(), cmds); err != nil {
		t.Fatalf("RegisterCommands: %v", err)
	}
	for _, call := range api.callsTo("setMyCommands") {
		body := string(call.Body)
		if strings.Contains(body, "link-") {
			t.Errorf("published a hyphenated command name: %s", body)
		}
		if strings.Contains(body, `"all_private_chats"`) {
			// The private-chat menu carries no workspace commands at all.
			continue
		}
		if !strings.Contains(body, `"link_code"`) {
			t.Errorf("link-code was not published as link_code: %s", body)
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

// TestParsePicksAPersonByIDNeverByHandle: "/grant approver" sent as a reply
// means the person replied to, by the id Telegram assigned them. A handle in
// the text is just text: renameable, and reassignable once released.
func TestParsePicksAPersonByIDNeverByHandle(t *testing.T) {
	tr := newTransport(t, newStubAPI(t))
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"a reply": {`{"update_id":31,"message":{"message_id":5,"from":{"id":42},"chat":{"id":-100,"type":"supergroup"},
			"text":"/grant approver","entities":[{"type":"bot_command","offset":0,"length":6}],
			"reply_to_message":{"from":{"id":77,"username":"bo"}}}}`, "77"},
		"a text mention": {`{"update_id":32,"message":{"message_id":5,"from":{"id":42},"chat":{"id":-100,"type":"supergroup"},
			"text":"/grant Bo approver","entities":[{"type":"bot_command","offset":0,"length":6},
			{"type":"text_mention","offset":7,"length":2,"user":{"id":78}}]}}`, "78"},
		"a reply to the bot itself": {`{"update_id":33,"message":{"message_id":5,"from":{"id":42},"chat":{"id":-100,"type":"supergroup"},
			"text":"/grant approver","entities":[{"type":"bot_command","offset":0,"length":6}],
			"reply_to_message":{"from":{"id":9,"is_bot":true}}}}`, ""},
		"an @handle": {`{"update_id":34,"message":{"message_id":5,"from":{"id":42},"chat":{"id":-100,"type":"supergroup"},
			"text":"/grant @bo approver","entities":[{"type":"bot_command","offset":0,"length":6},
			{"type":"mention","offset":7,"length":3}]}}`, ""},
	} {
		d, err := tr.Parse([]byte(tc.body))
		if err != nil || len(d.Messages) != 1 {
			t.Fatalf("%s: parse: %v (%d messages)", name, err, len(d.Messages))
		}
		if got := d.Messages[0].Options["user"]; got != tc.want {
			t.Errorf("%s: picked %q, want %q", name, got, tc.want)
		}
	}
}

// TestGroupTextIsReadOnlyWhenAddressed: a group is other people's conversation.
// With privacy mode off, or the bot made an administrator, Telegram delivers
// all of it, and every line used to be read as a possible payment — a model
// call each, and for anyone without a wallet a private message from a bot they
// had not spoken to.
func TestGroupTextIsReadOnlyWhenAddressed(t *testing.T) {
	at := "@" + botName
	cases := map[string]struct {
		text     string
		entities []map[string]any
		replyTo  int64
		want     string // the text handed on, or "" for nothing at all
	}{
		"two people talking":           {text: "lunch at one?"},
		"a payment said to the room":   {text: "send 5000 to ada"},
		"opens with the bot's name":    {text: at + " send 5 to ada", entities: []map[string]any{mention(0, len(at))}, want: "send 5 to ada"},
		"name, then a comma":           {text: at + ", send 5 to ada", entities: []map[string]any{mention(0, len(at))}, want: "send 5 to ada"},
		"the name in another case":     {text: "@Stelfin_Test_Bot send 5 to ada", entities: []map[string]any{mention(0, len(at))}, want: "send 5 to ada"},
		"a reply to the bot":           {text: "send 5 to ada", replyTo: botID, want: "send 5 to ada"},
		"a reply to somebody else":     {text: "send 5 to ada", replyTo: 77},
		"the bot mentioned in passing": {text: "ask " + at + " about it", entities: []map[string]any{mention(4, len(at))}},
		"somebody else mentioned":      {text: "@ada send 5 to bo", entities: []map[string]any{mention(0, 4)}},
		"only the name":                {text: at, entities: []map[string]any{mention(0, len(at))}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tr := started(t, newStubAPI(t))
			d, err := tr.Parse(groupUpdate(500, c.text, c.entities, c.replyTo))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if c.want == "" {
				if len(d.Messages) != 0 {
					t.Fatalf("handed on %q, which nobody said to the bot", d.Messages[0].Args)
				}
				// Not an error either: the delivery is acknowledged, or
				// Telegram retries every line of the conversation.
				if d.Ack.Status != http.StatusOK {
					t.Fatalf("ack = %d, want 200", d.Ack.Status)
				}
				return
			}
			if len(d.Messages) != 1 {
				t.Fatalf("got %d messages, want 1", len(d.Messages))
			}
			// The name is not part of what they asked for. This text is
			// tokenized and shown back to them as their own words.
			if got := d.Messages[0].Args; got != c.want {
				t.Errorf("Args = %q, want %q", got, c.want)
			}
			if d.Messages[0].Command != "" {
				t.Errorf("Command = %q, want free text", d.Messages[0].Command)
			}
		})
	}
}

// TestAPrivateChatNeedsNoAddressing: nobody else is there.
func TestAPrivateChatNeedsNoAddressing(t *testing.T) {
	tr := started(t, newStubAPI(t))
	d, err := tr.Parse(textUpdate(501, 42, 42, "private", "send 5 to ada"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Messages) != 1 || d.Messages[0].Args != "send 5 to ada" || !d.Messages[0].Conversation.IsDM {
		t.Fatalf("messages = %+v", d.Messages)
	}
}

// TestBeforeTheBotKnowsItsNameNothingInAGroupIsForIt: the safe direction to be
// wrong in. Somebody repeats themselves, as against the bot answering a room.
// Commands are unaffected: a command is addressed by being one.
func TestBeforeTheBotKnowsItsNameNothingInAGroupIsForIt(t *testing.T) {
	tr := newTransport(t, newStubAPI(t)) // never started
	at := "@" + botName

	d, err := tr.Parse(groupUpdate(502, at+" send 5 to ada", []map[string]any{mention(0, len(at))}, 0))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Messages) != 0 {
		t.Fatalf("read group text before knowing who the bot is: %+v", d.Messages)
	}

	d, err = tr.Parse(groupUpdate(503, "/whoami",
		[]map[string]any{{"type": "bot_command", "offset": 0, "length": 7}}, 0))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Messages) != 1 || d.Messages[0].Command != "whoami" {
		t.Fatalf("a command was dropped before Start: %+v", d.Messages)
	}
}

// TestACommandForAnotherBotIsNotOurs: in a group with several bots Telegram
// delivers "/help@someotherbot" to all of them when it can.
func TestACommandForAnotherBotIsNotOurs(t *testing.T) {
	tr := started(t, newStubAPI(t))
	command := func(text string) []map[string]any {
		return []map[string]any{{"type": "bot_command", "offset": 0, "length": len(strings.Fields(text)[0])}}
	}

	for text, ours := range map[string]bool{
		"/help":                     true,
		"/help@" + botName:          true,
		"/help@STELFIN_TEST_BOT":    true,
		"/help@someotherbot":        false,
		"/pay@someotherbot 5 to me": false,
	} {
		d, err := tr.Parse(groupUpdate(504, text, command(text), 0))
		if err != nil {
			t.Fatalf("Parse(%q): %v", text, err)
		}
		if got := len(d.Messages) == 1; got != ours {
			t.Errorf("%q: handed on = %v, want %v", text, got, ours)
		}
	}
}

// TestStartAsksWhoItIsAndSaysWhereToDeliver: the webhook is set on every start
// so that the secret Telegram echoes back is always the one this process is
// checking against. Set by hand, the two drift the first time somebody rotates
// one, and every delivery is refused with nothing to say why.
func TestStartAsksWhoItIsAndSaysWhereToDeliver(t *testing.T) {
	api := newStubAPI(t)
	api.respond("getMe", `{"ok":true,"result":{"id":9000,"is_bot":true,"username":"stelfin_test_bot"}}`, http.StatusOK)
	tr, err := New(Config{
		Token: "12345:TEST-TOKEN", WebhookSecret: testSecret,
		APIURL: api.server.URL, HTTPClient: api.server.Client(),
		WebhookURL: "https://stelfin.example/webhook/telegram",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tr.Username() != "" {
		t.Fatalf("knew its name before asking: %q", tr.Username())
	}
	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tr.Username() != botName {
		t.Errorf("Username = %q, want %q", tr.Username(), botName)
	}

	calls := api.callsTo("setWebhook")
	if len(calls) != 1 {
		t.Fatalf("made %d setWebhook calls, want 1", len(calls))
	}
	var sent struct {
		URL            string   `json:"url"`
		SecretToken    string   `json:"secret_token"`
		AllowedUpdates []string `json:"allowed_updates"`
	}
	if err := json.Unmarshal(calls[0].Body, &sent); err != nil {
		t.Fatalf("decode setWebhook: %v", err)
	}
	if sent.URL != "https://stelfin.example/webhook/telegram" {
		t.Errorf("url = %q", sent.URL)
	}
	if sent.SecretToken != testSecret {
		t.Error("the secret given to Telegram is not the one deliveries are checked against")
	}
	// Only what the transport reads. Anything else is a delivery acknowledged
	// and thrown away.
	if len(sent.AllowedUpdates) != 1 || sent.AllowedUpdates[0] != "message" {
		t.Errorf("allowed_updates = %v, want [message]", sent.AllowedUpdates)
	}
}

// TestStartLeavesTheWebhookAloneWithoutAnAddress: a laptop sharing a bot token
// with a deployment must not take the deployment's deliveries by starting.
func TestStartLeavesTheWebhookAloneWithoutAnAddress(t *testing.T) {
	api := newStubAPI(t)
	started(t, api)
	if calls := api.callsTo("setWebhook"); len(calls) != 0 {
		t.Fatalf("set a webhook with no address to set: %s", calls[0].Body)
	}
}

// TestStartFailsRatherThanGuessWhoItIs: a bot that took a missing answer for an
// empty name would treat every "@" as its own.
func TestStartFailsRatherThanGuessWhoItIs(t *testing.T) {
	api := newStubAPI(t)
	api.respond("getMe", `{"ok":true,"result":{}}`, http.StatusOK)
	tr := newTransport(t, api)
	if err := tr.Start(context.Background()); err == nil {
		t.Fatal("Start accepted a getMe with nobody in it")
	}
	if tr.Username() != "" {
		t.Errorf("Username = %q after a failed Start", tr.Username())
	}
}

// TestSendSplitsAReplyTooLongForOneMessage: Telegram refuses a message over
// 4096 code units whole, and what the member sees is nothing.
func TestSendSplitsAReplyTooLongForOneMessage(t *testing.T) {
	api := newStubAPI(t)
	tr := newTransport(t, api)

	var long strings.Builder
	for i := range 200 {
		long.WriteString("  /command — does the thing that this particular command does\n")
		_ = i
	}

	err := tr.Send(context.Background(),
		chat.Conversation{Channel: chat.Telegram, SpaceID: "-100", MessageID: "7"},
		chat.Actor{UserID: "42"}, chat.Reply{Text: long.String(), Ephemeral: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := api.callsTo("sendMessage")
	if len(calls) < 2 {
		t.Fatalf("made %d sendMessage call(s) for a %d-character reply, want it split", len(calls), long.Len())
	}
	for i, c := range calls {
		var body struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(c.Body, &body); err != nil {
			t.Fatalf("decode call %d: %v", i, err)
		}
		// In UTF-16 code units, which is how Telegram counts: the dash in
		// each line is three bytes and one unit.
		if n := len(utf16.Encode([]rune(body.Text))); n > 4096 || n == 0 {
			t.Errorf("call %d carries %d code units", i, n)
		}
		// Every piece of a private reply goes to the same private chat.
		if body.ChatID != "42" {
			t.Errorf("call %d went to chat %q, want the member's own", i, body.ChatID)
		}
	}
}
