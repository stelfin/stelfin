package money

import (
	"errors"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		in   string
		want Stroops
	}{
		{"0", 0},
		{"0.0000000", 0},
		{"-0", 0},
		{"1", One},
		{"1.0000000", One},
		{"0.0000001", 1},
		{"5000", 5000 * One},
		{"5000.5", 50_005_000_000},
		{"5000.50", 50_005_000_000},
		{"-1", -One},
		{"-0.0000001", -1},
		{"0.1234567", 1_234_567},

		// Boundaries. int64 max is 922337203685.4775807 whole units.
		{"922337203685.4775807", maxStroops},
		{"-922337203685.4775808", minStroops},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) returned error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		in      string
		wantErr error
	}{
		{"", ErrSyntax},
		{".", ErrSyntax},
		{".5", ErrSyntax},            // no digit before the point
		{"5.", ErrSyntax},            // trailing point
		{"+1", ErrSyntax},            // explicit plus not accepted
		{" 1", ErrSyntax},            // caller must trim
		{"1 ", ErrSyntax},            //
		{"1,000", ErrSyntax},         // separators are the normalizer's job
		{"5k", ErrSyntax},            //
		{"1e5", ErrSyntax},           // no exponents
		{"--1", ErrSyntax},           //
		{"1.2.3", ErrSyntax},         // Cut takes the first dot, "2.3" has a non-digit
		{"abc", ErrSyntax},           //
		{"0.12345678", ErrPrecision}, // 8 places: must not silently truncate
		{"922337203685.4775808", ErrOverflow},
		{"-922337203685.4775809", ErrOverflow},
		{"99999999999999999999", ErrOverflow},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) = %d, want error %v", c.in, got, c.wantErr)
			continue
		}
		if !errors.Is(err, c.wantErr) {
			t.Errorf("Parse(%q) error = %v, want %v", c.in, err, c.wantErr)
		}
	}
}

// TestPrecisionIsAnErrorNotATruncation pins the choice that matters most here:
// an over-precise amount is rejected rather than rounded. Silently dropping the
// eighth decimal would change what the user agreed to pay.
func TestPrecisionIsAnErrorNotATruncation(t *testing.T) {
	if _, err := Parse("1.00000005"); !errors.Is(err, ErrPrecision) {
		t.Fatalf("Parse(1.00000005) error = %v, want ErrPrecision", err)
	}
}

func TestStringRoundTrip(t *testing.T) {
	values := []Stroops{
		0, 1, -1, One, -One,
		5000 * One,
		50_005_000_000,
		maxStroops,
		minStroops,
		minStroops + 1,
		maxStroops - 1,

		// Beyond float64's 53-bit mantissa. If a float ever enters this path,
		// these are the values that break first.
		1 << 53,
		1<<53 + 1,
		-(1 << 53),
		-(1<<53 + 1),
	}
	for _, v := range values {
		got, err := Parse(v.String())
		if err != nil {
			t.Errorf("Parse(%q) from Stroops(%d): %v", v.String(), int64(v), err)
			continue
		}
		if got != v {
			t.Errorf("round trip of %d via %q gave %d", int64(v), v.String(), int64(got))
		}
	}
}

// FuzzStringRoundTrip asserts Parse(String(x)) == x across the whole int64
// domain. Every representable amount must survive the wire format exactly.
func FuzzStringRoundTrip(f *testing.F) {
	for _, seed := range []int64{0, 1, -1, 1 << 53, maxInt64, minInt64} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v int64) {
		s := Stroops(v)
		got, err := Parse(s.String())
		if err != nil {
			t.Fatalf("Parse(%q) from Stroops(%d): %v", s.String(), v, err)
		}
		if got != s {
			t.Fatalf("round trip of %d via %q gave %d", v, s.String(), int64(got))
		}
	})
}

// FuzzParseNeverPanics feeds arbitrary text to the parser. Malformed input must
// always come back as an error, never a panic and never a silent zero.
func FuzzParseNeverPanics(f *testing.F) {
	for _, seed := range []string{"", "0", "-1.5", "1,000", "\x00", "999999999999999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := Parse(s)
		if err != nil {
			return
		}
		// Anything that parses must render back to something that parses
		// identically, otherwise the accepted language is inconsistent.
		again, err := Parse(v.String())
		if err != nil || again != v {
			t.Fatalf("Parse(%q) = %d but its String() %q re-parsed to %d (err %v)",
				s, int64(v), v.String(), int64(again), err)
		}
	})
}

func TestAddOverflow(t *testing.T) {
	if _, err := maxStroops.Add(1); !errors.Is(err, ErrOverflow) {
		t.Errorf("max+1 error = %v, want ErrOverflow", err)
	}
	if _, err := minStroops.Add(-1); !errors.Is(err, ErrOverflow) {
		t.Errorf("min-1 error = %v, want ErrOverflow", err)
	}
	if got, err := maxStroops.Add(-1); err != nil || got != maxStroops-1 {
		t.Errorf("max+(-1) = %d, %v", int64(got), err)
	}
	if got, err := minStroops.Add(maxStroops); err != nil || got != -1 {
		t.Errorf("min+max = %d, %v; want -1", int64(got), err)
	}
}

func TestSubOverflow(t *testing.T) {
	// The asymmetric case: -t is not representable when t is the minimum.
	if got, err := Stroops(-1).Sub(minStroops); err != nil || got != maxStroops {
		t.Errorf("-1 - min = %d, %v; want %d", int64(got), err, int64(maxStroops))
	}
	if got, err := minStroops.Sub(minStroops); err != nil || got != 0 {
		t.Errorf("min - min = %d, %v; want 0", int64(got), err)
	}
	if _, err := Stroops(0).Sub(minStroops); !errors.Is(err, ErrOverflow) {
		t.Errorf("0 - min error = %v, want ErrOverflow", err)
	}
	if _, err := maxStroops.Sub(-1); !errors.Is(err, ErrOverflow) {
		t.Errorf("max - (-1) error = %v, want ErrOverflow", err)
	}
}

func TestNegOverflow(t *testing.T) {
	if _, err := minStroops.Neg(); !errors.Is(err, ErrOverflow) {
		t.Errorf("neg(min) error = %v, want ErrOverflow", err)
	}
	if got, err := maxStroops.Neg(); err != nil || got != -maxStroops {
		t.Errorf("neg(max) = %d, %v", int64(got), err)
	}
}

func TestSum(t *testing.T) {
	// The ledger's zero-sum invariant runs through here: a balanced set of
	// signed entries must total exactly zero.
	got, err := Sum(MustParse("5000.5"), MustParse("-3000.25"), MustParse("-2000.25"))
	if err != nil {
		t.Fatalf("Sum returned error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("Sum of a balanced set = %s, want 0", got)
	}

	if _, err := Sum(maxStroops, 1); !errors.Is(err, ErrOverflow) {
		t.Errorf("Sum(max, 1) error = %v, want ErrOverflow", err)
	}

	// An intermediate overflow must be caught even when the final total would
	// have been representable.
	if _, err := Sum(maxStroops, maxStroops, minStroops, minStroops); !errors.Is(err, ErrOverflow) {
		t.Errorf("Sum with intermediate overflow error = %v, want ErrOverflow", err)
	}

	if got, err := Sum(); err != nil || !got.IsZero() {
		t.Errorf("Sum() = %d, %v; want 0, nil", int64(got), err)
	}
}

func TestSign(t *testing.T) {
	for _, c := range []struct {
		in   Stroops
		want int
	}{{-5, -1}, {0, 0}, {5, 1}, {minStroops, -1}, {maxStroops, 1}} {
		if got := c.in.Sign(); got != c.want {
			t.Errorf("Stroops(%d).Sign() = %d, want %d", int64(c.in), got, c.want)
		}
	}
}

// TestNoFloatPrecisionLoss demonstrates why this package exists. Above 2^53
// float64 steps by two, so 2^53 and 2^53+1 collapse onto the same float — a
// difference of one stroop that any float-touching code path would erase. The
// exact-integer path keeps them apart.
func TestNoFloatPrecisionLoss(t *testing.T) {
	a := Stroops(1 << 53)
	b := Stroops(1<<53 + 1)

	if float64(a) != float64(b) {
		t.Fatalf("expected float64 to conflate %d and %d, but it did not; "+
			"revisit whether this test still demonstrates anything", int64(a), int64(b))
	}
	if a.String() == b.String() {
		t.Fatalf("distinct amounts %d and %d rendered identically as %q",
			int64(a), int64(b), a.String())
	}
}

const (
	maxInt64 = 1<<63 - 1
	minInt64 = -1 << 63
)

func TestDisplay(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0", "0.00"},
		{"1", "1.00"},
		{"5000", "5,000.00"},
		{"5000.5", "5,000.50"},
		{"5000.55", "5,000.55"},
		{"5000.555", "5,000.555"},
		{"0.0000001", "0.0000001"},
		{"123", "123.00"},
		{"1234", "1,234.00"},
		{"1234567.89", "1,234,567.89"},
		{"-5000.5", "-5,000.50"},
		{"922337203685.4775807", "922,337,203,685.4775807"},
	}
	for _, c := range cases {
		if got := MustParse(c.in).Display(); got != c.want {
			t.Errorf("Parse(%q).Display() = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDisplayNeverRounds: every digit shown must be real. A display that
// rounded would tell the user they are sending an amount they are not.
func TestDisplayNeverRounds(t *testing.T) {
	oneStroopShort := MustParse("1") - 1
	if got := oneStroopShort.Display(); got == "1.00" {
		t.Errorf("Display() = %q for %d stroops, which is not one whole unit",
			got, int64(oneStroopShort))
	}
	if want := "0.9999999"; oneStroopShort.Display() != want {
		t.Errorf("Display() = %q, want %q", oneStroopShort.Display(), want)
	}
}
