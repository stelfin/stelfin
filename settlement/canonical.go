package settlement

import (
	"strconv"
	"strings"
)

// The canonical form of a description: the exact bytes both implementations
// must produce.
//
// It is a line-oriented text format rather than JSON, and that choice is the
// whole reason this file is short.
//
// JSON looks like the obvious answer and is a trap. Two encoders agree on the
// value of a string and disagree on almost everything else that matters here:
// which characters get escaped and how (< versus <), whether non-ASCII is
// escaped at all, how a float is spelled, whether a key order survives, whether
// a trailing newline exists. Every one of those differences turns a byte
// comparison into a false alarm, and a false alarm on a confirmation screen is
// trained away rather than investigated.
//
// So: one field per line, tab-separated, in a fixed order, with a single
// escaping rule that both sides implement in four lines. Nothing is inferred,
// nothing is optional, and there is no encoder in the middle with opinions.

// canonicalVersion prefixes the form.
//
// A change to the format changes this, so two builds that disagree fail loudly
// on the first line rather than subtly on the fortieth.
const canonicalVersion = "stelfin-tx-description-1"

// Canonical renders the description as the bytes to compare.
//
// The browser produces the same bytes from the same XDR. If they differ, the
// page refuses to show a sign button — not because the difference is known to
// be an attack, but because a description nobody can reproduce is not a
// description.
func (d *TxDescription) Canonical() string {
	var b strings.Builder

	line := func(parts ...string) {
		for i, p := range parts {
			if i > 0 {
				b.WriteByte('\t')
			}
			b.WriteString(escape(p))
		}
		b.WriteByte('\n')
	}

	line(canonicalVersion)
	line("kind", d.Kind)
	line("network", d.Network)
	line("source", d.Source)
	line("sequence", d.Sequence)
	line("fee", d.Fee)
	line("min_time", d.MinTime)
	line("max_time", d.MaxTime)
	line("memo", d.MemoType, d.Memo)
	line("operations", strconv.Itoa(len(d.Operations)))

	for _, op := range d.Operations {
		index := strconv.Itoa(op.Index)
		line("op", index, op.Type, op.Source)
		for _, f := range op.Fields {
			line("field", index, f.Label, f.Kind, f.Value)
		}
	}

	// The hash is last and deliberately part of the form. It binds the
	// description to one envelope: a description that matched but named a
	// different transaction would be a correct description of the wrong thing.
	line("hash", d.Hash)

	return b.String()
}

// escape makes a value safe to put in a tab-separated line.
//
// Four replacements, in this order, and the order matters: backslash first, or
// an escaped newline would be re-escaped into something that decodes wrongly.
// Deliberately not a general-purpose encoder — a memo can contain anything, and
// this is the only thing standing between "anything" and a line format.
func escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
