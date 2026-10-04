package mrkdwn

import (
	"strings"
	"testing"
)

// oracle holds the conversion's answers, captured by running it over each input
// rather than reasoning about what it ought to say. Several of these cases were
// bugs once — __bold__ eating __init__, emphasis
// rewriting the inside of a URL, a bullet swallowing a bold line-start — so
// they are pinned as answers, not re-derived.
var oracle = []struct{ name, in, want string }{
	{"empty", "", ""},
	{"plain text is untouched", "just a sentence\nand another\n", "just a sentence\nand another\n"},

	// Code is passed through untouched: the tool is used to paste code and logs.
	{"fenced code is untouched", "before\n```\n**bold** [x](https://e.test) # H\n- a\n```\nafter\n", "before\n```\n**bold** [x](https://e.test) # H\n- a\n```\nafter\n"},
	{"fenced code with a language tag", "```go\nx := **y**\n# not a heading\n```\n", "```go\nx := **y**\n# not a heading\n```\n"},
	{"inline code is untouched", "run `**x**` and `# h` and `- a`\n", "run `**x**` and `# h` and `- a`\n"},
	{"inline code does not span lines", "`a\nb`\n", "`a\nb`\n"},
	{"four backticks", "````\n", "````\n"},
	{"two fences on one line", "```a``` and ```b```\n", "```a``` and ```b```\n"},
	{"unclosed fence protects nothing", "```\n**bold**\n", "```\n*bold*\n"},

	// Links convert before emphasis, and the result is protected from it.
	{"link", "see [the docs](https://e.test/docs) now\n", "see <https://e.test/docs|the docs> now\n"},
	{"link url keeps markdown metacharacters", "[x](https://e.test/**seg**)\n", "<https://e.test/**seg**|x>\n"},
	{"link text keeps its own markdown", "[**x**](https://e.test)\n", "<https://e.test|**x**>\n"},
	{"a space in the url is not a link", "[a b](https://e.test/a b)\n", "[a b](https://e.test/a b)\n"},
	{"link inside code is not converted", "`[x](https://e.test)`\n", "`[x](https://e.test)`\n"},
	{"two links on one line", "[a](https://a.test) and [b](https://b.test)\n", "<https://a.test|a> and <https://b.test|b>\n"},
	{"empty link text is not a link", "[](https://e.test)\n", "[](https://e.test)\n"},

	// Headings.
	{"heading", "# Title\nbody\n", "*Title*\nbody\n"},
	{"heading levels", "# one\n## two\n### three\n###### six\n####### seven\n", "*one*\n*two*\n*three*\n*six*\n####### seven\n"},
	{"heading strips bold inside it", "## **Bold** heading\n", "*Bold heading*\n"},
	{"heading with leading and trailing whitespace", "   ##   Title   \n", "*Title*\n"},
	{"hash with no space is not a heading", "#nothashtag\n", "#nothashtag\n"},
	{"heading with no text is not a heading", "# \n", "# \n"},
	{"heading must start the line", "text # not a heading\n", "text # not a heading\n"},

	// Bullets, applied before bold.
	{"bullets", "- one\n+ two\n* three\n", "• one\n• two\n• three\n"},
	{"indented bullets keep their indent", "  - one\n\t+ two\n", "  • one\n\t• two\n"},
	{"a bullet needs a separator", "-nospace\n*nospace\n", "-nospace\n*nospace\n"},
	{"bold at line start is not eaten by the bullet rule", "**bold** at the start\n", "*bold* at the start\n"},
	{"bullet then bold", "* **bold** item\n", "• *bold* item\n"},
	{"bullet with a tab separator", "-\ttabbed\n", "• tabbed\n"},
	// The discriminator for the rule order: bold first would leave "* x *" for
	// the bullet rule to eat, giving "• x *".
	{"bold with inner spaces is bold, not a bullet", "** x **\n", "* x *\n"},
	// And the price of that order: emphasis whose opening asterisk is followed
	// by a space really is indistinguishable from a bullet.
	{"spaced emphasis is read as a bullet", "*  spaced  *\n", "• spaced  *\n"},

	// Bold.
	{"bold", "**bold** text\n", "*bold* text\n"},
	{"two bolds on a line", "**a** and **b**\n", "*a* and *b*\n"},
	{"bold is not greedy", "**a**b**c**\n", "*a*b*c*\n"},
	{"bold does not span lines", "**a\nb**\n", "**a\nb**\n"},
	{"empty bold", "****\n", "****\n"},
	{"underscore bold is left alone", "__bold__ and __init__ and __main__\n", "__bold__ and __init__ and __main__\n"},
	{"single asterisk emphasis is left alone", "*already slack bold*\n", "*already slack bold*\n"},

	// Go replacement metacharacters must never be interpreted.
	{"dollar one in text", "cost $1 and $2\n", "cost $1 and $2\n"},
	{"dollar brace in text", "${HOME} and ${x} and $0\n", "${HOME} and ${x} and $0\n"},
	{"dollar metacharacters in a heading", "# $1 ${x} $$\n", "*$1 ${x} $$*\n"},
	{"dollar metacharacters in bold", "**$1 ${x}**\n", "*$1 ${x}*\n"},
	{"dollar metacharacters in a link", "[$1 ${x}](https://e.test/$1)\n", "<https://e.test/$1|$1 ${x}>\n"},
	{"dollar metacharacters in a bullet", "- $1 ${x}\n", "• $1 ${x}\n"},
	{"dollar metacharacters in code", "`$1 ${x}`\n", "`$1 ${x}`\n"},
	{"backslash in text", "a\\1 and \\\\ and $\\1\n", "a\\1 and \\\\ and $\\1\n"},

	// The sentinel must not survive from the input.
	{"sentinel in the input is stripped", "a\x1fb\x1f0\x1fc\n", "ab0c\n"},
	{"a forged placeholder resolves to nothing", "\x1f0\x1f and `code`\n", "0 and `code`\n"},

	// A Markdown table converts to itself; sez warns about it separately.
	{"markdown table", "| a | b |\n|---|---|\n| 1 | 2 |\n", "| a | b |\n|---|---|\n| 1 | 2 |\n"},

	// The shapes an agent actually pastes.
	{"a whole message", "# Deploy report\n\n- **ok**: 3 services\n- **failed**: 1\n\nSee [the run](https://ci.test/run/**1**) or run `kubectl get pods`.\n\n```\nERROR: **not** bold in here\n```\n", "*Deploy report*\n\n• *ok*: 3 services\n• *failed*: 1\n\nSee <https://ci.test/run/**1**|the run> or run `kubectl get pods`.\n\n```\nERROR: **not** bold in here\n```\n"},
	{"crlf keeps the carriage return inside the heading", "# Title\r\n- item\r\n", "*Title\r*\n• item\r\n"},
	{"no trailing newline", "# Title", "*Title*"},
	{"unicode", "# Título — ✅\n- naïve **café**\n", "*Título — ✅*\n• naïve *café*\n"},
}

func TestConvertMatchesTheOracle(t *testing.T) {
	t.Parallel()
	for _, tc := range oracle {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Convert(tc.in); got != tc.want {
				t.Errorf("Convert(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// The one place the port does not reproduce the oracle byte for byte.
//
// The oracle substitutes placeholders in a single pass, so the code span the
// link rule captured out of [`code`](url) is never put back: its answer here is
// "<https://e.test|\x1f0\x1f>", a raw U+001F delivered to Slack. Nothing about
// the scheme wants that — the input is stripped of sentinels precisely so none
// can reach the output — so it is fixed rather than ported.
func TestNestedProtectionResolves(t *testing.T) {
	t.Parallel()
	const in = "[`code`](https://e.test)\n"
	const want = "<https://e.test|`code`>\n"
	if got := Convert(in); got != want {
		t.Errorf("Convert(%q) = %q, want %q", in, got, want)
	}
}

// The property the whole tokenise-and-restore scheme exists to guarantee: no
// sentinel this package introduced, and none the message brought with it, may
// reach Slack.
func TestNoSentinelSurvives(t *testing.T) {
	t.Parallel()
	for _, tc := range oracle {
		if strings.Contains(Convert(tc.in), sentinel) {
			t.Errorf("Convert(%q) leaked a sentinel: %q", tc.in, Convert(tc.in))
		}
	}
	nested := []string{
		"[`a`](https://e.test)",
		"[`a` and `b`](https://e.test)",
		"[[`a`](https://e.test)](https://f.test)",
		"`[x](https://e.test)` [`y`](https://f.test)",
	}
	for _, in := range nested {
		if got := Convert(in); strings.Contains(got, sentinel) {
			t.Errorf("Convert(%q) leaked a sentinel: %q", in, got)
		}
	}
}

// Slack renders a message inside a code fence verbatim, which is why the tool
// is usable for pasting logs at all. Every fence and span in the input has to
// come out the far side byte for byte.
func TestCodeIsPassedThroughUntouched(t *testing.T) {
	t.Parallel()
	for _, code := range []string{
		"```\n- **a** [b](https://c.test) # d __e__\n```",
		"`**a**`",
		"`# not a heading`",
		"`- not a bullet`",
		"`__init__`",
		"`${x} $1`",
	} {
		in := "before\n" + code + "\nafter **bold**\n"
		got := Convert(in)
		if !strings.Contains(got, code) {
			t.Errorf("Convert(%q) = %q, want it to contain %q unchanged", in, got, code)
		}
		if !strings.Contains(got, "*bold*") {
			t.Errorf("Convert(%q) = %q; the text outside the code was not converted", in, got)
		}
	}
}

// A Go replacement template expands $1 and ${name}. The replacements are built
// from the message, and a message is attacker-controlled, so a text naming a
// capture group must arrive as those characters and not as whatever the group
// happened to hold.
func TestReplacementMetacharactersAreNotInterpreted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"**$1**", "*$1*"},
		{"**${1}**", "*${1}*"},
		{"**${name}**", "*${name}*"},
		{"# $1 ${1} ${name} $$", "*$1 ${1} ${name} $$*"},
		{"- ${1}", "• ${1}"},
		{"[${1}](https://e.test/${1})", "<https://e.test/${1}|${1}>"},
		{"`${1}`", "`${1}`"},
		{"**$0**", "*$0*"},
	} {
		if got := Convert(tc.in); got != tc.want {
			t.Errorf("Convert(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The __bold__ rule was removed on purpose: it turned __init__ into _init_ in
// every pasted Python traceback. Reintroducing it is the regression this holds
// the line against.
func TestUnderscoreBoldIsNeverConverted(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"__init__", "__main__", "def __repr__(self):", "__bold__",
		"a __b__ c", "MAX__VALUE", "__all__ = [\"x\"]",
	} {
		if got := Convert(in); got != in {
			t.Errorf("Convert(%q) = %q, want it unchanged", in, got)
		}
	}
}

// A URL is not prose. Emphasis inside it would produce a link that no longer
// points anywhere, which is why links are converted before the emphasis rules
// and protected from them until the end.
func TestLinkDestinationIsNotRewritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"[x](https://e.test/**seg**)", "<https://e.test/**seg**|x>"},
		{"[x](https://e.test/__seg__)", "<https://e.test/__seg__|x>"},
		{"[x](https://e.test/-item)", "<https://e.test/-item|x>"},
		{"[x](https://e.test/a**b**c)", "<https://e.test/a**b**c|x>"},
		{"see [x](https://e.test/**a**) and **bold**", "see <https://e.test/**a**|x> and *bold*"},
	} {
		if got := Convert(tc.in); got != tc.want {
			t.Errorf("Convert(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Convert is a pure function of its argument: nothing carries between calls,
// so a fragment protected by one message can never surface in the next.
func TestConvertKeepsNoStateBetweenCalls(t *testing.T) {
	t.Parallel()
	first := Convert("`secret` [a](https://a.test)")
	for range 3 {
		Convert("`other` [b](https://b.test)")
	}
	if got := Convert("`secret` [a](https://a.test)"); got != first {
		t.Errorf("Convert = %q on a repeat call, want %q", got, first)
	}
	// A placeholder cannot be forged: the sentinels are stripped from the input
	// before any fragment is protected, so what is left is the bare digits.
	if got := Convert("\x1f0\x1f"); got != "0" {
		t.Errorf("Convert(%q) = %q, want %q", "\x1f0\x1f", got, "0")
	}
}

func FuzzConvertLeaksNoSentinel(f *testing.F) {
	for _, tc := range oracle {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := Convert(in)
		if strings.Contains(got, sentinel) {
			t.Errorf("Convert(%q) = %q leaked a sentinel", in, got)
		}
	})
}
