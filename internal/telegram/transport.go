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
	"time"

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
	// HTTPClient overrides the client used for outbound calls.
	HTTPClient *http.Client
}

// Transport implements chat.Transport for Telegram.
type Transport struct {
	token  string
	secret []byte
	apiURL string
	http   *http.Client

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
		token:  cfg.Token,
		secret: []byte(cfg.WebhookSecret),
		apiURL: strings.TrimSuffix(apiURL, "/"),
		http:   client,
		admin:  make(map[string]adminAnswer),
	}, nil
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

	cmd, args, _ := splitCommand(m.Text, m.Entities)

	conv := chat.Conversation{
		Channel:   chat.Telegram,
		SpaceID:   strconv.FormatInt(m.Chat.ID, 10),
		MessageID: strconv.FormatInt(m.MessageID, 10),
		IsDM:      m.Chat.Type == "private",
	}
	if m.MessageThreadID != 0 {
		conv.ThreadID = strconv.FormatInt(m.MessageThreadID, 10)
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
		ReceivedAt:   time.Now(),
	}}
	return ack, nil
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
			return fmt.Errorf(
				"telegram: cannot send privately to this member — they have never "+
					"started a chat with the bot: %w", err)
		}
		return err
	}
	return nil
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
		entry := botCommand{Command: c.Name, Description: c.Description}
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
	return t.call(ctx, "setMyCommands", setMyCommandsRequest{
		Commands: append(append([]botCommand(nil), all...), admin...),
		Scope:    map[string]any{"type": "all_chat_administrators"},
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
