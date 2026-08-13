package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ezedike-evan/stelfin/api/intent"
)

// Inbound handling: the WhatsApp payload, and what to do with it.
//
// Two properties matter more than the parsing itself.
//
// Meta retries any delivery it considers slow or failed, so the same message
// arrives more than once. A message is therefore claimed exactly once, by
// message id, before anything acts on it — otherwise one instruction would
// produce two confirmations and, if the user tapped both, two payments.
//
// And the message body is untrusted input from the open internet. It reaches
// the model only as tokens the backend produced, and anything the model says
// about it is re-checked against those tokens. Nothing in this file trusts the
// text for anything except being text.

// Messenger sends a reply back over WhatsApp.
type Messenger interface {
	Send(ctx context.Context, to, body string) error
}

// InboundMessage is one text message from a user.
type InboundMessage struct {
	// ID is Meta's message id, stable across retries.
	ID string
	// From is the sender's phone number in E.164 without the leading '+',
	// which is how Meta reports it.
	From string
	// Text is the message body.
	Text string
}

// metaPayload is the slice of Meta's webhook envelope this server reads.
type metaPayload struct {
	Entry []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Messages []struct {
					ID   string `json:"id"`
					From string `json:"from"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// ParseInbound extracts the text messages from a Meta webhook body.
//
// Deliveries also carry status callbacks (sent/delivered/read) and non-text
// message types. Both are skipped rather than erroring: a payload this server
// has nothing to do with is a normal delivery, not a failure.
func ParseInbound(body []byte) ([]InboundMessage, error) {
	var payload metaPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("api: unparseable webhook payload: %w", err)
	}

	var out []InboundMessage
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" {
				continue
			}
			for _, m := range change.Value.Messages {
				// Only text for now. A voice note grounds against a transcript
				// that is itself model output, which is a lower trust tier and
				// needs its own handling before it can move money.
				if m.Type != "text" || m.ID == "" || m.From == "" {
					continue
				}
				out = append(out, InboundMessage{
					ID:   m.ID,
					From: m.From,
					Text: m.Text.Body,
				})
			}
		}
	}
	return out, nil
}

// claimMessage records a message id, reporting whether this caller won it.
//
// An insert that either succeeds or conflicts, rather than a read followed by
// a write: two concurrent retries of the same delivery cannot both proceed.
func (s *Service) claimMessage(ctx context.Context, m InboundMessage) (bool, error) {
	var claimed bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO processed_messages (id, sender)
		VALUES ($1, $2)
		ON CONFLICT (id) DO NOTHING
		RETURNING true`,
		m.ID, m.From,
	).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		// The conflict path: another delivery of this message already claimed it.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("api: claim message %s: %w", m.ID, err)
	}
	return true, nil
}

// HandleInbound processes one message end to end.
//
// It returns nil for messages it deliberately ignores — a duplicate delivery,
// something it cannot act on — because the caller's job is to keep the webhook
// healthy, not to surface every non-event as a failure.
func (s *Service) HandleInbound(
	ctx context.Context, m InboundMessage, msgr Messenger, links Linker,
) error {
	claimed, err := s.claimMessage(ctx, m)
	if err != nil {
		return err
	}
	if !claimed {
		// Meta retried something already handled. Silence is correct: replying
		// again would tell the user twice.
		return nil
	}

	ownerRef := "+" + strings.TrimPrefix(m.From, "+")

	// An unenrolled phone number has nothing PrepareSend could resolve "from",
	// and running the decoder for it would spend an LLM call to reject
	// something the account state already rules out. Checked first, deciding
	// before any message content is even looked at.
	enrolled, err := s.hasStellarAccount(ctx, ownerRef)
	if err != nil {
		return err
	}
	if !enrolled {
		return s.replyWithEnrollLink(ctx, msgr, links, ownerRef)
	}

	confirmation, err := s.PrepareSend(ctx, ownerRef, []string{m.Text})
	if err != nil {
		return s.replyWithProblem(ctx, msgr, ownerRef, err)
	}

	link, err := links.IssueConfirmLink(ownerRef, confirmation.Hash, time.Now().Add(confirmLinkLifetime))
	if err != nil {
		return err
	}

	// The reply repeats the user's own words alongside the amount read back
	// out of the transaction, so a decode that drifted from what they meant is
	// visible in the message rather than only on the confirmation page.
	body := fmt.Sprintf(
		"Send %s %s to %s?\n\nYou said: %q to %q\n\nTap to confirm — the link expires in %d minutes:\n%s",
		confirmation.AmountDisplay, confirmation.AssetCode, confirmation.ToLabel,
		confirmation.SaidAmount, confirmation.SaidDestination,
		int(confirmLinkLifetime.Minutes()), link,
	)
	return msgr.Send(ctx, ownerRef, body)
}

// replyWithEnrollLink sends a new (or not-yet-enrolled) user the link that
// creates their account.
func (s *Service) replyWithEnrollLink(ctx context.Context, msgr Messenger, links Linker, ownerRef string) error {
	link, err := links.IssueEnrollLink(ownerRef, time.Now().Add(enrollLinkLifetime))
	if err != nil {
		return err
	}
	body := fmt.Sprintf(
		"Let's get your wallet set up first — it only takes a moment. Tap the link below:\n%s\n\n"+
			"Once that's done, message me again with what you'd like to send.",
		link,
	)
	return msgr.Send(ctx, ownerRef, body)
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

// Linker mints the links a WhatsApp reply carries authority through. The
// Server implements it.
type Linker interface {
	IssueConfirmLink(ownerRef, hash string, expiresAt time.Time) (string, error)
	IssueEnrollLink(ownerRef string, expiresAt time.Time) (string, error)
}

// replyWithProblem turns a failure into something the user can act on.
//
// The mapping is deliberately narrow: the user is told what they can fix and
// nothing else. An internal error becomes a generic apology rather than a
// description of what broke, and an unrecognised message never guesses.
func (s *Service) replyWithProblem(ctx context.Context, msgr Messenger, to string, cause error) error {
	var ambiguous *intent.AmbiguousError

	var body string
	switch {
	case errors.As(cause, &ambiguous):
		body = fmt.Sprintf("Which one did you mean? I have %s saved.",
			strings.Join(ambiguous.Candidates, ", "))
	case errors.Is(cause, intent.ErrDestinationNotFound):
		body = "I don't have that person saved. Send me their phone number or wallet address and I'll use that."
	case errors.Is(cause, intent.ErrDestinationInvalid):
		body = "That doesn't look like a valid phone number or wallet address. Can you check it?"
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

	if err := msgr.Send(ctx, to, body); err != nil {
		return fmt.Errorf("api: reply after %v: %w", cause, err)
	}
	return nil
}
