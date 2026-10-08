package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/ledger/store"
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
		switch err := s.provisioningOpen(ctx, scope); {
		case errors.Is(err, store.ErrProvisioningOff):
			// Said here, in chat, rather than on a page the link would have
			// led to: there is nothing this person can do about it themselves.
			return s.reply(ctx, out, m, provisioningOffReply)
		case err != nil:
			return err
		}
		// Asked now, before a link exists. The limiter refuses somebody first
		// seen minutes ago, and it used to do so only after they had been
		// handed a link, opened it, and had a key generated for a wallet they
		// were not going to get.
		wait, err := s.store.ProvisioningWait(ctx, scope.Org, m.Actor.Channel, m.Actor.UserID)
		if err != nil {
			return err
		}
		if wait > 0 {
			return s.reply(ctx, out, m, tooNewReply(wait))
		}
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

// provisioningOffReply answers a free-text instruction in a workspace that
// does not pay for members' wallets.
const provisioningOffReply = "To send in your own words I need a wallet to send from, " +
	"and you don't have one here yet.\n\n" +
	"If you have a Stellar wallet already, /link it and I'll use that — you sign " +
	"each payment in your own wallet. Or an admin can have me set wallets up for " +
	"members, with /provisioning on, once a treasury is linked. " +
	"To pay from the treasury instead, use /pay."

// tooNewReply tells somebody a wallet cannot be set up for them quite yet, and
// when it can.
//
// The reason is given, because a wait with no reason reads as a fault, and
// because it is not about them: a wallet costs the operator a reserve, and
// making anybody who has just appeared wait is what stops a script from adding
// itself to a group and asking for one after another.
func tooNewReply(wait time.Duration) string {
	minutes := int(wait.Round(time.Minute) / time.Minute)
	when := "in about a minute"
	if minutes > 1 {
		when = fmt.Sprintf("in about %d minutes", minutes)
	}
	return "I can set a wallet up for you, but not quite yet: I wait a little after first " +
		"meeting someone, and after a workspace is created, before paying for one. " +
		"Message me again " + when + " and I'll send you the link.\n\n" +
		"If you already have a Stellar wallet you don't need to wait — /link it and I'll use that."
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
		"Let's get your wallet set up first. Open this within %d minutes, on the device "+
			"you'll pay from — the key is made there and stays there:\n%s\n\n"+
			"Once that's done, message me again with what you'd like to send.",
		int(enrollLinkLifetime.Minutes()), link,
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
	IssueLinkLink(scope Scope, hash string, expiresAt time.Time) (string, error)
	IssueApproveLink(scope Scope, id store.ProposalID, expiresAt time.Time) (string, error)
	IssueReclaimLink(scope Scope, hash string, expiresAt time.Time) (string, error)
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
		// None of these is a question. Each message is read on its own, so an
		// answer to "which one did you mean?" would arrive as a new message
		// with no instruction in it. They say what to send again instead.
		body = fmt.Sprintf("More than one saved name matches that: %s. "+
			"Send the whole instruction again with the one you mean.",
			strings.Join(ambiguous.Candidates, ", "))
	case errors.Is(cause, intent.ErrDestinationNotFound):
		body = "I don't have anyone saved under that name. Send it again with their wallet " +
			"address — send 5 to G… — or save the name first with /save."
	case errors.Is(cause, intent.ErrDestinationInvalid):
		body = "That doesn't look like a valid wallet address. Check it and send the instruction again."
	case errors.Is(cause, intent.ErrAmountUnreadable), errors.Is(cause, intent.ErrAmountAmbiguous):
		body = "I couldn't tell how much. Send it again with the amount as a number, like: send 50 to G…"
	case sourceMissing(cause):
		body = "Your wallet doesn't exist on this network yet, so there's nothing to send from. " +
			"It needs to be funded, and to hold the asset, before I can build a payment from it."
	case errors.Is(cause, ErrReadOnly):
		body = "stelfin is in read-only mode right now, so I can't start a payment. " +
			"Nothing was sent — try again once it's back."
	case errors.Is(cause, ErrNoAccount):
		body = "You don't have a wallet here to send from yet. Message me what you'd like " +
			"to send and I'll walk you through setting one up, or /link one you already have."
	case errors.Is(cause, ErrNotASend), errors.Is(cause, intent.ErrSpanMismatch),
		errors.Is(cause, intent.ErrSpanOutOfRange), errors.Is(cause, intent.ErrUnknownAction),
		errors.Is(cause, intent.ErrMissingField), errors.Is(cause, intent.ErrUndecodable):
		// Includes the case where the decode could not be grounded, and the
		// one where the model read the message and found no instruction in it
		// — "hello", or a question. That last is the model working, not
		// failing, and used to be answered with an apology for a fault.
		body = notAnInstructionReply
	default:
		// Anything unrecognised is ours, not theirs.
		body = "Something went wrong on my side. Nothing was sent — please try again in a moment."
	}

	if err := s.reply(ctx, out, m, body); err != nil {
		return fmt.Errorf("api: reply after %v: %w", cause, err)
	}
	return nil
}

// notAnInstructionReply answers a message that is not something to act on.
//
// It says what a sentence here can do and where everything else is, because the
// person who wrote "what's our balance?" was not making a mistake.
const notAnInstructionReply = "I didn't find a payment in that. In your own words I can do one " +
	"thing: send from your wallet — for example, send 5 to G… or send 5 to mum once you've saved " +
	"mum with /save. For everything else there are commands: /help lists them."

// sourceMissing reports a payment that could not be built because the account
// it would come from is not on the network. A linked wallet is only an address
// somebody proved they hold the key for; nothing says it was ever funded.
func sourceMissing(err error) bool {
	var problem *horizonclient.Error
	return errors.As(err, &problem) && problem.Problem.Status == http.StatusNotFound
}
