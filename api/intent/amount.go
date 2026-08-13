package intent

import (
	"errors"
	"fmt"
	"strings"

	"github.com/stelfin/stelfin/internal/money"
)

// Amount normalization is deterministic Go, never model output.
//
// The model's only job is to point at the tokens the user's amount lives in.
// Turning "5,000" or "five thousand" into a stroop count is arithmetic, and
// arithmetic performed by a language model is arithmetic nobody checked. Every
// rule below is total and testable: given the same text it always yields the
// same amount, or refuses.
//
// Refusal is the default. An amount this package cannot read with certainty is
// an amount the user gets asked about, never one that gets guessed.

var (
	// ErrAmountUnreadable reports text that is not an amount this package can
	// interpret with certainty.
	ErrAmountUnreadable = errors.New("intent: amount is not readable")

	// ErrAmountAmbiguous reports text with more than one defensible reading.
	ErrAmountAmbiguous = errors.New("intent: amount is ambiguous")
)

// multipliers recognised as a suffix on a numeric amount.
var multipliers = map[string]int64{
	"k":  1_000,
	"m":  1_000_000,
	"bn": 1_000_000_000,
	"b":  1_000_000_000,
}

// numberWords covers spelled-out amounts. Deliberately small: every entry is a
// word whose value is beyond argument. "a couple", "a few" and "some" are
// absent because they have no single correct value.
var numberWords = map[string]int64{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4,
	"five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9,
	"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14,
	"fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

var wordScales = map[string]int64{
	"hundred":  100,
	"thousand": 1_000,
	"million":  1_000_000,
}

// NormalizeAmount converts the text of an amount span into stroops.
//
// It accepts digit forms with thousands separators and decimals ("5,000",
// "5000.50"), a k/m/bn suffix ("5k", "2.5m"), and spelled-out English up to
// millions ("five thousand", "twenty five"). Anything else is refused.
func NormalizeAmount(text string) (money.Stroops, error) {
	cleaned := strings.TrimSpace(strings.ToLower(text))
	if cleaned == "" {
		return 0, fmt.Errorf("%w: empty", ErrAmountUnreadable)
	}

	// Currency symbols and codes are asset selection, decided elsewhere from
	// the user's account rather than inferred from a glyph. Strip them so the
	// numeric reading is unaffected, but never let them pick the asset here.
	cleaned = strings.TrimLeft(cleaned, "₦$€£ ")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return 0, fmt.Errorf("%w: only a currency symbol", ErrAmountUnreadable)
	}

	if v, err := normalizeDigits(cleaned); err == nil {
		return v, nil
	} else if !errors.Is(err, ErrAmountUnreadable) {
		return 0, err
	}
	return normalizeWords(cleaned)
}

// normalizeDigits handles "5000", "5,000", "5000.50", "5k", "2.5m".
func normalizeDigits(text string) (money.Stroops, error) {
	// Spaces are not removed. A digit amount never contains one — the tokenizer
	// keeps "5,000" whole — so a space means two separate numbers, and
	// collapsing them would silently read "5 5" as fifty-five.
	body := text

	multiplier := int64(1)
	for suffix, m := range multipliers {
		if len(body) > len(suffix) && strings.HasSuffix(body, suffix) {
			// "bn" must win over "b"; check that the longer match is not also
			// present before accepting the shorter one.
			if suffix == "b" && strings.HasSuffix(body, "bn") {
				continue
			}
			multiplier = m
			body = strings.TrimSuffix(body, suffix)
			break
		}
	}

	// Thousands separators are removed only where they sit between digits, so
	// "5,000" reads as five thousand while a stray comma is still rejected.
	body = stripGroupingCommas(body)
	if body == "" || !isNumeric(body) {
		return 0, fmt.Errorf("%w: %q", ErrAmountUnreadable, text)
	}

	base, err := money.Parse(body)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %v", ErrAmountUnreadable, text, err)
	}
	if multiplier == 1 {
		return base, nil
	}

	// Multiply in stroop space with an explicit overflow check. money.Stroops
	// is an integer count, so this cannot lose precision, only exceed range.
	scaled := base * money.Stroops(multiplier)
	if base != 0 && scaled/money.Stroops(multiplier) != base {
		return 0, fmt.Errorf("%w: %q overflows", ErrAmountUnreadable, text)
	}
	return scaled, nil
}

func stripGroupingCommas(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ',' && i > 0 && i+1 < len(s) && isDigit(s[i-1]) && isDigit(s[i+1]) {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// normalizeWords handles "five thousand", "twenty five", "two hundred".
func normalizeWords(text string) (money.Stroops, error) {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '-'
	})
	if len(fields) == 0 {
		return 0, fmt.Errorf("%w: %q", ErrAmountUnreadable, text)
	}

	var total, current int64
	sawAny := false
	for _, word := range fields {
		if word == "and" {
			continue
		}
		switch {
		case numberWords[word] != 0 || word == "zero":
			current += numberWords[word]
			sawAny = true
		case wordScales[word] != 0:
			scale := wordScales[word]
			if current == 0 {
				// "thousand" with nothing in front of it means one thousand.
				current = 1
			}
			if scale >= 1_000 {
				total += current * scale
				current = 0
			} else {
				current *= scale
			}
			sawAny = true
		default:
			return 0, fmt.Errorf("%w: %q contains %q", ErrAmountUnreadable, text, word)
		}
	}
	if !sawAny {
		return 0, fmt.Errorf("%w: %q", ErrAmountUnreadable, text)
	}

	whole := total + current
	amount := money.Stroops(whole) * money.One
	if whole != 0 && amount/money.One != money.Stroops(whole) {
		return 0, fmt.Errorf("%w: %q overflows", ErrAmountUnreadable, text)
	}
	return amount, nil
}

func isNumeric(s string) bool {
	dots := 0
	for i := 0; i < len(s); i++ {
		switch {
		case isDigit(s[i]):
		case s[i] == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
