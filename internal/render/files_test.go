package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slackread"
)

// A message whose content is a file has text that is often empty, so the
// transcript has to name the file or the message reads as nothing at all.
func TestTranscriptNamesTheFilesOnAMessage(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{
		TS: "1785708834.862009", User: "U1", Text: "here's the trace",
		Files: []slackread.File{
			{ID: "F1", Name: "trace.pdf", Size: 1258291},
			{ID: "F2", Name: "out.log", Size: 14336},
		},
	}}
	if err := WriteTranscript(&b, msgs, Names{"U1": "jane"}, Options{Location: utc}); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{"2 files", "trace.pdf (1.2 MB)", "out.log (14.0 KB)"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript = %q, want it to contain %q", got, want)
		}
	}
}

func TestOneFileIsSingular(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{
		TS: "1785708834.862009", User: "U1",
		Files: []slackread.File{{ID: "F1", Name: "trace.pdf", Size: 10}},
	}}
	if err := WriteTranscript(&b, msgs, Names{}, Options{Location: utc}); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.Contains(got, "1 file:") {
		t.Fatalf("transcript = %q, want a singular file count", got)
	}
}

// A filename is chosen by whoever uploaded it. One carrying a newline would
// break the transcript into lines that look like separate messages, and one
// carrying an escape sequence would drive the terminal it is printed to.
func TestAFileNameCannotBreakTheTranscript(t *testing.T) {
	got := FileLabel(slackread.File{ID: "F1", Name: "in\nnocent\x1b[31m.pdf\r"})
	if strings.ContainsAny(got, "\n\r\x1b") {
		t.Fatalf("FileLabel = %q, want the control characters gone", got)
	}
	if got != "innocent[31m.pdf" {
		t.Fatalf("FileLabel = %q, want the printable text kept", got)
	}
}

// A bidi override is not a control character by the ASCII test, and it does
// more damage than one: it reverses everything after it, so a .scr renders as a
// .pdf in a transcript nobody would think to distrust. U+009B is the C1
// control-sequence introducer, which terminals that honour it act on exactly as
// they do on ESC-[.
func TestAFileNameCannotSpoofItsExtensionOrDriveTheTerminal(t *testing.T) {
	got := FileLabel(slackread.File{ID: "F1", Name: "annex\u202Efdp.scr"})
	if strings.ContainsRune(got, 0x202e) {
		t.Fatalf("FileLabel = %q, want the bidi override gone", got)
	}
	if got != "annexfdp.scr" {
		t.Fatalf("FileLabel = %q, want the name to read as what it is", got)
	}
	if got := FileLabel(slackread.File{ID: "F1", Name: "a\u009b31mb.pdf"}); strings.ContainsRune(got, 0x9b) {
		t.Fatalf("FileLabel = %q, want the C1 introducer gone", got)
	}
}

func TestFileLabelFallsBackFromNameToTitleToID(t *testing.T) {
	for _, c := range []struct {
		file slackread.File
		want string
	}{
		{slackread.File{ID: "F1", Name: "a.pdf", Title: "A"}, "a.pdf"},
		{slackread.File{ID: "F1", Title: "A title"}, "A title"},
		{slackread.File{ID: "F1"}, "F1"},
	} {
		if got := FileLabel(c.file); got != c.want {
			t.Errorf("FileLabel(%+v) = %q, want %q", c.file, got, c.want)
		}
	}
}

// The structured transcript is what a program reads, and "partial": true alone
// does not say a message has attachments — blocks set it too, and they cannot
// be downloaded.
func TestStructuredTranscriptCarriesTheFiles(t *testing.T) {
	msgs := Messages([]slackread.Message{{
		TS: "1785708834.862009", User: "U1", Text: "here",
		Files: []slackread.File{{
			ID: "F1", Name: "trace.pdf", Size: 1258291,
			Mimetype: "application/pdf", Mode: "hosted",
		}},
	}}, Names{"U1": "jane"})

	if len(msgs) != 1 || len(msgs[0].Files) != 1 {
		t.Fatalf("messages = %+v, want one message carrying one file", msgs)
	}
	want := FileRef{ID: "F1", Name: "trace.pdf", Size: 1258291, Mimetype: "application/pdf", Mode: "hosted"}
	if msgs[0].Files[0] != want {
		t.Fatalf("file = %+v, want %+v", msgs[0].Files[0], want)
	}

	blob, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"files"`, `"id":"F1"`, `"name":"trace.pdf"`, `"size":1258291`, `"mode":"hosted"`} {
		if !strings.Contains(string(blob), key) {
			t.Errorf("JSON = %s, want it to contain %s", blob, key)
		}
	}
}

// A message with no attachments carries no files key at all, so a consumer can
// test for the field rather than for an empty array.
func TestAMessageWithoutFilesHasNoFilesKey(t *testing.T) {
	msgs := Messages([]slackread.Message{{TS: "1785708834.862009", Text: "just words"}}, Names{})
	blob, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "files") {
		t.Fatalf("JSON = %s, want no files key", blob)
	}
}

// A file with no name of its own is still addressable, and the structured form
// resolves it the same way the transcript does.
func TestAFileRefFallsBackToTheTitleAndTheID(t *testing.T) {
	msgs := Messages([]slackread.Message{{
		TS: "1.1",
		Files: []slackread.File{
			{ID: "F1", Title: "A title"},
			{ID: "F2"},
		},
	}}, Names{})
	if got := msgs[0].Files[0].Name; got != "A title" {
		t.Errorf("name = %q, want the title", got)
	}
	if got := msgs[0].Files[1].Name; got != "F2" {
		t.Errorf("name = %q, want the id", got)
	}
}

// A skipped file gets a line of its own: no line would be indistinguishable
// from a message that never carried the file.
func TestWriteFilesReportsEveryOutcome(t *testing.T) {
	var b bytes.Buffer
	err := WriteFiles(&b, []FileResult{
		{ID: "F1", Name: "trace.pdf", Status: "written", Path: "eng-1.1/trace.pdf", Bytes: 1258291},
		{ID: "F2", Name: "design-notes", Status: "skipped", Reason: "external file, no bytes to fetch"},
		{ID: "F3", Name: "report.pdf", Status: "failed", Reason: "HTTP 404"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), b.String())
	}
	for i, want := range []string{
		"eng-1.1/trace.pdf (1.2 MB)",
		"design-notes — external file, no bytes to fetch",
		"report.pdf — HTTP 404",
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want it to contain %q", i, lines[i], want)
		}
	}
	if !strings.HasPrefix(lines[0], "wrote") || !strings.HasPrefix(lines[1], "skipped") ||
		!strings.HasPrefix(lines[2], "failed") {
		t.Errorf("lines do not lead with what happened:\n%s", b.String())
	}
}

// Blocks narrows Partial to the part a caller can neither show nor preserve.
// The two travel together through the real transform, so a consumer can tell a
// message it may act on from one it may not.
func TestTheStructuredTranscriptDistinguishesBlocksFromFiles(t *testing.T) {
	for _, c := range []struct {
		name            string
		msg             slackread.Message
		partial, blocks bool
	}{
		{"plain text", slackread.Message{TS: "1.1", Text: "hello"}, false, false},
		{"files only", slackread.Message{TS: "1.1", Files: []slackread.File{{ID: "F1", Name: "a.pdf"}}}, true, false},
		{"a section block", slackread.Message{
			TS: "1.1", Text: "fallback",
			Blocks: []byte(`[{"type":"section"}]`),
		}, true, true},
		{"rich text is the text", slackread.Message{
			TS: "1.1", Text: "hi",
			Blocks: []byte(`[{"type":"rich_text"}]`),
		}, false, false},
		{"both", slackread.Message{
			TS: "1.1", Text: "fallback",
			Blocks: []byte(`[{"type":"image"}]`),
			Files:  []slackread.File{{ID: "F1", Name: "a.pdf"}},
		}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Messages([]slackread.Message{c.msg}, nil)
			if len(got) != 1 {
				t.Fatalf("messages = %d, want 1", len(got))
			}
			if got[0].Partial != c.partial {
				t.Errorf("partial = %v, want %v", got[0].Partial, c.partial)
			}
			if got[0].Blocks != c.blocks {
				t.Errorf("blocks = %v, want %v", got[0].Blocks, c.blocks)
			}
			// The invariant a consumer relies on: anything that sets blocks
			// sets partial, so an old reader of partial is never surprised.
			if got[0].Blocks && !got[0].Partial {
				t.Error("blocks without partial; the narrowing must not widen")
			}
		})
	}
}

// blocks is emitted even when false, so a consumer can tell "no blocks" from
// "this hear is too old to say".
func TestTheBlocksKeyIsAlwaysPresent(t *testing.T) {
	got := Messages([]slackread.Message{{TS: "1.1", Text: "hello"}}, nil)
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"blocks":false`) {
		t.Fatalf("marshalled = %s, want an explicit blocks:false", b)
	}
}
