package app

import (
	"log/slog"
	"net/url"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/stelfin/stelfin/api/decoder"
	"github.com/stelfin/stelfin/internal/config"
)

// decoderFor builds what reads a sentence on this deployment, and says which
// it is.
//
// Said at start because the choice is otherwise invisible until somebody
// writes a sentence, and then it shows up as how well they were understood.
//
// A model is always backed by the fixed grammar. Without that, the one way to
// send money in a sentence stops whenever somebody else's API does. With no
// model configured the grammar is all there is, which is a deployment that
// reads "send 5 to mum" and nothing looser, and says so here.
func decoderFor(cfg *config.Config, log *slog.Logger) (decoder.Decoder, error) {
	switch cfg.DecoderKind() {
	case config.DecoderGrammar:
		log.Warn("no language model is configured; a sentence is read only in the form " +
			"\"send <amount> to <who>\"")
		return decoder.Grammar{}, nil

	case config.DecoderOpenAI:
		model, err := decoder.NewOpenAI(decoder.OpenAIConfig{
			BaseURL: cfg.DecoderBaseURL,
			APIKey:  cfg.DecoderAPIKey,
			Model:   cfg.DecoderModel,
		})
		if err != nil {
			return nil, err
		}
		log.Info("sentences are read by a compatible model",
			"provider", providerHost(cfg.DecoderBaseURL), "model", cfg.DecoderModel)
		return decoder.WithGrammar(model, log), nil

	default:
		log.Info("sentences are read by Anthropic", "model", cfg.DecoderModel)
		return decoder.WithGrammar(decoder.New(decoder.Config{
			APIKey: cfg.AnthropicAPIKey,
			Model:  anthropic.Model(cfg.DecoderModel),
			// Config has already rejected anything that is not a known level.
			Effort: anthropic.OutputConfigEffort(cfg.DecoderEffort),
		}), log), nil
	}
}

// providerHost is the host of a provider's base URL, for the log. Only the
// host: a base URL can carry a path somebody put a credential in.
func providerHost(base string) string {
	if base == "" {
		base = decoder.DefaultOpenAIBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return u.Host
}
