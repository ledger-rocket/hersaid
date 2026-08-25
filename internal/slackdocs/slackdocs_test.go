package slackdocs

import (
	"strings"
	"testing"
	"time"
)

func md(t *testing.T, in string) string {
	t.Helper()
	got, err := Markdown([]byte(in))
	if err != nil {
		t.Fatalf("Markdown() error = %v", err)
	}
	return strings.TrimRight(got, "\n")
}

func TestHeadingsCarryTheirLevel(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`<h1>Plan</h1>`, "# Plan"},
		{`<h2>Scope</h2>`, "## Scope"},
		{`<h3>Detail</h3>`, "### Detail"},
	} {
		if got := md(t, c.in); got != c.want {
			t.Errorf("Markdown(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInlineMarks(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"bold", `<p><b>Status:</b> ready</p>`, "**Status:** ready"},
		{"italic", `<p><i>maybe</i></p>`, "_maybe_"},
		{"strike", `<p><del>gone</del></p>`, "~~gone~~"},
		{"code", `<p>run <code>make</code></p>`, "run `make`"},
		{"link", `<p><a href="https://x.test">docs</a></p>`, "[docs](https://x.test)"},
		{"bare link", `<p><a href="https://x.test">https://x.test</a></p>`, "<https://x.test>"},
		{"empty mark", `<p><b></b>text</p>`, "text"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := md(t, c.in); got != c.want {
				t.Errorf("= %q, want %q", got, c.want)
			}
		})
	}
}

func TestLists(t *testing.T) {
	got := md(t, `<ul><li>one</li><li>two</li></ul>`)
	if got != "- one\n- two" {
		t.Errorf("bulleted = %q", got)
	}
	got = md(t, `<ol><li>first</li><li>second</li></ol>`)
	if got != "1. first\n2. second" {
		t.Errorf("numbered = %q", got)
	}
	got = md(t, `<ul><li>outer<ul><li>inner</li></ul></li></ul>`)
	if got != "- outer\n  - inner" {
		t.Errorf("nested = %q", got)
	}
}

func TestABreakIsALineNotASpace(t *testing.T) {
	if got := md(t, `<p>one<br>two</p>`); got != "one\ntwo" {
		t.Errorf("= %q, want two lines", got)
	}
}

func TestBlocksAreSeparatedByABlankLine(t *testing.T) {
	got := md(t, `<h1>Title</h1><p>body</p><p>more</p>`)
	if got != "# Title\n\nbody\n\nmore" {
		t.Errorf("= %q", got)
	}
}

func TestAnEmptyElementLeavesNoGap(t *testing.T) {
	got := md(t, `<p>one</p><p></p><div></div><p>two</p>`)
	if got != "one\n\ntwo" {
		t.Errorf("= %q, want no stray blank block", got)
	}
}

func TestTablesGetAHeaderRow(t *testing.T) {
	got := md(t, `<table><tr><td>a</td><td>b</td></tr><tr><td>1</td><td>2</td></tr></table>`)
	want := "| a | b |\n| --- | --- |\n| 1 | 2 |"
	if got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

func TestAPipeInACellCannotBreakTheTable(t *testing.T) {
	got := md(t, `<table><tr><td>a|b</td></tr></table>`)
	if !strings.Contains(got, `a\|b`) {
		t.Errorf("= %q, want the pipe escaped", got)
	}
}

func TestPreformattedTextKeepsItsLinesAndItsMarkup(t *testing.T) {
	got := md(t, "<pre>line one\nline *two*</pre>")
	if got != "```\nline one\nline *two*\n```" {
		t.Errorf("= %q", got)
	}
}

// A canvas is written by anyone who can edit it, so its text is as untrusted as
// a Slack filename: an escape sequence in a heading reaches the terminal that
// prints the Markdown.
func TestUntrustedTextCannotDriveTheTerminal(t *testing.T) {
	in := "<h1>Plan" + string(rune(0x1b)) + "[31m" + string(rune(0x202e)) + "</h1>"
	got := md(t, in)
	for _, banned := range []rune{0x1b, 0x202e} {
		if strings.ContainsRune(got, banned) {
			t.Errorf("output carries %U", banned)
		}
	}
	if got != "# Plan[31m" {
		t.Errorf("= %q, want the printable text kept", got)
	}
}

// Scripts and styles are markup a reader never wants, and the one place markup
// could carry something executable if the output were ever rendered as HTML.
func TestScriptsAndStylesAreDropped(t *testing.T) {
	got := md(t, `<p>before</p><script>alert(1)</script><style>p{}</style><p>after</p>`)
	if strings.Contains(got, "alert") || strings.Contains(got, "p{}") {
		t.Errorf("= %q, want the script and style gone", got)
	}
	if got != "before\n\nafter" {
		t.Errorf("= %q", got)
	}
}

// x/net/html accepts anything, so bad markup degrades to the text it still
// holds rather than to an error a reader has to act on.
func TestMangledMarkupStillYieldsItsText(t *testing.T) {
	got, err := Markdown([]byte(`<h1>Title<p>unclosed <b>everything`))
	if err != nil {
		t.Fatalf("error = %v, want the text that survived", err)
	}
	for _, want := range []string{"Title", "unclosed", "everything"} {
		if !strings.Contains(got, want) {
			t.Errorf("output = %q, want it to contain %q", got, want)
		}
	}
}

// The wrapper divs Slack puts around everything must not each become a block.
func TestNestedWrappersDoNotMultiplyBlocks(t *testing.T) {
	got := md(t, `<div class="quip-canvas-content"><div><h1>Plan</h1></div><div><p>body</p></div></div>`)
	if got != "# Plan\n\nbody" {
		t.Errorf("= %q", got)
	}
}

// The parser refuses more than 512 open elements, so a document nested past
// that is an error — and the error must reach the caller rather than being
// answered with the bytes. Printing unparsed markup to a terminal is what this
// package exists to prevent, and the fallback that did it was reachable by
// writing enough nested tags.
func TestMarkupTooDeepToParseIsAnErrorNotRawBytes(t *testing.T) {
	hostile := strings.Repeat("<div>", 600) + "text" + string(rune(0x1b)) + "[31m"
	got, err := Markdown([]byte(hostile))
	if err == nil {
		t.Fatalf("Markdown() = %q, nil error; want the parser's refusal", got)
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Fatal("the raw body came back alongside the error")
	}
	if got != "" {
		t.Fatalf("Markdown() = %q, want nothing", got)
	}
}

// Within what the parser accepts, nesting is rendered rather than truncated.
// A bound of our own below the parser's would silently drop content from a
// document Slack was perfectly willing to serve.
func TestNestingTheParserAcceptsIsRendered(t *testing.T) {
	const depth = 120
	in := strings.Repeat("<ul><li>x", depth) + strings.Repeat("</li></ul>", depth)
	got := md(t, in)
	if n := strings.Count(got, "- x"); n != depth {
		t.Fatalf("rendered %d items, want all %d", n, depth)
	}
}

// One wide row among many narrow ones would be paid for by every row.
func TestASparseTableDoesNotExpandToItsBoundingBox(t *testing.T) {
	var b strings.Builder
	b.WriteString("<table>")
	b.WriteString("<tr>")
	for i := 0; i < 5000; i++ {
		b.WriteString("<td>x</td>")
	}
	b.WriteString("</tr>")
	for i := 0; i < 5000; i++ {
		b.WriteString("<tr><td>y</td></tr>")
	}
	b.WriteString("</table>")

	done := make(chan int, 1)
	go func() {
		got, _ := Markdown([]byte(b.String()))
		done <- len(got)
	}()
	select {
	case n := <-done:
		if n > 10*b.Len() {
			t.Fatalf("output is %d bytes for %d of input; the padding is unbounded", n, b.Len())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("rendering a sparse table did not finish")
	}
}

// A link's own text must not be able to close the link and open another.
func TestAnchorTextCannotForgeASecondLink(t *testing.T) {
	got := md(t, `<p><a href="https://evil.example">click](https://safe.example) [APPROVED</a></p>`)
	// An escaped bracket is text, not syntax, so only the unescaped pairs count.
	if real := strings.Count(got, "](") - strings.Count(got, `\](`); real != 1 {
		t.Fatalf("= %q, want exactly one real link, found %d", got, real)
	}
	if !strings.Contains(got, `\]`) || !strings.Contains(got, `\[`) {
		t.Fatalf("= %q, want the brackets in the label escaped", got)
	}
	if !strings.Contains(got, "https://evil.example") {
		t.Fatalf("= %q, want the real destination", got)
	}
}

// A parenthesis in the target would otherwise end the link early and spill the
// rest into the text around it.
func TestAParenthesisInAHrefCannotTruncateIt(t *testing.T) {
	got := md(t, `<p><a href="https://x.test/a)b">link</a></p>`)
	if !strings.Contains(got, "%29") {
		t.Fatalf("= %q, want the parenthesis encoded", got)
	}
	if strings.Contains(got, "a)b") {
		t.Fatalf("= %q, want no raw parenthesis in the destination", got)
	}
}

// The output may be rendered somewhere that honours a link's scheme.
func TestOnlyOrdinarySchemesSurviveAsDestinations(t *testing.T) {
	for _, href := range []string{"javascript:alert(1)", "data:text/html;base64,x", "vbscript:x"} {
		got := md(t, `<p><a href="`+href+`">click</a></p>`)
		if strings.Contains(got, "](") {
			t.Errorf("href %q produced a link: %q", href, got)
		}
		if got != "click" {
			t.Errorf("href %q = %q, want the text alone", href, got)
		}
	}
	if got := md(t, `<p><a href="mailto:a@b.test">mail</a></p>`); !strings.Contains(got, "mailto:") {
		t.Errorf("mailto was dropped: %q", got)
	}
	// A bare path has no scheme and must survive.
	if got := md(t, `<p><a href="/docs/T1/F1">doc</a></p>`); !strings.Contains(got, "/docs/T1/F1") {
		t.Errorf("relative link was dropped: %q", got)
	}
}

// A backtick inside a code span would close it early and turn the rest into
// prose; a fence run inside a block would do the same to the block.
func TestCodeDelimitersOutgrowTheirContent(t *testing.T) {
	got := md(t, "<p><code>a`b</code></p>")
	if !strings.Contains(got, "``") {
		t.Fatalf("= %q, want a longer delimiter", got)
	}
	got = md(t, "<pre>before\n```\nafter</pre>")
	if !strings.HasPrefix(got, "````") {
		t.Fatalf("= %q, want a fence longer than the content's", got)
	}
}

// One backslash was enough to reopen the forgery the bracket escaping closed.
//
// The assertion is on the exact output, not on substrings, because the two
// outcomes differ by one character and the wrong one contains the right one:
// escaping only the bracket yields `click\\]`, which CommonMark reads as an
// escaped backslash and then a LIVE bracket — so the label ends there and the
// text after it becomes a destination. Escaping the backslash first yields
// `click\\\]`, where the bracket is escaped and the label runs to the end.
// A substring check cannot tell those apart, since the second contains the
// first.
func TestABackslashCannotUnescapeALabel(t *testing.T) {
	got := md(t, `<p><a href="https://real.example">click\](https://attacker.example) [ok</a></p>`)
	want := `[click\\\](https://attacker.example) \[ok](https://real.example)`
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestABackslashCannotUnescapeATableCell(t *testing.T) {
	got := md(t, `<table><tr><td>a\|b</td><td>c</td></tr></table>`)
	if !strings.Contains(got, `a\\`) {
		t.Fatalf("= %q, want the backslash escaped so the pipe stays inert", got)
	}
}

func TestABackslashInADestinationCannotEscapeTheClosingParen(t *testing.T) {
	got := md(t, `<p><a href="https://x.test/a\">link</a> after</p>`)
	if !strings.Contains(got, "%5C") {
		t.Fatalf("= %q, want the backslash encoded", got)
	}
	if strings.Contains(got, `\)`) {
		t.Fatalf("= %q, want no escaped closing paren", got)
	}
}

// Slack stores canvas text entity-encoded, and the parser hands it back
// decoded — so what somebody typed as a tag arrives as markup.
func TestTextThatLooksLikeMarkupStaysText(t *testing.T) {
	got := md(t, `<p>&lt;img src=x onerror=alert(1)&gt;</p>`)
	// An escaped bracket is text; only an unescaped one would be markup, so the
	// check has to discount the escaped occurrences rather than search for the
	// substring, which the escaped form also contains.
	if raw := strings.Count(got, "<img") - strings.Count(got, `\<img`); raw != 0 {
		t.Fatalf("= %q, want no unescaped tag (found %d)", got, raw)
	}
	if got != `\<img src=x onerror=alert(1)\>` {
		t.Fatalf("= %q, want the text preserved and inert", got)
	}
}

// A newline inside a text node is how HTML separates words. Stripping it as a
// control character fuses them.
func TestWhitespaceInsideATextNodeSeparatesWords(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"<p>one\ntwo</p>", "one two"},
		{"<p>one\ttwo</p>", "one two"},
		{"<p>one\r\ntwo</p>", "one two"},
		{"<p>one   \n   two</p>", "one two"},
	} {
		if got := md(t, c.in); got != c.want {
			t.Errorf("Markdown(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A document that is only text is a document, not an empty one.
func TestBareTextIsContent(t *testing.T) {
	if got := md(t, "hello"); got != "hello" {
		t.Fatalf("= %q, want the text", got)
	}
	got := md(t, "<div>before<p>body</p>after</div>")
	for _, want := range []string{"before", "body", "after"} {
		if !strings.Contains(got, want) {
			t.Errorf("= %q, want it to contain %q", got, want)
		}
	}
}

// A table longer than the padding bound is rendered ragged, not truncated: one
// that simply stopped would look exactly like a complete one.
func TestALongTableKeepsEveryRow(t *testing.T) {
	const rows = 25000
	var b strings.Builder
	b.WriteString("<table>")
	for i := 0; i < rows; i++ {
		b.WriteString("<tr><td>r</td></tr>")
	}
	b.WriteString("</table>")

	got := md(t, b.String())
	if n := strings.Count(got, "| r |"); n != rows {
		t.Fatalf("rendered %d rows, want all %d", n, rows)
	}
}
