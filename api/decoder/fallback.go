package decoder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/stelfin/stelfin/api/intent"
)

// Decoder is what every decoder here is.
type Decoder interface {
	Decode(ctx context.Context, turns []string) (intent.Decoded, error)
}

// WithGrammar returns a decoder that asks model first and falls back to the
// fixed grammar when model cannot help.
//
// "Cannot help" is two different things, and the grammar answers both.
//
// The model could not be asked: the provider is down, rate-limited, out of
// credit, or misconfigured. That is nobody's fault who is typing, and a product
// whose only way to send money stops when somebody else's API does is one
// outage away from looking broken. The sentence most people write is the one
// the grammar reads.
//
// Or the model was asked and found no instruction. Usually it is right. But a
// small model will sometimes say that of "send 5 to mum", and the grammar,
// which reads that sentence and almost nothing else, is a cheap second opinion
// that cannot be talked into anything: what it returns is verified against the
// tokens like any other decode.
//
// When neither can read the message, the error says which kind of failure it
// was, because what to tell the person differs. If the model answered, the
// message is not an instruction. If it did not, the message may be perfectly
// good, and they should be told how to write it so that it goes through anyway.
func WithGrammar(model Decoder, log *slog.Logger) Decoder {
	if log == nil {
		log = slog.Default()
	}
	return &withGrammar{model: model, log: log}
}

type withGrammar struct {
	model Decoder
	log   *slog.Logger
}

func (w *withGrammar) Decode(ctx context.Context, turns []string) (intent.Decoded, error) {
	decoded, err := w.model.Decode(ctx, turns)
	if err == nil {
		return decoded, nil
	}

	answered := errors.Is(err, intent.ErrUndecodable)
	if !answered {
		// Logged here because this is the last place that knows. From here on
		// it is either read by the grammar or reported to the person as
		// unavailable, and neither of those says what went wrong.
		w.log.Error("the model could not read a message; trying the fixed grammar", "error", err)
	}

	if plain, plainErr := (Grammar{}).Decode(ctx, turns); plainErr == nil {
		return plain, nil
	}
	if answered {
		return intent.Decoded{}, err
	}
	return intent.Decoded{}, fmt.Errorf("%w: %v", intent.ErrDecoderUnavailable, err)
}
