package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/render"
)

// fileHost serves the bytes a stubbed file points at. It stands in for
// files.slack.com, which is a different host from the API and so cannot be the
// fake Slack itself.
func fileHost(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/download"
}

// messageWithFiles stubs conversations.history answering for one message.
func (e *env) messageWithFiles(files string) {
	e.srv.Reply("conversations.history", fmt.Sprintf(
		`{"ok":true,"messages":[{"ts":"1785708834.862009","user":"U0STEPAN0","text":"here","files":[%s]}]}`,
		files))
}

func TestFilesWritesIntoADirectoryPerMessage(t *testing.T) {
	e := newEnv(t)
	e.warm()
	url := fileHost(t, "the trace")
	e.messageWithFiles(fmt.Sprintf(
		`{"id":"F0123ABCD","name":"trace.pdf","mimetype":"application/pdf","size":9,"mode":"hosted","url_private_download":%q}`, url))

	out := t.TempDir()
	stdout, stderr, code := e.run("files", "#eng", "1785708834.862009", "--out", out)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	path := filepath.Join(out, "eng-1785708834.862009", "trace.pdf")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != "the trace" {
		t.Fatalf("file holds %q, want the body", got)
	}
	if !strings.Contains(stdout, "wrote") || !strings.Contains(stdout, "trace.pdf") {
		t.Fatalf("stdout = %q, want it to name what was written", stdout)
	}
}

// --max-size is the only new flag whose effect nothing else pins. parseSize is
// tested, and the ceiling is tested inside slackfiles, but not the wire between
// them: a limit() that ignored the flag and always returned the default would
// pass every other test in the suite.
//
// The stub declares size 0 so the pre-download skip cannot be what refuses it.
// What is under test is the bound on the bytes that actually arrive.
func TestMaxSizeReachesTheDownloadBound(t *testing.T) {
	e := newEnv(t)
	e.warm()
	url := fileHost(t, strings.Repeat("x", 2048))
	e.messageWithFiles(fmt.Sprintf(
		`{"id":"F0123ABCD","name":"big.pdf","mimetype":"application/pdf","size":0,"mode":"hosted","url_private_download":%q}`, url))

	out := t.TempDir()
	stdout, _, code := e.run("files", "#eng", "1785708834.862009", "--out", out, "--max-size", "1K")

	if code != 1 {
		t.Fatalf("exit = %d, want 1; 2048 bytes must not pass a 1K ceiling", code)
	}
	if !strings.Contains(stdout, "failed") {
		t.Fatalf("stdout = %q, want the file reported as failed", stdout)
	}
	_, err := os.Stat(filepath.Join(out, "eng-1785708834.862009", "big.pdf"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the oversize file was written; the ceiling did not bind")
	}
}

// The single message is asked for as a window of one timestamp, which is what
// makes the answer that message whether it is a parent, a reply, or neither.
func TestFilesAsksForExactlyTheOneMessage(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.messageWithFiles(`{"id":"F1","name":"a link","mode":"external"}`)

	e.run("files", "#eng", "1785708834.862009", "--out", t.TempDir())

	q := e.query(0)
	for _, c := range []struct{ key, want string }{
		{"channel", "C0ENG0000"},
		{"oldest", "1785708834.862009"},
		{"latest", "1785708834.862009"},
		{"inclusive", "true"},
		{"limit", "1"},
	} {
		if got := q.Get(c.key); got != c.want {
			t.Errorf("%s = %q, want %q", c.key, got, c.want)
		}
	}
}

// A caller that asked for a message's attachments and got fewer must be able to
// tell, whatever else was written.
func TestFilesExitsNonZeroWhenAnythingWasSkipped(t *testing.T) {
	e := newEnv(t)
	e.warm()
	url := fileHost(t, "the trace")
	e.messageWithFiles(fmt.Sprintf(
		`{"id":"F1","name":"trace.pdf","mimetype":"application/pdf","size":9,"mode":"hosted","url_private_download":%q},`+
			`{"id":"F2","name":"design-notes","mode":"external","is_external":true}`, url))

	out := t.TempDir()
	stdout, _, code := e.run("files", "#eng", "1785708834.862009", "--out", out)

	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	// The file that could be fetched still was.
	_, err := os.Stat(filepath.Join(out, "eng-1785708834.862009", "trace.pdf"))
	if err != nil {
		t.Fatalf("the fetchable file was not written: %v", err)
	}
	if !strings.Contains(stdout, "skipped") || !strings.Contains(stdout, "external") {
		t.Fatalf("stdout = %q, want the skip and its reason", stdout)
	}
}

func TestFilesJSONReportsEachFile(t *testing.T) {
	e := newEnv(t)
	e.warm()
	url := fileHost(t, "the trace")
	e.messageWithFiles(fmt.Sprintf(
		`{"id":"F1","name":"trace.pdf","mimetype":"application/pdf","size":9,"mode":"hosted","url_private_download":%q},`+
			`{"id":"F2","name":"design-notes","mode":"external"}`, url))

	out := t.TempDir()
	stdout, _, code := e.run("files", "#eng", "1785708834.862009", "--out", out, "--json")

	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var got struct {
		Kind    string `json:"kind"`
		TS      string `json:"ts"`
		Dir     string `json:"dir"`
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
		Files []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"files"`
	}
	err := json.Unmarshal([]byte(stdout), &got)
	if err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if got.Kind != "files" || got.TS != "1785708834.862009" || got.Channel.Name != "eng" {
		t.Fatalf("envelope = %+v, want it to name the message", got)
	}
	if len(got.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(got.Files))
	}
	if got.Files[0].Status != "written" || got.Files[0].Path == "" {
		t.Fatalf("first file = %+v, want it written with a path", got.Files[0])
	}
	if got.Files[1].Status != "skipped" || got.Files[1].Reason == "" {
		t.Fatalf("second file = %+v, want it skipped with a reason", got.Files[1])
	}
	if got.Dir == "" {
		t.Fatalf("dir is empty, want it named when something was written")
	}
}

// A message whose files are all links wrote nothing, and must not point a
// consumer at a directory that was never made.
func TestFilesJSONNamesNoDirectoryWhenNothingWasWritten(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.messageWithFiles(`{"id":"F1","name":"a link","mode":"external"}`)

	stdout, _, code := e.run("files", "#eng", "1785708834.862009", "--out", t.TempDir(), "--json")

	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var got map[string]any
	err := json.Unmarshal([]byte(stdout), &got)
	if err != nil {
		t.Fatalf("stdout is not one JSON object: %v", err)
	}
	if _, ok := got["dir"]; ok {
		t.Fatalf("dir = %v, want it absent", got["dir"])
	}
}

// A permalink to a reply names that reply, so its own files are the ones asked
// for rather than the thread parent's.
func TestFilesFollowsAPermalinkToTheMessageItNames(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785708834.862009","user":"U0STEPAN0","files":[{"id":"F1","name":"a link","mode":"external"}]}]}`)

	link := "https://acme.slack.com/archives/C0ENG0000/p1785708834862009?thread_ts=1785700000.000100"
	e.run("files", link, "--out", t.TempDir())

	if got := e.query(0).Get("latest"); got != "1785708834.862009" {
		t.Fatalf("latest = %q, want the message the link names, not its thread", got)
	}
}

// A conversation's history is its channel-level messages. A file posted inside
// a thread belongs to a message that never appears there, so asking history
// alone would fail on one of the commonest places to post a file.
func TestFilesFindsAFileOnAThreadReply(t *testing.T) {
	e := newEnv(t)
	e.warm()
	url := fileHost(t, "the trace")
	// History knows nothing about the reply, exactly as Slack answers.
	e.srv.Reply("conversations.history", `{"ok":true,"messages":[]}`)
	e.srv.Reply("conversations.replies", fmt.Sprintf(
		`{"ok":true,"messages":[`+
			`{"ts":"1785700000.000100","user":"U0ANN0000","text":"the parent"},`+
			`{"ts":"1785708834.862009","user":"U0STEPAN0","text":"here","files":[`+
			`{"id":"F1","name":"trace.pdf","mimetype":"application/pdf","size":9,"mode":"hosted","url_private_download":%q}]}]}`,
		url))

	out := t.TempDir()
	stdout, stderr, code := e.run("files", "#eng", "1785708834.862009", "--out", out)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	_, err := os.Stat(filepath.Join(out, "eng-1785708834.862009", "trace.pdf"))
	if err != nil {
		t.Fatalf("the reply's file was not written: %v", err)
	}
	if !strings.Contains(stdout, "trace.pdf") {
		t.Fatalf("stdout = %q, want it to name the file", stdout)
	}
	// The reply is asked for by its own timestamp, which is what
	// conversations.replies accepts alongside a parent's.
	if got := e.srv.Requests()[1]; got.Endpoint != "conversations.replies" ||
		got.Query.Get("ts") != "1785708834.862009" {
		t.Fatalf("second request = %s ts=%q, want replies for the message's own ts",
			got.Endpoint, got.Query.Get("ts"))
	}
}

// The thread lookup is a fallback, not a second call every time.
func TestFilesDoesNotAskForARepliesPageItDoesNotNeed(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.messageWithFiles(`{"id":"F1","name":"a link","mode":"external"}`)

	e.run("files", "#eng", "1785708834.862009", "--out", t.TempDir())

	for _, r := range e.srv.Requests() {
		if r.Endpoint == "conversations.replies" {
			t.Fatal("conversations.replies was called for a message history already answered for")
		}
	}
}

func TestFilesRefusesAMessageWithNoFiles(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785708834.862009","user":"U0STEPAN0","text":"just words"}]}`)

	_, stderr, code := e.run("files", "#eng", "1785708834.862009", "--out", t.TempDir())

	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no files") {
		t.Fatalf("stderr = %q, want it to say the message carries no files", stderr)
	}
}

// A ts in neither the channel nor a thread names nothing, and the error says
// which timestamp and what Slack answered.
//
// It does not claim the message is missing. A rate limit or an expired token
// reaches the same branch, and neither is evidence about whether the message
// exists.
func TestFilesReportsATimestampThatNamesNothing(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history", `{"ok":true,"messages":[]}`)
	e.srv.Reply("conversations.replies", `{"ok":false,"error":"thread_not_found"}`)

	_, stderr, code := e.run("files", "#eng", "1785708834.862009", "--out", t.TempDir())

	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{"1785708834.862009", "thread_not_found"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}

// The thread exists and the message is not in it, which is the one answer that
// genuinely means no such message.
func TestFilesReportsATimestampAbsentFromItsThread(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history", `{"ok":true,"messages":[]}`)
	e.srv.Reply("conversations.replies",
		`{"ok":true,"messages":[{"ts":"1785700000.000100","user":"U0ANN0000","text":"the parent"}]}`)

	_, stderr, code := e.run("files", "#eng", "1785708834.862009", "--out", t.TempDir())

	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no message at ts 1785708834.862009") {
		t.Fatalf("stderr = %q, want it to name the missing message", stderr)
	}
}

func TestFilesUsage(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no arguments", []string{"files"}, "needs an address and a ts"},
		{"no ts", []string{"files", "#eng"}, "needs an address and a ts"},
		{"too many", []string{"files", "#eng", "1.1", "2.2"}, "needs an address and a ts"},
		{
			"a permalink and a ts",
			[]string{"files", "https://acme.slack.com/archives/C0ENG0000/p1785708834862009", "1.1"},
			"already names the timestamp",
		},
		{"--out on read", []string{"read", "#eng", "--out", "/tmp"}, "apply to files"},
		{"--max-size on thread", []string{"thread", "#eng", "1.1", "--max-size", "1M"}, "apply to files"},
		{"--out with no directory", []string{"files", "#eng", "1.1", "--out"}, "needs a directory"},
		{"--max-size with no size", []string{"files", "#eng", "1.1", "--max-size"}, "needs a size"},
		{"--max-size nonsense", []string{"files", "#eng", "1.1", "--max-size", "big"}, "not a size"},
		{"--max-size zero", []string{"files", "#eng", "1.1", "--max-size", "0"}, "larger than nothing"},
		// The ceiling has to stay one below the maximum: the download reads a
		// byte past it to tell a file of exactly the limit from an overrun, and
		// at the maximum that addition wraps negative — which reads no bytes at
		// all and calls the empty result a success.
		{
			"--max-size at the int64 maximum",
			[]string{"files", "#eng", "1.1", "--max-size", "9223372036854775807"},
			"larger than any file Slack stores",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.warm()
			_, stderr, code := e.run(c.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr, c.want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, c.want)
			}
			if e.srv.Hits() != 0 {
				t.Fatalf("a misuse made %d requests, want 0", e.srv.Hits())
			}
		})
	}
}

func TestMaxSizeAccepts(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"1", 1},
		{"512K", 512 << 10},
		{"512k", 512 << 10},
		{"100M", 100 << 20},
		{"100MB", 100 << 20},
		{"2G", 2 << 30},
		{"1024B", 1024},
		// Far larger than Slack stores, and still accepted: the guard against
		// overflow must not refuse sizes that are merely big.
		{"1024G", 1 << 40},
	} {
		got, err := parseSize(c.in)
		if err != nil {
			t.Errorf("parseSize(%q) error = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// The directory name is built from a channel label and a timestamp, and neither
// may become more than one path element.
func TestMessageDirIsAlwaysOneElement(t *testing.T) {
	for _, c := range []struct {
		name, id, ts, want string
	}{
		{"eng", "C0ENG0000", "1785708834.862009", "eng-1785708834.862009"},
		{"", "D0STEPAN0", "1785708834.862009", "D0STEPAN0-1785708834.862009"},
		{"../etc", "C1", "1.1", "..-etc-1.1"},
		{"..", "C1", "1.1", "--1.1"},
		{"a b/c", "C1", "1.1", "a-b-c-1.1"},
	} {
		got := messageDir(render.ChannelRef{Name: c.name, ID: c.id}, c.ts)
		if got != c.want {
			t.Errorf("messageDir(%q, %q) = %q, want %q", c.name, c.ts, got, c.want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("messageDir(%q, %q) = %q, which is more than one element", c.name, c.ts, got)
		}
	}
}
