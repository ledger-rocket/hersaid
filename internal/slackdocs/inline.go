package slackdocs

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/ledger-rocket/hersaid/internal/termsafe"
)

// inline renders a node's contents as one line of Markdown, marks included.
func inline(n *html.Node) string { return strings.TrimSpace(render(n, true, 0)) }

// inlineExceptLists is inline for a list item: the item's own text, without the
// nested list that the item renderer handles separately.
func inlineExceptLists(n *html.Node) string { return strings.TrimSpace(render(n, false, 0)) }

func render(n *html.Node, lists bool, depth int) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode:
			b.WriteString(clean(c.Data))
		case c.Type != html.ElementNode:
			continue
		default:
			b.WriteString(element(c, lists, depth+1))
		}
	}
	return collapse(b.String())
}

// marks are the inline elements rendered as a Markdown mark around their text.
var marks = map[atom.Atom]string{
	atom.B: "**", atom.Strong: "**",
	atom.I: "_", atom.Em: "_",
	atom.Del: "~~", atom.S: "~~",
}

func element(c *html.Node, lists bool, depth int) string {
	if mark, ok := marks[c.DataAtom]; ok {
		return wrap(mark, render(c, lists, depth))
	}
	switch c.DataAtom {
	case atom.Br:
		return "\n"
	case atom.Code:
		return wrapCode(render(c, lists, depth))
	case atom.A:
		return link(c, lists, depth)
	case atom.Img:
		return image(c)
	case atom.Ul, atom.Ol:
		if !lists {
			return ""
		}
		return render(c, lists, depth)
	case atom.Script, atom.Style:
		return ""
	}
	return render(c, lists, depth)
}

// link renders an anchor: its text alone when it has no usable destination,
// the bare destination when the text is empty or the same, else a link.
func link(c *html.Node, lists bool, depth int) string {
	text := strings.TrimSpace(render(c, lists, depth))
	href := destination(attr(c, "href"))
	switch {
	case href == "":
		return text
	case text == "", text == href:
		return "<" + href + ">"
	}
	return "[" + escapeLabel(text) + "](" + href + ")"
}

// image renders an img with its alt text, when it has one.
func image(c *html.Node) string {
	src := destination(attr(c, "src"))
	if alt := attr(c, "alt"); alt != "" {
		return "![" + escapeLabel(termsafe.Text(alt)) + "](" + src + ")"
	}
	return "![](" + src + ")"
}

// wrap applies a mark, and declines to apply one to nothing: "**" around an
// empty string is two asterisks a reader has to wonder about.
func wrap(mark, s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	// Leading and trailing spaces move outside the mark, since Markdown will not
	// emphasise a string that begins with one.
	lead := s[:len(s)-len(strings.TrimLeft(s, " "))]
	tail := s[len(strings.TrimRight(s, " ")):]
	return lead + mark + strings.TrimSpace(s) + mark + tail
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// clean is where untrusted text becomes printable, and then safe to put in a
// Markdown document. The canvas is written by whoever can edit it, so a heading
// may carry an escape sequence or a bidi override exactly as a filename may.
//
// Newlines and tabs become spaces rather than being stripped. They are control
// characters, but inside a text node they are how HTML separates words, and
// deleting them fuses the words either side — collapse below turns the run of
// them into one space.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t', '\r':
			return ' '
		}
		return r
	}, s)
	s = termsafe.Text(strings.ReplaceAll(s, "\u00a0", " "))
	return angleEscaper.Replace(s)
}

// angleEscaper keeps decoded markup from becoming markup again.
//
// Slack stores canvas text entity-encoded and the parser hands it back decoded,
// so text somebody typed as an HTML tag arrives here as angle brackets. A
// Markdown reader that honours raw HTML would treat it as an element rather
// than as the words that were written.
var angleEscaper = strings.NewReplacer("<", "\\<", ">", "\\>")

// collapse squeezes the runs of whitespace that indented markup leaves behind,
// while keeping the line breaks a <br> introduced.
func collapse(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	return strings.Join(lines, "\n")
}

// hasBlockChild reports whether n contains something that is a block in its own
// right, which is what decides whether a div emits or merely recurses.
func hasBlockChild(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch c.DataAtom {
		case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
			atom.P, atom.Ul, atom.Ol, atom.Pre, atom.Blockquote, atom.Table, atom.Hr:
			return true
		case atom.Div:
			return true
		}
	}
	return false
}

// textOf is the raw text under n, for a <pre> where markup must not be applied.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(termsafe.TextKeepingNewlines(x.Data))
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// escapeLabel neutralises the brackets that would let a link's own text close
// the link and open another.
//
// A canvas is written by whoever can edit it, and the output is meant to be
// read by a person or handed to an agent. Anchor text of `x](https://safe) [ok`
// with an href somewhere else renders as two links, one of which lies about
// where it goes — structure the document never contained.
func escapeLabel(s string) string {
	return labelEscaper.Replace(s)
}

// The backslash goes first, and that ordering is the whole of it. Escaping only
// the brackets leaves a label ending in a backslash as an escaped backslash
// followed by a live bracket, so the label closes there and the text after it
// names a destination of its own. One backslash was enough to reopen the
// forgery this escaping exists to close.
var labelEscaper = strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]")

// destination renders a link target so that it cannot end early or carry
// anything that is not a URL.
//
// A closing parenthesis inside an href would otherwise truncate the target and
// spill the rest into the surrounding text. The scheme is checked because the
// output may be rendered somewhere that would honour a javascript: or data:
// link, and a canvas is not a place to accept one from.
func destination(href string) string {
	h := termsafe.Text(strings.TrimSpace(href))
	if h == "" {
		return ""
	}
	if scheme, _, ok := strings.Cut(h, ":"); ok && !strings.ContainsAny(scheme, "/?#") {
		switch strings.ToLower(scheme) {
		case "http", "https", "mailto", "slack":
		default:
			return ""
		}
	}
	// The backslash is encoded for the same reason the parentheses are: a
	// destination ending in one escapes the paren that should close it, and the
	// text after the link is swallowed into the target.
	return strings.NewReplacer(
		"\\", "%5C", "(", "%28", ")", "%29", " ", "%20", "<", "%3C", ">", "%3E",
	).Replace(h)
}

// wrapCode delimits a code span with more backticks than it contains, since a
// backtick in the content would otherwise close the span early and let the rest
// of it become prose.
func wrapCode(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	mark := "`"
	for strings.Contains(s, mark) {
		mark += "`"
	}
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		pad = " "
	}
	return mark + pad + s + pad + mark
}
