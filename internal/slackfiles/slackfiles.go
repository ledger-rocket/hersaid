// Package slackfiles downloads the bytes behind a Slack file.
//
// It is separate from slackread because every response there passes through one
// get() whose contract is that the body is an enveloped JSON document. A file
// is a stream of bytes of unbounded length, so a download either breaks that
// contract or bypasses the function that enforces it. It imports cred.Read
// alone, so a command that downloads still has no write code in its import
// graph.
//
// A file's name is chosen by whoever uploaded it, including an external Slack
// Connect guest, so it is untrusted input to a path and is treated as such
// throughout: nothing derived from it can name a directory.
package slackfiles

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slackhosts"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

// maxHops bounds redirect following. Slack answers a file with the bytes or
// with one redirect to storage; three is slack enough for a chain that is
// genuinely Slack's and short enough that a loop cannot be spun here.
const maxHops = 3

// ErrNoCredential is a client built with the zero credential, mirroring
// slackread: cred.Read{} is constructible in any package, so the refusal lives
// here rather than in each caller's control flow.
var ErrNoCredential = errors.New("no read credential")

// SlackHosts is where Slack serves file bytes from: its own hosts, and the
// object storage its pre-signed URLs point at.
//
// The storage entry is broad — it is anyone's bucket — and so it is reachable
// only by redirect, never as the host a download starts at. No credential
// travels there: the bearer is dropped the moment the host changes, and a
// pre-signed URL carries its own authorisation in the query string. What such a
// host can do is serve bytes, which is what was asked for.
var SlackHosts = slackhosts.Allow("slack.com", "slack-files.com").AndViaRedirect("amazonaws.com")

// Status is what became of one file.
type Status string

const (
	Written Status = "written"
	Skipped Status = "skipped"
	Failed  Status = "failed"
)

// Result is the outcome for one file. Reason is set on Skipped and Failed and
// says which, in words a reader can act on.
type Result struct {
	File   slackread.File
	Status Status
	Path   string
	Bytes  int64
	Reason string
}

// Client downloads files with a read credential.
type Client struct {
	hc    *http.Client
	cred  cred.Read
	hosts slackhosts.Hosts
}

// New returns a client authenticating as c, following redirects only within
// hosts. The host set is a parameter rather than a default so that no code path
// can widen where a request carrying a bearer may be sent without saying so at
// the call site.
func New(c cred.Read, timeout time.Duration, hosts slackhosts.Hosts) *Client {
	cl := &Client{cred: c, hosts: hosts}
	cl.hc = &http.Client{
		Timeout:       timeout,
		Transport:     slackhttp.Transport(),
		CheckRedirect: cl.checkRedirect,
	}
	return cl
}

// permits reports whether a request carrying the bearer may be sent to rawURL.
//
// A plaintext scheme is refused, because a bearer on the wire in clear is the
// thing every other rule here exists to prevent. Loopback is the exception: a
// token that never leaves the machine has not been disclosed by travelling, and
// a test server is the only thing that answers there.
func (c *Client) permits(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("download url: %w", err)
	}
	host := u.Hostname()
	if u.Scheme != "https" && !isLoopback(host) {
		return fmt.Errorf("refusing to download over %s: a bearer may not travel in clear", u.Scheme)
	}
	if !c.hosts.PermitsBearer(host) {
		return fmt.Errorf("refusing to download from %s: not a host Slack serves files from", host)
	}
	return nil
}

func isLoopback(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// redirectError is a refusal to follow a redirect. It carries a hostname and a
// reason and never a URL.
//
// A Location Slack sends points at object storage and carries its credential in
// the query string, and url.URL.Redacted masks a userinfo password only — it
// leaves the query untouched. A refusal that named the target would put an AWS
// signature into the reason of a Result, and from there onto a terminal and
// into --json output. download unwraps this type rather than letting
// http.Client wrap it, for the same reason.
type redirectError struct{ reason string }

func (e *redirectError) Error() string { return e.reason }

// checkRedirect is the whole of the redirect policy. slackhttp refuses every
// redirect because a Location can move an Authorization header to a host of
// someone else's choosing; a file download cannot take that line, because Slack
// may answer with a redirect to storage it does not serve itself. So the header
// is what is refused, rather than the hop.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxHops {
		return &redirectError{fmt.Sprintf("refusing a redirect to %s: more than %d hops",
			req.URL.Hostname(), maxHops)}
	}
	first := via[0].URL
	if first.Scheme == "https" && req.URL.Scheme != "https" {
		return &redirectError{fmt.Sprintf("refusing a redirect to %s: that is a downgrade from https",
			req.URL.Hostname())}
	}
	if !c.hosts.PermitsRedirect(req.URL.Hostname()) {
		return &redirectError{fmt.Sprintf("refusing a redirect to %s: not a host Slack serves files from",
			req.URL.Hostname())}
	}
	// Go drops Authorization across a domain change of its own accord. Doing it
	// here as well means the property holds because this package says so, and a
	// test can hold it to that.
	if req.URL.Host != first.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

// Fetch downloads every file in files into dir, one Result each, in order.
//
// It is best effort: a file with no bytes behind it, one larger than max, and
// one Slack refuses each leave the others alone. dir is created on the first
// file actually written, so a message whose files are all links leaves no empty
// directory behind.
//
// max bounds each file separately, in bytes.
func (c *Client) Fetch(ctx context.Context, files []slackread.File, out, msgDir string, max int64) []Result {
	results := make([]Result, 0, len(files))
	d := &dest{out: out, msgDir: msgDir}
	defer d.close()

	taken := map[string]bool{}
	for _, f := range files {
		if reason := skipReason(f, max); reason != "" {
			results = append(results, Result{File: f, Status: Skipped, Reason: reason})
			continue
		}
		name := unique(safeName(f), f.ID, taken)
		taken[foldName(name)] = true
		n, err := c.download(ctx, f, d, name, max)
		if err != nil {
			results = append(results, Result{File: f, Status: Failed, Reason: err.Error()})
			continue
		}
		results = append(results, Result{File: f, Status: Written, Path: d.path(name), Bytes: n})
	}
	return results
}

// skipReason names why a file has no bytes to fetch, or is empty when it has.
// The size test is against what Slack says the file is: it saves a download
// that would be abandoned anyway, and the real bound is enforced on the bytes
// that arrive.
func skipReason(f slackread.File, max int64) string {
	switch {
	case f.Mode == "external" || f.IsExternal:
		return "external file, no bytes to fetch"
	case f.Mode == "tombstone":
		return "deleted file"
	case f.Mode == "email":
		return "email body, not a stored file"
	case f.URLPrivateDownload == "":
		return "no download url"
	case f.Size > max:
		return fmt.Sprintf("%d bytes exceeds the %d byte limit", f.Size, max)
	}
	return ""
}

func (c *Client) download(ctx context.Context, f slackread.File, d *dest, name string, max int64) (int64, error) {
	resp, body, cancel, err := c.open(ctx, f)
	if err != nil {
		return 0, err
	}
	defer cancel()
	defer resp.Body.Close()
	return writeFile(body, d, name, max, f.Size)
}

// Read returns a file's bytes rather than writing them.
//
// It is the same request under the same rules — the host check, the bearer, the
// sign-in-page refusal — differing only in where the body goes. A canvas is
// read rather than saved: it is a document to look at, and 30KB of HTML has no
// business becoming a file on the way to being rendered.
//
// max bounds what is held in memory, on bytes that arrive rather than on the
// size Slack declared.
func (c *Client) Read(ctx context.Context, f slackread.File, max int64) ([]byte, error) {
	resp, body, cancel, err := c.open(ctx, f)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()

	if max >= math.MaxInt64 {
		max = math.MaxInt64 - 1
	}
	buf, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > max {
		return nil, fmt.Errorf("longer than the %d byte limit", max)
	}
	return buf, nil
}

// open makes the request and hands back a response whose body is the file. The
// returned cancel releases the request deadline and must be called.
func (c *Client) open(ctx context.Context, f slackread.File) (*http.Response, *bufio.Reader, context.CancelFunc, error) {
	// Structural, as in slackread: a zero credential cannot reach the wire,
	// whatever the caller did with ResolveRead's error.
	if c.cred.IsZero() {
		return nil, nil, nil, ErrNoCredential
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	ok := false
	defer func() {
		if !ok {
			cancel()
		}
	}()

	// The URL comes from Slack's own reply, but so would a hostile one if that
	// reply were ever not Slack's. Checking it here is what makes the host set a
	// bound on everywhere the bearer can go, rather than only on where a
	// redirect can take it.
	if err := c.permits(f.URLPrivateDownload); err != nil {
		return nil, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URLPrivateDownload, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	c.cred.Authorize(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		// http.Client wraps a CheckRedirect refusal in a *url.Error carrying the
		// request URL. Returning the refusal itself keeps the reason and drops
		// the URL, which is the whole point of the typed error.
		var re *redirectError
		if errors.As(err, &re) {
			return nil, nil, nil, re
		}
		return nil, nil, nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	buffered := bufio.NewReader(resp.Body)
	if err := checkBodyIsTheFile(resp, f, buffered); err != nil {
		resp.Body.Close()
		return nil, nil, nil, err
	}
	ok = true
	return resp, buffered, cancel, nil
}

func (c *Client) timeout() time.Duration {
	if c.hc.Timeout > 0 {
		return c.hc.Timeout
	}
	return slackhttp.DefaultTimeout
}

// signIn is the refusal a token without files:read earns. Slack answers such a
// request with HTTP 200 and its sign-in page, so the status says nothing.
var signIn = errors.New("Slack answered with a web page rather than the file, " +
	"which is what a token without the files:read scope gets")

// checkBodyIsTheFile refuses a response that is a web page where a file was
// expected. body is a peekable reader so that what is inspected is still there
// to be written.
//
// A canvas forces the inspection. It is typed application/vnd.slack-docs and
// served as text/html, so a rule going by type alone must either refuse every
// canvas or accept every sign-in page delivered in place of one — and the
// second is worse, because hear files takes its file list from
// conversations.history, a different scope. A token without files:read would
// then write the login page to disk and report it written.
//
// The distinction is structural rather than a search for words on the page:
// Slack serves a canvas as a fragment, and its sign-in page as a whole HTML
// document. A fragment that opens a document is not the canvas that was asked
// for.
func checkBodyIsTheFile(resp *http.Response, f slackread.File, body *bufio.Reader) error {
	if !isHTML(resp.Header.Get("Content-Type")) {
		return nil
	}
	if isHTML(f.Mimetype) {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(f.Mimetype)), CanvasMimetype) {
		return signIn
	}
	head, _ := body.Peek(1024)
	if isWholeDocument(head) {
		return signIn
	}
	return nil
}

// isWholeDocument reports markup that opens an HTML page rather than continuing
// one.
func isWholeDocument(head []byte) bool {
	h := skipPreamble(strings.ToLower(string(head)))
	for _, tag := range []string{"<!doctype", "<html", "<head", "<body"} {
		if !strings.HasPrefix(h, tag) {
			continue
		}
		// A prefix match is not a tag: <header> begins with <head and is
		// perfectly ordinary inside a canvas.
		rest := h[len(tag):]
		if rest == "" || rest[0] == '>' || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' {
			return true
		}
	}
	return false
}

// skipPreamble drops the whitespace and comments a document may open with, so
// that what is examined is the first thing that is actually markup.
func skipPreamble(h string) string {
	for {
		h = strings.TrimLeft(h, " \t\r\n\ufeff")
		if !strings.HasPrefix(h, "<!--") {
			return h
		}
		end := strings.Index(h, "-->")
		if end < 0 {
			return ""
		}
		h = h[end+3:]
	}
}

func isHTML(contentType string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToLower(contentType)), "text/html")
}

// CanvasMimetype is what Slack calls a canvas. Its bytes are HTML.
const CanvasMimetype = "application/vnd.slack-docs"

// dest is where one message's files are written, and the only thing in this
// package that touches the filesystem.
//
// --out is opened as an os.Root and every step goes through it, so nothing
// derived from Slack's reply can be made to resolve outside it. The message
// directory is the reason: a local process that plants a symlink at
// <out>/<channel>-<ts> before the download would otherwise have MkdirAll, the
// temporary file and the final link all follow it, and authenticated Slack
// content would land wherever the link pointed.
//
// The root is opened on the first file that actually has bytes to write, so a
// message whose files are all links leaves no directory behind.
type dest struct {
	out    string
	msgDir string
	root   *os.Root
}

func (d *dest) ready() error {
	if d.root != nil {
		return nil
	}
	// --out itself is the caller's own argument, so creating it and following
	// it is intended. Everything below it is not the caller's choice.
	if err := os.MkdirAll(d.out, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(d.out)
	if err != nil {
		return err
	}
	if err := root.Mkdir(d.msgDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		root.Close()
		return err
	}
	d.root = root
	return nil
}

func (d *dest) close() {
	if d.root != nil {
		d.root.Close()
	}
}

// path is the name to report, which is the one a person can act on. rel is what
// the root is addressed with, and os.Root takes slash-separated names.
func (d *dest) path(name string) string { return filepath.Join(d.out, d.msgDir, name) }
func (d *dest) rel(name string) string  { return path.Join(d.msgDir, name) }

// temp creates a file inside the message directory that no other name refers
// to. os.Root has no CreateTemp, and O_EXCL makes the loop safe without one:
// whoever creates the name owns it.
func (d *dest) temp() (string, *os.File, error) {
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf(".hear-%d", i)
		f, err := d.root.OpenFile(d.rel(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("no free temporary name in the message directory")
}

// writeFile streams body into the message directory as name.
//
// The bytes go to a temporary file and are linked into place only once the
// whole body has been read and accepted, so a refused, truncated or interrupted
// download leaves nothing that looks complete. It is a link rather than a
// rename because rename replaces silently: linking is the one step that both
// puts the file in place and refuses to overwrite, without a window between the
// check and the act.
//
// declared is the size Slack attributes to the file, or zero when it says
// nothing. It is not trusted as a bound — max is enforced on what arrives — but
// a body that stops short of it is a truncation the transport did not report,
// and writing it would produce a file that looks complete and is not.
func writeFile(body io.Reader, d *dest, name string, max, declared int64) (int64, error) {
	if err := d.ready(); err != nil {
		return 0, err
	}
	tmpName, tmp, err := d.temp()
	if err != nil {
		return 0, err
	}
	defer d.root.Remove(d.rel(tmpName))
	defer tmp.Close()

	// max+1 so that a file of exactly max bytes is written and one byte more is
	// seen as the overrun it is. Clamped here rather than only at the flag that
	// usually supplies it: at MaxInt64 the increment wraps negative, LimitReader
	// then reads nothing, and an empty file would be reported as a success.
	if max >= math.MaxInt64 {
		max = math.MaxInt64 - 1
	}
	n, err := io.Copy(tmp, io.LimitReader(body, max+1))
	if err != nil {
		return 0, err
	}
	if n > max {
		return 0, fmt.Errorf("longer than the %d byte limit", max)
	}
	if declared > 0 && n < declared {
		return 0, fmt.Errorf("received %d bytes, fewer than the %d Slack declared", n, declared)
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := d.place(tmpName, name); err != nil {
		return 0, err
	}
	return n, nil
}

// place puts the finished temporary file at name without replacing anything
// already there.
//
// A hard link is the one step that both puts the file in place and refuses to
// overwrite, with no window between checking that the target is free and taking
// it. Not every filesystem has links — a FAT-formatted drive named by --out
// does not — so the fallback below keeps the same refusal by other means.
func (d *dest) place(tmpName, name string) error {
	err := d.root.Link(d.rel(tmpName), d.rel(name))
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists", d.path(name))
	}
	return d.copyExclusive(tmpName, name)
}

// copyExclusive is the placement for a filesystem with no hard links.
//
// The name is claimed first with O_EXCL, which is the atomic no-clobber step a
// link would otherwise have been, and the bytes are then copied to a second
// temporary file and renamed over that claim. Rename replaces, but what it
// replaces is this call's own empty placeholder, and the claim already proved
// nothing else was there.
//
// The residue if the process dies mid-copy is an empty file rather than a
// partial one. That is the best available here: no filesystem without links
// offers an atomic no-clobber rename, and an empty file is visibly not a
// download where half a PDF is not.
func (d *dest) copyExclusive(tmpName, name string) error {
	claim, err := d.root.OpenFile(d.rel(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s exists", d.path(name))
		}
		return err
	}
	claim.Close()

	src, err := d.root.Open(d.rel(tmpName))
	if err != nil {
		d.root.Remove(d.rel(name))
		return err
	}
	defer src.Close()

	copyName, dst, err := d.temp()
	if err != nil {
		d.root.Remove(d.rel(name))
		return err
	}
	defer d.root.Remove(d.rel(copyName))

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		d.root.Remove(d.rel(name))
		return err
	}
	// The close is where a full disk is reported on several filesystems, so it
	// fails the same way a short write does: with nothing left at name.
	if err := dst.Close(); err != nil {
		d.root.Remove(d.rel(name))
		return err
	}
	if err := d.root.Rename(d.rel(copyName), d.rel(name)); err != nil {
		d.root.Remove(d.rel(name))
		return err
	}
	return nil
}

// safeName is the filename to write f as.
//
// Slack's name is chosen by whoever uploaded the file, so it is reduced to its
// last path element — under both separators, because a Windows-style name
// reaching a Unix path is still someone else's choice of directory — and
// replaced entirely by the file id when what remains could not be a filename or
// could still name somewhere else.
func safeName(f slackread.File) string {
	name := f.Name
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "" || name == "." || name == ".." || strings.ContainsFunc(name, isPathHostile) {
		return f.ID
	}
	return name
}

// isPathHostile reports a rune that must not reach a filename.
//
// The separators and the ASCII controls are the path-safety part. unicode.Cf
// and the C1 controls are the display part: a name carrying U+202E renders
// reversed in every listing that shows it, and U+009B is a control-sequence
// introducer on terminals that honour it. A name holding any of them is
// replaced by the file id rather than repaired, so nothing here has to decide
// what the uploader meant.
func isPathHostile(r rune) bool {
	return r == 0 || r == '/' || r == '\\' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// foldName is the key two names collide under. It is lowercased because the
// filesystem this ships on is case-insensitive by default: Trace.pdf and
// trace.pdf are one path on APFS, and a map keyed on the exact bytes would let
// the second through to fail at placement instead of being given its file id.
//
// Unicode normalisation is not folded. APFS preserves it and compares it, so
// two names differing only in normalisation are two paths there, and folding
// them would rename a file that had no collision.
func foldName(name string) string { return strings.ToLower(name) }

// unique keeps two files on one message from writing to the same path. The
// loser gains its file id, which is unique within a workspace, before the
// extension rather than after it, so the name still says what kind of file it
// is.
func unique(name, id string, taken map[string]bool) string {
	if !taken[foldName(name)] {
		return name
	}
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + "-" + id + ext
}
