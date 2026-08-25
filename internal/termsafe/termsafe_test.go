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
	spoofed := "annex" + string(rlo) + "fdp.scr"
	if got := Text(spoofed); got != "annexfdp.scr" {
		t.Fatalf("Text(%q) = %q, want the name to read as the .scr it is", spoofed, got)
	}
}

// The predicate and the renderer must agree, or the filesystem rule and the
// display rule disagree about the same name.
func TestIsHostileAgreesWithText(t *testing.T) {
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
