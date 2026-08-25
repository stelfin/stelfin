package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
)

// The free-text send path: a message that is an instruction to pay someone
// rather than a command.
//
// The message body is untrusted input from the open internet. It reaches the
// model only as tokens the backend produced, and anything the model says about
// it is re-checked against those tokens. Nothing here trusts the text for
// anything except being text.

// Replier delivers a reply back to the conversation a message arrived on.
//
// *chat.Registry implements it. The indirection exists so the core can be
// tested without a platform, and so the registry's refusal to post an authority
// link where bystanders can read it sits on the path every reply takes.
type Replier interface {
	Send(ctx context.Context, to chat.Conversation, actor chat.Actor, r chat.Reply) error
}

// HandleSend turns one free-text message into a payment awaiting approval.
//
// Tenancy, the exactly-once claim and command dispatch all happen before this,
// in core. By the time anything here runs the org is known, the message is this
// process's to act on, and what is left is the part this package exists for:
// turning untrusted text into something a person can check and sign.
func (s *Service) HandleSend(
	ctx context.Context, scope Scope, m chat.Inbound, out Replier, links Linker,
) error {
	if err := scope.check(); err != nil {
		return err
	}

	// An owner with no account has nothing PrepareSend could resolve "from",
	// and running the decoder for them would spend an LLM call to reject
	// something the account state already rules out. Checked first, deciding
	// before any message content is even looked at.
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
