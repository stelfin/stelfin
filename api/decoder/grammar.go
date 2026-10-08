package decoder

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/stelfin/stelfin/api/intent"
)

// Grammar reads the one sentence a payment can always be written as, with no
// model at all:
//
//	send 5 to mum
//	pay 250 to GABC…
//
// A verb, one amount, the word "to", and who. It exists so that the product
// does not stop when a model does. A deployment given no model still has
// /send; and one whose provider is down, rate-limited or out of credit still
// reads the instruction most people write.
//
// It is deliberately a grammar and not a guess. Anything it does not recognise
// exactly it declines, and it declines "send 5 XLM to mum" too: the word
// between the amount and "to" might name an asset this deployment does not pay
// in, and reading past it would turn five of one thing into five of another.
// Looser reading is the model's job, and the model's output is re-checked
// against what was typed where this is trusted to be what it is.
//
// What it returns is in the same shape as any decoder's and goes through the
// same intent.Verify. It is not believed either.
type Grammar struct{}

// verbs are the words that start a payment, the same ones intent.Verify accepts
// as a send.
var grammarVerbs = map[string]bool{"send": true, "pay": true, "transfer": true}

// Decode reads the last turn of the conversation.
func (Grammar) Decode(_ context.Context, turns []string) (intent.Decoded, error) {
	if len(turns) == 0 {
		return intent.Decoded{}, errors.New("decoder: conversation is empty")
	}
	turn := len(turns) - 1
	tokens := intent.Tokenize(turns[turn])

	// Trailing punctuation is how sentences end, not part of who is paid.
	end := len(tokens)
	for end > 0 && isPunctuation(tokens[end-1].Text) {
		end--
	}

	// verb amount "to" who…
	if end < 4 || !grammarVerbs[strings.ToLower(tokens[0].Text)] || !strings.EqualFold(tokens[2].Text, "to") {
		return intent.Decoded{}, fmt.Errorf("%w: not in the form \"send <amount> to <who>\"", ErrUndecodable)
	}
	if !startsWithDigit(tokens[1].Text) {
		return intent.Decoded{}, fmt.Errorf("%w: %q is not an amount", ErrUndecodable, tokens[1].Text)
	}

	field := func(start, stop int) intent.Field {
		return intent.Field{
			Text: intent.Text(tokens, start, stop),
			Span: intent.Span{Turn: turn, Start: start, End: stop},
		}
	}

	// An address is one token that is an address. Anything else is a name the
	// member saved, however many words it runs to.
	kind := intent.DestinationBeneficiary
	if end == 4 && strkey.IsValidEd25519PublicKey(tokens[3].Text) {
		kind = intent.DestinationAddress
	}

	return intent.Decoded{
		Action:          field(0, 1),
		Amount:          field(1, 2),
		Destination:     field(3, end),
		DestinationKind: kind,
	}, nil
}

func isPunctuation(s string) bool {
	return len(s) == 1 && strings.ContainsAny(s, ".!?,;:")
}

func startsWithDigit(s string) bool {
	return s != "" && s[0] >= '0' && s[0] <= '9'
}
