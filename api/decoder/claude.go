// Package decoder turns a conversation into a structured proposal using a
// language model.
//
// The model's only job is to point at tokens. It is given the backend's own
// tokenization, numbered, and must answer with indices into it — never with
// free text it composed itself. Everything it returns is re-checked against
// those tokens by intent.Verify before any of it can reach a transaction, so a
// model that hallucinates, is steered by injected instructions, or simply
// returns nonsense produces a rejected decode rather than a wrong payment.
//
// Two rules make that work, and both live here rather than in the prompt:
// the backend defines the tokens (a model asked to supply its own indices
// could supply whichever ones match its claim), and the backend does the
// arithmetic (see api/intent.NormalizeAmount).
package decoder

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/stelfin/stelfin/api/intent"
)

// DefaultModel is the model used when none is configured.
const DefaultModel anthropic.Model = "claude-opus-5"

// DefaultMaxTokens bounds the decode response. The output is a small JSON
// object, but adaptive thinking shares this budget, so it needs headroom.
const DefaultMaxTokens int64 = 4096

// ErrUndecodable reports a conversation the model could not read as a
// supported instruction. It is a normal outcome, not a failure: the caller
// asks the user rather than guessing.
//
// The same value as intent.ErrUndecodable, which is where a caller that does
// not otherwise depend on this package checks for it.
var ErrUndecodable = intent.ErrUndecodable

// messagesAPI is the slice of the Anthropic client this package uses.
// Narrowing it keeps the prompt and parsing testable without a network or an
// API key.
type messagesAPI interface {
	New(context.Context, anthropic.MessageNewParams, ...option.RequestOption) (*anthropic.Message, error)
}

// Config describes a decoder.
type Config struct {
	// APIKey authenticates to the Anthropic API. Empty falls back to the
	// SDK's own resolution (ANTHROPIC_API_KEY, then a configured profile).
	APIKey string
	// Model is the model id. Empty means DefaultModel.
	Model anthropic.Model
	// Effort controls how hard the model works. Empty means the API default.
	Effort anthropic.OutputConfigEffort
}

// Claude decodes conversations with the Anthropic API.
type Claude struct {
	messages  messagesAPI
	model     anthropic.Model
	effort    anthropic.OutputConfigEffort
	maxTokens int64
}

// New returns a Claude decoder.
func New(cfg Config) *Claude {
	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	client := anthropic.NewClient(opts...)

	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	return &Claude{
		messages:  &client.Messages,
		model:     model,
		effort:    cfg.Effort,
		maxTokens: DefaultMaxTokens,
	}
}

// Decode asks the model to locate the instruction in the conversation.
//
// The returned Decoded is untrusted. It must go through intent.Verify against
// the same tokenization before anything acts on it.
func (c *Claude) Decode(ctx context.Context, turns []string) (intent.Decoded, error) {
	if len(turns) == 0 {
		return intent.Decoded{}, errors.New("decoder: conversation is empty")
	}

	params := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System: []anthropic.TextBlockParam{{
			Text: systemPrompt,
			// The contract is identical on every request, so caching it makes
			// every decode after the first cheaper.
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(renderTokens(turns))),
		},
		// Adaptive thinking: the model decides how much reasoning a message
		// needs. A one-word "balance" should not cost what an ambiguous
		// multi-turn send does.
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: c.effort,
			Format: anthropic.JSONOutputFormatParam{Schema: responseSchema()},
		},
	}

	msg, err := c.messages.New(ctx, params)
	if err != nil {
		return intent.Decoded{}, fmt.Errorf("decoder: %w", err)
	}

	// A refusal is a successful HTTP response with no usable content. Reading
	// content blindly here would panic on an empty slice.
	if msg.StopReason == anthropic.StopReasonRefusal {
		return intent.Decoded{}, fmt.Errorf("%w: the model declined this message", ErrUndecodable)
	}
	if msg.StopReason == anthropic.StopReasonMaxTokens {
		return intent.Decoded{}, errors.New("decoder: response was truncated; raise max tokens")
	}

	raw, err := firstText(msg)
	if err != nil {
		return intent.Decoded{}, err
	}

	return parseDecoded(raw)
}

// firstText pulls the JSON body out of the response, skipping thinking blocks.
func firstText(msg *anthropic.Message) (string, error) {
	for _, block := range msg.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			return text.Text, nil
		}
	}
	return "", errors.New("decoder: response carried no text block")
}
