package chat

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitLeavesAShortReplyAlone(t *testing.T) {
	for _, text := range []string{"one line", "two\nlines", "a paragraph\n\nand another"} {
		got := Split(text, 100)
		if len(got) != 1 || got[0] != text {
			t.Errorf("Split(%q) = %q, want it whole", text, got)
		}
	}
	if got := Split("  \n ", 100); got != nil {
		t.Errorf("Split of nothing = %q, want nothing to send", got)
	}
}

// TestSplitNeverExceedsTheLimit is the property the function exists for: a
// piece one unit over is a message the platform refuses whole.
func TestSplitNeverExceedsTheLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("What I can do here:\n")
	for i := range 60 {
		fmt.Fprintf(&b, "  /command-%d — does the thing that command number %d does\n", i, i)
	}
	b.WriteString("\nOr just tell me what you want to send, in your own words.")
	text := b.String()

	for _, limit := range []int{2000, 500, 80, 30} {
		pieces := Split(text, limit)
		if len(pieces) < 2 {
			t.Fatalf("limit %d: a %d-unit reply came back as %d piece(s)", limit, units(text), len(pieces))
		}
		for i, p := range pieces {
			if n := units(p); n > limit {
				t.Errorf("limit %d: piece %d is %d units", limit, i, n)
			}
			if strings.TrimSpace(p) != p || p == "" {
				t.Errorf("limit %d: piece %d has stray whitespace or is empty: %q", limit, i, p)
			}
		}
		// Nothing lost and nothing reordered: every word, in order.
		if got, want := strings.Fields(strings.Join(pieces, " ")), strings.Fields(text); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("limit %d: the pieces do not carry the same words in the same order", limit)
		}
	}
}

// TestSplitBreaksWhereAReaderWould: between paragraphs before between lines,
// between lines before between words.
func TestSplitBreaksWhereAReaderWould(t *testing.T) {
	first := strings.Repeat("a", 40)
	second := "line one\nline two"
	got := Split(first+"\n\n"+second, 50)
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("pieces = %q, want the break at the blank line", got)
	}

	lines := strings.Repeat("a", 30) + "\n" + strings.Repeat("b", 30)
	got = Split(lines, 40)
	if len(got) != 2 || got[0] != strings.Repeat("a", 30) || got[1] != strings.Repeat("b", 30) {
		t.Fatalf("pieces = %q, want the break at the line end", got)
	}
}

// TestSplitNeverBreaksALink: half a link is a link that does not work, carrying
// half of somebody's authority to sign.
func TestSplitNeverBreaksALink(t *testing.T) {
	link := "https://stelfin.example/approve#" + strings.Repeat("x", 180)
	text := strings.Repeat("Some words before the link. ", 8) + "Sign it here:\n" + link + "\n\nAnd some after."

	for _, limit := range []int{220, 300, 400} {
		var found bool
		for _, p := range Split(text, limit) {
			if strings.Contains(p, link) {
				found = true
			} else if strings.Contains(p, "stelfin.example") {
				t.Errorf("limit %d: a piece carries part of the link: %q", limit, p)
			}
		}
		if !found {
			t.Errorf("limit %d: no piece carries the link whole", limit)
		}
	}
}

// TestSplitCountsTheWayThePlatformsDo: an emoji is one rune, four bytes and two
// UTF-16 code units, and Telegram's limit is in the last of those.
func TestSplitCountsTheWayThePlatformsDo(t *testing.T) {
	word := strings.Repeat("👍", 10) // 10 runes, 20 units
	text := word + " " + word

	if got := Split(text, 41); len(got) != 1 {
		t.Errorf("41 units fit in 41, and came back as %d pieces", len(got))
	}
	got := Split(text, 40)
	if len(got) != 2 || got[0] != word || got[1] != word {
		t.Errorf("pieces = %q, want the two words apart", got)
	}

	// A word longer than a whole message is the one thing that gets cut, and
	// never through the middle of a character.
	for _, p := range Split(strings.Repeat("👍", 25), 9) {
		if n := units(p); n > 9 || n%2 != 0 {
			t.Errorf("piece %q is %d units: over the limit, or half an emoji", p, n)
		}
	}
}
