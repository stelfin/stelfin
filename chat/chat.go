// Package chat is the channel-agnostic contract between a messaging platform
// and the rest of stelfin.
//
// Everything above this package is per-platform: signature verification,
// envelope shapes, rate limits, the wire format of a reply. Everything below it
// is the product, and knows only that a person said something somewhere.
//
// Two separations carry the design.
//
// Who spoke is not where to reply. In a one-to-one chat those collapse into a
// single phone number, which is why the WhatsApp-era code had one Send(to,
// body) and got away with it. In a group they are different facts: the actor is
// a person with roles, the conversation is a room several hundred people can
// read. Merging them is how a payment link ends up posted where everyone can
// tap it.
//
// And a transport identity is not an account. An Actor proves "the same chat
// account as last time" and nothing more — chat accounts are free to create and
// handles are free to rename. Authority over money comes from a signature the
// network verifies, never from who is speaking.
package chat

import "time"

// Channel names a messaging platform.
type Channel string

const (
	Telegram Channel = "telegram"
	Discord  Channel = "discord"
)

// Valid reports whether c is a channel this build knows.
//
// Webhook routing is keyed on a path segment, so an unknown value must be a
// refusal rather than something that flows onward as an empty string.
func (c Channel) Valid() bool {
	switch c {
	case Telegram, Discord:
		return true
	}
	return false
}

// Actor is the transport identity of the person who spoke.
type Actor struct {
	Channel Channel
	// UserID is the platform's own immutable identifier — a Telegram user id, a
	// Discord snowflake.
	//
	// Never the @handle. Handles are renameable and, once released, reassignable
	// to someone else; binding treasury authority to one would let a recycled
	// username inherit a role.
	UserID string
	// Handle is for display only and is never used to look anything up.
	Handle string
	// Roles are the platform's own role identifiers as reported for this space:
	// Discord role snowflakes, or the Telegram membership status. What they mean
	// is decided by the org, not here.
	Roles []string
	// IsSpaceAdmin reports platform-level administrative authority over the
	// space the message arrived in.
	IsSpaceAdmin bool
}

// Ref is the stable, opaque owner reference for this actor.
//
// It is deliberately prefixed by channel. Two platforms' numeric id spaces are
// unrelated, and an unprefixed id would let a Discord snowflake collide with a
// Telegram user id in any table keyed on the reference.
func (a Actor) Ref() string {
	return string(a.Channel) + ":" + a.UserID
}

// Conversation identifies where a reply goes.
type Conversation struct {
	Channel Channel
	// SpaceID is the Telegram chat id or the Discord guild id.
	//
	// This is the tenant key. It resolves to exactly one org, and that lookup
	// happens before any message content is interpreted.
	SpaceID string
	// ThreadID is the channel or forum topic the message arrived in.
	ThreadID string
	// MessageID is what to reply to, where the platform supports it.
	MessageID string
	// InteractionToken is Discord's one-shot followup handle. It expires fifteen
	// minutes after the interaction and is the only route by which a reply can
	// reach the invoker privately.
	InteractionToken string
	// IsDM reports a one-to-one conversation, where nobody but the actor can
	// read the reply and "ephemeral" is satisfied for free.
	IsDM bool
}

// Inbound is one instruction, channel-agnostic.
type Inbound struct {
	// DedupeID is stable across a platform's delivery retries and unique across
	// every channel this build serves. It is claimed exactly once before
	// anything acts on the message: a retried delivery must not produce a second
	// confirmation, because a user who tapped both would pay twice.
	DedupeID string

	Actor        Actor
	Conversation Conversation

	// Command is the slash command name, lowercase, with the leading '/' and any
	// "@botname" suffix already removed. Empty means free text.
	Command string

	// Args is the argument text with every prefix already stripped.
	//
	// This, and only this, is the string handed to intent.Tokenize, and it is
	// also the string shown back to the user as what they said. That equality is
	// load-bearing: every span the decoder returns is checked against a
	// tokenization of this exact string, so tokenizing anything else — a version
	// with the prefix still attached, or one normalised a second time — turns
	// the span check into theatre.
	Args string

	// Options are structured slash-command arguments. Discord populates them
	// from the interaction body; Telegram has no equivalent and leaves them nil,
	// with everything in Args.
	Options map[string]string

	ReceivedAt time.Time
}

// Reply is an outbound message.
//
// There is deliberately no field for suppressing link previews. That is not a
// per-message choice: a preview crawler fetching a confirmation link both leaks
// the URL to the platform's servers and consumes a single-use token, so every
// transport suppresses previews unconditionally and the conformance suite
// checks that it does. A field here would be something a future caller could
// forget.
type Reply struct {
	Text string
	// Ephemeral asks the platform to show this only to the actor. It is required
	// for any reply carrying a link that grants authority — see ErrLinkInPublic.
	Ephemeral bool
}
