package slacktest

import "bytes"

// endpointHost and endpointPath are kept apart so this file, which the guard
// also walks, never contains a contiguous endpoint of its own.
const (
	endpointHost = "slack" + ".com"
	endpointPath = "/api"
)

// NamesRealEndpoint reports whether src reaches production Slack rather than
// merely describing it.
//
// The distinction is use versus mention. A doc comment explaining which hosts a
// permalink may carry is prose and must be allowed to name one; a string a
// client can be constructed from is not. So comments are stripped and what
// remains is searched — regular and raw strings alike, since both are strings a
// caller can pass.
//
// A command must name production; that is its job, and it does so through the
// exported constant, which is greppable and reviewable. Nothing under test ever
// should, so in a test file the constant is refused as well — a hole the older
// substring check never covered, because the constant's name does not contain
// the host.
func NamesRealEndpoint(src []byte, isTestFile bool) bool {
	code := withoutComments(src)
	if bytes.Contains(code, []byte(endpointHost+endpointPath)) {
		return true
	}
	if !isTestFile {
		return false
	}
	return bytes.Contains(code, []byte("slackread"+".SlackAPI")) ||
		bytes.Contains(code, []byte("slackwrite"+".SlackAPI"))
}

// withoutComments returns src with its comments removed and everything else,
// strings included, left in place.
//
// String state has to be tracked while doing it: an endpoint literal contains
// "//" after its scheme, so a scan that treated that as the start of a comment
// would discard the very text this guard exists to find.
func withoutComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); {
		next, isComment := skipComment(src, i)
		switch {
		case isComment:
			i = next
		case isQuote(src[i]):
			end := literalEnd(src, i)
			out = append(out, src[i:end]...)
			i = end
		default:
			out = append(out, src[i])
			i++
		}
	}
	return out
}

// skipComment returns the index just past the comment that starts at i, keeping
// a line comment's newline, and whether one starts there at all.
func skipComment(src []byte, i int) (int, bool) {
	if src[i] != '/' || i+1 >= len(src) {
		return i, false
	}
	switch src[i+1] {
	case '/':
		return skipTo(src, i, []byte("\n"), true), true
	case '*':
		return skipTo(src, i+2, []byte("*/"), false), true
	}
	return i, false
}

// isQuote reports whether c opens a string, raw string or rune literal.
func isQuote(c byte) bool { return c == '"' || c == '`' || c == '\'' }

// skipTo returns the index just past the next occurrence of stop, or the end of
// src when there is none. keepStop leaves the terminator in place, which a line
// comment needs so the newline still separates what follows.
func skipTo(src []byte, from int, stop []byte, keepStop bool) int {
	rel := bytes.Index(src[from:], stop)
	if rel < 0 {
		return len(src)
	}
	if keepStop {
		return from + rel
	}
	return from + rel + len(stop)
}

// literalEnd returns the index just past the string, raw string or rune literal
// beginning at i. An unterminated literal runs to the end of src, which is what
// the compiler would reject anyway.
func literalEnd(src []byte, i int) int {
	quote := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if quote != '`' {
				j++
			}
		case quote:
			return j + 1
		case '\n':
			// Only a raw string may span lines; the others end here.
			if quote != '`' {
				return j
			}
		}
	}
	return len(src)
}
