// Package intent turns a user's chat message into a payment instruction.
//
// The language model in this pipeline is a decoder, never an authority. Every
// field it emits must carry a provenance span into *this* package's
// tokenization of the raw message, and the span is re-checked here before the
// field is believed. A model that hallucinates an amount, invents a recipient,
// or is steered by instructions injected from outside the current message
// cannot produce spans that survive that check.
//
// What span verification does not catch, by construction, is an instruction
// injected into the message the user actually sent: those tokens really are
// present, so the span is genuine. That case belongs to the confirmation
// screen, which renders from the signed payload — see Grounded.
package intent

import (
	"strings"
	"unicode"
)

// Token is one unit of a tokenized message.
//
// Byte offsets are retained so a confirmation UI can highlight exactly the text
// a field was derived from. Showing the user the span their money came from is
// the last line of defence against a decode they did not mean.
type Token struct {
	// Index is the token's position within its turn, counting from zero.
	Index int
	// Text is the token itself.
	Text string
	// Start and End are byte offsets into the original message.
	Start, End int
}

// Tokenize splits a message deterministically.
//
// This function must be the only tokenizer in the system, and the model must
// never be asked to supply token indices computed against its own idea of
// tokens. The whole scheme rests on the backend and the model referring to the
// same positions, which only holds if the backend defines them.
//
// A token is either a run of letters, digits and marks — including separators
// that sit *between* alphanumerics, so "5,000", "5.5k" and "o'clock" stay
// whole — or a single punctuation character standing alone. Whitespace is a
// boundary and is never itself a token.
func Tokenize(message string) []Token {
	runes := []rune(message)
	// byteAt[i] is the byte offset of rune i, with a final entry for the end.
	byteAt := make([]int, len(runes)+1)
	off := 0
	for i, r := range runes {
		byteAt[i] = off
		off += len(string(r))
	}
	byteAt[len(runes)] = off

	var tokens []Token
	i := 0
	for i < len(runes) {
		if unicode.IsSpace(runes[i]) {
			i++
			continue
		}

		start := i
		if isWordRune(runes[i]) {
			i++
			for i < len(runes) {
				switch {
				case isWordRune(runes[i]):
					i++
				case isInternalSeparator(runes[i]) &&
					i+1 < len(runes) && isWordRune(runes[i+1]):
					// A separator only stays inside a token when a word
					// character follows it: "5,000" is one token, "5," is not.
					i += 2
				default:
					goto emit
				}
			}
		} else {
			// Standalone punctuation. Kept as its own token rather than
			// discarded, so spans remain stable if a model refers to one.
			i++
		}

	emit:
		tokens = append(tokens, Token{
			Index: len(tokens),
			Text:  string(runes[start:i]),
			Start: byteAt[start],
			End:   byteAt[i],
		})
	}
	return tokens
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

func isInternalSeparator(r rune) bool {
	switch r {
	case ',', '.', '\'', '-', '_', '/', ':':
		return true
	}
	return false
}

// Text joins the tokens in [start, end) with single spaces, which is the
// canonical form a span is compared against.
func Text(tokens []Token, start, end int) string {
	if start < 0 || end > len(tokens) || start >= end {
		return ""
	}
	parts := make([]string, 0, end-start)
	for _, tok := range tokens[start:end] {
		parts = append(parts, tok.Text)
	}
	return strings.Join(parts, " ")
}
