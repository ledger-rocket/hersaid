// Package termsafe renders untrusted text that is about to be printed.
//
// Two sources of it meet here. A Slack filename is chosen by whoever uploaded
// the file, including an external guest. A local path is chosen by whoever
// named it and reaches the tool through a shell glob. Neither is safe to print
// raw: a control character drives the terminal, a newline restructures the
// output it appears in, and a bidi override reverses everything after it, so a
// .scr reads as a .pdf in the very line a person is reading in order to decide.
//
// It is one package because the rule must not differ between the render that
// shows what will be sent and the error that refuses it. A hostile name is
// likelier to appear in the refusal.
package termsafe

import (
	"strings"
	"unicode"
)

// Text returns s with everything that could drive or rearrange a terminal
// removed.
//
// The result may be shorter than the input. This is for display: the raw value
// is what still goes to the filesystem and into the audit record.
func Text(s string) string {
	return strings.Map(func(r rune) rune {
		if IsHostile(r) {
			return -1
		}
		return r
	}, s)
}

// IsHostile reports a rune that must not reach a terminal.
//
// unicode.IsControl covers C0 and C1 alike - 0x1b, and 0x9b, which is the
// control-sequence introducer that terminals honouring it act on exactly as
// they do on ESC-[. The format category covers the bidi overrides and isolates.
//
// The predicate is exported alongside Text because the filesystem needs the
// same question answered - a name holding one of these is unusable rather than
// merely unprintable - and two copies are how the display rule and the path
// rule drift apart.
func IsHostile(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// TextKeepingNewlines is Text for content where a line break is part of what is
// being shown — the inside of a code block, where removing them would run every
// line together. Everything else that could drive a terminal still goes.
func TextKeepingNewlines(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if IsHostile(r) {
			return -1
		}
		return r
	}, s)
}
