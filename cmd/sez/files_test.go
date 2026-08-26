package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/slackhosts"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// sink is the fake object storage Slack's upload URL points at.
type sink struct {
	mu     sync.Mutex
	hits   int
	auths  []string
	sizes  []int
	ctypes []string
}

func (k *sink) snapshot() (int, []string, []int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.hits, append([]string(nil), k.auths...), append([]int(nil), k.sizes...)
}

func (k *sink) contentTypes() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.ctypes...)
}

// uploadSink starts the storage endpoint and points uploadHosts at it, so the
// transfer leg is permitted to reach a local address for the length of a test.
func uploadSink(t *testing.T) (string, *sink) {
	t.Helper()
	url, k := sinkOnly(t)
	permitLoopback(t)
	return url, k
}

// permitLoopback is what uploadSink does besides serving. It is separate so a
// test can serve without permitting, which is the only way to prove the host
// check refuses rather than merely that it is consulted.
func permitLoopback(t *testing.T) {
	t.Helper()
	saved := uploadHosts
	uploadHosts = slackhosts.Allow("127.0.0.1", "localhost")
	t.Cleanup(func() { uploadHosts = saved })
}

func sinkOnly(t *testing.T) (string, *sink) {
	t.Helper()
	k := &sink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		k.mu.Lock()
		k.hits++
		k.auths = append(k.auths, r.Header.Get("Authorization"))
		k.ctypes = append(k.ctypes, r.Header.Get("Content-Type"))
		k.sizes = append(k.sizes, len(body))
		k.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, k
}

// stages stubs the staging leg, handing back one ticket per id in turn.
func stages(s *slacktest.Server, sinkURL string, ids ...string) {
	bodies := make([]string, 0, len(ids))
	for _, id := range ids {
		bodies = append(bodies, fmt.Sprintf(
			`{"ok":true,"file_id":%q,"upload_url":"%s/u?X-Amz-Signature=CANARY42"}`, id, sinkURL))
	}
	s.Pages("files.getUploadURLExternal", bodies...)
}

// completes stubs the sharing leg with a reply naming ids in channel C0123ENG01.
func completes(s *slacktest.Server, ts string, ids ...string) {
	if ts != "" {
		s.Reply("chat.getPermalink",
			`{"ok":true,"permalink":"https://acme.slack.com/archives/C0123ENG01/p1"}`)
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		shares := ""
		if ts != "" {
			shares = fmt.Sprintf(`,"shares":{"public":{"C0123ENG01":[{"ts":%q}]}}`, ts)
		}
		parts = append(parts, fmt.Sprintf(`{"id":%q%s}`, id, shares))
	}
	s.Reply("files.completeUploadExternal", `{"ok":true,"files":[`+strings.Join(parts, ",")+`]}`)
}

func tmpFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func endpointsCalled(s *slacktest.Server) []string {
	out := []string{}
	for _, r := range s.Requests() {
		out = append(out, r.Endpoint)
	}
	return out
}

func filesOf(t *testing.T, r record) []map[string]any {
	t.Helper()
	raw, ok := r["files"]
	if !ok {
		t.Fatal("the record has no files key; a post record's field set is fixed")
	}
	if raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("files = %#v, want an array", raw)
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("files entry = %#v, want an object", e)
		}
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------- the send

func TestFilesAreStagedThenSharedAsOneMessage(t *testing.T) {
	logPath := isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, k := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	stages(s, sinkURL, "F0000001", "F0000002")
	completes(s, "1785712596.256909", "F0000001", "F0000002")
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://acme.slack.com/archives/C0123ENG01/p1"}`)

	a := tmpFile(t, "trace.pdf", strings.Repeat("a", 40))
	b := tmpFile(t, "out.log", strings.Repeat("b", 14))

	code, stdout, stderr := sez(t, s, "--send", "--file", a, "--file", b, "#eng", "here's the repro")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	// Two staged, two transferred, one share. Never a chat.postMessage: an
	// upload is a different endpoint, not a message with something attached.
	if got := endpointsCalled(s); strings.Contains(strings.Join(got, ","), "chat.postMessage") {
		t.Fatalf("endpoints = %v, want no chat.postMessage", got)
	}
	hits, auths, sizes := k.snapshot()
	if hits != 2 {
		t.Fatalf("the storage endpoint saw %d transfers, want 2", hits)
	}
	for i, a := range auths {
		if a != "" {
			t.Errorf("transfer %d carried Authorization %q, want none", i, a)
		}
	}
	if len(sizes) != 2 || sizes[0] != 40 || sizes[1] != 14 {
		t.Fatalf("transferred sizes = %v, want [40 14] in the order given", sizes)
	}

	var share *slacktest.Request
	for _, r := range s.Requests() {
		if r.Endpoint == "files.completeUploadExternal" {
			rr := r
			share = &rr
		}
	}
	if share == nil {
		t.Fatal("no files.completeUploadExternal was received")
	}
	if got := share.Body.Get("channel_id"); got != "C0123ENG01" {
		t.Errorf("channel_id = %q, want the resolved id", got)
	}
	if got := share.Body.Get("initial_comment"); got != "here's the repro" {
		t.Errorf("initial_comment = %q", got)
	}
	if got := share.Body.Get("files"); !strings.Contains(got, `"id":"F0000001"`) ||
		!strings.Contains(got, `"id":"F0000002"`) {
		t.Errorf("files = %s, want both ids", got)
	}
	if !strings.Contains(stdout, "F0000001") || !strings.Contains(stdout, "F0000002") {
		t.Errorf("stdout = %q, want it to name the file ids", stdout)
	}
	// A share with a timestamp is as reconcilable as any other message, so the
	// link is fetched and printed exactly as a send does.
	if !strings.Contains(stdout, "https://acme.slack.com/archives/") {
		t.Errorf("stdout = %q, want the permalink", stdout)
	}
	if got := k.contentTypes(); len(got) != 2 || got[0] != "application/pdf" {
		t.Errorf("transfer Content-Types = %v, want the files' own", got)
	}

	recs := readLog(t, logPath)
	if got := phases(recs); len(got) < 2 || got[0] != "intent" || got[len(got)-1] != "outcome" {
		t.Fatalf("phases = %v, want intent first and outcome last", got)
	}
	last := recs[len(recs)-1]
	if got, _ := last["action"].(string); got != "message.upload" {
		t.Errorf("action = %v, want message.upload", last["action"])
	}
	if ok, _ := last["ok"].(bool); !ok {
		t.Errorf("ok = %v, want true", last["ok"])
	}
	if got, _ := last["ts"].(string); got != "1785712596.256909" {
		t.Errorf("ts = %v, want the share timestamp Slack returned", last["ts"])
	}
	files := filesOf(t, last)
	if len(files) != 2 {
		t.Fatalf("files = %v, want two", files)
	}
	if got, _ := files[0]["id"].(string); got != "F0000001" {
		t.Errorf("files[0].id = %v, want F0000001", files[0]["id"])
	}
	if got, _ := files[0]["path"].(string); got != a {
		t.Errorf("files[0].path = %v, want the absolute path the render showed", files[0]["path"])
	}
	if got, _ := files[0]["name"].(string); got != "trace.pdf" {
		t.Errorf("files[0].name = %v", files[0]["name"])
	}
	if got, _ := files[0]["size"].(float64); int(got) != 40 {
		t.Errorf("files[0].size = %v, want 40", files[0]["size"])
	}
	if got, _ := files[0]["mimetype"].(string); got != "application/pdf" {
		t.Errorf("files[0].mimetype = %v, want application/pdf from the extension", files[0]["mimetype"])
	}
	if got, _ := last["permalink"].(string); got == "" {
		t.Errorf("permalink = %v, want the link recorded", last["permalink"])
	}

	// The intent line names what was about to go out, before any id exists.
	intent := recs[0]
	got := filesOf(t, intent)
	if len(got) != 2 {
		t.Fatalf("intent files = %v, want both files named before anything left", got)
	}
	// An explicit null, not a missing key: a map lookup returns nil for both,
	// so the two are only distinguishable by asking whether the key is there.
	if v, present := got[0]["id"]; !present || v != nil {
		t.Errorf("intent files[0].id = %v (present=%t), want an explicit null", v, present)
	}
	if got[0]["path"] != a || got[0]["name"] != "trace.pdf" {
		t.Errorf("intent files[0] = %v, want the path and name it was about to send", got[0])
	}
	if n, _ := got[0]["size"].(float64); int(n) != 40 {
		t.Errorf("intent files[0].size = %v, want 40", got[0]["size"])
	}

	// The credential in the upload URL must not survive a success either.
	body, _ := os.ReadFile(logPath)
	for _, where := range []struct{ name, text string }{
		{"stdout", stdout}, {"stderr", stderr}, {"the audit log", string(body)},
	} {
		if strings.Contains(where.text, "CANARY42") {
			t.Errorf("%s carries the upload URL's credential", where.name)
		}
	}
}

// Nothing is visible until the sharing leg, so a failure before it means the
// message was not posted — and must not be reported as though part of it was.
func TestAFailedTransferSharesNothingAtAll(t *testing.T) {
	logPath := isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	// The first file stages; the second cannot get a ticket.
	s.Pages("files.getUploadURLExternal",
		fmt.Sprintf(`{"ok":true,"file_id":"F0000001","upload_url":"%s/u?X-Amz-Signature=CANARY42"}`, sinkURL),
		`{"ok":false,"error":"file_uploads_disabled"}`)
	completes(s, "1.1", "F0000001")

	a := tmpFile(t, "trace.pdf", "aaaa")
	b := tmpFile(t, "out.log", "bbbb")

	code, stdout, stderr := sez(t, s, "--send", "--file", a, "--file", b, "#eng", "both attached")
	if code == 0 {
		t.Fatalf("exit = 0, want failure (stdout: %s)", stdout)
	}
	for _, r := range s.Requests() {
		if r.Endpoint == "files.completeUploadExternal" {
			t.Fatal("the sharing leg ran; a failure before it must post nothing")
		}
	}
	// The file that did stage is named, so an upload nobody can see is still
	// findable.
	if !strings.Contains(stderr+stdout, "F0000001") {
		t.Errorf("output did not name the staged file: %q / %q", stdout, stderr)
	}

	recs := readLog(t, logPath)
	last := recs[len(recs)-1]
	if ok, isBool := last["ok"].(bool); !isBool || ok {
		t.Fatalf("ok = %v, want false: a staging failure cannot have posted anything", last["ok"])
	}
}

// The pre-signed upload URL must not reach the terminal or the log by any
// route, including the one that reports a failure.
func TestTheUploadURLNeverReachesTheOutputOrTheLog(t *testing.T) {
	logPath := isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	// A URL nothing answers at, so the transfer fails after the URL is known.
	s.Reply("files.getUploadURLExternal",
		`{"ok":true,"file_id":"F0000001","upload_url":"https://127.0.0.1:1/u?X-Amz-Signature=CANARY42"}`)
	saved := uploadHosts
	uploadHosts = slackhosts.Allow("127.0.0.1")
	t.Cleanup(func() { uploadHosts = saved })

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, stdout, stderr := sez(t, s, "--send", "--file", a, "#eng", "here")
	if code == 0 {
		t.Fatal("exit = 0, want the transfer to have failed")
	}
	body, _ := os.ReadFile(logPath)
	for _, where := range []struct{ name, text string }{
		{"stdout", stdout}, {"stderr", stderr}, {"the audit log", string(body)},
	} {
		if strings.Contains(where.text, "CANARY42") {
			t.Errorf("%s carries the upload URL's credential:\n%s", where.name, where.text)
		}
	}

	// A transfer that failed posted nothing, so it is failed and not unknown.
	// Unknown is what forbids an automatic retry; recording it here would ask a
	// person to go and check Slack for a message that cannot exist.
	recs := readLog(t, logPath)
	last := recs[len(recs)-1]
	if ok, isBool := last["ok"].(bool); !isBool || ok {
		t.Fatalf("ok = %v, want false: a transfer failure cannot have posted anything", last["ok"])
	}
}

// ---------------------------------------------------------------- pre-flight

func TestEveryPreflightRefusalCostsNoRequest(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(dir, "huge.bin")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: the size is a claim about the inode, which is what pre-flight reads.
	if err := f.Truncate(1_000_000_001); err != nil {
		t.Skipf("sparse files unavailable: %v", err)
	}
	f.Close()

	real1 := tmpFile(t, "trace.pdf", "aaaa")
	link := filepath.Join(filepath.Dir(real1), "same.pdf")
	if err := os.Symlink(real1, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	eleven := make([]string, 0, 22)
	for i := 0; i < 11; i++ {
		eleven = append(eleven, "--file", tmpFile(t, fmt.Sprintf("f%d.log", i), "x"))
	}

	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"missing path", []string{"--file", filepath.Join(dir, "nope.pdf")}, "nope.pdf"},
		{"a directory", []string{"--file", dir}, "director"},
		{"an empty file", []string{"--file", empty}, "empty"},
		{"over the ceiling", []string{"--file", huge}, "large"},
		{"the same file twice", []string{"--file", real1, "--file", link}, "same file"},
		{"too many files", eleven, "10"},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			seedCache(t)
			s := slacktest.New(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

			args := append(append([]string{"--send"}, c.args...), "#eng", "text")
			code, _, stderr := sez(t, s, args...)
			if code == 0 {
				t.Fatal("exit = 0, want a refusal")
			}
			if s.Hits() != 0 {
				t.Fatalf("Slack received %d requests; a pre-flight refusal must cost none", s.Hits())
			}
			if !strings.Contains(strings.ToLower(stderr), c.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------- the text

// Omitting the text with --file means no comment. Without this rule sez would
// read stdin to EOF, which on a terminal is a hang.
func TestWithFilesAndNoTextNothingIsReadFromStdin(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	stages(s, sinkURL, "F0000001")
	completes(s, "1.1", "F0000001")

	a := tmpFile(t, "trace.pdf", "aaaa")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--send", "--file", a, "#eng"}, refusingReader{t}, &stdout, &stderr, s.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	for _, r := range s.Requests() {
		if r.Endpoint != "files.completeUploadExternal" {
			continue
		}
		if got := r.BodyFields(); strings.Contains(strings.Join(got, ","), "initial_comment") {
			t.Fatalf("body fields = %v, want no initial_comment when none was given", got)
		}
	}
}

func TestWithFilesADashStillReadsTheCommentFromStdin(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	stages(s, sinkURL, "F0000001")
	completes(s, "1.1", "F0000001")

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, _, stderr := sezStdin(t, s, "piped comment\n", "--send", "--file", a, "#eng", "-")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, r := range s.Requests() {
		if r.Endpoint == "files.completeUploadExternal" {
			if got := r.Body.Get("initial_comment"); got != "piped comment" {
				t.Fatalf("initial_comment = %q, want what was piped", got)
			}
			return
		}
	}
	t.Fatal("no sharing leg was received")
}

// Asking for a comment and supplying blank space is a mistake, not a request
// for no comment: omitting the argument is how that is asked for.
func TestAWhitespaceOnlyCommentIsStillRefused(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, _, _ := sezStdin(t, s, "   \n", "--send", "--file", a, "#eng", "-")
	if code == 0 {
		t.Fatal("exit = 0, want a refusal")
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests, want none", s.Hits())
	}
}

// Without --file the old rule stands: an empty message is refused.
func TestWithoutFilesAnEmptyMessageIsStillRefused(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	code, _, _ := sezStdin(t, s, "", "--send", "#eng", "-")
	if code == 0 {
		t.Fatal("exit = 0, want the empty message refused")
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests, want none", s.Hits())
	}
}

// ---------------------------------------------------------------- refusals

func TestFilesCannotBeAttachedToAnExistingMessage(t *testing.T) {
	for _, verb := range []string{"--edit", "--delete"} {
		t.Run(verb, func(t *testing.T) {
			isolate(t)
			seedCache(t)
			s := slacktest.New(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

			a := tmpFile(t, "trace.pdf", "aaaa")
			code, _, stderr := sez(t, s, "--send", verb, "--file", a, "#eng", "1785712596.256909", "text")
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)", code)
			}
			if s.Hits() != 0 {
				t.Fatalf("Slack received %d requests, want none", s.Hits())
			}
			if !strings.Contains(stderr, "--file") {
				t.Errorf("stderr = %q, want it to name the combination refused", stderr)
			}
		})
	}
}

func TestADashIsNotAFilename(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	code, _, _ := sez(t, s, "--send", "--file", "-", "#eng", "text")
	if code == 0 {
		t.Fatal("exit = 0, want --file - refused")
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests, want none", s.Hits())
	}
}

// ---------------------------------------------------------------- the render

func TestTheDryRunShowsEachFileByItsAbsolutePath(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	a := tmpFile(t, "trace.pdf", strings.Repeat("a", 2048))
	code, stdout, _ := sez(t, s, "--file", a, "#eng", "here's the repro")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests; a dry run sends nothing", s.Hits())
	}
	for _, want := range []string{a, "2.0 KB", "here's the repro"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("render = %q, want it to show %q", stdout, want)
		}
	}
}

func TestTheDryRunSaysWhenThereIsNoComment(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	a := tmpFile(t, "trace.pdf", "aaaa")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--file", a, "#eng"}, refusingReader{t}, &stdout, &stderr, s.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no comment") {
		t.Errorf("render = %q, want it to say there is no comment", stdout.String())
	}
}

// The render is what a person reads before authorising a send, so it is
// exactly the wrong surface to be forgeable by a filename.
func TestAHostileFilenameCannotRestructureTheRender(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	name := "in" + string(rune(0x1b)) + "[31mnocent" + string(rune(0x202e)) + "fdp.scr"
	a := tmpFile(t, name, "aaaa")
	code, stdout, stderr := sez(t, s, "--file", a, "#eng", "look")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, banned := range []rune{0x1b, 0x202e} {
		if strings.ContainsRune(stdout, banned) {
			t.Errorf("render carries %U; a filename must not drive the terminal", banned)
		}
	}
}

// ---------------------------------------------------------------- the rest

func TestAThreadedUploadCarriesItsParent(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	stages(s, sinkURL, "F0000001")
	completes(s, "1.1", "F0000001")

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, _, stderr := sez(t, s, "--send", "--thread", "1785708834.862009",
		"--file", a, "#eng", "in the thread")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, r := range s.Requests() {
		if r.Endpoint == "files.completeUploadExternal" {
			if got := r.Body.Get("thread_ts"); got != "1785708834.862009" {
				t.Fatalf("thread_ts = %q, want the parent given", got)
			}
			if got := r.Body.Get("initial_comment"); got != "in the thread" {
				t.Fatalf("initial_comment = %q, want it alongside the thread", got)
			}
			return
		}
	}
	t.Fatal("no sharing leg was received")
}

// The allowlist is stronger than the credential gate and applies to an upload
// exactly as it does to a message.
func TestTheWriteAllowlistRefusesAnUploadBeforeAnythingStages(t *testing.T) {
	isolate(t)
	seedCache(t)
	t.Setenv("HERSAID_WRITE_CHANNELS", "#other")
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, _, _ := sez(t, s, "--send", "--file", a, "#eng", "text")
	if code == 0 {
		t.Fatal("exit = 0, want the allowlist to refuse")
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests, want none", s.Hits())
	}
}

// completeUploadExternal takes a conversation id, so a name the cache cannot
// resolve has to fail before any byte moves rather than after all of them have.
func TestAnUnresolvedChannelFailsBeforeAnythingStages(t *testing.T) {
	isolate(t)
	// No seedCache: the conversation list is empty.
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, _, _ := sez(t, s, "--send", "--file", a, "#eng", "text")
	if code == 0 {
		t.Fatal("exit = 0, want the unresolved channel refused")
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests; resolution must precede staging", s.Hits())
	}
}

func TestMarkdownIsConvertedInTheComment(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	stages(s, sinkURL, "F0000001")
	completes(s, "1.1", "F0000001")

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, _, stderr := sez(t, s, "--send", "--md", "--file", a, "#eng", "**bold**")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, r := range s.Requests() {
		if r.Endpoint == "files.completeUploadExternal" {
			if got := r.Body.Get("initial_comment"); got != "*bold*" {
				t.Fatalf("initial_comment = %q, want mrkdwn", got)
			}
			return
		}
	}
	t.Fatal("no sharing leg was received")
}

// A reply with no share timestamp is a success anchored on the file ids.
func TestAnUploadWithoutAShareTimestampStillSucceeds(t *testing.T) {
	logPath := isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	stages(s, sinkURL, "F0000001")
	completes(s, "", "F0000001")

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, stdout, stderr := sez(t, s, "--send", "--file", a, "#eng", "text")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "F0000001") {
		t.Errorf("stdout = %q, want the file id as the anchor", stdout)
	}
	last := readLog(t, logPath)[len(readLog(t, logPath))-1]
	if last["ts"] != nil {
		t.Errorf("ts = %v, want null when the reply carried no share", last["ts"])
	}
	if ok, _ := last["ok"].(bool); !ok {
		t.Errorf("ok = %v, want true", last["ok"])
	}
}

// The host check must refuse, not merely be consulted. Every other test puts
// the sink inside uploadHosts, so an implementation that never checked — or
// that allowed everything — would pass them all while sending file bytes to
// whatever host a leg 1 reply happened to name.
func TestNoByteLeavesForAHostOutsideTheAllowlist(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, k := sinkOnly(t) // served, deliberately not permitted
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	stages(s, sinkURL, "F0000001")

	a := tmpFile(t, "trace.pdf", "aaaa")
	code, _, stderr := sez(t, s, "--send", "--file", a, "#eng", "text")
	if code == 0 {
		t.Fatal("exit = 0, want the upload host refused")
	}
	if hits, _, _ := k.snapshot(); hits != 0 {
		t.Fatalf("the unpermitted host received %d transfers, want none", hits)
	}
	if strings.Contains(stderr, "CANARY42") {
		t.Errorf("the refusal quoted the upload URL: %q", stderr)
	}
	for _, r := range s.Requests() {
		if r.Endpoint == "files.completeUploadExternal" {
			t.Fatal("the sharing leg ran after a refused transfer")
		}
	}
}

// The intent line is the record that survives SIGKILL, so it has to be on disk
// before the first request — not merely first in the file, which is also true
// of an implementation that wrote it afterwards.
func TestTheIntentIsOnDiskBeforeTheFirstRequest(t *testing.T) {
	logPath := isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	var atFirstRequest []string
	s.ReplyFunc("files.getUploadURLExternal", func(w http.ResponseWriter, r *http.Request) {
		if atFirstRequest == nil {
			atFirstRequest = phases(readLog(t, logPath))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"file_id":"F0000001","upload_url":"%s/u?X-Amz-Signature=CANARY42"}`, sinkURL)
	})
	completes(s, "1.1", "F0000001")

	a := tmpFile(t, "trace.pdf", "aaaa")
	if code, _, stderr := sez(t, s, "--send", "--file", a, "#eng", "text"); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if len(atFirstRequest) != 1 || atFirstRequest[0] != "intent" {
		t.Fatalf("phases when the first request arrived = %v, want exactly [intent]", atFirstRequest)
	}
}

// A hostile name is likelier to appear in a refusal than in a successful
// render, so the refusal is the path that most needs the escaping.
func TestAHostileFilenameCannotDriveTheTerminalThroughARefusal(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	name := "empty" + string(rune(0x1b)) + "[31m" + string(rune(0x202e)) + ".log"
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, nil, 0o600); err != nil { // empty: refused by pre-flight
		t.Fatal(err)
	}
	code, stdout, stderr := sez(t, s, "--send", "--file", path, "#eng", "text")
	if code == 0 {
		t.Fatal("exit = 0, want the empty file refused")
	}
	for _, banned := range []rune{0x1b, 0x202e} {
		if strings.ContainsRune(stderr+stdout, banned) {
			t.Errorf("the refusal carries %U; a filename must not drive the terminal", banned)
		}
	}
}

// --------------------------------------------------------------- signals

// Interrupted while staging, the record says failed: nothing is visible until
// the sharing leg, so there is no doubt to record. The file that already
// staged is named, because an upload nobody can see is still findable.
func TestASignalWhileStagingRecordsAFailureAndNamesWhatStaged(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a subprocess")
	}
	logPath := isolate(t)
	seedCache(t)
	s := slacktest.New(t)

	arrived := make(chan struct{})
	var once sync.Once
	k := &sink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		k.hits++
		n := k.hits
		k.mu.Unlock()
		if n == 1 { // the first file lands
			w.WriteHeader(http.StatusOK)
			return
		}
		once.Do(func() { close(arrived) }) // the second is held open
		<-releaseOn(t, r)
	}))
	t.Cleanup(srv.Close)
	release(t)
	stages(s, srv.URL, "F0000001", "F0000002")

	a := tmpFile(t, "one.pdf", "aaaa")
	b := tmpFile(t, "two.log", "bbbb")
	cmd := startHelper(t, s, "--send", "--file", a, "--file", b, "#eng", "text")
	waitFor(t, cmd, arrived)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitOrKill(t, cmd)

	recs := readLog(t, logPath)
	if len(recs) < 2 {
		t.Fatalf("records = %v, want an intent and an outcome", phases(recs))
	}
	last := recs[len(recs)-1]
	if ok, isBool := last["ok"].(bool); !isBool || ok {
		t.Fatalf("ok = %v, want false; staging cannot have posted anything", last["ok"])
	}
	// The id of what did stage, which only a live progress value can supply:
	// a snapshot taken before the first request cannot know it.
	if !strings.Contains(string(mustRead(t, logPath)), "F0000001") {
		t.Error("the record does not name the file that staged")
	}
}

// Interrupted once the sharing leg is on the wire, the answer is genuinely
// unknown — and unknown is what forbids the automatic retry that would
// duplicate a message which may well have been delivered.
func TestASignalWhileSharingRecordsUnknownExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a subprocess")
	}
	logPath := isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := sinkOnly(t)
	stages(s, sinkURL, "F0000001")

	arrived := make(chan struct{})
	s.ReplyFunc("files.completeUploadExternal", func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-releaseOn(t, r)
	})
	release(t)

	a := tmpFile(t, "trace.pdf", "aaaa")
	cmd := startHelper(t, s, "--send", "--file", a, "#eng", "text")
	waitFor(t, cmd, arrived)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitOrKill(t, cmd)

	recs := readLog(t, logPath)
	var outcomes int
	for _, r := range recs {
		if p, _ := r["phase"].(string); p != "intent" {
			outcomes++
		}
	}
	// One lock, one written flag: whichever of the handler and the normal path
	// gets there first writes, and the other does not.
	if outcomes != 1 {
		t.Fatalf("phases = %v, want exactly one record that is not the intent", phases(recs))
	}
	last := recs[len(recs)-1]
	if last["ok"] != nil {
		t.Fatalf("ok = %v, want null: the sharing leg was in flight", last["ok"])
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// releaseOn is what a held-open handler blocks on: the request being cancelled,
// or the test finishing. Without the second, an implementation that never
// closes the connection leaves the handler running, and httptest.Server.Close
// waits for it — so a broken implementation hangs the suite instead of failing
// it.
func releaseOn(t *testing.T, r *http.Request) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
		case <-releaseChan(t):
		}
		close(out)
	}()
	return out
}

var releases sync.Map // *testing.T -> chan struct{}

func releaseChan(t *testing.T) chan struct{} {
	v, _ := releases.LoadOrStore(t, make(chan struct{}))
	return v.(chan struct{})
}

// release arranges for every held handler in this test to be let go before the
// servers are closed. Registered after the server's own cleanup so that it runs
// first: t.Cleanup is last-in, first-out.
func release(t *testing.T) {
	t.Cleanup(func() {
		close(releaseChan(t))
		releases.Delete(t)
	})
}

// waitOrKill bounds the wait for the subprocess. An implementation that catches
// the signal and then blocks must fail this test rather than stall the suite.
func waitOrKill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("the process did not exit within 20s of SIGTERM")
	}
}

// The one-outcome-line invariant, pinned where it can actually be contended,
// and pinned on durability rather than on bookkeeping.
//
// The subprocess test above cannot do it: after SIGTERM the process exits once
// the handler returns, so the normal path never races it. And a test that only
// raced a flag would pass an implementation that set the flag and then wrote
// outside the lock — which is the shape that loses the record, because
// audit.OnAbort exits the process the moment the handler returns.
//
// So every goroutine that returns from writeOutcome must already be able to see
// the record on disk. A loser that returned first would be free to exit.
func TestNoWriterReturnsBeforeTheOutcomeIsDurable(t *testing.T) {
	for round := 0; round < 40; round++ {
		path := filepath.Join(t.TempDir(), "posts.jsonl")
		log, err := audit.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		p := &progress{}
		rec := audit.Post{Phase: audit.Outcome, Channel: "#eng", OK: audit.Succeeded}

		var wg sync.WaitGroup
		var early atomic.Int32
		var wrote atomic.Int32
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				before := p.writtenAlready()
				if err := p.writeOutcome(io.Discard, log, rec); err != nil {
					t.Error(err)
				}
				if !before {
					wrote.Add(1)
				}
				b, _ := os.ReadFile(path)
				if !bytes.Contains(b, []byte(`"phase":"outcome"`)) {
					early.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		log.Close()

		if n := early.Load(); n != 0 {
			t.Fatalf("round %d: %d writers returned before the record was durable", round, n)
		}
		b, _ := os.ReadFile(path)
		if n := bytes.Count(b, []byte("\n")); n != 1 {
			t.Fatalf("round %d: %d outcome lines, want exactly 1", round, n)
		}
	}
}

// The handler reads the progress value while Stage is appending to it. Without
// the mutex this is a data race, which is a bug the race detector reports and
// nothing else would.
func TestTheProgressValueIsSafeToReadWhileItGrows(t *testing.T) {
	p := &progress{}
	base := audit.Post{Channel: "#eng", Files: []audit.File{{Name: "a"}, {Name: "b"}}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p.stagedFile("F1")
			p.enter(completing)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = p.recordForLocked(base, syscall.SIGTERM)
		}
	}()
	wg.Wait()
}

// The refusal path is where a hostile filename is likeliest to appear, and it
// is the path that was escaping it everywhere except pre-flight. A file that
// passes pre-flight and is then refused a ticket carries its own name into the
// error, and from there to the terminal.
func TestAHostileFilenameCannotDriveTheTerminalThroughAStagingRefusal(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s.Reply("files.getUploadURLExternal", `{"ok":false,"error":"file_uploads_disabled"}`)

	name := "in" + string(rune(0x1b)) + "[31mnocent" + string(rune(0x202e)) + "fdp.pdf"
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("real content"), 0o600); err != nil {
		t.Skipf("the filesystem refused the name: %v", err)
	}

	code, stdout, stderr := sez(t, s, "--send", "--file", path, "#eng", "text")
	if code == 0 {
		t.Fatal("exit = 0, want the refused ticket to fail the send")
	}
	for _, banned := range []rune{0x1b, 0x202e} {
		if strings.ContainsRune(stderr+stdout, banned) {
			t.Errorf("output carries %U from the filename; a refusal must not drive the terminal", banned)
		}
	}
}

// The note after a failed stage says how far the send got, not that there is
// something to go and deal with. An upload Slack never completes is discarded —
// files.info on one of these ids answers file_deleted immediately — so a note
// telling somebody to clean them up sends them looking for nothing.
func TestTheStagedNoteDoesNotPromiseSomethingToCleanUp(t *testing.T) {
	isolate(t)
	seedCache(t)
	s := slacktest.New(t)
	sinkURL, _ := uploadSink(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	s.Pages("files.getUploadURLExternal",
		fmt.Sprintf(`{"ok":true,"file_id":"F0000001","upload_url":"%s/u?X-Amz-Signature=CANARY42"}`, sinkURL),
		`{"ok":false,"error":"file_uploads_disabled"}`)

	a := tmpFile(t, "one.pdf", "aaaa")
	b := tmpFile(t, "two.log", "bbbb")
	code, _, stderr := sez(t, s, "--send", "--file", a, "--file", b, "#eng", "text")
	if code == 0 {
		t.Fatal("exit = 0, want the failure")
	}
	if !strings.Contains(stderr, "F0000001") {
		t.Fatalf("stderr = %q, want the id that reached Slack", stderr)
	}
	if !strings.Contains(stderr, "discarded") {
		t.Fatalf("stderr = %q, want it to say the upload is discarded", stderr)
	}
	if !strings.Contains(stderr, "nothing to clean up") {
		t.Fatalf("stderr = %q, want it to say there is nothing to do", stderr)
	}
	// The old wording sent people looking for files that do not exist.
	if strings.Contains(stderr, "never shared:") {
		t.Fatalf("stderr = %q, want the note not to imply something was left behind", stderr)
	}
}
