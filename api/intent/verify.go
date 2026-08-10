package intent

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ezedike-evan/stelfin/internal/money"
)

// Action is what the user is asking for.
type Action string

const (
	ActionSend    Action = "send"
	ActionBalance Action = "balance"
	ActionAddress Action = "address"
	ActionHistory Action = "history"
)

// actionVerbs maps the words a user may use onto an action. Matching happens
// against the user's own tokens, so a model cannot introduce an action by
// naming it — the word has to be in the message.
var actionVerbs = map[string]Action{
	"send": ActionSend, "transfer": ActionSend, "pay": ActionSend,
	"give": ActionSend, "credit": ActionSend,

	"balance": ActionBalance, "bal": ActionBalance,

	"address": ActionAddress, "wallet": ActionAddress,

	"history": ActionHistory, "transactions": ActionHistory,
}

// DestinationKind says how a destination should be resolved. The model
// proposes it; resolution itself is a deterministic lookup elsewhere.
type DestinationKind string

const (
	DestinationBeneficiary DestinationKind = "beneficiary"
	DestinationPhone       DestinationKind = "phone"
	DestinationAddress     DestinationKind = "address"
)

// Span points at a contiguous run of tokens within one turn of the
// conversation. End is exclusive.
//
// Spans are turn-indexed because a multi-turn flow can take the amount from one
// message and the recipient from another; a bare token index would be
// meaningless across turns.
type Span struct {
	Turn  int
	Start int
	End   int
}

// Field is a single claim from the model: some text, and where in the user's
// message it came from. Untrusted until Verify has checked it.
type Field struct {
	Text string
	Span Span
}

// Decoded is raw model output. Nothing here may reach money code before Verify.
type Decoded struct {
	Action          Field
	Amount          Field
	Destination     Field
	DestinationKind DestinationKind
}

// Grounded is what survived verification: every value traced to text the user
// actually wrote.
//
// The confirmation screen must render from this and from the transaction it
// produces, never from anything the model wrote. That is what protects against
// an instruction injected into the user's own message, which Verify passes by
// design because those tokens really are present.
type Grounded struct {
	Action Action
	Amount money.Stroops

	// AmountText and DestinationText are the user's exact words, kept for the
	// confirmation so they can be shown the phrase their money came from.
	AmountText      string
	DestinationText string
	DestinationKind DestinationKind

	AmountSpan      Span
	DestinationSpan Span
}

var (
	// ErrSpanOutOfRange reports a span that does not address real tokens.
	ErrSpanOutOfRange = errors.New("intent: span is out of range")

	// ErrSpanMismatch reports a field whose text is not what its span actually
	// says. This is the signature of a hallucinated value or a decode steered
	// by something outside the user's message.
	ErrSpanMismatch = errors.New("intent: field text does not match its span")

	// ErrUnknownAction reports a span that does not contain a verb we act on.
	ErrUnknownAction = errors.New("intent: no recognised action in span")

	// ErrMissingField reports a required field the model did not provide.
	ErrMissingField = errors.New("intent: required field is missing")
)

// Verify checks every field against the conversation's own tokens and returns
// the grounded instruction.
//
// conversation is indexed by turn, each turn holding the tokens produced by
// Tokenize for that message. It must be the backend's tokenization: if the
// model supplied the tokens, it could supply positions to match any claim and
// the whole mechanism would be decorative.
func Verify(conversation [][]Token, d Decoded) (*Grounded, error) {
	action, err := verifyAction(conversation, d.Action)
	if err != nil {
		return nil, err
	}

	g := &Grounded{Action: action}
	if action != ActionSend {
		// Only a send carries an amount and a destination. Requiring them for
		// a balance check would reject perfectly good messages.
		return g, nil
	}

	if d.Amount.Text == "" {
		return nil, fmt.Errorf("%w: amount", ErrMissingField)
	}
	amountText, err := groundedText(conversation, d.Amount)
	if err != nil {
		return nil, fmt.Errorf("amount: %w", err)
	}
	amount, err := NormalizeAmount(amountText)
	if err != nil {
		return nil, fmt.Errorf("amount: %w", err)
	}
	if amount.Sign() <= 0 {
		return nil, fmt.Errorf("intent: amount %s is not positive", amount)
	}

	if d.Destination.Text == "" {
		return nil, fmt.Errorf("%w: destination", ErrMissingField)
	}
	destText, err := groundedText(conversation, d.Destination)
	if err != nil {
		return nil, fmt.Errorf("destination: %w", err)
	}
	switch d.DestinationKind {
	case DestinationBeneficiary, DestinationPhone, DestinationAddress:
	default:
		return nil, fmt.Errorf("intent: unknown destination kind %q", d.DestinationKind)
	}

	g.Amount = amount
	g.AmountText = amountText
	g.AmountSpan = d.Amount.Span
	g.DestinationText = destText
	g.DestinationKind = d.DestinationKind
	g.DestinationSpan = d.Destination.Span
	return g, nil
}

// groundedText returns the text a span actually covers, after confirming the
// model's claim about it. The returned value is always the *user's* text, never
// the model's — so even a claim that matches is not the one carried forward.
func groundedText(conversation [][]Token, f Field) (string, error) {
	if f.Span.Turn < 0 || f.Span.Turn >= len(conversation) {
		return "", fmt.Errorf("%w: turn %d of %d", ErrSpanOutOfRange, f.Span.Turn, len(conversation))
	}
	tokens := conversation[f.Span.Turn]
	if f.Span.Start < 0 || f.Span.End > len(tokens) || f.Span.Start >= f.Span.End {
		return "", fmt.Errorf("%w: [%d,%d) of %d tokens",
			ErrSpanOutOfRange, f.Span.Start, f.Span.End, len(tokens))
	}

	actual := Text(tokens, f.Span.Start, f.Span.End)
	if !equivalent(actual, f.Text) {
		return "", fmt.Errorf("%w: span says %q, model claimed %q", ErrSpanMismatch, actual, f.Text)
	}
	return actual, nil
}

func verifyAction(conversation [][]Token, f Field) (Action, error) {
	if f.Text == "" {
		return "", fmt.Errorf("%w: action", ErrMissingField)
	}
	text, err := groundedText(conversation, f)
	if err != nil {
		return "", fmt.Errorf("action: %w", err)
	}

	// The verb must appear in the span the model pointed at, not merely be a
	// plausible label for the message.
	for _, word := range strings.Fields(strings.ToLower(text)) {
		if action, ok := actionVerbs[strings.Trim(word, ".,!?")]; ok {
			return action, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownAction, text)
}

// equivalent compares a model's claim with the user's text, ignoring case and
// whitespace runs. Neither can change a number's value or a name's identity, so
// tolerating them costs nothing; tolerating anything more would start to let
// the model rewrite what the user said.
func equivalent(a, b string) bool {
	return canonical(a) == canonical(b)
}

func canonical(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
