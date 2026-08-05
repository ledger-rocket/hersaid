// Package mrkdwn converts Markdown to Slack's mrkdwn dialect.
//
// The rules run in a fixed order, because the order is what makes them safe:
// code spans and
// completed links are tokenised out first and restored last, so no emphasis
// rule can rewrite the inside of a code block or a URL.
//
// The conversion is best-effort and deliberately small. The tool's main use is
// pasting code, logs and diffs into Slack, so leaving a construct alone is
// always the better failure: an unconverted asterisk is legible, a mangled
// identifier is not.
package mrkdwn

import (
	"regexp"
	"strconv"
	"strings"
)

// sentinel brackets the index of a protected fragment. It is stripped from the
// input first, so no message can forge a placeholder and name a fragment that
// was protected out of some other part of itself.
const sentinel = "\x1f"

var (
	// Fenced blocks are tried before inline spans, so ```a``` is one fragment
	// rather than two empty spans around an "a".
	codeRe = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")

	// \v is in Perl's \s but not Go's, and the oracle's URL class is [^)\s].
	linkRe = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s\v]+)\)`)

	headingRe = regexp.MustCompile("(?m)^[ \t]*#{1,6}[ \t]+(.+?)[ \t]*$")

	// Bullets are rewritten before bold so that a line beginning "**bold**" is
	// not read as a "*" bullet — and, because a separator is required, neither
	// is "*emphasis*".
	bulletRe = regexp.MustCompile(`(?m)^([ \t]*)[-+*][ \t]+`)

	// No (?s): bold that appears to span lines is a stray pair of asterisks far
	// more often than it is emphasis, and joining paragraphs into one bold run
	// is the worse guess.
	boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

	placeholderRe = regexp.MustCompile(sentinel + `(\d+)` + sentinel)
)

// Convert rewrites Markdown as Slack mrkdwn.
//
// Code — fenced and inline — is passed through byte for byte. Links become
// <url|text>, headings and **bold** become *bold*, and -, + or * bullets
// become •.
//
// Markdown's __bold__ is deliberately not converted: it mangled __init__,
// __main__ and every other dunder in pasted Python, and a tool whose main use
// is pasting code cannot afford that.
func Convert(s string) string {
	s = strings.ReplaceAll(s, sentinel, "")

	var protected []string
	protect := func(fragment string) string {
		protected = append(protected, fragment)
		return sentinel + strconv.Itoa(len(protected)-1) + sentinel
	}

	s = replace(codeRe, s, func(g []string) string { return protect(g[0]) })
	// Before emphasis, and protected like code: the destination of a link is
	// not prose, so [x](https://e.test/**seg**) must reach Slack with its path
	// intact rather than emphasised into nonsense.
	s = replace(linkRe, s, func(g []string) string { return protect("<" + g[2] + "|" + g[1] + ">") })

	s = replace(headingRe, s, func(g []string) string {
		// A heading is already bold, so bold inside it would nest and Slack
		// renders neither.
		return "*" + strings.ReplaceAll(g[1], "**", "") + "*"
	})
	s = replace(bulletRe, s, func(g []string) string { return g[1] + "• " })
	s = replace(boldRe, s, func(g []string) string { return "*" + g[1] + "*" })

	return restore(s, protected)
}

// restore puts the protected fragments back.
//
// It repeats until nothing more resolves, because a fragment can contain a
// placeholder: the link rule captures text that code protection has already
// replaced, as in [`code`](https://e.test). The oracle substitutes once and so
// leaves that inner placeholder in its output — a raw U+001F delivered to
// Slack. This is the one place the port does not reproduce the oracle, and it
// is a defect fix rather than a redesign; see TestNestedProtectionResolves.
//
// It terminates: a fragment can only name a fragment protected before it, so
// each round resolves strictly earlier indices.
func restore(s string, protected []string) string {
	for range len(protected) + 1 {
		next := replace(placeholderRe, s, func(g []string) string {
			i, err := strconv.Atoi(g[1])
			if err != nil || i >= len(protected) {
				// Unreachable while the input is stripped of sentinels, and
				// left alone rather than deleted if it ever is not.
				return g[0]
			}
			return protected[i]
		})
		if next == s {
			break
		}
		s = next
	}
	return s
}

// replace rewrites every match of re with f applied to the match and its
// submatches, f's result being used literally.
//
// Every rule goes through it rather than through ReplaceAllString because that
// expands $1 and ${name} in its replacement. Restoring a protected fragment
// puts message text in exactly that position, and a message is
// attacker-controlled — including by an external Slack Connect guest, whose
// display name can be anything at all — so a `${1}` pasted into Slack would
// otherwise name a capture group and vanish from the output. Using it for the
// rules whose replacement happens to be a constant costs nothing and leaves no
// exception to notice.
func replace(re *regexp.Regexp, s string, f func(groups []string) string) string {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range matches {
		groups := make([]string, len(m)/2)
		for i := range groups {
			if m[2*i] >= 0 {
				groups[i] = s[m[2*i]:m[2*i+1]]
			}
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(f(groups))
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
