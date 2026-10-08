//go:build live

// Run with:
//
//	STELFIN_DECODER_API_KEY=… STELFIN_DECODER_MODEL=… \
//	STELFIN_DECODER_BASE_URL=https://api.groq.com/openai/v1 \
//	go test -tags=live ./api/decoder/ -run Live -v
//
// Every other test of the decoder stands in for the model with something that
// can only point at tokens, which proves everything that checks a model and
// nothing about any model's judgement. This asks a real one. It is tagged out
// of the default build because it needs a key, costs a few cents, and its
// result is a fact about a provider on a day, not about this code.
package decoder

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stelfin/stelfin/api/intent"
)

func TestLiveModelReadsOrdinarySentences(t *testing.T) {
	key, model := os.Getenv("STELFIN_DECODER_API_KEY"), os.Getenv("STELFIN_DECODER_MODEL")
	if key == "" || model == "" {
		t.Skip("set STELFIN_DECODER_API_KEY and STELFIN_DECODER_MODEL to ask a real model")
	}
	d, err := NewOpenAI(OpenAIConfig{
		BaseURL: os.Getenv("STELFIN_DECODER_BASE_URL"), APIKey: key, Model: model,
	})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	address := "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"
	sentences := []struct {
		text, amount, to string
		kind             intent.DestinationKind
	}{
		{"send 5 to mum", "5", "mum", intent.DestinationBeneficiary},
		{"please pay 250 to the auditor", "250", "the auditor", intent.DestinationBeneficiary},
		{"can you send 5,000 to payroll for me", "5,000", "payroll", intent.DestinationBeneficiary},
		{"transfer 12.50 to " + address, "12.50", address, intent.DestinationAddress},
		{"hey, send mum 20", "20", "mum", intent.DestinationBeneficiary},
		{"I'd like to pay the landlord 1200 today", "1200", "the landlord", intent.DestinationBeneficiary},
		{"send 3 to " + address + " thanks", "3", address, intent.DestinationAddress},
	}
	read := 0
	for _, s := range sentences {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		decoded, err := d.Decode(ctx, []string{s.text})
		cancel()
		if err != nil {
			t.Errorf("%q: %v", s.text, err)
			continue
		}
		// The only reading that counts: the one that survives verification.
		g, err := intent.Verify([][]intent.Token{intent.Tokenize(s.text)}, decoded)
		if err != nil {
			t.Errorf("%q: the model's answer did not verify: %v (%+v)", s.text, err, decoded)
			continue
		}
		if g.AmountText != s.amount || g.DestinationKind != s.kind {
			t.Errorf("%q: read as %s to %q (%s), want %s to %q (%s)",
				s.text, g.AmountText, g.DestinationText, g.DestinationKind, s.amount, s.to, s.kind)
			continue
		}
		read++
		t.Logf("%q -> %s to %q", s.text, g.AmountText, g.DestinationText)
	}
	t.Logf("%s read %d of %d", model, read, len(sentences))

	// And things that are not instructions are said not to be.
	for _, text := range []string{"hello there", "what's our balance?", "lunch at one?"} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		decoded, err := d.Decode(ctx, []string{text})
		cancel()
		if err == nil {
			if _, verr := intent.Verify([][]intent.Token{intent.Tokenize(text)}, decoded); verr == nil {
				t.Errorf("%q was read as a payment: %+v", text, decoded)
			}
		}
	}
}
