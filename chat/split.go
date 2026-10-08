package chat

import (
	"strings"
	"unicode/utf16"
)

// Split breaks a reply into pieces a platform will accept, each at most limit
// long, to be sent in order.
//
// Every platform caps a message, and a reply that grows — a list of commands, a
// roster of members — crosses the cap one day without anybody having changed
// the code that sends it. The platform then refuses the whole message, and what
// the member sees is nothing.
//
// Length is counted in UTF-16 code units, which is how Telegram counts and at
// least as strict as how Discord does. Counting bytes would split replies that
// fit; counting runes would pass ones that do not.
//
// It breaks where a reader would: between paragraphs if it can, between lines
// if it must, between words if a single line is too long. Only a single word
// longer than a whole message is ever cut, and nothing this bot sends is one —
// a signing link is a few hundred characters. So a link is never broken across
// two messages, which matters more than tidiness: half a link is a link that
// does not work, carrying half of somebody's authority to sign.
func Split(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" || limit <= 0 {
		return nil
	}
	if units(text) <= limit {
		return []string{text}
	}

	var out []string
	var piece strings.Builder
	flush := func() {
		if s := strings.TrimSpace(piece.String()); s != "" {
			out = append(out, s)
		}
		piece.Reset()
	}
	// add appends s, with sep before it if the piece is not empty, starting a
	// new piece first when it would not fit.
	add := func(s, sep string) {
		if piece.Len() > 0 && units(piece.String())+units(sep)+units(s) > limit {
			flush()
		}
		if piece.Len() > 0 {
			piece.WriteString(sep)
		}
		piece.WriteString(s)
	}

	for _, paragraph := range strings.Split(text, "\n\n") {
		if units(paragraph) <= limit {
			add(paragraph, "\n\n")
			continue
		}
		// A paragraph too long for one message goes in line by line, and still
		// starts after a blank line if it shares a message with what came
		// before.
		sep := "\n\n"
		for _, line := range strings.Split(paragraph, "\n") {
			if units(line) <= limit {
				add(line, sep)
				sep = "\n"
				continue
			}
			for _, word := range strings.Fields(line) {
				for units(word) > limit {
					// Longer than a whole message. Nothing to do but cut.
					flush()
					head, tail := cut(word, limit)
					out = append(out, head)
					word = tail
				}
				add(word, sep)
				sep = " "
			}
			sep = "\n"
		}
	}
	flush()
	return out
}

// units is the length of s in UTF-16 code units.
func units(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// cut divides s after at most limit UTF-16 code units, never inside a
// character.
func cut(s string, limit int) (head, tail string) {
	n := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1
		}
		if n+w > limit {
			return s[:i], s[i:]
		}
		n += w
	}
	return s, ""
}
