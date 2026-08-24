package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
)

// Inbound handling: what to do with one message from a chat platform.
//
// Two properties matter more than any of the plumbing.
//
// Platforms retry a delivery they consider slow or failed, so the same message
// arrives more than once. A message is therefore claimed exactly once, by its
// dedupe id, before anything acts on it — otherwise one instruction would
// produce two confirmations and, if the user tapped both, two payments.
//
// And the message body is untrusted input from the open internet. It reaches
// the model only as tokens the backend produced, and anything the model says
// about it is re-checked against those tokens. Nothing here trusts the text for
// anything except being text.

// Replier delivers a reply back to the conversation a message arrived on.
//
// *chat.Registry implements it. The indirection exists so the core can be
// tested without a platform, and so the registry's refusal to post an authority
// link where bystanders can read it sits on the path every reply takes.
type Replier interface {
	Send(ctx context.Context, to chat.Conversation, actor chat.Actor, r chat.Reply) error
}

// claimMessage records a dedupe id, reporting whether this caller won it.
//
// An insert that either succeeds or conflicts, rather than a read followed by a
// write: two concurrent retries of the same delivery cannot both proceed.
func (s *Service) claimMessage(ctx context.Context, m chat.Inbound) (bool, error) {
	var claimed bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO processed_messages (id, sender)
		VALUES ($1, $2)
		ON CONFLICT (id) DO NOTHING
		RETURNING true`,
		m.DedupeID, m.Actor.Ref(),
	).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		// The conflict path: another delivery of this message already claimed it.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("api: claim message %s: %w", m.DedupeID, err)
	}
	return true, nil
}

// HandleInbound processes one message end to end.
//
// It returns nil for messages it deliberately ignores — a duplicate delivery,
// something it cannot act on — because the caller's job is to keep the webhook
// healthy, not to surface every non-event as a failure.
func (s *Service) HandleInbound(
	ctx context.Context, m chat.Inbound, out Replier, links Linker,
) error {
	if m.DedupeID == "" {
		return errors.New("api: inbound message has no dedupe id")
	}

	// Tenancy first, before a single byte of the message content is looked at.
	//
	// An unregistered space is not an error and gets no reply: the bot has been
	// added somewhere nobody has run setup, and answering would be talking to a
	// room that never asked. It also costs nothing — no claim, no decoder call.
	org, ok, err := s.store.OrgForSpace(ctx, m.Conversation.Channel, m.Conversation.SpaceID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if !org.Active() {
		// Suspended tenants keep their history readable and move no money.
		return s.reply(ctx, out, m, "This workspace is suspended. Nothing can be sent right now.")
	}
	claimed, err := s.claimMessage(ctx, m)
	if err != nil {
		return err
	}
	if !claimed {
		// The platform retried something already handled. Silence is correct:
		// replying again would tell the user twice.
		return nil
	}

	// The transport identity becomes the domain identity here, and nowhere
	// else. Actor.Ref is channel-scoped, so the same numeric id on two
	// platforms stays two different owners; the org comes from the space the
	// message arrived in, resolved before any of its content was looked at.
	scope := Scope{Org: org.ID, OwnerRef: m.Actor.Ref()}

	// An unenrolled owner has nothing PrepareSend could resolve "from", and
	// running the decoder for them would spend an LLM call to reject something
	// the account state already rules out. Checked first, deciding before any
	// message content is even looked at.
	enrolled, err := s.hasStellarAccount(ctx, scope)
	if err != nil {
		return err
	}
	if !enrolled {
		return s.replyWithEnrollLink(ctx, out, links, m, scope)
	}

	confirmation, err := s.PrepareSend(ctx, scope, []string{m.Args})
	if err != nil {
		return s.replyWithProblem(ctx, out, m, err)
	}

	link, err := links.IssueConfirmLink(scope, confirmation.Hash, time.Now().Add(confirmLinkLifetime))
	if err != nil {
		return err
	}

	// The reply repeats the user's own words alongside the amount read back out
	// of the transaction, so a decode that drifted from what they meant is
	// visible in the message rather than only on the confirmation page.
	body := fmt.Sprintf(
		"Send %s %s to %s?\n\nYou said: %q to %q\n\nTap to confirm — the link expires in %d minutes:\n%s",
		confirmation.AmountDisplay, confirmation.AssetCode, confirmation.ToLabel,
		confirmation.SaidAmount, confirmation.SaidDestination,
		int(confirmLinkLifetime.Minutes()), link,
	)
	return s.reply(ctx, out, m, body)
}

// reply sends one message back to where m came from.
//
// Every reply is ephemeral. Two of them carry a link that authorises money, and
// the rest quote the user's own payment instruction back at them — neither is
// something a group of several hundred people needs to read. Making it
// unconditional means a new reply cannot leak by forgetting the flag, and the
// registry refuses an authority link that reaches here without it anyway.
func (s *Service) reply(ctx context.Context, out Replier, m chat.Inbound, body string) error {
	return out.Send(ctx, m.Conversation, m.Actor, chat.Reply{Text: body, Ephemeral: true})
}

// replyWithEnrollLink sends a not-yet-enrolled user the link that creates their
// account.
func (s *Service) replyWithEnrollLink(
	ctx context.Context, out Replier, links Linker, m chat.Inbound, scope Scope,
) error {
	link, err := links.IssueEnrollLink(scope, time.Now().Add(enrollLinkLifetime))
	if err != nil {
		return err
	}
	body := fmt.Sprintf(
		"Let's get your wallet set up first — it only takes a moment. Tap the link below:\n%s\n\n"+
			"Once that's done, message me again with what you'd like to send.",
		link,
	)
	return s.reply(ctx, out, m, body)
}

// confirmLinkLifetime bounds how long a confirmation link is usable. It is
// shorter than the transaction's own time bounds so an abandoned link stops
// working before the envelope does.
const confirmLinkLifetime = 10 * time.Minute

// enrollLinkLifetime is shorter than confirmLinkLifetime: the provisioning
// transaction it names carries settlement.DefaultTimeout (180s) of on-chain
// validity from the moment it is built, not from when the link is tapped, so
// the link must expire well inside that window rather than at a round number
// chosen independently of it.
const enrollLinkLifetime = 2 * time.Minute

// Linker mints the links a reply carries authority through. The Server
// implements it.
type Linker interface {
	IssueConfirmLink(scope Scope, hash string, expiresAt time.Time) (string, error)
	IssueEnrollLink(scope Scope, expiresAt time.Time) (string, error)
}

// replyWithProblem turns a failure into something the user can act on.
//
// The mapping is deliberately narrow: the user is told what they can fix and
// nothing else. An internal error becomes a generic apology rather than a
// description of what broke, and an unrecognised message never guesses.
func (s *Service) replyWithProblem(ctx context.Context, out Replier, m chat.Inbound, cause error) error {
	var ambiguous *intent.AmbiguousError

	var body string
	switch {
	case errors.As(cause, &ambiguous):
		body = fmt.Sprintf("Which one did you mean? I have %s saved.",
			strings.Join(ambiguous.Candidates, ", "))
	case errors.Is(cause, intent.ErrDestinationNotFound):
		body = "I don't have that person saved. Send me their wallet address and I'll use that."
	case errors.Is(cause, intent.ErrDestinationInvalid):
		body = "That doesn't look like a valid wallet address. Can you check it?"
	case errors.Is(cause, intent.ErrAmountUnreadable), errors.Is(cause, intent.ErrAmountAmbiguous):
		body = "How much would you like to send? A number works best, like 5000."
	case errors.Is(cause, ErrNoAccount):
		body = "Your wallet isn't set up yet. Give me a moment and try again."
	case errors.Is(cause, ErrNotASend), errors.Is(cause, intent.ErrSpanMismatch),
		errors.Is(cause, intent.ErrSpanOutOfRange), errors.Is(cause, intent.ErrUnknownAction),
		errors.Is(cause, intent.ErrMissingField):
		// Includes the case where the decode could not be grounded. The user
		// gets a question rather than a guess.
		body = "I didn't catch that. Try something like: send 5000 to brother"
	default:
		// Anything unrecognised is ours, not theirs.
		body = "Something went wrong on my side. Nothing was sent — please try again in a moment."
	}

	if err := s.reply(ctx, out, m, body); err != nil {
		return fmt.Errorf("api: reply after %v: %w", cause, err)
	}
	return nil
}
