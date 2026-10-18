package termsafe

import "testing"

// Named by code point and built from runes, so that no test fixture in this
// file is itself a string that reorders the file it lives in.
const (
	esc      = rune(0x1b)   // ESC, the ASCII sequence introducer
	csi      = rune(0x9b)   // C1 CSI, which terminals act on as they do ESC-[
	rlo      = rune(0x202e) // right-to-left override
	isolate  = rune(0x2066) // left-to-right isolate
	ltrMark  = rune(0x200e) // left-to-right mark
	cedilla  = rune(0x00e7) // ç
	nihon    = rune(0x65e5) // the first character of 日本語
	barChart = rune(0x1f4ca)
)

func TestTextRemovesWhatWouldDriveATerminal(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, in, want string
	}{
		{"plain", "trace.pdf", "trace.pdf"},
		{"escape", "in" + string(esc) + "[31mnocent.pdf", "in[31mnocent.pdf"},
		{"c1 introducer", "a" + string(csi) + "31mb.pdf", "a31mb.pdf"},
		{"newline", "one\ntwo", "onetwo"},
		{"carriage return", "one\rtwo", "onetwo"},
		{"tab", "one\ttwo", "onetwo"},
		{"nul", "one\x00two", "onetwo"},
		{"bidi override", "annex" + string(rlo) + "fdp.scr", "annexfdp.scr"},
		{"bidi isolate", "annex" + string(isolate) + "fdp.scr", "annexfdp.scr"},
		{"left to right mark", "a" + string(ltrMark) + "b", "ab"},
		{"accents kept", "re" + string(cedilla) + "u.pdf", "re" + string(cedilla) + "u.pdf"},
		{"cjk kept", string(nihon) + ".pdf", string(nihon) + ".pdf"},
		{"emoji kept", "chart-" + string(barChart) + ".png", "chart-" + string(barChart) + ".png"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := Text(c.in); got != c.want {
				t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The bidi case is worth stating outright: the point is not that a rune
// vanished but that the name now reads as what it is. Rendered raw, the input
// below shows as "annexrcs.pdf" in a terminal that honours the override.
func TestABidiOverrideCannotSpoofAnExtension(t *testing.T) {
	t.Parallel()
	spoofed := "annex" + string(rlo) + "fdp.scr"
	if got := Text(spoofed); got != "annexfdp.scr" {
		t.Fatalf("Text(%q) = %q, want the name to read as the .scr it is", spoofed, got)
	}
}

// The predicate and the renderer must agree, or the filesystem rule and the
// display rule disagree about the same name.
func TestIsHostileAgreesWithText(t *testing.T) {
	t.Parallel()
	for _, r := range []rune{0x00, 0x09, 0x0a, 0x0d, esc, 0x7f, csi, rlo, isolate, ltrMark} {
		if !IsHostile(r) {
			t.Errorf("IsHostile(%U) = false, want true", r)
		}
		if Text(string(r)) != "" {
			t.Errorf("Text(%U) kept a rune IsHostile rejects", r)
		}
	}
	for _, r := range []rune{'a', '/', ' ', '.', cedilla, nihon, barChart} {
		if IsHostile(r) {
			t.Errorf("IsHostile(%U) = true, want false", r)
		}
		if Text(string(r)) == "" {
			t.Errorf("Text(%U) dropped a rune IsHostile accepts", r)
		}
	}
}

// Message text keeps the two joiners that shape what is written: the
// zero-width joiner builds one emoji out of several, and the zero-width
// non-joiner keeps Persian letters apart. Neither moves the cursor or reorders
// a line. The bidi controls and the invisible tag characters still go.
func TestTextKeepingNewlinesKeepsTheJoiners(t *testing.T) {
	t.Parallel()
	const (
		zwj  = rune(0x200d)
		zwnj = rune(0x200c)
		man  = rune(0x1f468)
		girl = rune(0x1f467)
		tagA = rune(0xe0061) // TAG LATIN SMALL LETTER A, invisible
	)
	family := string(man) + string(zwj) + string(girl)
	persian := "mi" + string(zwnj) + "khaham"
	for _, c := range []struct {
		name, in, want string
	}{
		{"zero-width joiner", family + " deployed", family + " deployed"},
		{"zero-width non-joiner", persian, persian},
		{"bidi override still goes", "a" + string(rlo) + "b", "ab"},
		{"tag characters still go", "ok" + string(tagA) + string(tagA), "ok"},
		{"line breaks kept", "one\ntwo", "one\ntwo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := TextKeepingNewlines(c.in); got != c.want {
				t.Errorf("TextKeepingNewlines(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A name keeps the joiners as message text does, and loses a line break, which
// message text keeps.
func TestLabelKeepsTheJoinersAndDropsLineBreaks(t *testing.T) {
	t.Parallel()
	coder := "Sam " + string(rune(0x1f469)) + string(rune(0x200d)) + string(rune(0x1f4bb))
	for _, c := range []struct {
		name, in, want string
	}{
		{"zero-width joiner kept", coder, coder},
		{"line break removed", "ann\nboss", "annboss"},
		{"bidi override removed", "a" + string(rlo) + "b", "ab"},
		{"escape removed", "a" + string(esc) + "[2Jb", "a[2Jb"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := Label(c.in); got != c.want {
				t.Errorf("Label(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
