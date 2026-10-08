package decoder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stelfin/stelfin/api/intent"
)

// good is a decode of "send 5 to mum", as a model should write it.
const good = `{"action":{"text":"send","turn":0,"start":0,"end":1},` +
	`"amount":{"text":"5","turn":0,"start":1,"end":2},` +
	`"destination":{"text":"mum","turn":0,"start":3,"end":4},"destination_kind":"beneficiary"}`

func completion(content, finish string) string {
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"finish_reason": finish,
			"message":       map[string]string{"role": "assistant", "content": content},
		}},
	})
	return string(body)
}

// provider is a compatible API that answers each request with the next reply.
type provider struct {
	*httptest.Server
	calls    atomic.Int32
	requests []chatRequest
	auth     string
}

type reply struct {
	status int
	body   string
	header map[string]string
}

func newProvider(t *testing.T, replies ...reply) *provider {
	t.Helper()
	p := &provider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(p.calls.Add(1)) - 1
		raw, _ := io.ReadAll(r.Body)
		var req chatRequest
		_ = json.Unmarshal(raw, &req)
		p.requests = append(p.requests, req)
		p.auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("request to %s, want /v1/chat/completions", r.URL.Path)
		}

		if n >= len(replies) {
			n = len(replies) - 1
		}
		for k, v := range replies[n].header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(replies[n].status)
		_, _ = io.WriteString(w, replies[n].body)
	}))
	t.Cleanup(p.Close)
	return p
}

func decoderFor(t *testing.T, p *provider, waits *[]time.Duration) *OpenAI {
	t.Helper()
	d, err := NewOpenAI(OpenAIConfig{
		BaseURL: p.URL + "/v1/", APIKey: "key-123", Model: "some-model",
		sleep: func(_ context.Context, d time.Duration) error {
			if waits != nil {
				*waits = append(*waits, d)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	return d
}

func TestOpenAIDecodesAnAnswer(t *testing.T) {
	p := newProvider(t, reply{status: 200, body: completion(good, "stop")})
	got, err := decoderFor(t, p, nil).Decode(t.Context(), []string{"send 5 to mum"})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Action.Text != "send" || got.Amount.Text != "5" || got.Destination.Text != "mum" ||
		got.DestinationKind != intent.DestinationBeneficiary || got.Destination.Span != (intent.Span{Turn: 0, Start: 3, End: 4}) {
		t.Fatalf("decoded = %+v", got)
	}
	// And it survives the check everything a model says has to survive.
	if _, err := intent.Verify([][]intent.Token{intent.Tokenize("send 5 to mum")}, got); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	req := p.requests[0]
	if req.Model != "some-model" || req.Temperature != 0 || req.MaxTokens == 0 {
		t.Errorf("request = %+v", req)
	}
	if p.auth != "Bearer key-123" {
		t.Errorf("Authorization = %q", p.auth)
	}
	// The model is shown the backend's own numbering, and told the shape of
	// its answer in words every compatible model understands.
	if len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, "[1] 5") {
		t.Errorf("the model was not shown the tokens: %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[0].Content, "destination_kind") || !strings.Contains(req.Messages[0].Content, "one JSON object") {
		t.Errorf("the model was not told the shape of its answer:\n%s", req.Messages[0].Content)
	}
}

// TestOpenAIReadsAnswersModelsActuallyGive: asked for one object and nothing
// else, models still wrap it. Rejecting those safely is still rejecting every
// answer from a whole family of models.
func TestOpenAIReadsAnswersModelsActuallyGive(t *testing.T) {
	for name, content := range map[string]string{
		"plain":               good,
		"fenced":              "```json\n" + good + "\n```",
		"thinking first":      "<think>The user wants to send 5 to {mum}. Tokens: [0] send…</think>\n" + good,
		"thinking and fence":  "<think>ok</think>\n```json\n" + good + "\n```\n",
		"prose around it":     "Here is the answer:\n" + good + "\nLet me know if you need anything else.",
		"a brace in a string": strings.Replace(good, `"text":"mum"`, `"text":"mum"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t, reply{status: 200, body: completion(content, "stop")})
			got, err := decoderFor(t, p, nil).Decode(t.Context(), []string{"send 5 to mum"})
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Destination.Text != "mum" {
				t.Errorf("decoded = %+v", got)
			}
		})
	}
}

func TestExtractJSON(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                             `{"a":1}`,
		"```json\n{\"a\":1}\n```":             `{"a":1}`,
		`<think>{not this}</think>{"a":1}`:    `{"a":1}`,
		`{"a":"}{","b":{"c":2}} trailing {x}`: `{"a":"}{","b":{"c":2}}`,
		`{"a":"quote \" and brace }"}`:        `{"a":"quote \" and brace }"}`,
		`<think>never finished {"a":1}`:       ``,
		`no object here`:                      `no object here`,
		`{"a":1`:                              `{"a":1`,
	} {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOpenAIRejectsWhatItWasNotAskedFor: nothing is repaired. An answer with a
// field the schema does not have comes from a model that has stopped following
// the contract, and the fields it was asked for are no more trustworthy.
func TestOpenAIRejectsWhatItWasNotAskedFor(t *testing.T) {
	for name, content := range map[string]string{
		"an extra field": strings.Replace(good, `"destination_kind"`, `"memo":"rent","destination_kind"`, 1),
		"not JSON":       "I think they want to send 5 to mum.",
		"half an object": good[:len(good)-20],
	} {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t, reply{status: 200, body: completion(content, "stop")})
			if _, err := decoderFor(t, p, nil).Decode(t.Context(), []string{"send 5 to mum"}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestOpenAISaysWhenThereIsNoInstruction(t *testing.T) {
	for name, r := range map[string]reply{
		"an empty action": {status: 200, body: completion(`{"action":{"text":"","turn":0,"start":0,"end":0}}`, "stop")},
		"declined":        {status: 200, body: completion("", "content_filter")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decoderFor(t, newProvider(t, r), nil).Decode(t.Context(), []string{"hello there"})
			if !errors.Is(err, intent.ErrUndecodable) {
				t.Fatalf("error = %v, want ErrUndecodable", err)
			}
		})
	}
}

func TestOpenAIRefusesATruncatedAnswer(t *testing.T) {
	p := newProvider(t, reply{status: 200, body: completion(good[:40], "length")})
	_, err := decoderFor(t, p, nil).Decode(t.Context(), []string{"send 5 to mum"})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want it to say the answer was cut off", err)
	}
}

// TestOpenAIRetriesWhatWaitingCanFix: a rate limit and a provider having a bad
// moment. And it waits as long as it is asked to, within reason.
func TestOpenAIRetriesWhatWaitingCanFix(t *testing.T) {
	var waits []time.Duration
	p := newProvider(t,
		reply{status: 429, body: `{"error":{"message":"rate limit"}}`, header: map[string]string{"Retry-After": "2"}},
		reply{status: 503, body: `upstream unavailable`},
		reply{status: 200, body: completion(good, "stop")},
	)
	if _, err := decoderFor(t, p, &waits).Decode(t.Context(), []string{"send 5 to mum"}); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := p.calls.Load(); got != 3 {
		t.Errorf("made %d requests, want 3", got)
	}
	if len(waits) != 2 || waits[0] != 2*time.Second {
		t.Errorf("waited %v, want the 2s the provider asked for and then a backoff", waits)
	}
}

func TestOpenAIGivesUpAndDoesNotWaitForever(t *testing.T) {
	var waits []time.Duration
	p := newProvider(t, reply{status: 500, body: `boom`, header: map[string]string{"Retry-After": "3600"}})
	_, err := decoderFor(t, p, &waits).Decode(t.Context(), []string{"send 5 to mum"})
	if err == nil {
		t.Fatal("accepted a provider that never answered")
	}
	if errors.Is(err, intent.ErrUndecodable) {
		t.Errorf("a provider failure was reported as the message not being an instruction: %v", err)
	}
	if got := p.calls.Load(); got != openAIRetries+1 {
		t.Errorf("made %d requests, want %d", got, openAIRetries+1)
	}
	for _, w := range waits {
		if w > openAIMaxBackoff {
			t.Errorf("waited %s because the provider said to; a person is waiting on this", w)
		}
	}
}

// TestOpenAIDoesNotRetryWhatIsWrong: a bad key or an unknown model. Asking
// again asks the same wrong thing.
func TestOpenAIDoesNotRetryWhatIsWrong(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		p := newProvider(t, reply{status: status, body: `{"error":{"message":"Invalid API Key"}}`})
		_, err := decoderFor(t, p, nil).Decode(t.Context(), []string{"send 5 to mum"})
		if err == nil || !strings.Contains(err.Error(), "Invalid API Key") {
			t.Errorf("%d: error = %v, want the provider's reason for the log", status, err)
		}
		if got := p.calls.Load(); got != 1 {
			t.Errorf("%d: made %d requests, want 1", status, got)
		}
	}
}

// TestOpenAIDropsAResponseFormatTheProviderDoesNotHave: some compatible servers
// do not implement it. The shape of the answer rests on the prompt.
func TestOpenAIDropsAResponseFormatTheProviderDoesNotHave(t *testing.T) {
	p := newProvider(t,
		reply{status: 400, body: `{"error":{"message":"'response_format' is not supported with this model"}}`},
		reply{status: 200, body: completion(good, "stop")},
	)
	if _, err := decoderFor(t, p, nil).Decode(t.Context(), []string{"send 5 to mum"}); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(p.requests) != 2 || p.requests[0].ResponseFormat == nil || p.requests[1].ResponseFormat != nil {
		t.Fatalf("requests = %+v, want the second without a response format", p.requests)
	}
}

func TestOpenAIDoesNotLeakWhereItCalled(t *testing.T) {
	d, err := NewOpenAI(OpenAIConfig{
		BaseURL: "http://127.0.0.1:1/secret-path?token=abc", APIKey: "key-123", Model: "m",
		sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	_, err = d.Decode(t.Context(), []string{"send 5 to mum"})
	if err == nil {
		t.Fatal("reached a provider that is not there")
	}
	for _, leak := range []string{"secret-path", "token=abc", "key-123", "127.0.0.1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error carries %q: %v", leak, err)
		}
	}
}

func TestNewOpenAINeedsAKeyAndAModel(t *testing.T) {
	if _, err := NewOpenAI(OpenAIConfig{Model: "m"}); err == nil {
		t.Error("accepted no key")
	}
	// No default model: there is none that is right for more than one provider.
	if _, err := NewOpenAI(OpenAIConfig{APIKey: "k"}); err == nil {
		t.Error("accepted no model")
	}
	d, err := NewOpenAI(OpenAIConfig{APIKey: "k", Model: "m"})
	if err != nil || d.url != DefaultOpenAIBaseURL+"/chat/completions" {
		t.Errorf("default URL = %q, %v", d.url, err)
	}
}
