package connector_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stelfin/stelfin/api/intent"
	"github.com/stelfin/stelfin/connector"
)

func TestATabularAmountIsAPlainDecimal(t *testing.T) {
	cases := map[string]string{
		"5000":       "5000",
		"5,000":      "5000",
		"  5000  ":   "5000",
		"5000.50":    "5000.50",
		"0.0000001":  "0.0000001",
		".5":         "0.5",
		"0005":       "5",
		"1,234,567":  "1234567",
		"'5000":      "5000",
		"12.3456789": "12.3456789",
	}
	for cell, want := range cases {
		t.Run(cell, func(t *testing.T) {
			got, err := connector.NormalizeTabularAmount(connector.Wrap(cell))
			if err != nil {
				t.Fatalf("refused %q: %v", cell, err)
			}
			if got != want {
				t.Fatalf("%q became %q, want %q", cell, got, want)
			}
		})
	}
}

// TestASpreadsheetIsNotASentence is the whole reason this exists separately.
//
// intent.NormalizeAmount reads every one of these, and is right to: a chat
// message is one human who can be asked a follow-up. A spreadsheet is two
// hundred rows nobody reads individually, where a guess that is wrong is wrong
// two hundred times under a single approval.
func TestASpreadsheetIsNotASentence(t *testing.T) {
	refused := []string{
		"five thousand",
		"5k",
		"2.5m",
		"$5000",
		"₦5,000",
		"5000 USDC",
		"about 5000",
		"5000/month",
		"=B2*C2",
		"(50)",
		"-50",
		"+50",
		"0",
		"0.00",
		"",
		"   ",
		"5.",
		"5..5",
		"1.00000001",
		"5,00,0",
		"5,0000",
		"1,2345,678",
		"5,",
		",500",
		"5.0,0",
		"1e3",
		"NaN",
	}
	for _, cell := range refused {
		t.Run(cell, func(t *testing.T) {
			if got, err := connector.NormalizeTabularAmount(connector.Wrap(cell)); err == nil {
				t.Fatalf("%q was read as %q", cell, got)
			} else if !errors.Is(err, connector.ErrAmountUnreadable) {
				t.Fatalf("%q: error = %v, want ErrAmountUnreadable", cell, err)
			}
		})
	}
}

// TestTheChatParserIsDeliberatelyMoreGenerous pins the difference rather than
// leaving it to a comment. If these ever agree, one of them has drifted.
func TestTheChatParserIsDeliberatelyMoreGenerous(t *testing.T) {
	for _, text := range []string{"five thousand", "5k", "$5000"} {
		if _, err := intent.NormalizeAmount(text); err != nil {
			t.Errorf("the chat parser stopped reading %q: %v", text, err)
		}
		if _, err := connector.NormalizeTabularAmount(connector.Wrap(text)); err == nil {
			t.Errorf("the tabular parser started reading %q", text)
		}
	}
}

// TestAnErrorNamesTheCellWithoutRepeatingItWhole: a cell is arbitrary text from
// outside, and a log line carrying a megabyte of it — or a terminal escape —
// is a second problem on top of the first.
func TestAnErrorNamesTheCellWithoutRepeatingItWhole(t *testing.T) {
	long := strings.Repeat("9", 500) + "x"
	_, err := connector.NormalizeTabularAmount(connector.Wrap(long))
	if err == nil {
		t.Fatal("accepted")
	}
	if len(err.Error()) > 250 {
		t.Errorf("the error is %d characters long", len(err.Error()))
	}

	_, err = connector.NormalizeTabularAmount(connector.Wrap("50\x1b[31m0"))
	if err == nil {
		t.Fatal("accepted")
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Error("the error carries a terminal escape sequence through")
	}
}

// TestTheResultIsCanonical: the amount is compared and hashed downstream, so
// two spellings of one number must produce one string.
func TestTheResultIsCanonical(t *testing.T) {
	for _, group := range [][]string{
		{"5000", "5,000", "0005000", " 5000 ", "'5000"},
		{"0.5", ".5", "0.50000000000"[:len("0.5")]},
	} {
		var first string
		for i, spelling := range group {
			got, err := connector.NormalizeTabularAmount(connector.Wrap(spelling))
			if err != nil {
				t.Fatalf("%q: %v", spelling, err)
			}
			if i == 0 {
				first = got
				continue
			}
			if got != first {
				t.Errorf("%q became %q but %q became %q", group[0], first, spelling, got)
			}
		}
	}
}
