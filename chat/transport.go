package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Ack is the synchronous response a webhook delivery receives before any work
// begins.
//
// It is computed from the already-verified body and written immediately.
// Discord declares an interaction failed if it is not acknowledged within three
// seconds, and Telegram retries a slow response — which would start the same
// payment flow twice. Acknowledging first and working afterwards is therefore
// not an optimisation, it is the only correct order.
type Ack struct {
	Status      int
	ContentType string
	Body        []byte
}

// Delivery is one verified, parsed webhook request.
type Delivery struct {
	Ack Ack
	// Messages may be empty. A delivery this transport has nothing to act on —
	// a Discord PING, a Telegram edit, a join notification — is a normal
	// outcome, not an error.
	Messages []Inbound
}

// Transport is one messaging platform.
//
// The three methods are ordered by trust: nothing is parsed that was not
// verified, and nothing is acted on that was not parsed.
type Transport interface {
	Channel() Channel

	// Verify authenticates a raw request and returns the exact bytes that were
	// authenticated. Implementations read the body once, with ReadLimited, and
	// verify those bytes.
	Verify(r *http.Request) ([]byte, error)

	// Parse turns verified bytes into a Delivery.
	Parse(body []byte) (Delivery, error)

	// Send delivers a reply. Link previews are always suppressed, and outbound
	// text is never rendered as markup.
	Send(ctx context.Context, to Conversation, actor Actor, r Reply) error

	// RegisterCommands publishes the command set to the platform. Idempotent:
	// it is called on every start.
	RegisterCommands(ctx context.Context, cmds []Command) error
}

// ErrLinkInPublic reports an attempt to post an authority-carrying link
// somewhere more than the actor can read it.
//
// A confirmation link is single-use payment authority. The WhatsApp-era code
// worried about preview crawlers fetching it; in a group the bystander problem
// is worse, because every member can see the link and any of them can tap it
// first. Making this a refusal rather than a convention means a new command
// cannot leak authority by forgetting a flag.
var ErrLinkInPublic = errors.New("chat: a reply carrying an authority link must be ephemeral")

// Registry holds the transports this deployment serves and routes replies.
type Registry struct {
	byChannel map[Channel]Transport
	// linkPrefix is the base URL this deployment mints authority links under.
	// Any reply containing it must be private to the actor.
	linkPrefix string
}

// NewRegistry returns a Registry over the given transports.
//
// linkPrefix is the deployment's base URL. It is required even with no
// transports registered: the check it enables is the point of the type, and a
// registry that silently skipped it because the prefix was empty would fail
// open.
func NewRegistry(linkPrefix string, ts ...Transport) (*Registry, error) {
	if strings.TrimSpace(linkPrefix) == "" {
		return nil, errors.New("chat: link prefix is required")
	}
	byChannel := make(map[Channel]Transport, len(ts))
	for _, t := range ts {
		if t == nil {
			return nil, errors.New("chat: nil transport")
		}
		c := t.Channel()
		if !c.Valid() {
			return nil, fmt.Errorf("chat: transport reports unknown channel %q", c)
		}
		if _, dup := byChannel[c]; dup {
			return nil, fmt.Errorf("chat: two transports registered for %q", c)
		}
		byChannel[c] = t
	}
	return &Registry{byChannel: byChannel, linkPrefix: strings.TrimSuffix(linkPrefix, "/")}, nil
}

// Lookup returns the transport for a channel.
func (r *Registry) Lookup(c Channel) (Transport, bool) {
	t, ok := r.byChannel[c]
	return t, ok
}

// Channels reports which channels are served, for startup logging.
func (r *Registry) Channels() []Channel {
	out := make([]Channel, 0, len(r.byChannel))
	for c := range r.byChannel {
		out = append(out, c)
	}
	return out
}

// RegisterCommands publishes cmds to every transport.
func (r *Registry) RegisterCommands(ctx context.Context, cmds []Command) error {
	for c, t := range r.byChannel {
		if err := t.RegisterCommands(ctx, cmds); err != nil {
			return fmt.Errorf("chat: register commands on %s: %w", c, err)
		}
	}
	return nil
}

// Send dispatches a reply to the right transport, after the link policy check.
func (r *Registry) Send(ctx context.Context, to Conversation, actor Actor, m Reply) error {
	if strings.Contains(m.Text, r.linkPrefix) && !m.Ephemeral && !to.IsDM {
		return ErrLinkInPublic
	}
	t, ok := r.byChannel[to.Channel]
	if !ok {
		return fmt.Errorf("chat: no transport for channel %q", to.Channel)
	}
	return t.Send(ctx, to, actor, m)
}
