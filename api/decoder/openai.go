package decoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stelfin/stelfin/api/intent"
)

// A decoder for any provider that speaks the OpenAI chat-completions wire
// format, which by now is most of them: OpenAI itself, Groq, OpenRouter,
// Together, DeepSeek, Mistral, and anything self-hosted behind a compatible
// server.
//
// It asks exactly what the Claude decoder asks. The model is shown the
// backend's own tokenization and answers with positions in it; nothing it says
// is believed until intent.Verify has checked those positions against the
// tokens. A weaker model therefore produces more rejected decodes, not wrong
// payments, which is what makes it safe to let the operator choose one.
//
// Plain net/http and no SDK. The whole of what is needed is one POST and one
// JSON document back, and an SDK per provider would be a dependency per
// provider for the sake of that.

// Defaults for the compatible decoder.
const (
	// DefaultOpenAIBaseURL is used when no base URL is configured.
	DefaultOpenAIBaseURL = "https://api.openai.com/v1"

	// openAIMaxTokens bounds the answer. It is one small JSON object.
	openAIMaxTokens = 1024
	// openAIAttemptTimeout bounds one request. A person is waiting on the
	// reply, and a model that has not answered in this long is not going to
	// answer in a way that was worth the wait.
	openAIAttemptTimeout = 20 * time.Second
	// openAIRetries is how many more times a request is made after a failure
	// that waiting can fix: a rate limit, or the provider having a bad moment.
	openAIRetries = 2
	// openAIMaxBackoff caps how long a provider can ask us to wait.
	openAIMaxBackoff = 5 * time.Second
	// openAIMaxBody bounds what is read of a response.
	openAIMaxBody = 1 << 20
)

// OpenAIConfig describes a compatible decoder.
type OpenAIConfig struct {
	// BaseURL is the API root, up to and including the version: for example
	// https://api.groq.com/openai/v1. Empty means DefaultOpenAIBaseURL.
	BaseURL string
	// APIKey is sent as a bearer token. Required.
	APIKey string
	// Model is the model id, as the provider names it. Required: there is no
	// default that is right for more than one provider.
	Model string
	// HTTPClient overrides the client used. Its Timeout is ignored; each
	// attempt is bounded by a context.
	HTTPClient *http.Client

	// sleep waits between attempts. Replaced in tests so they do not.
	sleep func(context.Context, time.Duration) error
}

// OpenAI decodes conversations through an OpenAI-compatible API.
type OpenAI struct {
	url    string
	key    string
	model  string
	client *http.Client
	sleep  func(context.Context, time.Duration) error
}

// NewOpenAI returns a compatible decoder.
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	switch {
	case strings.TrimSpace(cfg.APIKey) == "":
		return nil, errors.New("decoder: the compatible decoder needs an API key")
	case strings.TrimSpace(cfg.Model) == "":
		return nil, errors.New("decoder: the compatible decoder needs a model id")
	}
	base := strings.TrimSuffix(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultOpenAIBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	sleep := cfg.sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	return &OpenAI{
		url: base + "/chat/completions", key: cfg.APIKey, model: cfg.Model,
		client: client, sleep: sleep,
	}, nil
}

// jsonInstruction tells the model the shape of its answer.
//
// In the prompt rather than only in a response_format, because a schema in the
// request is something a few models on a few providers honour and the rest
// reject outright, while "answer with one JSON object" is understood by all of
// them. The schema's real enforcement is on this side either way: the answer
// is parsed strictly here and then verified span by span.
func jsonInstruction() string {
	schema, _ := json.Marshal(responseSchema())
	return "Answer with one JSON object and nothing else: no prose before or after it, " +
		"and no code fence. It must match this JSON Schema, with no other fields:\n" + string(schema)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	Temperature    float64           `json:"temperature"`
	MaxTokens      int               `json:"max_tokens"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string      `json:"finish_reason"`
		Message      chatMessage `json:"message"`
	} `json:"choices"`
}

// Decode asks the model to locate the instruction in the conversation.
//
// The returned Decoded is untrusted. It must go through intent.Verify against
// the same tokenization before anything acts on it.
func (o *OpenAI) Decode(ctx context.Context, turns []string) (intent.Decoded, error) {
	if len(turns) == 0 {
		return intent.Decoded{}, errors.New("decoder: conversation is empty")
	}

	req := chatRequest{
		Model: o.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt + "\n\n" + jsonInstruction()},
			{Role: "user", Content: renderTokens(turns)},
		},
		// Zero: the same sentence should be read the same way twice.
		Temperature:    0,
		MaxTokens:      openAIMaxTokens,
		ResponseFormat: map[string]string{"type": "json_object"},
	}

	content, err := o.complete(ctx, req)
	if err != nil {
		return intent.Decoded{}, err
	}
	return parseDecoded(extractJSON(content))
}

// complete sends the request and returns the model's text.
//
// It retries what waiting can fix and nothing else. And it drops the response
// format once, if the provider objects to it: some compatible servers do not
// implement it at all, and the instruction in the prompt is what the answer's
// shape actually rests on.
func (o *OpenAI) complete(ctx context.Context, req chatRequest) (string, error) {
	backoff := 400 * time.Millisecond
	var last error
	for attempt := 0; attempt <= openAIRetries; attempt++ {
		content, status, retryAfter, err := o.once(ctx, req)
		switch {
		case err == nil:
			return content, nil
		case ctx.Err() != nil:
			return "", fmt.Errorf("decoder: %w", ctx.Err())
		case status == http.StatusBadRequest && req.ResponseFormat != nil &&
			strings.Contains(strings.ToLower(err.Error()), "response_format"):
			req.ResponseFormat = nil
			attempt-- // Not a retry of the same request: a different one.
			last = err
			continue
		case status == http.StatusTooManyRequests || status >= 500 || status == 0:
			last = err
		default:
			// The request itself is wrong — a bad key, an unknown model. Asking
			// again asks the same wrong thing.
			return "", err
		}

		if attempt == openAIRetries {
			break
		}
		wait := backoff
		if retryAfter > 0 {
			wait = retryAfter
		}
		if wait > openAIMaxBackoff {
			wait = openAIMaxBackoff
		}
		if err := o.sleep(ctx, wait); err != nil {
			return "", fmt.Errorf("decoder: %w", err)
		}
		backoff *= 2
	}
	return "", last
}

// once makes one request. status is zero when no response arrived.
func (o *OpenAI) once(ctx context.Context, req chatRequest) (content string, status int, retryAfter time.Duration, err error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("decoder: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, openAIAttemptTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, fmt.Errorf("decoder: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.key)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		// Not the error itself, which can carry the URL and whatever a proxy
		// said. That it could not be reached is all anybody can act on.
		return "", 0, 0, errors.New("decoder: the model provider could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, openAIMaxBody))
	if err != nil {
		return "", resp.StatusCode, 0, errors.New("decoder: the model provider's answer could not be read")
	}

	if resp.StatusCode != http.StatusOK {
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, convErr := strconv.ParseFloat(s, 64); convErr == nil && secs > 0 {
				retryAfter = time.Duration(secs * float64(time.Second))
			}
		}
		return "", resp.StatusCode, retryAfter, fmt.Errorf(
			"decoder: the model provider answered %d: %s", resp.StatusCode, providerMessage(raw))
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return "", resp.StatusCode, 0, errors.New("decoder: the model provider's answer was not a completion")
	}
	choice := parsed.Choices[0]
	if choice.FinishReason == "length" {
		return "", resp.StatusCode, 0, errors.New("decoder: response was truncated; the model ran out of tokens")
	}
	if choice.FinishReason == "content_filter" {
		return "", resp.StatusCode, 0, fmt.Errorf("%w: the model declined this message", ErrUndecodable)
	}
	return choice.Message.Content, resp.StatusCode, 0, nil
}

// providerMessage pulls the provider's own explanation out of an error body,
// bounded, for the log. The body is the provider's text and may be anything.
func providerMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		msg = envelope.Error.Message
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// extractJSON finds the JSON object in a model's answer.
//
// Asked for one object and nothing else, models still wrap it: reasoning models
// put their thinking in front, in <think> tags, and many put the object in a
// code fence however plainly they were told not to. Left as they come, every
// such answer fails to parse — safely, but a decoder that rejects every answer
// from a whole family of models is one that does not work with them.
//
// So the thinking is dropped and the first complete object is taken. Nothing is
// repaired. What is returned is exactly what the model wrote between one brace
// and its match, and it is still parsed strictly and verified after that.
func extractJSON(content string) string {
	s := content
	for {
		open := strings.Index(s, "<think>")
		if open < 0 {
			break
		}
		end := strings.Index(s[open:], "</think>")
		if end < 0 {
			// Thinking that never finished: there is no answer after it.
			s = s[:open]
			break
		}
		s = s[:open] + s[open+end+len("</think>"):]
	}

	start := strings.IndexByte(s, '{')
	if start < 0 {
		return strings.TrimSpace(s)
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return strings.TrimSpace(s[start:])
}
