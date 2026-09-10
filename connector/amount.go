package connector

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Reading an amount out of a spreadsheet.
//
// api/intent.NormalizeAmount is generous on purpose: it reads "5k", "five
// thousand", "₦5,000". That generosity is right for a chat message, because a
// chat message is one human who can be asked a follow-up question, and refusing
// to understand them is a worse outcome than guessing and confirming.
//
// A spreadsheet is not one human. It is two hundred rows nobody will read
// individually, and a guess that is wrong is wrong two hundred times with a
// single approval on top. So this reads far less, and refuses far more:
//
//   - no words. "five thousand" in a cell is a comment, not an amount.
//   - no magnitude suffixes. "5k" in a payroll column is ambiguous between a
//     typo and five thousand, and the cost of picking wrong is a payment.
//   - no currency symbols. The asset is decided from the org's own
//     configuration, never from a glyph somebody typed.
//   - no negatives, no zero. A spreadsheet expressing a refund as -50 means
//     something this package cannot know.
//
// What it accepts is a plain decimal, optionally with thousands separators and
// surrounding whitespace. Everything else is an error naming the row.

// ErrAmountUnreadable reports a cell this package will not interpret.
var ErrAmountUnreadable = errors.New("connector: amount cannot be read from a cell")

// MaxDecimalPlaces is Stellar's precision. A cell with more digits than the
// chain can hold is not a rounding problem — it is a number that means
// something other than what will be paid.
const MaxDecimalPlaces = 7

// NormalizeTabularAmount reads a cell as an exact decimal amount string.
//
// It returns the amount in the canonical form the rest of the system parses,
// rather than a number: the arithmetic belongs to internal/money, and having
// two packages that can turn text into a quantity is how they come to disagree.
func NormalizeTabularAmount(cell Untrusted[string]) (string, error) {
	raw := cell.Unwrap()
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", fmt.Errorf("%w: it is empty", ErrAmountUnreadable)
	}

	// A leading apostrophe is how spreadsheets mark a cell as text. It is not
	// part of the number and is stripped; anything else non-numeric is not.
	text = strings.TrimPrefix(text, "'")

	// Formulas are refused rather than evaluated. A cell reading "=B2*C2" has a
	// value the exporter may or may not have resolved, and paying the formula
	// text is impossible while paying a stale cached value is worse.
	if strings.HasPrefix(text, "=") {
		return "", fmt.Errorf("%w: %q is a formula, and its value depends on a sheet "+
			"this cannot see", ErrAmountUnreadable, clip(raw))
	}

	// Parentheses are accounting's negative. Recognised only to refuse it
	// precisely, because "(50)" silently read as fifty is a payment in the
	// wrong direction.
	if strings.HasPrefix(text, "(") && strings.HasSuffix(text, ")") {
		return "", fmt.Errorf("%w: %q is a negative in accounting notation",
			ErrAmountUnreadable, clip(raw))
	}
	if strings.HasPrefix(text, "-") {
		return "", fmt.Errorf("%w: %q is negative, and a payment is not",
			ErrAmountUnreadable, clip(raw))
	}
	if strings.HasPrefix(text, "+") {
		// Harmless in itself, and refused because accepting it means accepting
		// that the cell contains something other than a number.
		return "", fmt.Errorf("%w: %q has a sign", ErrAmountUnreadable, clip(raw))
	}

	whole, fraction, hasPoint := strings.Cut(text, ".")

	// Thousands separators are validated, not merely removed. "5,00,0" is a
	// typo, and reading it as five thousand is precisely the guess this parser
	// exists not to make — the person who typed it meant something, and it is
	// not for this to decide what.
	//
	// Only the whole part may carry them: a comma after the decimal point is
	// a different convention entirely, and the two cannot be told apart from
	// one cell.
	if strings.Contains(fraction, ",") {
		return "", fmt.Errorf("%w: %q has a separator after the decimal point",
			ErrAmountUnreadable, clip(raw))
	}
	ungrouped, ok := stripGrouping(whole)
	if !ok {
		return "", fmt.Errorf(
			"%w: %q does not group its digits in threes, so it is a typo rather "+
				"than a number", ErrAmountUnreadable, clip(raw))
	}
	whole = ungrouped
	if whole == "" && !hasPoint {
		return "", fmt.Errorf("%w: %q", ErrAmountUnreadable, clip(raw))
	}
	if strings.Contains(fraction, ".") {
		return "", fmt.Errorf("%w: %q has more than one decimal point",
			ErrAmountUnreadable, clip(raw))
	}
	if !allDigits(whole) || (hasPoint && !allDigits(fraction)) {
		return "", fmt.Errorf("%w: %q is not a plain number. A spreadsheet cell is "+
			"not a sentence, so nothing here is guessed at", ErrAmountUnreadable, clip(raw))
	}
	if whole == "" {
		// ".5" is a number a person means, and writing it back as "0.5" is the
		// only interpretation available.
		whole = "0"
	}
	if hasPoint && fraction == "" {
		return "", fmt.Errorf("%w: %q ends in a decimal point", ErrAmountUnreadable, clip(raw))
	}
	if len(fraction) > MaxDecimalPlaces {
		return "", fmt.Errorf(
			"%w: %q has %d decimal places and this chain holds %d. Rounding it "+
				"would pay a different amount than the sheet says",
			ErrAmountUnreadable, clip(raw), len(fraction), MaxDecimalPlaces)
	}

	// Leading zeros are dropped so "0005" and "5" produce one canonical string,
	// which matters because the result is compared and hashed downstream.
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}

	out := whole
	if fraction != "" {
		out += "." + fraction
	}
	if isZero(whole, fraction) {
		return "", fmt.Errorf("%w: %q is zero, and a payment of nothing is a mistake "+
			"rather than an instruction", ErrAmountUnreadable, clip(raw))
	}
	return out, nil
}

// stripGrouping removes thousands separators, refusing anything mis-grouped.
//
// A grouped number is one to three digits, then any number of three-digit
// groups. Nothing else is a number somebody meant.
func stripGrouping(whole string) (string, bool) {
	if !strings.Contains(whole, ",") {
		return whole, true
	}
	groups := strings.Split(whole, ",")
	for i, g := range groups {
		switch {
		case !allDigits(g) || g == "":
			return "", false
		case i == 0 && len(g) > 3:
			return "", false
		case i > 0 && len(g) != 3:
			return "", false
		}
	}
	return strings.Join(groups, ""), true
}

func allDigits(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if !isDigit(r) {
			return false
		}
	}
	return true
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isZero(whole, fraction string) bool {
	return strings.Trim(whole, "0") == "" && strings.Trim(fraction, "0") == ""
}

// clip bounds what an error message quotes back.
//
// A cell is arbitrary text from outside; a log line carrying a megabyte of it,
// or a terminal escape sequence, is a second problem on top of the first.
func clip(s string) string {
	const limit = 40
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= limit {
			b.WriteString("…")
			break
		}
		if unicode.IsControl(r) {
			b.WriteRune('�')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
