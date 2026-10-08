// Package telegram is the Telegram Bot API transport.
//
// One property shapes everything here and is worth stating before the code:
// **Telegram does not sign its webhook deliveries.** It echoes back the secret
// supplied at setWebhook, in a header. That proves the caller knew the secret;
// it says nothing whatsoever about the body. This is a genuine downgrade from
// the HMAC-signed webhook this package replaces, and it has consequences that
// are implemented rather than merely noted:
//
//   - The secret must be long and random, and is validated like one.
//   - Nothing inside a delivery may establish privilege. Administrative status
//     in particular is fetched from the Bot API (IsSpaceAdmin), never read out
//     of the update, because a caller who has the secret can claim anything.
//
// The transport is otherwise a thin, honest mapping onto chat.Transport.
package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/stelfin/stelfin/chat"
)

// secretHeader is where Telegram echoes the setWebhook secret.
const secretHeader = "X-Telegram-Bot-Api-Secret-Token"

// MinSecretLength is the shortest webhook secret this package accepts.
//
// Telegram permits 1 to 256 characters. One character is a catastrophe: with no
// signature over the body, this secret is the only thing standing between the
// open internet and a delivery this server will act on. Held to the same bar as
// the token secret that signs payment links.
const MinSecretLength = 32

// adminCacheTTL bounds how stale an administrative answer may be.
//
// Short, because the consequence of staleness is that someone demoted moments
// ago can run one more administrative command. Non-zero, because the
// alternative is a round trip to Telegram on every message that checks a role.
const adminCacheTTL = 60 * time.Second

// Config describes the transport.
type Config struct {
	// Token is the bot token from BotFather. It is the credential in the API
	// URL path, so it must never appear in an error or a log line.
	Token string
	// WebhookSecret is the value passed to setWebhook and echoed back on every
	// delivery. At least MinSecretLength characters.
	WebhookSecret string
	// APIURL overrides the Bot API root, for tests. Empty uses DefaultAPIURL.
	APIURL string
	// WebhookURL is where Telegram should deliver updates: this deployment's
	// own /webhook/telegram, as the internet reaches it. When set, Start tells
	// Telegram so. Empty leaves the webhook as it is, which is right for a
	// server Telegram cannot reach — a laptop, a test.
	WebhookURL string
	// HTTPClient overrides the client used for outbound calls.
	HTTPClient *http.Client
}

// Transport implements chat.Transport for Telegram.
type Transport struct {
	token      string
	secret     []byte
	apiURL     string
	webhookURL string
	http       *http.Client

	// me is who this bot is, once Start has asked. Nil until then.
	me atomic.Pointer[botIdentity]

	mu    sync.Mutex
	admin map[string]adminAnswer
}

type adminAnswer struct {
	isAdmin bool
	at      time.Time
}

// New returns a Transport.
func New(cfg Config) (*Transport, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("telegram: bot token is required")
	}
	if len(cfg.WebhookSecret) < MinSecretLength {
		return nil, fmt.Errorf(
			"telegram: webhook secret is %d characters, want at least %d — "+
				"Telegram does not sign deliveries, so this secret is the only thing "+
				"authenticating them", len(cfg.WebhookSecret), MinSecretLength)
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: callTimeout}
	}
	return &Transport{
		token:      cfg.Token,
		secret:     []byte(cfg.WebhookSecret),
		apiURL:     strings.TrimSuffix(apiURL, "/"),
		webhookURL: strings.TrimSpace(cfg.WebhookURL),
		http:       client,
		admin:      make(map[string]adminAnswer),
	}, nil
}

// botIdentity is what getMe says about this bot.
type botIdentity struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type setWebhookRequest struct {
	URL         string `json:"url"`
	SecretToken string `json:"secret_token"`
	// AllowedUpdates names the only kind of update this transport reads.
	// Anything else Telegram would send is a delivery acknowledged and thrown
	// away.
	AllowedUpdates []string `json:"allowed_updates"`
}

// Start asks Telegram who this bot is, and tells it where to deliver.
//
// Who it is comes first and matters more. Until the bot knows its own name it
// cannot tell "@stelfinbot send 5 to ada" from two people talking about lunch,
// or "/help@stelfinbot" from "/help@someotherbot", and Parse declines to guess:
// see addressed.
//
// The webhook is set on every start rather than once by hand. It is idempotent,
// and doing it here means the secret Telegram echoes back is always the one
// this process is checking against. Set by hand, the two drift the first time
// somebody rotates one and forgets the other, and every delivery is refused
// with nothing to say why.
func (t *Transport) Start(ctx context.Context) error {
	var me botIdentity
	if err := t.call(ctx, "getMe", struct{}{}, &me); err != nil {
		return err
	}
	if me.ID == 0 || me.Username == "" {
		return errors.New("telegram: getMe did not say who this bot is")
	}
	t.me.Store(&me)

	if t.webhookURL == "" {
		return nil
	}
	return t.call(ctx, "setWebhook", setWebhookRequest{
		URL:            t.webhookURL,
		SecretToken:    string(t.secret),
		AllowedUpdates: []string{"message"},
	}, nil)
}

// Username is this bot's @name without the @, or "" before Start has
// succeeded.
func (t *Transport) Username() string {
	if me := t.me.Load(); me != nil {
		return me.Username
	}
	return ""
}

func (t *Transport) Channel() chat.Channel { return chat.Telegram }

// Verify checks the echoed secret and returns the exact bytes read.
//
// Constant-time, because a byte-at-a-time comparison against a secret an
// attacker can retry is a timing oracle for the secret itself.
func (t *Transport) Verify(r *http.Request) ([]byte, error) {
	got := r.Header.Get(secretHeader)
	if subtle.ConstantTimeCompare([]byte(got), t.secret) != 1 {
		return nil, chat.ErrUnauthenticated
	}
	return chat.ReadLimited(r)
}

// update is the slice of Telegram's webhook envelope this transport reads.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	MessageID       int64    `json:"message_id"`
	MessageThreadID int64    `json:"message_thread_id"`
	From            *user    `json:"from"`
	Chat            *tgChat  `json:"chat"`
	Text            string   `json:"text"`
	Entities        []entity `json:"entities"`
	// ReplyTo is the message this one answers. Only its author is read.
	ReplyTo *struct {
		From *user `json:"from"`
	} `json:"reply_to_message"`
}

type user struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Parse turns a verified delivery into a chat.Delivery.
//
// Telegram sends one update per request. Anything this transport cannot act on
// — an edit, a join notification, a photo, a message from another bot — yields
// an acknowledgement and no messages. That is an ordinary occurrence, not a
// failure: erroring on it would make the webhook look broken during normal use
// and would invite Telegram to retry something a retry cannot fix.
func (t *Transport) Parse(body []byte) (chat.Delivery, error) {
	ack := chat.Delivery{Ack: chat.Ack{Status: http.StatusOK}}

	var u update
	if err := json.Unmarshal(body, &u); err != nil {
		return chat.Delivery{}, fmt.Errorf("telegram: decode update: %w", err)
	}

	m := u.Message
	switch {
	case m == nil, m.From == nil, m.Chat == nil:
		return ack, nil
	case m.From.IsBot:
		// Including this bot's own messages, echoed back in some group
		// configurations. A bot must not be able to instruct a payment.
		return ack, nil
	case strings.TrimSpace(m.Text) == "":
		// Text only. A voice note or a photo caption grounds against something
		// that is itself model output or an afterthought, which is a lower
		// trust tier than a typed instruction and needs its own handling before
		// it can reach the send path.
		return ack, nil
	case u.UpdateID == 0:
		// The dedupe claim is what makes retries safe. Without an id there is
		// nothing to claim, so the message must not be processed at all.
		return ack, nil
	}

	// Edits are deliberately not handled: this transport reads only `message`,
	// never `edited_message`. Re-issuing a payment instruction by editing the
	// message that produced it is a confusing authority model, and the original
	// delivery has already been claimed.

	cmd, args, isCommand := splitCommand(m.Text, m.Entities)
	private := m.Chat.Type == "private"
	me := t.me.Load()

	switch {
	case isCommand:
		// "/help@someotherbot", in a group with several bots. Telegram delivers
		// it to all of them when it can, and it is none of this one's business.
		if target := commandTarget(m.Text, m.Entities); target != "" &&
			me != nil && !strings.EqualFold(target, me.Username) {
			return ack, nil
		}
	case !private:
		// Words in a group, with no command. Only an instruction if they were
		// said to the bot.
		rest, ok := addressed(m, me)
		if !ok {
			return ack, nil
		}
		args = rest
	}

	conv := chat.Conversation{
		Channel:   chat.Telegram,
		SpaceID:   strconv.FormatInt(m.Chat.ID, 10),
		MessageID: strconv.FormatInt(m.MessageID, 10),
		IsDM:      private,
	}
	if m.MessageThreadID != 0 {
		conv.ThreadID = strconv.FormatInt(m.MessageThreadID, 10)
	}

	// Who a command is about, by the platform's own immutable id: the author
	// of the message being replied to, or a person mentioned by a text
	// mention, which is how Telegram links someone who has no username.
	//
	// Never by @handle. A handle is free to rename and, once released, free
	// for someone else to take, and a role granted to one would follow it.
	// This is what lets "/grant approver" in reply to somebody's message mean
	// that person, without them having to speak to the bot first.
	var options map[string]string
	if picked := pickedUser(m); picked != "" {
		options = map[string]string{"user": picked}
	}

	ack.Messages = []chat.Inbound{{
		// Channel-prefixed: a Telegram update id and a Discord snowflake share
		// no id space, and an unprefixed claim would let one hide the other.
		DedupeID: "telegram:" + strconv.FormatInt(u.UpdateID, 10),
		Actor: chat.Actor{
			Channel: chat.Telegram,
			UserID:  strconv.FormatInt(m.From.ID, 10),
			Handle:  m.From.Username,
			// Roles and IsSpaceAdmin are deliberately left unset. Telegram's
			// authentication does not cover the body, so a privilege claim
			// inside it is worth nothing; IsSpaceAdmin asks the Bot API.
		},
		Conversation: conv,
		Command:      cmd,
		Args:         args,
		Options:      options,
		ReceivedAt:   time.Now(),
	}}
	return ack, nil
}

// addressed reports whether a message in a group, with no command in it, was
// said to this bot, and returns the text with the bot's name taken off.
//
// A group is other people's conversation. With privacy mode off, or with the
// bot made an administrator, Telegram delivers all of it, and every message
// used to be read as a possible payment: a model call for each line of
// somebody's chat, and for anyone without a wallet a private message from a bot
// they had not spoken to, telling them how to get one.
//
// So words count as an instruction only when they are plainly for the bot:
//
//   - the message replies to one of the bot's own; or
//   - it opens with the bot's @name. "@stelfinbot send 5 to ada" is; "ask
//     @stelfinbot about it" is not, for the same reason "he said /pay 5000
//     yesterday" is not a command.
//
// The name is removed from what is returned. That text is tokenized and shown
// back to the member as what they asked for, and "@stelfinbot" is not part of
// what they asked for.
//
// Before the bot knows its own name nothing in a group is addressed to it. That
// is the safe direction to be wrong in: somebody repeats themselves, as against
// the bot answering a room.
func addressed(m *message, me *botIdentity) (string, bool) {
	if me == nil {
		return m.Text, false
	}
	if m.ReplyTo != nil && m.ReplyTo.From != nil && m.ReplyTo.From.ID == me.ID {
		return m.Text, true
	}

	units := utf16.Encode([]rune(m.Text))
	for _, e := range m.Entities {
		if e.Type != "mention" || e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > len(units) {
			continue
		}
		name := string(utf16.Decode(units[e.Offset : e.Offset+e.Length]))
		if !strings.EqualFold(name, "@"+me.Username) {
			continue
		}
		if !leads(units[:e.Offset], nil, e.Offset) {
			continue
		}
		rest := string(utf16.Decode(units[e.Offset+e.Length:]))
		rest = strings.TrimLeft(rest, " \t\n,:;—-")
		if strings.TrimSpace(rest) == "" {
			// Only the name. There is nothing to act on, and a bare mention
			// is how people point at a bot, not how they instruct one.
			return m.Text, false
		}
		return rest, true
	}
	return m.Text, false
}

// commandTarget returns the bot a leading command names, as in
// "/help@somebot", or "" when it names none.
func commandTarget(text string, entities []entity) string {
	units := utf16.Encode([]rune(text))
	for _, e := range entities {
		if e.Type != "bot_command" || e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > len(units) {
			continue
		}
		raw := string(utf16.Decode(units[e.Offset : e.Offset+e.Length]))
		if _, target, ok := strings.Cut(raw, "@"); ok {
			return target
		}
		return ""
	}
	return ""
}

// pickedUser returns the id of the person a message points at, or "".
//
// A reply wins over a mention: replying is the deliberate gesture, and a
// message may mention several people in passing. A bot is never picked — the
// bot itself least of all.
func pickedUser(m *message) string {
	if m.ReplyTo != nil && m.ReplyTo.From != nil && !m.ReplyTo.From.IsBot && m.ReplyTo.From.ID != 0 {
		return strconv.FormatInt(m.ReplyTo.From.ID, 10)
	}
	for _, e := range m.Entities {
		if e.Type == "text_mention" && e.User != nil && !e.User.IsBot && e.User.ID != 0 {
			return strconv.FormatInt(e.User.ID, 10)
		}
	}
	return ""
}

// sendMessageRequest is the outbound wire body.
//
// Note what is absent: parse_mode. It is never set, and that is a security
// decision rather than a stylistic one. With Markdown or HTML enabled, a
// user-controlled string — a saved recipient's label, an org name, a memo —
// can render as a link whose visible text is one URL and whose target is
// another. In a bot that moves money that is a phishing primitive carrying the
// bot's own credibility. Plain text removes the class entirely.
type sendMessageRequest struct {
	ChatID             string              `json:"chat_id"`
	Text               string              `json:"text"`
	MessageThreadID    string              `json:"message_thread_id,omitempty"`
	ReplyParameters    *replyParameters    `json:"reply_parameters,omitempty"`
	LinkPreviewOptions *linkPreviewOptions `json:"link_preview_options"`
}

type replyParameters struct {
	MessageID                string `json:"message_id"`
	AllowSendingWithoutReply bool   `json:"allow_sending_without_reply"`
}

// linkPreviewOptions disables previews.
//
// Unconditional, on every message. Telegram fetches a URL server-side to build
// a preview, which for a confirmation link both discloses it to Telegram's
// infrastructure and consumes the single-use token behind it — the user then
// taps a link that has already been spent.
type linkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled"`
}

// Send delivers a reply.
//
// Telegram has no ephemeral messages in groups, so an ephemeral reply is
// delivered as a direct message to the actor. That requires the actor to have
// started a chat with the bot at least once; if they have not, Telegram refuses
// with 403 and the error says so plainly, because the fix is a person doing
// something rather than a retry.
func (t *Transport) Send(ctx context.Context, to chat.Conversation, actor chat.Actor, r chat.Reply) error {
	if strings.TrimSpace(r.Text) == "" {
		return errors.New("telegram: refusing to send an empty message")
	}

	req := sendMessageRequest{
		ChatID:             to.SpaceID,
		Text:               r.Text,
		MessageThreadID:    to.ThreadID,
		LinkPreviewOptions: &linkPreviewOptions{IsDisabled: true},
	}
	if to.MessageID != "" {
		req.ReplyParameters = &replyParameters{
			MessageID: to.MessageID,
			// The message being replied to may have been deleted by the time
			// the reply is ready. Sending anyway is better than losing it.
			AllowSendingWithoutReply: true,
		}
	}

	if r.Ephemeral && !to.IsDM {
		if actor.UserID == "" {
			return errors.New("telegram: an ephemeral reply needs an actor to send it to")
		}
		// A DM is its own conversation: the thread id and the message being
		// replied to belong to the group and mean nothing here.
		req.ChatID = actor.UserID
		req.MessageThreadID = ""
		req.ReplyParameters = nil
	}

	if err := t.call(ctx, "sendMessage", req, nil); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden && r.Ephemeral && !to.IsDM {
			t.sayStartFirst(ctx, to, actor)
			return fmt.Errorf(
				"telegram: cannot send privately to this member — they have never "+
					"started a chat with the bot: %w", err)
		}
		return err
	}
	return nil
}

// startFirstNotice is posted to the group when a private reply cannot be
// delivered. It is fixed text on purpose: whatever the undelivered reply said —
// a signing link, someone's own instruction quoted back — none of it may be
// repeated where the room can read it.
const startFirstNotice = "I answer privately, and Telegram only lets me message " +
	"people who have started a chat with me. Open a chat with me, press Start, " +
	"then send that again."

// sayStartFirst tells the group, in place of a reply it could not deliver,
// what the person has to do.
//
// Without it the failure is silent on the only side that can fix it: the bot
// logs an error and the member, who typed a command and got nothing, concludes
// it is broken. Best effort — if this fails too there is nobody left to tell.
func (t *Transport) sayStartFirst(ctx context.Context, to chat.Conversation, actor chat.Actor) {
	text := startFirstNotice
	if actor.Handle != "" {
		text = "@" + actor.Handle + " " + text
	}
	req := sendMessageRequest{
		ChatID:             to.SpaceID,
		Text:               text,
		MessageThreadID:    to.ThreadID,
		LinkPreviewOptions: &linkPreviewOptions{IsDisabled: true},
	}
	if to.MessageID != "" {
		req.ReplyParameters = &replyParameters{MessageID: to.MessageID, AllowSendingWithoutReply: true}
	}
	_ = t.call(ctx, "sendMessage", req, nil)
}

// botCommand is one entry in setMyCommands.
type botCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type setMyCommandsRequest struct {
	Commands []botCommand   `json:"commands"`
	Scope    map[string]any `json:"scope,omitempty"`
}

// RegisterCommands publishes the command set.
//
// Two scopes: everything for group chats, and the administrative subset for
// chat administrators. Telegram's scoping is **only a hint to the autocomplete
// menu** — it will still deliver "/policy" typed by a non-administrator — so
// this is a usability affordance and never an authorisation boundary. The check
// that counts happens in Go, against the org's own roles.
func (t *Transport) RegisterCommands(ctx context.Context, cmds []chat.Command) error {
	all := make([]botCommand, 0, len(cmds))
	admin := make([]botCommand, 0, len(cmds))
	for _, c := range cmds {
		// Hyphens are not legal in a Telegram command and one bad name fails
		// the whole call, taking the menu for every other command with it.
		entry := botCommand{Command: wireName(c.Name), Description: c.Description}
		if c.MinRole >= chat.RoleAdmin {
			admin = append(admin, entry)
			continue
		}
		all = append(all, entry)
	}

	if err := t.call(ctx, "setMyCommands", setMyCommandsRequest{
		Commands: all,
		Scope:    map[string]any{"type": "all_group_chats"},
	}, nil); err != nil {
		return err
	}
	// Administrators see the general set as well, so send both.
	if err := t.call(ctx, "setMyCommands", setMyCommandsRequest{
		Commands: append(append([]botCommand(nil), all...), admin...),
		Scope:    map[string]any{"type": "all_chat_administrators"},
	}, nil); err != nil {
		return err
	}
	// A private chat is where the bot sends people and where nothing but an
	// introduction happens, so its menu offers the two things that get one.
	// Listing the workspace commands there would be a menu of things that
	// answer "not here".
	return t.call(ctx, "setMyCommands", setMyCommandsRequest{
		Commands: []botCommand{
			{Command: "start", Description: "What stelfin is, and how to begin"},
			{Command: "help", Description: "What stelfin is, and how to begin"},
		},
		Scope: map[string]any{"type": "all_private_chats"},
	}, nil)
}

type getChatMemberRequest struct {
	ChatID string `json:"chat_id"`
	UserID string `json:"user_id"`
}

type chatMember struct {
	Status string `json:"status"`
}

// IsSpaceAdmin reports whether a user administers a chat.
//
// Asked of Telegram rather than read from the update, because a delivery's body
// is not authenticated and a caller holding the webhook secret could otherwise
// claim to be an administrator.
func (t *Transport) IsSpaceAdmin(ctx context.Context, spaceID, userID string) (bool, error) {
	if spaceID == "" || userID == "" {
		return false, errors.New("telegram: space and user are required")
	}
	key := spaceID + "\x00" + userID

	t.mu.Lock()
	if got, ok := t.admin[key]; ok && time.Since(got.at) < adminCacheTTL {
		t.mu.Unlock()
		return got.isAdmin, nil
	}
	t.mu.Unlock()

	var member chatMember
	if err := t.call(ctx, "getChatMember",
		getChatMemberRequest{ChatID: spaceID, UserID: userID}, &member); err != nil {
		// Deliberately not cached: a failed lookup must not pin a "no" for a
		// minute, and must never be mistaken for an authoritative denial.
		return false, err
	}
	isAdmin := member.Status == "creator" || member.Status == "administrator"

	t.mu.Lock()
	t.admin[key] = adminAnswer{isAdmin: isAdmin, at: time.Now()}
	t.mu.Unlock()

	return isAdmin, nil
}
