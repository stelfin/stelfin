package decoder

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/stelfin/stelfin/api/intent"
)

// fakeMessages stands in for the Anthropic API. It records the request so the
// prompt can be asserted on, and returns a canned response.
type fakeMessages struct {
	reply *anthropic.Message
	err   error

	got anthropic.MessageNewParams
}

func (f *fakeMessages) New(
	_ context.Context, params anthropic.MessageNewParams, _ ...option.RequestOption,
) (*anthropic.Message, error) {
	f.got = params
	return f.reply, f.err
}

func jsonReply(t *testing.T, body any) *anthropic.Message {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode reply: %v", err)
	}
	// Build the message through its JSON form so the SDK's content union is
	// populated the way a real response would be.
	var msg anthropic.Message
	wire := map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant",
		"model": "claude-opus-5", "stop_reason": "end_turn",
		"content": []any{map[string]any{"type": "text", "text": string(encoded)}},
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return &msg
}

func testDecoder(f *fakeMessages) *Claude {
	return &Claude{messages: f, model: DefaultModel, maxTokens: DefaultMaxTokens}
}

func TestDecodeSend(t *testing.T) {
	f := &fakeMessages{reply: jsonReply(t, map[string]any{
		"action":           map[string]any{"text": "send", "turn": 0, "start": 0, "end": 1},
		"amount":           map[string]any{"text": "5,000", "turn": 0, "start": 1, "end": 2},
		"destination":      map[string]any{"text": "brother", "turn": 0, "start": 3, "end": 4},
		"destination_kind": "beneficiary",
	})}

	got, err := testDecoder(f).Decode(context.Background(), []string{"send 5,000 to brother"})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Amount.Text != "5,000" || got.Amount.Span != (intent.Span{Turn: 0, Start: 1, End: 2}) {
		t.Errorf("amount = %+v, want 5,000 at [1,2)", got.Amount)
	}
	if got.DestinationKind != intent.DestinationBeneficiary {
		t.Errorf("destination kind = %q, want beneficiary", got.DestinationKind)
	}
}

// TestDecodeOutputIsStillUntrusted: the decoder does not verify. A model that
// reports an amount its span does not support gets through here and is caught
// by intent.Verify — this test pins that division of responsibility, so the
// check is never quietly moved into the prompt.
func TestDecodeOutputIsStillUntrusted(t *testing.T) {
	f := &fakeMessages{reply: jsonReply(t, map[string]any{
		"action":           map[string]any{"text": "send", "turn": 0, "start": 0, "end": 1},
		"amount":           map[string]any{"text": "50,000", "turn": 0, "start": 1, "end": 2},
		"destination":      map[string]any{"text": "brother", "turn": 0, "start": 3, "end": 4},
		"destination_kind": "beneficiary",
	})}

	message := "send 5,000 to brother"
	got, err := testDecoder(f).Decode(context.Background(), []string{message})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	conversation := [][]intent.Token{intent.Tokenize(message)}
	if _, err := intent.Verify(conversation, got); !errors.Is(err, intent.ErrSpanMismatch) {
		t.Fatalf("Verify error = %v, want ErrSpanMismatch", err)
	}
}

// TestPromptCarriesBackendTokens is the load-bearing test of this package. If
// the model is ever sent the raw message instead of the backend's numbered
// tokens, it would count positions from its own tokenization and every span
// check downstream would be meaningless.
func TestPromptCarriesBackendTokens(t *testing.T) {
	f := &fakeMessages{reply: jsonReply(t, map[string]any{
		"action": map[string]any{"text": "balance", "turn": 0, "start": 0, "end": 1},
	})}

	message := "send 5,000 to brother"
	if _, err := testDecoder(f).Decode(context.Background(), []string{message}); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	prompt := promptText(t, f.got)
	for i, tok := range intent.Tokenize(message) {
		want := "[" + itoa(i) + "] " + tok.Text
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing numbered token %q", want)
		}
	}
	// A separator-bearing number must reach the model whole, matching how the
	// verifier will see it.
	if !strings.Contains(prompt, "5,000") {
		t.Error("prompt does not carry the amount token intact")
	}
}

func TestRequestShape(t *testing.T) {
	f := &fakeMessages{reply: jsonReply(t, map[string]any{
		"action": map[string]any{"text": "balance", "turn": 0, "start": 3, "end": 4},
	})}
	if _, err := testDecoder(f).Decode(context.Background(), []string{"what is my balance"}); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if f.got.Model != DefaultModel {
		t.Errorf("model = %q, want %q", f.got.Model, DefaultModel)
	}
	// Structured output is what keeps the response parseable without
	// coaxing the model with format instructions.
	if f.got.OutputConfig.Format.Schema == nil {
		t.Error("request carries no output schema")
	}
	if f.got.Thinking.OfAdaptive == nil {
		t.Error("adaptive thinking is not set")
	}
	if len(f.got.System) == 0 || f.got.System[0].CacheControl.Type == "" {
		t.Error("system prompt is not cached; every decode would pay for it again")
	}
}

func TestDecodeRejectsEmptyAction(t *testing.T) {
	f := &fakeMessages{reply: jsonReply(t, map[string]any{
		"action": map[string]any{"text": "", "turn": 0, "start": 0, "end": 1},
	})}
	_, err := testDecoder(f).Decode(context.Background(), []string{"hello there"})
	if !errors.Is(err, ErrUndecodable) {
		t.Fatalf("error = %v, want ErrUndecodable", err)
	}
}

func TestDecodeRejectsMissingAction(t *testing.T) {
	f := &fakeMessages{reply: jsonReply(t, map[string]any{"destination_kind": "beneficiary"})}
	_, err := testDecoder(f).Decode(context.Background(), []string{"hello there"})
	if !errors.Is(err, ErrUndecodable) {
		t.Fatalf("error = %v, want ErrUndecodable", err)
	}
}

// TestDecodeHandlesRefusal: a refusal is a successful HTTP response with no
// usable content. Reading content blindly would panic on the empty slice.
func TestDecodeHandlesRefusal(t *testing.T) {
	var msg anthropic.Message
	raw := `{"id":"msg_x","type":"message","role":"assistant","model":"claude-opus-5",` +
		`"stop_reason":"refusal","content":[]}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}

	_, err := testDecoder(&fakeMessages{reply: &msg}).Decode(context.Background(), []string{"hi"})
	if !errors.Is(err, ErrUndecodable) {
		t.Fatalf("error = %v, want ErrUndecodable", err)
	}
}

func TestDecodeReportsTruncation(t *testing.T) {
	var msg anthropic.Message
	raw := `{"id":"msg_x","type":"message","role":"assistant","model":"claude-opus-5",` +
		`"stop_reason":"max_tokens","content":[{"type":"text","text":"{\"acti"}]}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}

	_, err := testDecoder(&fakeMessages{reply: &msg}).Decode(context.Background(), []string{"hi"})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want a truncation error", err)
	}
}

func TestDecodeReportsUnparseableResponse(t *testing.T) {
	var msg anthropic.Message
	raw := `{"id":"msg_x","type":"message","role":"assistant","model":"claude-opus-5",` +
		`"stop_reason":"end_turn","content":[{"type":"text","text":"not json"}]}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}

	if _, err := testDecoder(&fakeMessages{reply: &msg}).Decode(context.Background(), []string{"hi"}); err == nil {
		t.Fatal("expected an error for an unparseable response")
	}
}

func TestDecodeRejectsEmptyConversation(t *testing.T) {
	if _, err := testDecoder(&fakeMessages{}).Decode(context.Background(), nil); err == nil {
		t.Fatal("expected an error for an empty conversation")
	}
}

func TestSchemaIsClosed(t *testing.T) {
	schema := responseSchema()
	if schema["additionalProperties"] != false {
		t.Error("top-level schema allows additional properties")
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties")
	}
	for _, name := range []string{"action", "amount", "destination"} {
		field, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("schema is missing field %q", name)
		}
		if field["additionalProperties"] != false {
			t.Errorf("field %q allows additional properties", name)
		}
	}
}

func promptText(t *testing.T, params anthropic.MessageNewParams) string {
	t.Helper()
	if len(params.Messages) == 0 {
		t.Fatal("request carried no messages")
	}
	var b strings.Builder
	for _, block := range params.Messages[0].Content {
		if block.OfText != nil {
			b.WriteString(block.OfText.Text)
		}
	}
	return b.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
