// Package discord is the Discord interactions transport.
//
// Two properties of the platform shape everything here.
//
// Discord signs its deliveries: Ed25519 over the concatenation of a timestamp
// header and the exact request body, against the application's public key. That
// makes the body trustworthy, which in turn makes the member's permission bits
// inside it trustworthy — so unlike Telegram, administrative status arrives
// with the message and needs no round trip to establish.
//
// And Discord is impatient: an interaction that is not answered within three
// seconds is declared failed, permanently, with an error the user sees. The
// answer is therefore a deferred acknowledgement computed from the verified
// body and written before any work begins; the real reply arrives up to fifteen
// minutes later by editing that deferred message.
package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stelfin/stelfin/chat"
)

const (
	signatureHeader = "X-Signature-Ed25519"
	timestampHeader = "X-Signature-Timestamp"
)

// Interaction types.
const (
	interactionPing    = 1
	interactionCommand = 2
)

// Interaction response types.
const (
	responsePong              = 1
	responseMessage           = 4
	responseDeferredEphemeral = 5
)

// Message flags.
const (
	// flagSuppressEmbeds stops Discord unfurling a link into a preview card.
	// A confirmation link is single-use payment authority; an unfurl both
	// discloses the URL to Discord's crawler and consumes the token behind it,
	// so the user then taps a link that has already been spent.
	flagSuppressEmbeds = 1 << 2 // 4
	// flagEphemeral shows a message only to the person who invoked the command.
	flagEphemeral = 1 << 6 // 64
)

// Guild permission bits that count as administering a space.
const (
	permAdministrator = 1 << 3 // 8
	permManageGuild   = 1 << 5 // 32
)

// signatureFreshness bounds how old a delivery's timestamp may be.
//
// Discord does not require this. Without it a captured interaction is
// replayable for as long as its signature is valid, which is forever. The
// dedupe claim catches a replay too, but this catches it before a database
// round trip and without consuming an id.
const signatureFreshness = 5 * time.Minute

// Config describes the transport.
type Config struct {
	// PublicKey is the application's Ed25519 public key, hex-encoded, from the
	// developer portal. It authenticates every delivery.
	PublicKey string
	// BotToken authenticates outbound REST calls that are not interaction
	// followups: registering commands, and posting to a channel.
	BotToken string
	// ApplicationID is the application's snowflake, needed to address followups
	// and to register commands.
	ApplicationID string
	// APIURL overrides the REST root, for tests.
	APIURL string
	// HTTPClient overrides the client used for outbound calls.
	HTTPClient *http.Client
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Transport implements chat.Transport for Discord.
type Transport struct {
	publicKey     ed25519.PublicKey
	botToken      string
	applicationID string
	apiURL        string
	http          *http.Client
	now           func() time.Time
}

// New returns a Transport.
func New(cfg Config) (*Transport, error) {
	key, err := hex.DecodeString(strings.TrimSpace(cfg.PublicKey))
	if err != nil {
		return nil, errors.New("discord: public key is not hex")
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("discord: public key is %d bytes, want %d",
			len(key), ed25519.PublicKeySize)
	}
	if strings.TrimSpace(cfg.BotToken) == "" {
		return nil, errors.New("discord: bot token is required")
	}
	if strings.TrimSpace(cfg.ApplicationID) == "" {
		return nil, errors.New("discord: application id is required")
	}

	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: callTimeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Transport{
		publicKey:     key,
		botToken:      cfg.BotToken,
		applicationID: cfg.ApplicationID,
		apiURL:        strings.TrimSuffix(apiURL, "/"),
		http:          client,
		now:           now,
	}, nil
}

func (d *Transport) Channel() chat.Channel { return chat.Discord }

// Verify checks the Ed25519 signature over timestamp || body.
//
// The body is read once, with ReadLimited, and those exact bytes are what the
// signature covers. Decoding the JSON and re-encoding it to verify would change
// the bytes and check nothing.
//
// Discord deliberately sends deliveries with invalid signatures when an
// interactions endpoint is registered, and refuses the endpoint if any of them
// receives a 2xx. Answering them with anything other than a refusal breaks
// setup, so this must never be "helpful".
func (d *Transport) Verify(r *http.Request) ([]byte, error) {
	sig, err := hex.DecodeString(r.Header.Get(signatureHeader))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, chat.ErrUnauthenticated
	}
	ts := r.Header.Get(timestampHeader)
	if ts == "" {
		return nil, chat.ErrUnauthenticated
	}

	body, err := chat.ReadLimited(r)
	if err != nil {
		return nil, err
	}

	signed := make([]byte, 0, len(ts)+len(body))
	signed = append(signed, ts...)
	signed = append(signed, body...)
	if !ed25519.Verify(d.publicKey, signed, sig) {
		return nil, chat.ErrUnauthenticated
	}

	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, chat.ErrUnauthenticated
	}
	if age := d.now().Sub(time.Unix(secs, 0)); age > signatureFreshness || age < -signatureFreshness {
		return nil, chat.ErrUnauthenticated
	}
	return body, nil
}

// interaction is the slice of Discord's interaction payload this reads.
type interaction struct {
	ID        string           `json:"id"`
	Type      int              `json:"type"`
	Token     string           `json:"token"`
	GuildID   string           `json:"guild_id"`
	ChannelID string           `json:"channel_id"`
	Data      *interactionData `json:"data"`
	Member    *guildMember     `json:"member"`
	User      *discordUser     `json:"user"`
}

type interactionData struct {
	Name    string          `json:"name"`
	Options []commandOption `json:"options"`
}

type commandOption struct {
	Name  string          `json:"name"`
	Type  int             `json:"type"`
	Value json.RawMessage `json:"value"`
}

type guildMember struct {
	User *discordUser `json:"user"`
	// Roles are role snowflakes. What they mean is decided by the org.
	Roles []string `json:"roles"`
	// Permissions is a decimal string bitfield of this member's effective
	// permissions in this channel. It is inside a signed body, so unlike
	// Telegram it can be believed.
	Permissions string `json:"permissions"`
}

type discordUser struct {
	ID       string `json:"id"`
	Bot      bool   `json:"bot"`
	Username string `json:"username"`
}

// optionTypeString is Discord's STRING application command option type.
const optionTypeString = 3

// Parse turns a verified interaction into a chat.Delivery.
func (d *Transport) Parse(body []byte) (chat.Delivery, error) {
	var in interaction
	if err := json.Unmarshal(body, &in); err != nil {
		return chat.Delivery{}, fmt.Errorf("discord: decode interaction: %w", err)
	}

	switch in.Type {
	case interactionPing:
		// The liveness check Discord runs against a registered endpoint.
		return chat.Delivery{Ack: ack(interactionResponse{Type: responsePong})}, nil

	case interactionCommand:
		// fall through

	default:
		// Components, autocomplete, modals. Nothing here handles them yet, and
		// a deferred acknowledgement would leave the invoker watching a
		// spinner that never resolves — so close the interaction instead.
		return chat.Delivery{Ack: refusal("stelfin does not handle that yet.")}, nil
	}

	user := in.User
	if in.Member != nil && in.Member.User != nil {
		user = in.Member.User
	}
	switch {
	case in.ID == "", in.Data == nil, user == nil, user.ID == "":
		return chat.Delivery{Ack: refusal("stelfin could not read that.")}, nil
	case user.Bot:
		// A bot must not be able to instruct a payment.
		return chat.Delivery{Ack: refusal("stelfin does not take instructions from bots.")}, nil
	}

	// Deferred, and ephemeral. Deferring buys fifteen minutes to do the work;
	// the ephemeral flag is set here and cannot be changed afterwards, so the
	// reply that edits this message is private to the invoker whatever it turns
	// out to contain. Given that two of the replies carry payment authority,
	// deferring publicly and hoping is not an option.
	deferred := ack(interactionResponse{
		Type: responseDeferredEphemeral,
		Data: &interactionResponseData{Flags: flagEphemeral},
	})

	actor := chat.Actor{
		Channel: chat.Discord,
		UserID:  user.ID,
		Handle:  user.Username,
	}
	if in.Member != nil {
		actor.Roles = in.Member.Roles
		actor.IsSpaceAdmin = administers(in.Member.Permissions)
	}

	// A guild interaction names the guild; a direct message does not.
	spaceID, isDM := in.GuildID, false
	if spaceID == "" {
		spaceID, isDM = in.ChannelID, true
	}

	options, args := readOptions(in.Data.Options)

	return chat.Delivery{
		Ack: deferred,
		Messages: []chat.Inbound{{
			// Channel-prefixed: a Discord snowflake and a Telegram update id
			// share no id space, and an unprefixed claim would let one hide
			// the other.
			DedupeID: "discord:" + in.ID,
			Actor:    actor,
			Conversation: chat.Conversation{
				Channel:          chat.Discord,
				SpaceID:          spaceID,
				ThreadID:         in.ChannelID,
				InteractionToken: in.Token,
				IsDM:             isDM,
			},
			Command:    strings.ToLower(in.Data.Name),
			Args:       args,
			Options:    options,
			ReceivedAt: d.now(),
		}},
	}, nil
}

// readOptions flattens command options, and reports the free-text argument
// where there is one.
//
// Args is defined as the exact string that gets tokenized and shown back to the
// user as what they said. A slash command carrying several typed options has no
// such string, so Args stays empty for those and Options is authoritative;
// building a sentence out of them here would produce text nobody typed and hand
// it to the tokenizer as if they had. A command with a single string option —
// the free-text form — does have one, and that is what Args carries.
func readOptions(opts []commandOption) (map[string]string, string) {
	if len(opts) == 0 {
		return nil, ""
	}
	out := make(map[string]string, len(opts))
	stringOptions := 0
	var soleString string

	for _, o := range opts {
		var value string
		if err := json.Unmarshal(o.Value, &value); err != nil {
			// Numbers and booleans arrive unquoted. Carried verbatim rather
			// than through a Go numeric type, which would reformat them — and
			// an amount that renders differently than it was typed is exactly
			// what the verification scheme exists to prevent.
			value = rawScalar(o.Value)
		}
		out[o.Name] = value
		if o.Type == optionTypeString {
			stringOptions++
			soleString = value
		}
	}
	if stringOptions == 1 {
		return out, soleString
	}
	return out, ""
}

// rawScalar renders a raw JSON scalar without going through a numeric type.
func rawScalar(raw json.RawMessage) string {
	return strings.Trim(string(raw), `"`)
}

// administers reports whether a permission bitfield carries authority over the
// guild. Discord sends it as a decimal string because the field exceeds 53 bits
// and would lose precision as a JSON number.
func administers(permissions string) bool {
	bits, err := strconv.ParseUint(permissions, 10, 64)
	if err != nil {
		return false
	}
	return bits&permAdministrator != 0 || bits&permManageGuild != 0
}

// interactionResponse is the synchronous answer Discord expects.
type interactionResponse struct {
	Type int                      `json:"type"`
	Data *interactionResponseData `json:"data,omitempty"`
}

type interactionResponseData struct {
	Content string `json:"content,omitempty"`
	Flags   int    `json:"flags,omitempty"`
}

// ack renders an interaction response.
//
// Marshalling always succeeds for these shapes, and there is nowhere useful to
// report a failure to: this runs before the response is written, and a Discord
// interaction that receives nothing is a visible error for the person who typed
// the command. An empty body would be that; a PONG at least keeps the endpoint
// registered.
func ack(resp interactionResponse) chat.Ack {
	body, err := json.Marshal(resp)
	if err != nil {
		body = []byte(`{"type":1}`)
	}
	return chat.Ack{
		Status:      http.StatusOK,
		ContentType: "application/json",
		Body:        body,
	}
}

// refusal closes an interaction with a short ephemeral message.
//
// Used where there is nothing to do but nothing is wrong either. It must not be
// a deferred response: deferring promises a reply, and a promise nothing will
// keep leaves the invoker watching a spinner until it times out.
func refusal(content string) chat.Ack {
	return ack(interactionResponse{
		Type: responseMessage,
		Data: &interactionResponseData{Content: content, Flags: flagEphemeral},
	})
}

// message is the outbound body for a followup or a channel post.
type message struct {
	Content string `json:"content"`
	Flags   int    `json:"flags"`
	// AllowedMentions is empty on purpose: a reply echoes the user's own words,
	// and without this a message quoting "@everyone" would ping the guild.
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

type allowedMentions struct {
	Parse []string `json:"parse"`
}

// Send delivers a reply.
//
// The interaction token is a wasting asset: it addresses the deferred message
// for fifteen minutes and then stops working. When it is spent, the only route
// left is an ordinary channel post — which is public. So a reply that must stay
// private is refused rather than posted, because the fallback would hand a
// confirmation link to everyone who can read the channel.
func (d *Transport) Send(ctx context.Context, to chat.Conversation, actor chat.Actor, r chat.Reply) error {
	if strings.TrimSpace(r.Text) == "" {
		return errors.New("discord: refusing to send an empty message")
	}

	body := message{
		Content:         r.Text,
		Flags:           flagSuppressEmbeds,
		AllowedMentions: allowedMentions{Parse: []string{}},
	}

	if to.InteractionToken != "" {
		// Editing the deferred message. Its ephemeral flag was fixed when the
		// interaction was acknowledged and cannot be changed here, so this is
		// private exactly when the acknowledgement was — which it always is.
		//
		// No bot token: the interaction token in the path is the credential,
		// and Discord rejects a bot-authenticated followup.
		path := fmt.Sprintf("/webhooks/%s/%s/messages/@original", d.applicationID, to.InteractionToken)
		err := d.call(ctx, http.MethodPatch, path, body, false)
		if err == nil {
			return nil
		}
		var apiErr *apiError
		if !errors.As(err, &apiErr) || !apiErr.expired() {
			return err
		}
		if r.Ephemeral && !to.IsDM {
			return fmt.Errorf(
				"discord: the interaction expired and the only route left is a public "+
					"channel post, which would disclose a private reply: %w", chat.ErrLinkInPublic)
		}
	} else if r.Ephemeral && !to.IsDM {
		return fmt.Errorf(
			"discord: a private reply needs an interaction to answer: %w", chat.ErrLinkInPublic)
	}

	channelID := to.ThreadID
	if channelID == "" {
		channelID = to.SpaceID
	}
	if channelID == "" {
		return errors.New("discord: no channel to post to")
	}
	return d.call(ctx, http.MethodPost, "/channels/"+channelID+"/messages", body, true)
}

// applicationCommand is one entry in the bulk command registration.
type applicationCommand struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Type        int                        `json:"type"`
	Options     []applicationCommandOption `json:"options,omitempty"`
	// DefaultMemberPermissions hides a command from members who do not hold
	// those permissions. "0" means nobody by default.
	//
	// A menu affordance, never an authorisation boundary: a guild administrator
	// can edit these in server settings, so the check that counts happens in Go
	// against the org's own roles.
	DefaultMemberPermissions *string `json:"default_member_permissions"`
}

type applicationCommandOption struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        int    `json:"type"`
	Required    bool   `json:"required"`
}

// RegisterCommands publishes the command set.
//
// A bulk overwrite, so a command removed from the code disappears from the
// guild rather than lingering as something a member can still invoke.
func (d *Transport) RegisterCommands(ctx context.Context, cmds []chat.Command) error {
	out := make([]applicationCommand, 0, len(cmds))
	for _, c := range cmds {
		entry := applicationCommand{
			Name:        c.Name,
			Description: c.Description,
			Type:        1, // CHAT_INPUT
		}
		if c.MinRole >= chat.RoleAdmin {
			nobody := "0"
			entry.DefaultMemberPermissions = &nobody
		}
		for _, o := range c.Options {
			entry.Options = append(entry.Options, applicationCommandOption{
				Name:        o.Name,
				Description: o.Description,
				Type:        optionType(o.Type),
				Required:    o.Required,
			})
		}
		out = append(out, entry)
	}
	return d.call(ctx, http.MethodPut,
		"/applications/"+d.applicationID+"/commands", out, true)
}

func optionType(t chat.OptionType) int {
	switch t {
	case chat.OptInteger:
		return 4
	case chat.OptBool:
		return 5
	case chat.OptUser:
		return 6
	default:
		return optionTypeString
	}
}
