package telegram

import (
	"testing"
	"unicode/utf16"
)

// entitiesFor builds the bot_command entity Telegram would report for a command
// at the start of text. Its length in UTF-16 units equals its character count,
// because Telegram commands are ASCII.
func entitiesFor(_ string, commandLength int) []entity {
	return []entity{{Type: "bot_command", Offset: 0, Length: commandLength}}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string]struct {
		text     string
		entities []entity
		cmd      string
		args     string
		ok       bool
	}{
		"plain command": {
			text: "/pay 5000 to ada", entities: entitiesFor("/pay 5000 to ada", 4),
			cmd: "pay", args: "5000 to ada", ok: true,
		},
		"addressed to this bot": {
			text:     "/pay@stelfinbot 5000 to ada",
			entities: []entity{{Type: "bot_command", Offset: 0, Length: 15}},
			cmd:      "pay", args: "5000 to ada", ok: true,
		},
		"uppercase": {
			text: "/PAY 5000", entities: entitiesFor("/PAY 5000", 4),
			cmd: "pay", args: "5000", ok: true,
		},
		"no arguments": {
			text: "/balance", entities: entitiesFor("/balance", 8),
			cmd: "balance", args: "", ok: true,
		},
		"free text": {
			text: "send 5000 to ada", entities: nil,
			cmd: "", args: "send 5000 to ada", ok: false,
		},
		"command after prose is not a command": {
			text:     "he said /pay 5000",
			entities: []entity{{Type: "bot_command", Offset: 8, Length: 4}},
			cmd:      "", args: "he said /pay 5000", ok: false,
		},
		"command after a mention of the bot": {
			text: "@stelfinbot /pay 5000",
			entities: []entity{
				{Type: "mention", Offset: 0, Length: 11},
				{Type: "bot_command", Offset: 12, Length: 4},
			},
			cmd: "pay", args: "5000", ok: true,
		},
		"leading whitespace": {
			text:     "  /balance",
			entities: []entity{{Type: "bot_command", Offset: 2, Length: 8}},
			cmd:      "balance", args: "", ok: true,
		},
		"a digit before the command is prose": {
			text:     "1 /pay 5000",
			entities: []entity{{Type: "bot_command", Offset: 2, Length: 4}},
			cmd:      "", args: "1 /pay 5000", ok: false,
		},
		"other entity types are ignored": {
			text:     "https://example.test",
			entities: []entity{{Type: "url", Offset: 0, Length: 20}},
			cmd:      "", args: "https://example.test", ok: false,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cmd, args, ok := splitCommand(c.text, c.entities)
			if ok != c.ok || cmd != c.cmd || args != c.args {
				t.Fatalf("splitCommand(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.text, cmd, args, ok, c.cmd, c.args, c.ok)
			}
		})
	}
}

// TestSplitCommandHandlesAstralPlaneRunes is the whole reason this function
// exists rather than a TrimPrefix.
//
// Telegram reports entity offsets in UTF-16 code units. An emoji is one Go rune
// and one grapheme, but two UTF-16 units — so a message with an emoji before
// the command reports an offset that does not match any byte or rune index in
// the Go string. Slicing by it shifts the arguments, and the arguments are the
// exact string that gets tokenized and shown back to the user as what they
// said. A shift there makes every provenance span downstream a check against
// text the user never wrote.
func TestSplitCommandHandlesAstralPlaneRunes(t *testing.T) {
	// "👍" is U+1F44D: one rune, four bytes, TWO UTF-16 code units. So the
	// command that follows it starts at UTF-16 offset 3 ("👍" + a space), while
	// its rune index is 2 and its byte index is 5. All three disagree, which is
	// exactly the situation that breaks a naive implementation.
	text := "👍 /pay 5000 to ada"
	units := utf16.Encode([]rune(text))
	if len(units) == len([]rune(text)) {
		t.Fatal("test bug: the fixture has no astral-plane rune, so it proves nothing")
	}

	entities := []entity{{Type: "bot_command", Offset: 3, Length: 4}}
	cmd, args, ok := splitCommand(text, entities)

	if !ok {
		t.Fatal("a command preceded by an emoji was not recognised")
	}
	if cmd != "pay" {
		t.Errorf("cmd = %q, want %q", cmd, "pay")
	}
	if args != "5000 to ada" {
		t.Errorf("args = %q, want %q — the offset was applied to the wrong index space", args, "5000 to ada")
	}
}

// TestSplitCommandRefusesOutOfRangeOffsets: the slice would panic, and a panic
// in a webhook handler is a denial of service on a body that authenticated only
// with a shared secret.
func TestSplitCommandRefusesOutOfRangeOffsets(t *testing.T) {
	for _, e := range []entity{
		{Type: "bot_command", Offset: 0, Length: 999},
		{Type: "bot_command", Offset: 0, Length: 0},
		{Type: "bot_command", Offset: 0, Length: -1},
	} {
		cmd, args, ok := splitCommand("/pay 5000", []entity{e})
		if ok {
			t.Errorf("%+v: accepted an impossible entity (cmd=%q args=%q)", e, cmd, args)
		}
	}
}

// TestSplitCommandNeverLosesText: whatever comes back as args must be text the
// user actually typed, so that tokenizing it and showing it are the same thing.
func TestSplitCommandNeverLosesText(t *testing.T) {
	text := "/pay 5,000 USDC to \"the landlord\" 🏠"
	entities := []entity{{Type: "bot_command", Offset: 0, Length: 4}}

	_, args, ok := splitCommand(text, entities)
	if !ok {
		t.Fatal("command not recognised")
	}
	if want := `5,000 USDC to "the landlord" 🏠`; args != want {
		t.Fatalf("args = %q, want %q", args, want)
	}
}
