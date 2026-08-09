// Package money implements exact fixed-point arithmetic for Stellar amounts.
//
// Stellar represents every amount as a signed 64-bit count of stroops, where one
// whole unit of any asset is 10^7 stroops. Note that this differs from USDC on
// EVM chains, which uses 6 decimals — any bridge or price feed crossing that
// boundary is a rounding-bug surface and must convert explicitly.
//
// Nothing here accepts or produces a floating-point value. float64 carries 53
// bits of mantissa and cannot represent the full int64 stroop range, so a single
// round-trip through float can silently alter a balance. There is deliberately
// no Float64() method and no way to construct a Stroops from one.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Stroops is a signed count of stroops: the smallest indivisible unit of any
// Stellar asset. Amounts are signed because ledger entries are signed — a
// balanced transaction sums to zero.
type Stroops int64

const (
	// Precision is the number of decimal places in a Stellar amount.
	Precision = 7

	// One is the number of stroops in one whole unit of any asset.
	One Stroops = 10_000_000
)

var (
	// ErrOverflow reports an amount outside the representable int64 range.
	ErrOverflow = errors.New("money: amount overflows int64 stroops")

	// ErrSyntax reports a malformed amount string.
	ErrSyntax = errors.New("money: malformed amount")

	// ErrPrecision reports an amount with more decimal places than Stellar can
	// represent. This is an error rather than a silent truncation: quietly
	// dropping a digit changes what the user agreed to pay.
	ErrPrecision = errors.New("money: amount exceeds 7 decimal places")
)

// Parse converts a canonical decimal string to Stroops.
//
// It is deliberately strict. The input must be an optional '-' followed by at
// least one digit, then optionally '.' and one to seven digits. No spaces, no
// thousands separators, no exponents, no bare '.5', no '+'. Presentation forms
// like "5,000" or "5k" are the normalizer's problem, not this package's — see
// api/intent. Keeping Parse strict means anything that reaches it has already
// been through a deliberate conversion.
func Parse(s string) (Stroops, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty string", ErrSyntax)
	}

	body := s
	neg := false
	if body[0] == '-' {
		neg = true
		body = body[1:]
	}

	intPart, fracPart, hasDot := strings.Cut(body, ".")
	if intPart == "" {
		return 0, fmt.Errorf("%w: %q has no digits before the decimal point", ErrSyntax, s)
	}
	if hasDot && fracPart == "" {
		return 0, fmt.Errorf("%w: %q has a trailing decimal point", ErrSyntax, s)
	}
	if !allDigits(intPart) || !allDigits(fracPart) {
		return 0, fmt.Errorf("%w: %q contains a non-digit", ErrSyntax, s)
	}
	if len(fracPart) > Precision {
		return 0, fmt.Errorf("%w: %q has %d", ErrPrecision, s, len(fracPart))
	}

	// Right-pad the fraction to exactly Precision digits, then read the whole
	// thing as a single integer. This avoids a multiply that could overflow
	// before the range check, and lets strconv own the boundary conditions.
	digits := intPart + fracPart + strings.Repeat("0", Precision-len(fracPart))
	if neg {
		digits = "-" + digits
	}

	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("%w: %q", ErrOverflow, s)
		}
		return 0, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	return Stroops(v), nil
}

// MustParse is Parse for compile-time constants in tests and migrations. It
// panics on malformed input and must never be reached by user-supplied data.
func MustParse(s string) Stroops {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// String renders the amount in Stellar's canonical form: a signed decimal with
// exactly seven places, which is what txnbuild and Horizon expect on the wire.
// It round-trips exactly through Parse for every representable value.
func (s Stroops) String() string {
	u, neg := s.abs()
	whole, frac := u/uint64(One), u%uint64(One)

	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d", sign, whole, Precision, frac)
}

// abs returns the magnitude as a uint64 along with the sign. Going through
// uint64 is what makes the most-negative int64 representable: -(-1<<63) would
// overflow back to itself in signed arithmetic.
func (s Stroops) abs() (mag uint64, neg bool) {
	if s < 0 {
		return uint64(-(s + 1)) + 1, true
	}
	return uint64(s), false
}

// Add returns s+t, or ErrOverflow if the result is not representable.
func (s Stroops) Add(t Stroops) (Stroops, error) {
	sum := s + t
	// Overflow happened iff both operands share a sign that the result doesn't.
	if (s > 0 && t > 0 && sum < 0) || (s < 0 && t < 0 && sum >= 0) {
		return 0, fmt.Errorf("%w: %s + %s", ErrOverflow, s, t)
	}
	return sum, nil
}

// Sub returns s-t, or ErrOverflow if the result is not representable.
func (s Stroops) Sub(t Stroops) (Stroops, error) {
	if t == minStroops {
		// -t is not representable, so compute s+2^63 without ever forming it.
		// The result fits only when s is negative: s+2^63 <= 2^63-1 iff s <= -1.
		if s >= 0 {
			return 0, fmt.Errorf("%w: %s - %s", ErrOverflow, s, t)
		}
		return s + maxStroops + 1, nil
	}
	return s.Add(-t)
}

// Neg returns -s, or ErrOverflow for the most-negative value.
func (s Stroops) Neg() (Stroops, error) {
	if s == minStroops {
		return 0, fmt.Errorf("%w: negating %s", ErrOverflow, s)
	}
	return -s, nil
}

// IsZero reports whether the amount is exactly zero.
func (s Stroops) IsZero() bool { return s == 0 }

// Sign returns -1, 0 or +1.
func (s Stroops) Sign() int {
	switch {
	case s < 0:
		return -1
	case s > 0:
		return 1
	default:
		return 0
	}
}

// Sum adds every amount, returning ErrOverflow if any intermediate result is
// not representable. It is the arithmetic behind the ledger's zero-sum
// invariant, so it must never wrap silently.
func Sum(amounts ...Stroops) (Stroops, error) {
	var total Stroops
	for i, a := range amounts {
		next, err := total.Add(a)
		if err != nil {
			return 0, fmt.Errorf("summing amount %d of %d: %w", i+1, len(amounts), err)
		}
		total = next
	}
	return total, nil
}

const (
	minStroops Stroops = -1 << 63
	maxStroops Stroops = 1<<63 - 1
)

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
