package slackfiles

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slackhosts"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

const token = "xoxp-fake-read-token"

// loopback is what a test server answers on. Production permits the hosts Slack
// serves files from; a test permits the one standing in for them.
var loopback = slackhosts.Allow("127.0.0.1")

// readCred builds a credential from the environment, which is the only way to
// make one outside package cred — deliberately, since the token field is
// unexported so that no test can assemble a credential a user never enrolled.
func readCred(t *testing.T) cred.Read {
	t.Helper()
	t.Setenv("SLACK_READ_TOKEN", token)
	c, err := cred.ResolveRead()
	if err != nil {
		t.Fatalf("ResolveRead() error = %v", err)
	}
	return c
}

// fileHost serves body at /file and records what it was asked with.
func fileHost(t *testing.T, body []byte, contentType string) (*httptest.Server, *[]string) {
	t.Helper()
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &auths
}

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %s: %v", rawURL, err)
	}
	return u.Hostname()
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %s: %v", rawURL, err)
	}
	return u.Port()
}

func client(t *testing.T, hosts slackhosts.Hosts) *Client {
	t.Helper()
	return New(readCred(t), 5*time.Second, hosts)
}

func hosted(name, downloadURL string, size int64) slackread.File {
	return slackread.File{
		ID: "F0123ABCD", Name: name, Mimetype: "application/pdf",
		Size: size, Mode: "hosted", URLPrivateDownload: downloadURL,
	}
}

// A name is chosen by whoever uploaded the file, so it is untrusted input to a
// path. No name may cause a write outside the directory that was asked for.
func TestAHostileNameCannotEscapeTheDirectory(t *testing.T) {
	body := []byte("the file")
	srv, _ := fileHost(t, body, "application/pdf")
	c := client(t, loopback)

	for _, name := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		`..\..\windows\system32\config`,
		"..",
		".",
		"",
		"sub/dir/report.pdf",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "eng-123.456")
			f := hosted(name, srv.URL+"/file", int64(len(body)))

			results := c.Fetch(context.Background(), []slackread.File{f}, filepath.Dir(dir), filepath.Base(dir), 1<<20)
			if results[0].Status != Written {
				t.Fatalf("status = %s (%s), want written", results[0].Status, results[0].Reason)
			}
			// The written path is inside the message directory, and the only
			// thing under the root is that directory.
			if parent := filepath.Dir(results[0].Path); parent != dir {
				t.Fatalf("wrote to %s, which is not inside %s", results[0].Path, dir)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "eng-123.456" {
				t.Fatalf("root holds %v, want only the message directory", entries)
			}
		})
	}
}

// Without files:read Slack answers 200 with its sign-in page rather than with
// an error. Written blindly that is a PDF containing a login form.
func TestASignInPageIsRefusedRatherThanWritten(t *testing.T) {
	srv, _ := fileHost(t, []byte("<html><body>Sign in to Slack</body></html>"), "text/html; charset=utf-8")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	results := c.Fetch(context.Background(), []slackread.File{hosted("trace.pdf", srv.URL+"/file", 41)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "files:read") {
		t.Fatalf("reason = %q, want it to name the missing scope", results[0].Reason)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Fatalf("directory holds %v, want nothing written", entries)
	}
}

// An HTML file is still a file: the refusal is about the body not being what
// was asked for, not about HTML as such.
func TestAnHTMLAttachmentStillDownloads(t *testing.T) {
	body := []byte("<html><body>a real page someone uploaded</body></html>")
	srv, _ := fileHost(t, body, "text/html")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	f := hosted("page.html", srv.URL+"/file", int64(len(body)))
	f.Mimetype = "text/html"
	results := c.Fetch(context.Background(), []slackread.File{f}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Written {
		t.Fatalf("status = %s (%s), want written", results[0].Status, results[0].Reason)
	}
}

// Slack's declared size is metadata about the file, not a promise about the
// response, so the ceiling has to hold when the two disagree.
func TestTheCeilingHoldsWhenTheDeclaredSizeUnderstatesTheBody(t *testing.T) {
	srv, _ := fileHost(t, make([]byte, 5000), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	// Declared as 10 bytes, so the pre-check lets it through; 5000 arrive.
	results := c.Fetch(context.Background(), []slackread.File{hosted("big.pdf", srv.URL+"/file", 10)}, filepath.Dir(dir), filepath.Base(dir), 1000)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "1000") {
		t.Fatalf("reason = %q, want it to name the limit", results[0].Reason)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Fatalf("directory holds %v, want nothing left behind", entries)
	}
}

// A file of exactly the limit is within it.
func TestAFileOfExactlyTheLimitIsWritten(t *testing.T) {
	srv, _ := fileHost(t, make([]byte, 1000), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	results := c.Fetch(context.Background(), []slackread.File{hosted("exact.pdf", srv.URL+"/file", 1000)}, filepath.Dir(dir), filepath.Base(dir), 1000)

	if results[0].Status != Written {
		t.Fatalf("status = %s (%s), want written", results[0].Status, results[0].Reason)
	}
	if results[0].Bytes != 1000 {
		t.Fatalf("wrote %d bytes, want 1000", results[0].Bytes)
	}
}

// A redirect may not send a request carrying a bearer token to a host chosen by
// whoever controls the response.
func TestARedirectToAnUnlistedHostIsRefused(t *testing.T) {
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A host the allowlist does not name. It is never resolved, because the
		// refusal happens before the hop is taken.
		http.Redirect(w, r, "https://storage.example.invalid/file", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", redirector.URL+"/file", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "refusing a redirect") {
		t.Fatalf("reason = %q, want it to name the refusal", results[0].Reason)
	}
	if !strings.Contains(results[0].Reason, "storage.example.invalid") {
		t.Fatalf("reason = %q, want it to name the host that was refused", results[0].Reason)
	}
}

// A refused redirect must not report the URL it refused.
//
// Slack hands file bytes off to object storage with a pre-signed URL whose
// query string is the credential. url.URL.Redacted masks a userinfo password
// and nothing else, so naming the target would put an AWS signature into the
// Result reason, onto the terminal, and into --json — a credential disclosed by
// the very code that refused to send one.
func TestARefusedRedirectDoesNotReportTheURL(t *testing.T) {
	const canary = "X-Amz-Signature=DEADBEEFCAFE"
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://storage.example.invalid/f?"+canary, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", redirector.URL+"/file", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	for _, banned := range []string{canary, "DEADBEEFCAFE", "?"} {
		if strings.Contains(results[0].Reason, banned) {
			t.Fatalf("reason = %q, want no trace of %q from the refused URL", results[0].Reason, banned)
		}
	}
}

// A body that stops short of the size Slack declared is a truncation the
// transport did not report. Written, it becomes a file that looks complete and
// is not — which is worse than a failure, because nothing later can tell.
func TestABodyShorterThanTheDeclaredSizeIsRefused(t *testing.T) {
	srv, _ := fileHost(t, []byte("short"), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", srv.URL+"/file", 5000)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed; 5 bytes is not the 5000 Slack declared", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "fewer than") {
		t.Fatalf("reason = %q, want it to say the body was short", results[0].Reason)
	}
	if _, err := os.Stat(filepath.Join(dir, "trace.pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the short body was written; nothing incomplete may be left in place")
	}
}

// The message directory is derived from Slack's reply, so a local process that
// plants a symlink there must not be able to steer authenticated content out of
// --out. os.Root refuses the traversal; without it every step would follow the
// link.
func TestASymlinkedMessageDirectoryCannotEscapeTheOutputRoot(t *testing.T) {
	out := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(out, "eng-123.456")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	srv, _ := fileHost(t, []byte("the file"), "application/pdf")
	c := client(t, loopback)

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", srv.URL+"/file", 8)}, out, "eng-123.456", 1<<20)

	if results[0].Status == Written {
		t.Fatalf("status = written at %s; the symlink was followed out of --out", results[0].Path)
	}
	if entries, err := os.ReadDir(elsewhere); err == nil && len(entries) > 0 {
		t.Fatalf("%d entries landed outside --out", len(entries))
	}
}

// Two names that differ only in case are one path on the filesystem this ships
// on, so the second must be given its file id rather than left to collide at
// placement.
func TestNamesDifferingOnlyInCaseBothLand(t *testing.T) {
	srv, _ := fileHost(t, []byte("the file"), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	results := c.Fetch(context.Background(), []slackread.File{
		hosted("Trace.pdf", srv.URL+"/file", 8),
		hosted("trace.pdf", srv.URL+"/file", 8),
	}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	for i, r := range results {
		if r.Status != Written {
			t.Fatalf("file %d: status = %s (%s), want both written", i, r.Status, r.Reason)
		}
	}
	if results[0].Path == results[1].Path {
		t.Fatalf("both files landed at %s; the second must be uniquified", results[0].Path)
	}
}

// The host set bounds everywhere the bearer may go, not only where a redirect
// may take it. The download URL comes from Slack's own reply — but so would a
// hostile one, if that reply were ever not Slack's.
func TestTheInitialHostIsCheckedToo(t *testing.T) {
	srv, auths := fileHost(t, []byte("the file"), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, slackhosts.Allow("example.invalid"))

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", srv.URL+"/file", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "not a host Slack serves files from") {
		t.Fatalf("reason = %q, want it to name the refusal", results[0].Reason)
	}
	if len(*auths) != 0 {
		t.Fatalf("the server received %d requests, want 0", len(*auths))
	}
}

// A bearer on the wire in clear is what every other rule here exists to
// prevent, so a plaintext URL is refused before the request is built.
func TestAPlaintextDownloadURLIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, slackhosts.Allow("files.slack.com"))

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", "http://files.slack.com/f", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "in clear") {
		t.Fatalf("reason = %q, want it to name the plaintext refusal", results[0].Reason)
	}
}

// The credential is what must not follow a redirect. A pre-signed URL carries
// its own authorisation, so the bearer is both unnecessary there and exactly
// what must not leak.
func TestTheBearerIsDroppedWhenTheHostChanges(t *testing.T) {
	final, finalAuths := fileHost(t, []byte("the file"), "application/pdf")
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/file", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	dir := filepath.Join(t.TempDir(), "eng-123.456")
	// Both servers are on the loopback address, so they differ by port alone —
	// which Go's own cross-domain rule would treat as the same host and copy the
	// header across. The drop here is this package's, and this is what holds it
	// to it.
	c := client(t, slackhosts.Allow(hostOf(t, final.URL)))

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", redirector.URL+"/file", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Written {
		t.Fatalf("status = %s (%s), want written", results[0].Status, results[0].Reason)
	}
	if len(*finalAuths) != 1 {
		t.Fatalf("the redirect target received %d requests, want 1", len(*finalAuths))
	}
	if got := (*finalAuths)[0]; got != "" {
		t.Fatalf("the redirect target saw Authorization %q, want none", got)
	}
}

// The host the request started on is the one the credential belongs to, so a
// hop that stays there keeps it.
func TestTheBearerSurvivesARedirectOnTheSameHost(t *testing.T) {
	var auths []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if r.URL.Path == "/file" {
			http.Redirect(w, r, srv.URL+"/bytes", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("the file"))
	}))
	t.Cleanup(srv.Close)

	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, slackhosts.Allow(hostOf(t, srv.URL)))

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", srv.URL+"/file", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Written {
		t.Fatalf("status = %s (%s), want written", results[0].Status, results[0].Reason)
	}
	if len(auths) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(auths))
	}
	if auths[1] != "Bearer "+token {
		t.Fatalf("second request carried %q, want the bearer", auths[1])
	}
}

// A chain that never ends is a chain that spins here until the process is
// killed.
func TestARedirectLoopIsBounded(t *testing.T) {
	var hits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, srv.URL+"/file", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, slackhosts.Allow(hostOf(t, srv.URL)))

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", srv.URL+"/file", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if got := hits.Load(); got > maxHops+1 {
		t.Fatalf("server saw %d requests, want no more than %d", got, maxHops+1)
	}
}

// A file already on disk is someone's, and a download is not entitled to
// replace it.
func TestAnExistingFileIsNotOverwritten(t *testing.T) {
	srv, _ := fileHost(t, []byte("the new bytes"), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "trace.pdf")
	if err := os.WriteFile(existing, []byte("the old bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := client(t, loopback)

	results := c.Fetch(context.Background(), []slackread.File{hosted("trace.pdf", srv.URL+"/file", 13)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	kept, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "the old bytes" {
		t.Fatalf("file now holds %q, want it untouched", kept)
	}
}

// Two files on one message can share a name; neither may overwrite the other.
func TestTwoFilesWithOneNameBothLand(t *testing.T) {
	srv, _ := fileHost(t, []byte("bytes"), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	first := hosted("trace.pdf", srv.URL+"/file", 5)
	second := hosted("trace.pdf", srv.URL+"/file", 5)
	second.ID = "F9999ZZZZ"

	results := c.Fetch(context.Background(), []slackread.File{first, second}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	for i, r := range results {
		if r.Status != Written {
			t.Fatalf("file %d: status = %s (%s), want written", i, r.Status, r.Reason)
		}
	}
	if results[0].Path == results[1].Path {
		t.Fatalf("both files wrote to %s", results[0].Path)
	}
	if got := filepath.Base(results[1].Path); got != "trace-F9999ZZZZ.pdf" {
		t.Fatalf("second file is %s, want the id before the extension", got)
	}
}

// A file with nothing behind it is not a failure to report as one, and it must
// not stop the files that do have bytes.
func TestFilesWithNoBytesAreSkippedAndTheRestStillLand(t *testing.T) {
	body := []byte("the file")
	srv, _ := fileHost(t, body, "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	files := []slackread.File{
		{ID: "F1", Name: "design-notes", Mode: "external", IsExternal: true},
		hosted("trace.pdf", srv.URL+"/file", int64(len(body))),
		{ID: "F3", Name: "gone.pdf", Mode: "tombstone"},
		{ID: "F4", Name: "mail", Mode: "email"},
		{ID: "F5", Name: "nolink.pdf", Mode: "hosted"},
		{ID: "F6", Name: "huge.mp4", Mode: "hosted", Size: 1 << 40, URLPrivateDownload: srv.URL + "/file"},
	}
	results := c.Fetch(context.Background(), files, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	want := []Status{Skipped, Written, Skipped, Skipped, Skipped, Skipped}
	for i, w := range want {
		if results[i].Status != w {
			t.Fatalf("file %d: status = %s (%s), want %s", i, results[i].Status, results[i].Reason, w)
		}
	}
	if got, err := os.ReadFile(results[1].Path); err != nil || string(got) != string(body) {
		t.Fatalf("wrote %q (err %v), want the body", got, err)
	}
}

// A message whose files are all links leaves nothing behind, not an empty
// directory that looks like a download happened.
func TestNothingToWriteLeavesNoDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "eng-123.456")
	c := client(t, loopback)

	c.Fetch(context.Background(),
		[]slackread.File{{ID: "F1", Name: "a link", Mode: "external"}}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(%s) error = %v, want it not to exist", dir, err)
	}
}

// A body that stops early is not the file, and what arrived must not be left
// looking like it.
func TestATruncatedBodyLeavesNothingBehind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", "5000")
		w.Write(make([]byte, 100))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Closing without the promised bytes is what a dropped transfer is.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
		}
	}))
	t.Cleanup(srv.Close)

	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	results := c.Fetch(context.Background(), []slackread.File{hosted("trace.pdf", srv.URL+"/file", 5000)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory holds %v, want nothing — not a partial file and not a temporary one", entries)
	}
}

// An error status is a failure, whatever body came with it.
func TestANonOKStatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "nope")
	}))
	t.Cleanup(srv.Close)

	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	results := c.Fetch(context.Background(), []slackread.File{hosted("trace.pdf", srv.URL+"/file", 4)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "404") {
		t.Fatalf("reason = %q, want it to name the status", results[0].Reason)
	}
}

// The zero credential cannot reach the wire, whatever the caller did with
// ResolveRead's error.
func TestAZeroCredentialIssuesNoRequest(t *testing.T) {
	srv, auths := fileHost(t, []byte("the file"), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := New(cred.Read{}, 5*time.Second, SlackHosts)

	results := c.Fetch(context.Background(), []slackread.File{hosted("trace.pdf", srv.URL+"/file", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if len(*auths) != 0 {
		t.Fatalf("server received %d requests, want 0", len(*auths))
	}
}

// The bearer is what a download authenticates with, and it must actually be
// sent — a test suite that only checked refusals would pass with no auth at all.
func TestTheRequestCarriesTheBearer(t *testing.T) {
	srv, auths := fileHost(t, []byte("the file"), "application/pdf")
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, loopback)

	c.Fetch(context.Background(), []slackread.File{hosted("trace.pdf", srv.URL+"/file", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if len(*auths) != 1 || (*auths)[0] != "Bearer "+token {
		t.Fatalf("server saw %v, want one request carrying the bearer", *auths)
	}
}

// Object storage may serve the bytes and may never receive the credential. The
// allowlist that admits it says so, rather than leaving it to the redirect
// policy to be the only thing standing between the two.
func TestStorageIsReachableByRedirectAndNeverWithTheBearer(t *testing.T) {
	for _, c := range []struct {
		host           string
		bearer, viaRed bool
	}{
		{"files.slack.com", true, true},
		{"slack-files.com", true, true},
		{"my-bucket.s3.amazonaws.com", false, true},
		{"amazonaws.com", false, true},
		{"storage.example.invalid", false, false},
	} {
		if got := SlackHosts.PermitsBearer(c.host); got != c.bearer {
			t.Errorf("PermitsBearer(%q) = %v, want %v", c.host, got, c.bearer)
		}
		if got := SlackHosts.PermitsRedirect(c.host); got != c.viaRed {
			t.Errorf("PermitsRedirect(%q) = %v, want %v", c.host, got, c.viaRed)
		}
	}
}

// A download that starts at object storage would put the bearer there, which is
// the one thing admitting that host was supposed not to cost.
func TestADownloadCannotStartAtStorage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, SlackHosts)

	results := c.Fetch(context.Background(),
		[]slackread.File{hosted("trace.pdf", "https://my-bucket.s3.amazonaws.com/f", 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Failed {
		t.Fatalf("status = %s, want failed", results[0].Status)
	}
	if !strings.Contains(results[0].Reason, "not a host Slack serves files from") {
		t.Fatalf("reason = %q, want it to name the refusal", results[0].Reason)
	}
}

// The shape of a real download: it starts at a host that may hold the
// credential and is handed off to storage that may not. Every other redirect
// test permits its target for the bearer too, so this is the only one that
// fails if the hop stops consulting the redirect-only tier.
func TestADownloadIsHandedOffToStorageThatMayNotHoldTheBearer(t *testing.T) {
	storage, storageAuths := fileHost(t, []byte("the file"), "application/pdf")
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+"/file", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	// Both servers answer on the loopback address; addressing the first as
	// "localhost" is what makes them two hosts as far as the allowlist is
	// concerned, standing in for files.slack.com handing off to storage.
	hosts := slackhosts.Allow("localhost").AndViaRedirect("127.0.0.1")
	if hosts.PermitsBearer("127.0.0.1") {
		t.Fatal("the storage host is bearer-permitted; the test cannot show what it claims")
	}

	dir := filepath.Join(t.TempDir(), "eng-123.456")
	c := client(t, hosts)
	start := "http://localhost:" + portOf(t, redirector.URL) + "/file"

	results := c.Fetch(context.Background(), []slackread.File{hosted("trace.pdf", start, 8)}, filepath.Dir(dir), filepath.Base(dir), 1<<20)

	if results[0].Status != Written {
		t.Fatalf("status = %s (%s), want written", results[0].Status, results[0].Reason)
	}
	if len(*storageAuths) != 1 {
		t.Fatalf("storage received %d requests, want 1", len(*storageAuths))
	}
	if got := (*storageAuths)[0]; got != "" {
		t.Fatalf("storage saw Authorization %q, want none", got)
	}
}

// The fallback for filesystems without hard links keeps the guarantee the link
// was chosen for. It cannot be reached through Fetch on any filesystem the
// tests run on, so it is exercised directly.
func TestTheNoHardLinkFallbackStillRefusesToOverwrite(t *testing.T) {
	out := t.TempDir()
	d := &dest{out: out, msgDir: "eng-123.456"}
	t.Cleanup(d.close)
	if err := d.ready(); err != nil {
		t.Fatal(err)
	}
	tmpName, tmp, err := d.temp()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString("the bytes"); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	if err := d.copyExclusive(tmpName, "trace.pdf"); err != nil {
		t.Fatalf("copyExclusive() error = %v", err)
	}
	path := d.path("trace.pdf")
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "the bytes" {
		t.Fatalf("file holds %q (err %v), want the bytes", got, err)
	}

	err = d.copyExclusive(tmpName, "trace.pdf")
	if err == nil {
		t.Fatal("copyExclusive() over an existing file succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "exists") {
		t.Fatalf("error = %v, want it to say the file exists", err)
	}
	if after, _ := os.ReadFile(path); string(after) != "the bytes" {
		t.Fatalf("file now holds %q, want it untouched", after)
	}
}
