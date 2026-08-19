package telegram

import (
	"strings"
	"unicode"
	"unicode/utf16"
)

// entity is one span Telegram has annotated in a message.
type entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// splitCommand separates a leading bot command from its arguments.
//
// It returns the command name without its leading '/' and without any
// "@botname" suffix, lowercased, plus the remaining text. ok is false when the
// message is not a command, in which case args is the whole text, untouched,
// and the caller treats it as free text.
//
// # Which commands count
//
// Only a command that *leads* the message. "@stelfinbot /balance" and
// "👍 /balance" lead it; "he said /pay 5000 yesterday" does not, and acting on
// that would let someone quoting a conversation issue an instruction.
//
// "Leads" is defined as: everything before the command is free of letters and
// digits, once any mention of the bot is discounted. That admits the mention
// and the emoji, which people really type, and excludes prose, which is the
// case that matters.
//
// # Why the offsets need converting
//
// Telegram reports entity offsets in **UTF-16 code units**, not bytes and not
// runes. Any rune above U+FFFF counts as two, so slicing a Go string by a
// reported offset is wrong for a message like "👍 /pay 50 to bob" — an entirely
// ordinary message, in which the byte index, the rune index and the reported
// offset are three different numbers.
//
// Getting that wrong is worse than a display bug. args is the exact string that
// gets tokenized and the exact string shown back to the user as what they said;
// a shift of one code unit makes every provenance span downstream a check
// against text the user never wrote.
func splitCommand(text string, entities []entity) (cmd, args string, ok bool) {
	units := utf16.Encode([]rune(text))

	for _, e := range entities {
		if e.Type != "bot_command" {
			continue
		}
		if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > len(units) {
			// Telegram would have to be wrong for this to happen, but the
			// slices below are unchecked otherwise, and a panic in a webhook
			// handler is a denial of service on a body that authenticated with
			// nothing but a shared secret.
			return "", text, false
		}
		if !leads(units[:e.Offset], entities, e.Offset) {
			return "", text, false
		}

		raw := string(utf16.Decode(units[e.Offset : e.Offset+e.Length]))
		rest := string(utf16.Decode(units[e.Offset+e.Length:]))

		name := strings.TrimPrefix(raw, "/")
		if at := strings.IndexByte(name, '@'); at >= 0 {
			// "/pay@stelfinbot", in a group where several bots are present.
			name = name[:at]
		}
		if name == "" {
			return "", text, false
		}
		return strings.ToLower(name), strings.TrimSpace(rest), true
	}
	return "", text, false
}

// leads reports whether prefix — the UTF-16 units before a command — is
// insignificant enough for the command to count as leading the message.
//
// Mentions are blanked first, so "@stelfinbot /balance" qualifies while
// "ada said /balance" does not. What remains must carry no letters and no
// digits; whitespace, punctuation and emoji are all fine.
func leads(prefix []uint16, entities []entity, commandOffset int) bool {
	masked := append([]uint16(nil), prefix...)
	for _, e := range entities {
		if e.Type != "mention" && e.Type != "text_mention" {
			continue
		}
		if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > commandOffset {
			continue
		}
		for i := e.Offset; i < e.Offset+e.Length; i++ {
			masked[i] = ' '
		}
	}
	for _, r := range utf16.Decode(masked) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
