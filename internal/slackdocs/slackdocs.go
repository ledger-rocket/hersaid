// Package slackdocs renders a Slack canvas as Markdown.
//
// A canvas is typed application/vnd.slack-docs and served as HTML: headings,
// paragraphs, lists, tables and inline marks, with Slack's own ids and classes
// on nearly every element. None of that survives usefully in a terminal, and
// none of it is what a reader wants — so the document is walked and re-emitted
// as Markdown, which reads plainly and feeds an agent without a parser.
//
// The HTML comes from a Slack workspace, so it is untrusted in the ways any
// Slack text is: a heading can contain a control character, and a link can
// point anywhere. Text is passed through termsafe on the way out, and no markup
// is ever executed — this only reads a tree.
package slackdocs

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// cellEscaper neutralises the pipe that would otherwise start a new column.
// The backslash goes first, as it does for a link label: escaping only the pipe
// leaves `a\|b` as an escaped backslash and a live pipe, which shifts every
// column after it.
var cellEscaper = strings.NewReplacer(`\`, `\\`, "|", `\|`)

// maxCells is where padding stops being worth it.
//
// Nesting needs no bound of its own: x/net/html refuses a document with more
// than 512 open elements, so the tree is shallow whatever the markup says, and
// the walk below cannot run away. A table is the one shape that escapes that,
// because rows are siblings rather than nested — so a document well inside the
// size limit can hold one wide row among thousands of narrow ones, and padding
// every row out to the widest is the product of the two.
//
// Past this the table is emitted ragged instead. No row is dropped: storing and
// rendering them is linear in an input that is already bounded, and a table
// that simply stopped early would look exactly like a complete one.
const maxCells = 20000

// Markdown renders a canvas body as Markdown. It never fails on bad markup:
// x/net/html parses anything, so a mangled document degrades to the text it
// still contains rather than to an error.
// A parse failure is returned rather than answered with the bytes. It is
// reachable — x/net/html refuses more than 512 open elements — and a caller
// that fell back to the raw body would print markup nobody had inspected to a
// terminal, which is the one thing this package exists to prevent.
func Markdown(body []byte) (string, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("parsing the canvas: %w", err)
	}
	var w writer
	w.block(doc, 0)
	return w.done(), nil
}

// writer accumulates blocks. Blocks are joined by a blank line at the end
// rather than as they are written, so that an empty element cannot leave a
// stray gap and nothing has to track whether a separator is already pending.
type writer struct {
	blocks []string
	list   []listState
}

type listState struct {
	ordered bool
	n       int
}

func (w *writer) done() string {
	out := make([]string, 0, len(w.blocks))
	for _, b := range w.blocks {
		if strings.TrimSpace(b) != "" {
			out = append(out, b)
		}
	}
	return strings.Join(out, "\n\n") + "\n"
}

func (w *writer) emit(s string) { w.blocks = append(w.blocks, s) }

// block walks n looking for things that start a new block, recursing through
// everything else.
func (w *writer) block(n *html.Node, depth int) {
	// Text sitting directly in a block is content. Without this a document
	// that is only text renders as nothing, and the words either side of a
	// nested block are lost — which is not the graceful degradation this
	// package promises for markup it does not recognise.
	if n.Type == html.TextNode {
		if s := strings.TrimSpace(clean(n.Data)); s != "" {
			w.emit(s)
		}
		return
	}
	if n.Type == html.ElementNode && w.element(n, depth) {
		return
	}
	w.children(n, depth+1)
}

// element renders the block-level elements block knows, and reports whether n
// was one of them. Anything else is walked through by block.
func (w *writer) element(n *html.Node, depth int) bool {
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		w.emit(strings.Repeat("#", level) + " " + inline(n))
		return true
	case atom.P, atom.Div:
		// A div is a block in canvas markup, but it is also the wrapper
		// around everything, so it only emits when it holds text directly
		// rather than other blocks.
		if hasBlockChild(n) {
			w.children(n, depth+1)
			return true
		}
		if s := inline(n); strings.TrimSpace(s) != "" {
			w.emit(s)
		}
		return true
	case atom.Ul, atom.Ol:
		w.list = append(w.list, listState{ordered: n.DataAtom == atom.Ol})
		var items []string
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.DataAtom {
			case atom.Li:
				items = append(items, w.item(c, depth+1))
			case atom.Ul, atom.Ol:
				// A list directly inside a list, which the parser will hand
				// back for markup that omitted the item around it. Rendered
				// rather than dropped.
				w.block(c, depth+1)
			}
		}
		w.list = w.list[:len(w.list)-1]
		if len(items) > 0 {
			w.emit(strings.Join(items, "\n"))
		}
		return true
	case atom.Pre:
		body := strings.TrimRight(textOf(n), "\n")
		fence := "```"
		for strings.Contains(body, fence) {
			fence += "`"
		}
		w.emit(fence + "\n" + body + "\n" + fence)
		return true
	case atom.Blockquote:
		for line := range strings.SplitSeq(inline(n), "\n") {
			w.emit("> " + line)
		}
		return true
	case atom.Hr:
		w.emit("---")
		return true
	case atom.Table:
		if t := w.table(n); t != "" {
			w.emit(t)
		}
		return true
	case atom.Script, atom.Style, atom.Head:
		return true
	}
	return false
}

func (w *writer) children(n *html.Node, depth int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.block(c, depth)
	}
}

// item renders one list entry, indented for its depth. A nested list inside the
// item is walked by the same code, so depth is the length of the stack.
func (w *writer) item(n *html.Node, depth int) string {
	indent := max(len(w.list)-1, 0)
	marker := "- "
	if len(w.list) > 0 && w.list[len(w.list)-1].ordered {
		w.list[len(w.list)-1].n++
		marker = fmt.Sprintf("%d. ", w.list[len(w.list)-1].n)
	}
	line := strings.Repeat("  ", indent) + marker + strings.TrimSpace(inlineExceptLists(n))

	var nested []string
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || (c.DataAtom != atom.Ul && c.DataAtom != atom.Ol) {
			continue
		}
		w.list = append(w.list, listState{ordered: c.DataAtom == atom.Ol})
		for gc := c.FirstChild; gc != nil; gc = gc.NextSibling {
			if gc.Type == html.ElementNode && gc.DataAtom == atom.Li {
				nested = append(nested, w.item(gc, depth+1))
			}
		}
		w.list = w.list[:len(w.list)-1]
	}
	if len(nested) > 0 {
		line += "\n" + strings.Join(nested, "\n")
	}
	return line
}

// table renders a grid. A canvas table has no header marker of its own, so the
// first row is treated as one: Markdown has no way to write a table without it,
// and a header made of the first row reads better than a column of empties.
func (w *writer) table(n *html.Node) string {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode && x.DataAtom == atom.Tr {
			var cells []string
			for c := x.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.DataAtom == atom.Td || c.DataAtom == atom.Th) {
					cells = append(cells, cellEscaper.Replace(strings.TrimSpace(inline(c))))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	if len(rows) == 0 {
		return ""
	}
	width := 0
	for _, r := range rows {
		if len(r) > width {
			width = len(r)
		}
	}
	// One wide row among many narrow ones would otherwise be paid for by every
	// row: padding is the width times the count, which a small document can
	// make enormous. Past the bound the rows are emitted as they are. Markdown
	// readers accept a ragged table, and the cost is then what the document
	// itself cost.
	pad := width*len(rows) <= maxCells

	var b strings.Builder
	for i, r := range rows {
		if pad {
			for len(r) < width {
				r = append(r, "")
			}
		}
		b.WriteString("| " + strings.Join(r, " | ") + " |\n")
		if i == 0 {
			b.WriteString("|" + strings.Repeat(" --- |", len(r)) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
