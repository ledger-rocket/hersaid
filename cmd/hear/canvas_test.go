package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/render"
)

// canvasHost serves a canvas the way Slack does: an HTML fragment under the
// canvas mimetype. The plain fileHost serves application/pdf, so a test using
// it would pass even with the sign-in-page guard reverted.
func canvasHost(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// canvasFile stubs files.info for a canvas whose bytes come from url.
func (e *env) canvasFile(id, title, url string) {
	e.srv.Reply("files.info", fmt.Sprintf(
		`{"ok":true,"file":{"id":%q,"title":%q,"name":"canvas","mimetype":"application/vnd.slack-docs",`+
			`"mode":"quip","is_external":false,"size":40,"url_private_download":%q}}`, id, title, url))
}

func TestCanvasTargetTakesALinkOrAnID(t *testing.T) {
	for _, c := range []struct {
		in, want string
	}{
		{"F0123ABCDEF", "F0123ABCDEF"},
		{"https://acme.slack.com/docs/T0123ABCDE/F0123ABCDEF", "F0123ABCDEF"},
		{"https://acme.slack.com/docs/T0123ABCDE/F0123ABCDEF/", "F0123ABCDEF"},
	} {
		got, err := canvasTarget(c.in)
		if err != nil || got != c.want {
			t.Errorf("canvasTarget(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// canvasTarget answers only for a canvas's own address. Anything else comes
// back empty, meaning "this names something else" — a channel, most likely,
// which the caller resolves instead. Only a link that cannot be Slack's at all
// is an error here.
func TestCanvasTargetLeavesOtherAddressesToTheCaller(t *testing.T) {
	for _, in := range []string{
		"https://acme.slack.com/archives/C0123ABCD/p1785708834862009",
		"https://acme.slack.com/archives/C0123ABCD",
		"#eng",
		"C0123ABCD",
		"",
	} {
		got, err := canvasTarget(in)
		if err != nil {
			t.Errorf("canvasTarget(%q) error = %v, want it left to the caller", in, err)
		}
		if got != "" {
			t.Errorf("canvasTarget(%q) = %q, want empty", in, got)
		}
	}
}

// A link that is not Slack's is refused outright: nothing downstream trusts the
// host, but accepting one invites a reader to believe a pasted URL was checked
// when only its shape was.
func TestCanvasTargetRefusesAForeignHost(t *testing.T) {
	for _, in := range []string{
		"https://evil.example/docs/T0123ABCDE/F0123ABCDEF",
		"https://acme.slack.com.evil.example/docs/T0123ABCDE/F0123ABCDEF",
	} {
		got, err := canvasTarget(in)
		if err == nil {
			t.Errorf("canvasTarget(%q) = %q, want a refusal", in, got)
		}
	}
}

func TestCanvasPrintsMarkdown(t *testing.T) {
	e := newEnv(t)
	body := `<div class="quip-canvas-content"><h1>Plan</h1><p><b>Status:</b> ready</p>` +
		`<ul><li>one</li><li>two</li></ul></div>`
	url := canvasHost(t, body)
	e.canvasFile("F0123ABCDEF", "Plan", url)

	stdout, stderr, code := e.run(cmdCanvas, "https://acme.slack.com/docs/T0123ABCDE/F0123ABCDEF")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	want := "# Plan\n\n**Status:** ready\n\n- one\n- two\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestCanvasAsksForTheFileTheLinkNames(t *testing.T) {
	e := newEnv(t)
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, "<h1>Plan</h1>"))

	if _, stderr, code := e.run(cmdCanvas, "F0123ABCDEF"); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	for _, r := range e.srv.Requests() {
		if r.Endpoint == "files.info" {
			if got := r.Query.Get("file"); got != "F0123ABCDEF" {
				t.Fatalf("files.info file = %q, want the id from the link", got)
			}
			return
		}
	}
	t.Fatal("files.info was never called")
}

func TestCanvasHTMLPrintsWhatSlackServed(t *testing.T) {
	e := newEnv(t)
	body := `<div class="quip-canvas-content"><h1>Plan</h1></div>`
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, body))

	stdout, stderr, code := e.run(cmdCanvas, "--html", "F0123ABCDEF")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if stdout != body {
		t.Fatalf("stdout = %q, want the bytes Slack served", stdout)
	}
}

func TestCanvasJSONCarriesTheMarkdown(t *testing.T) {
	e := newEnv(t)
	e.canvasFile("F0123ABCDEF", "Deployment Plan", canvasHost(t, "<h1>Plan</h1><p>body</p>"))

	stdout, stderr, code := e.run(cmdCanvas, "--json", "F0123ABCDEF")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var got struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	err := json.Unmarshal([]byte(stdout), &got)
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if got.Kind != render.KindCanvas || got.ID != "F0123ABCDEF" || got.Title != "Deployment Plan" {
		t.Fatalf("envelope = %+v", got)
	}
	if !strings.Contains(got.Body, "# Plan") {
		t.Fatalf("body = %q, want Markdown", got.Body)
	}
}

// An ordinary attachment is not a canvas, and the refusal says where to go
// instead rather than leaving somebody to guess.
func TestCanvasRefusesAFileThatIsNotOne(t *testing.T) {
	e := newEnv(t)
	e.srv.Reply("files.info",
		`{"ok":true,"file":{"id":"F0123ABCD","name":"trace.pdf","mimetype":"application/pdf",`+
			`"mode":"hosted","size":10,"url_private_download":"https://files.slack.com/x"}}`)

	_, stderr, code := e.run(cmdCanvas, "F0123ABCD")
	if code == 0 {
		t.Fatal("exit = 0, want a refusal")
	}
	if !strings.Contains(stderr, "hear files") {
		t.Fatalf("stderr = %q, want it to name the verb that does handle it", stderr)
	}
}

func TestCanvasReportsSlacksRefusal(t *testing.T) {
	e := newEnv(t)
	e.srv.Reply("files.info", `{"ok":false,"error":"file_not_found"}`)

	_, stderr, code := e.run(cmdCanvas, "F0123ABCDEF")
	if code == 0 {
		t.Fatal("exit = 0, want a refusal")
	}
	if !strings.Contains(stderr, "file_not_found") {
		t.Fatalf("stderr = %q, want Slack's own reason", stderr)
	}
}

// A canvas is written by whoever can edit it, so its text is untrusted in the
// way any Slack text is — and it is printed straight to a terminal.
func TestACanvasCannotDriveTheTerminal(t *testing.T) {
	e := newEnv(t)
	body := "<h1>Plan" + string(rune(0x1b)) + "[31m" + string(rune(0x202e)) + "</h1>"
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, body))

	stdout, _, code := e.run(cmdCanvas, "F0123ABCDEF")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, banned := range []rune{0x1b, 0x202e} {
		if strings.ContainsRune(stdout, banned) {
			t.Errorf("stdout carries %U from the canvas", banned)
		}
	}
}

func TestCanvasUsage(t *testing.T) {
	for _, args := range [][]string{
		{cmdCanvas},
		{cmdCanvas, "F1", "F2"},
	} {
		e := newEnv(t)
		if _, _, code := e.run(args...); code != 2 {
			t.Errorf("run(%v) exit = %d, want 2", args, code)
		}
		if e.srv.Hits() != 0 {
			t.Errorf("run(%v) made %d requests, want none", args, e.srv.Hits())
		}
	}
}

// --html gives the markup Slack sent, not the bytes: HTML has no legitimate use
// for a control character and this goes to a terminal.
func TestCanvasHTMLStillCannotDriveTheTerminal(t *testing.T) {
	e := newEnv(t)
	body := "<h1>Plan" + string(rune(0x1b)) + "[31m" + string(rune(0x202e)) + "</h1>"
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, body))

	stdout, _, code := e.run(cmdCanvas, "--html", "F0123ABCDEF")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, banned := range []rune{0x1b, 0x202e} {
		if strings.ContainsRune(stdout, banned) {
			t.Errorf("--html output carries %U", banned)
		}
	}
	if !strings.Contains(stdout, "<h1>") {
		t.Fatalf("stdout = %q, want the markup kept", stdout)
	}
}

// A verb-specific flag on the wrong verb is a usage error here, not a silent
// no-op — the rule --since and --out already follow.
func TestHTMLAppliesToCanvasOnly(t *testing.T) {
	for _, args := range [][]string{
		{"--html", "read", "#eng"},
		{"--html", "whoami"},
		{"--html", cmdFiles, "#eng", fixtureTS},
	} {
		e := newEnv(t)
		_, stderr, code := e.run(args...)
		if code != 2 {
			t.Errorf("run(%v) exit = %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "--html applies to canvas") {
			t.Errorf("run(%v) stderr = %q", args, stderr)
		}
	}
}

// A canvas too deeply nested for the parser must fail rather than have its
// unparsed markup printed to a terminal.
func TestACanvasTooDeepToParseIsRefused(t *testing.T) {
	e := newEnv(t)
	body := strings.Repeat("<div>", 600) + "x" + string(rune(0x1b)) + "[31m"
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, body))

	stdout, _, code := e.run(cmdCanvas, "F0123ABCDEF")
	if code == 0 {
		t.Fatalf("exit = 0, want the parse failure reported (stdout %d bytes)", len(stdout))
	}
	if strings.ContainsRune(stdout, 0x1b) {
		t.Fatal("unparsed markup reached stdout")
	}
}

// channelInfo stubs conversations.info with the properties a channel carries.
func (e *env) channelInfo(props string) {
	e.srv.Reply("conversations.info",
		`{"ok":true,"channel":{"id":"C0123ENG01","name":"eng","properties":`+props+`}}`)
}

// The shape a real channel was observed carrying: no properties.canvas, a
// canvas tab and meeting notes, both naming the same document.
func TestACanvasIsFoundThroughItsChannel(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.channelInfo(`{"meeting_notes":{"file_id":"F0123ABCDEF"},` +
		`"tabs":[{"type":"canvas","data":{"file_id":"F0123ABCDEF"}}]}`)
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, "<h1>Plan</h1>"))

	for _, target := range []string{
		"#eng",
		"C0123ENG01",
		"https://acme.slack.com/archives/C0123ENG01",
	} {
		t.Run(target, func(t *testing.T) {
			stdout, stderr, code := e.run(cmdCanvas, target)
			if code != 0 {
				t.Fatalf("exit = %d (stderr: %s)", code, stderr)
			}
			if !strings.Contains(stdout, "# Plan") {
				t.Fatalf("stdout = %q", stdout)
			}
		})
	}
}

// properties.canvas is what conversations.info documents, even though the
// channel observed in practice did not use it.
func TestTheDocumentedCanvasPropertyIsRead(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.channelInfo(`{"canvas":{"file_id":"F0123ABCDEF"}}`)
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, "<h1>Plan</h1>"))

	stdout, stderr, code := e.run(cmdCanvas, "#eng")
	if code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "# Plan") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// A channel commonly lists one document as both a tab and its meeting notes.
// That is one canvas, not two, and must not look like an ambiguity.
func TestOneDocumentListedTwiceIsNotAmbiguous(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.channelInfo(`{"canvas":{"file_id":"F0123ABCDEF"},` +
		`"meeting_notes":{"file_id":"F0123ABCDEF"},` +
		`"tabs":[{"type":"canvas","data":{"file_id":"F0123ABCDEF"}}]}`)
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, "<h1>Plan</h1>"))

	if _, stderr, code := e.run(cmdCanvas, "#eng"); code != 0 {
		t.Fatalf("exit = %d (stderr: %s), want the one document", code, stderr)
	}
}

// A canvas and a notes document are different documents, and choosing between
// them would be choosing which one somebody meant.
func TestSeveralCanvasesAreNamedRatherThanGuessedAt(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.channelInfo(`{"canvas":{"file_id":"F0000000AAA"},` +
		`"meeting_notes":{"file_id":"F0000000BBB"}}`)

	_, stderr, code := e.run(cmdCanvas, "#eng")
	if code == 0 {
		t.Fatal("exit = 0, want the ambiguity reported")
	}
	for _, id := range []string{"F0000000AAA", "F0000000BBB"} {
		if !strings.Contains(stderr, id) {
			t.Errorf("stderr = %q, want it to name %s", stderr, id)
		}
	}
}

// A tab that is not a canvas is not a canvas.
func TestANonCanvasTabIsIgnored(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.channelInfo(`{"tabs":[{"type":"files","data":{"file_id":"F0000000AAA"}},` +
		`{"type":"bookmarks","data":{"file_id":"F0000000BBB"}}]}`)

	_, stderr, code := e.run(cmdCanvas, "#eng")
	if code == 0 {
		t.Fatal("exit = 0, want no canvas reported")
	}
	if !strings.Contains(stderr, "no canvas") {
		t.Fatalf("stderr = %q, want it to say there is none", stderr)
	}
}

func TestAChannelWithNoCanvasSaysSo(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.channelInfo(`{}`)

	_, stderr, code := e.run(cmdCanvas, "#eng")
	if code == 0 {
		t.Fatal("exit = 0, want a refusal")
	}
	if !strings.Contains(stderr, "no canvas") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// A message link names a message, and a message is not a canvas or a channel.
func TestAMessageLinkIsStillRefused(t *testing.T) {
	e := newEnv(t)
	e.warm()
	_, stderr, code := e.run(cmdCanvas, "https://acme.slack.com/archives/C0123ENG01/p1785708834862009")
	if code == 0 {
		t.Fatal("exit = 0, want a refusal")
	}
	if e.srv.Hits() != 0 {
		t.Fatalf("made %d requests, want none", e.srv.Hits())
	}
	if !strings.Contains(stderr, "canvas or channel link") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// Slack documents both names for a canvas tab.
func TestAChannelCanvasTabIsRead(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.channelInfo(`{"tabs":[{"type":"channel_canvas","data":{"file_id":"F0123ABCDEF"}}]}`)
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, "<h1>Plan</h1>"))

	stdout, stderr, code := e.run(cmdCanvas, "#eng")
	if code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "# Plan") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// The channel path must ask about the channel the target resolved to. Without
// this a resolver that returned the wrong id, or was skipped, would still pass.
func TestTheChannelCanvasLookupNamesTheResolvedChannel(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.channelInfo(`{"canvas":{"file_id":"F0123ABCDEF"}}`)
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, "<h1>Plan</h1>"))

	if _, stderr, code := e.run(cmdCanvas, "#eng"); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	for _, r := range e.srv.Requests() {
		if r.Endpoint == "conversations.info" {
			if got := r.Query.Get("channel"); got != engID {
				t.Fatalf("conversations.info channel = %q, want the resolved id", got)
			}
			return
		}
	}
	t.Fatal("conversations.info was never called")
}

// An id out of a reply reaches stderr when a channel holds several, so it is
// checked for shape rather than passed through.
func TestAMisshapenFileIDFromSlackIsNotReported(t *testing.T) {
	e := newEnv(t)
	e.warm()
	// The escape is JSON-encoded, as Slack would have to send it; a raw control
	// character would make the reply unparseable rather than hostile.
	e.channelInfo(`{"canvas":{"file_id":"F0123ABCDEF"},` +
		`"meeting_notes":{"file_id":"not\u001b[31man-id"}}`)
	e.canvasFile("F0123ABCDEF", "Plan", canvasHost(t, "<h1>Plan</h1>"))

	stdout, stderr, code := e.run(cmdCanvas, "#eng")
	if strings.ContainsRune(stdout+stderr, 0x1b) {
		t.Fatal("an escape sequence from a Slack reply reached the output")
	}
	// The misshapen one is discarded, so the one real canvas is unambiguous.
	if code != 0 {
		t.Fatalf("exit = %d (stderr: %s), want the sound id used", code, stderr)
	}
}

// A mimetype out of a reply reaches stderr on the not-a-canvas path.
func TestAMimetypeFromSlackCannotDriveTheTerminal(t *testing.T) {
	e := newEnv(t)
	e.srv.Reply("files.info",
		`{"ok":true,"file":{"id":"F0123ABCD","name":"x",`+
			`"mimetype":"application/\u001b[31mpdf","mode":"hosted","size":10,`+
			`"url_private_download":"https://files.slack.com/x"}}`)

	stdout, stderr, code := e.run(cmdCanvas, "F0123ABCD")
	if code == 0 {
		t.Fatal("exit = 0, want a refusal")
	}
	if strings.ContainsRune(stdout+stderr, 0x1b) {
		t.Fatalf("the refusal carries an escape from the reply: %q", stderr)
	}
}
