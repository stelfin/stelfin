package decoder

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stelfin/stelfin/api/intent"
)

func verified(t *testing.T, text string, d intent.Decoded) *intent.Grounded {
	t.Helper()
	g, err := intent.Verify([][]intent.Token{intent.Tokenize(text)}, d)
	if err != nil {
		t.Fatalf("Verify(%q): %v", text, err)
	}
	return g
}

func TestGrammarReadsTheOneSentence(t *testing.T) {
	address := keypair.MustRandom().Address()
	cases := map[string]struct {
		amount, to string
		kind       intent.DestinationKind
	}{
		"send 5 to mum":              {"5", "mum", intent.DestinationBeneficiary},
		"Send 5 to mum.":             {"5", "mum", intent.DestinationBeneficiary},
		"pay 5,000 to the auditor":   {"5,000", "the auditor", intent.DestinationBeneficiary},
		"transfer 2.50 to mum!":      {"2.50", "mum", intent.DestinationBeneficiary},
		"send 3 to " + address:       {"3", address, intent.DestinationAddress},
		"SEND 3 TO " + address + ".": {"3", address, intent.DestinationAddress},
	}
	for text, want := range cases {
		t.Run(text, func(t *testing.T) {
			d, err := (Grammar{}).Decode(context.Background(), []string{text})
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			// It is held to exactly what a model is held to.
			g := verified(t, text, d)
			if g.Action != intent.ActionSend || g.AmountText != want.amount ||
				g.DestinationText != want.to || g.DestinationKind != want.kind {
				t.Errorf("grounded = %+v", g)
			}
		})
	}
}

// TestGrammarDeclinesWhatItDoesNotRecogniseExactly: it is a grammar and not a
// guess. Reading past a word it does not know is how five of one thing becomes
// five of another.
func TestGrammarDeclinesWhatItDoesNotRecogniseExactly(t *testing.T) {
	for _, text := range []string{
		"",
		"hello",
		"send",
		"send 5",
		"send 5 to",
		"send 5 to .",
		"send mum 5",
		"send five to mum",
		"send 5 XLM to mum",    // an asset this deployment may not pay in
		"send 5 USDC to mum",   // even the right one: it cannot know that
		"please send 5 to mum", // prose in front is the model's to read
		"what's our balance?",
		"he said send 5 to mum", // quoting is not instructing
		"balance",
	} {
		if d, err := (Grammar{}).Decode(context.Background(), []string{text}); !errors.Is(err, intent.ErrUndecodable) {
			t.Errorf("%q: decoded as %+v (err %v), want it declined", text, d, err)
		}
	}
	if _, err := (Grammar{}).Decode(context.Background(), nil); err == nil {
		t.Error("an empty conversation was decoded")
	}
}

func TestGrammarReadsTheLastTurn(t *testing.T) {
	d, err := (Grammar{}).Decode(context.Background(), []string{"hello", "send 5 to mum"})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if d.Action.Span.Turn != 1 || d.Amount.Span.Turn != 1 || d.Destination.Span.Turn != 1 {
		t.Errorf("spans point at turn %d, want the last", d.Action.Span.Turn)
	}
}

type fixed struct {
	decoded intent.Decoded
	err     error
	calls   int
}

func (f *fixed) Decode(context.Context, []string) (intent.Decoded, error) {
	f.calls++
	return f.decoded, f.err
}

var nowhere = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestTheModelIsAskedFirst(t *testing.T) {
	model := &fixed{decoded: intent.Decoded{Action: intent.Field{Text: "from the model"}}}
	got, err := WithGrammar(model, nowhere).Decode(context.Background(), []string{"send 5 to mum"})
	if err != nil || got.Action.Text != "from the model" || model.calls != 1 {
		t.Fatalf("got %+v, %v after %d calls", got, err, model.calls)
	}
}

// TestAPaymentIsReadWhenTheModelCannotBeAsked: a product whose only way to send
// money stops when somebody else's API does is one outage from looking broken.
func TestAPaymentIsReadWhenTheModelCannotBeAsked(t *testing.T) {
	model := &fixed{err: errors.New("decoder: the model provider answered 429: rate limit")}
	got, err := WithGrammar(model, nowhere).Decode(context.Background(), []string{"send 5 to mum"})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	verified(t, "send 5 to mum", got)
}

// TestAModelThatMissedThePlainSentenceIsOverruled: a small model will sometimes
// say there is no instruction in "send 5 to mum".
func TestAModelThatMissedThePlainSentenceIsOverruled(t *testing.T) {
	model := &fixed{err: intent.ErrUndecodable}
	got, err := WithGrammar(model, nowhere).Decode(context.Background(), []string{"send 5 to mum"})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	verified(t, "send 5 to mum", got)
}

// TestWhatNeitherCanReadSaysWhy: the two failures need different things said.
// If the model answered, the message is not an instruction. If it did not, the
// message may be perfectly good.
func TestWhatNeitherCanReadSaysWhy(t *testing.T) {
	text := []string{"could you pay the auditor what we owe"}

	_, err := WithGrammar(&fixed{err: intent.ErrUndecodable}, nowhere).Decode(context.Background(), text)
	if !errors.Is(err, intent.ErrUndecodable) || errors.Is(err, intent.ErrDecoderUnavailable) {
		t.Errorf("the model answered and found nothing: error = %v", err)
	}

	_, err = WithGrammar(&fixed{err: errors.New("timeout")}, nowhere).Decode(context.Background(), text)
	if !errors.Is(err, intent.ErrDecoderUnavailable) || errors.Is(err, intent.ErrUndecodable) {
		t.Errorf("the model could not be asked: error = %v", err)
	}
}
