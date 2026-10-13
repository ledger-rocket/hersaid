package slackupload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/slackhosts"
)

// canary stands in for the part of a pre-signed URL that authorises the upload.
// No error, log line or message may contain it.
const canary = "X-Amz-Signature=DEADBEEFCAFE"

// loopback is what the fake upload endpoints run on. The real policy names
// Slack's hosts; a test server does not have one.
var loopback = slackhosts.Allow("127.0.0.1", "localhost", "invalid")

func client(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	// No API client: leg 2 never touches one, which is the point of testing it
	// on its own.
	return New(nil, loopback, timeout)
}

// local writes a file of n bytes and returns it open, as pre-flight would.
func local(t *testing.T, n int) Local {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.pdf")
	err := os.WriteFile(path, []byte(strings.Repeat("x", n)), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return Local{Path: path, Name: "trace.pdf", Size: int64(n), Mimetype: "application/pdf", File: f}
}

// sink records what an upload endpoint received.
type sink struct {
	mu     sync.Mutex
	hits   int
	auth   string
	ctype  string
	body   []byte
	length int64
}

// hitCount is what tells "refused before the request" from "the request failed
// for some other reason". A body check cannot: a redirect Go rewrites to a GET
// arrives with an empty body, which looks the same as never arriving.
func (s *sink) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func (s *sink) get() (string, string, []byte, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth, s.ctype, s.body, s.length
}

func (s *sink) authorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
}

func uploadHost(t *testing.T, status int) (*httptest.Server, *sink) {
	t.Helper()
	rec := &sink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.hits++
		rec.auth, rec.ctype, rec.body, rec.length = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body, r.ContentLength
		rec.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// The whole reason this package exists. A pre-signed URL carries its own
// authorisation, so the bearer is unnecessary there — and the host is one Slack
// chose, not one this code did.
func TestTheTransferCarriesNoBearer(t *testing.T) {
	t.Parallel()
	srv, rec := uploadHost(t, http.StatusOK)
	f := local(t, 64)

	err := client(t, 5*time.Second).transfer(context.Background(), srv.URL+"/u?"+canary, f)
	if err != nil {
		t.Fatal(err)
	}
	auth := rec.authorization()
	if auth != "" {
		t.Fatalf("the upload endpoint saw Authorization %q, want none", auth)
	}
}

// Exactly the declared count, with a length Slack's storage can act on: a
// LimitReader has no length of its own, and a chunked body is refused by some
// endpoints.
func TestTheTransferSendsExactlyTheDeclaredBytes(t *testing.T) {
	t.Parallel()
	srv, rec := uploadHost(t, http.StatusOK)
	f := local(t, 2048)

	err := client(t, 5*time.Second).transfer(context.Background(), srv.URL+"/u?"+canary, f)
	if err != nil {
		t.Fatal(err)
	}
	_, ctype, body, length := rec.get()
	if int64(len(body)) != f.Size {
		t.Fatalf("received %d bytes, want the declared %d", len(body), f.Size)
	}
	if length != f.Size {
		t.Fatalf("Content-Length = %d, want %d", length, f.Size)
	}
	if ctype != "application/pdf" {
		t.Fatalf("Content-Type = %q, want the file's", ctype)
	}
}

// The matrix. Every way leg 2 can fail must leave no trace of the URL that
// authorised it — not in the error, and so not in the audit record or on the
// terminal, both of which are built from err.Error().
func TestNoFailureModeReportsTheUploadURL(t *testing.T) {
	t.Parallel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(slow.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.invalid/landing?"+canary, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	refused, _ := uploadHost(t, http.StatusOK)
	refusedURL := refused.URL
	refused.Close() // nothing listens there now

	failing, _ := uploadHost(t, http.StatusRequestEntityTooLarge)

	for _, c := range []struct {
		name    string
		url     string
		timeout time.Duration
	}{
		{"malformed url", "https://%zz/u?" + canary, 5 * time.Second},
		{"unparseable port", "https://127.0.0.1:notaport/u?" + canary, 5 * time.Second},
		{"foreign host", "https://evil.example/u?" + canary, 5 * time.Second},
		{"plaintext", "http://files.slack.com/u?" + canary, 5 * time.Second},
		{"dns failure", "https://nowhere.invalid/u?" + canary, 5 * time.Second},
		{"connection refused", refusedURL + "/u?" + canary, 5 * time.Second},
		{"non 2xx", failing.URL + "/u?" + canary, 5 * time.Second},
		{"timeout", slow.URL + "/u?" + canary, 150 * time.Millisecond},
		{"refused redirect", redirector.URL + "/u?" + canary, 5 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := local(t, 64)
			err := client(t, c.timeout).transfer(context.Background(), c.url, f)
			if err == nil {
				t.Fatal("transfer() = nil error, want a failure")
			}
			for _, banned := range []string{canary, "DEADBEEFCAFE", "X-Amz-Signature", "?"} {
				if strings.Contains(err.Error(), banned) {
					t.Fatalf("error = %q, want no trace of %q from the upload URL", err, banned)
				}
			}
			if !strings.Contains(err.Error(), "trace.pdf") {
				t.Errorf("error = %q, want it to name the file", err)
			}
		})
	}
}

// A file overwritten while it was being read was sent as something other than
// what the render described. Nothing is shared yet, so this still fails before
// anything is posted.
func TestAFileThatChangesWhileSendingIsRefused(t *testing.T) {
	t.Parallel()
	f := local(t, 512)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		// Grown underneath the descriptor, at the same inode.
		err := os.WriteFile(f.Path, []byte(strings.Repeat("y", 4096)), 0o600)
		if err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	err := client(t, 5*time.Second).transfer(context.Background(), srv.URL+"/u?"+canary, f)
	if err == nil {
		t.Fatal("transfer() = nil error, want the change refused")
	}
	if !strings.Contains(err.Error(), "changed") {
		t.Fatalf("error = %q, want it to say the file changed", err)
	}
}

// The deadline scales with the body, because a flat multiple of a timeout that
// suits a small JSON reply fails a large upload on size alone.
func TestTheTransferDeadlineScalesWithTheBody(t *testing.T) {
	t.Parallel()
	const timeout = 20 * time.Second
	small := TransferDeadline(timeout, 1024)
	if small != 10*timeout {
		t.Fatalf("TransferDeadline(small) = %v, want the flat floor %v", small, 10*timeout)
	}
	big := TransferDeadline(timeout, MaxSize)
	if big <= 10*timeout {
		t.Fatalf("TransferDeadline(1GB) = %v, want more than the flat floor %v", big, 10*timeout)
	}
	// 1e9 bytes at the assumed floor rate, plus the base.
	if want := timeout + 1000*time.Second; big != want {
		t.Fatalf("TransferDeadline(1GB) = %v, want %v", big, want)
	}
	if d := TransferDeadline(24*time.Hour, MaxSize); d <= 0 {
		t.Fatalf("TransferDeadline at the timeout cap = %v, want it positive", d)
	}
}

func TestUploadHostsPermitSlackAndNothingElse(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		host string
		want bool
	}{
		{"files.slack.com", true},
		{"slack.com", true},
		{"slack-files.com", true},
		{"my-bucket.s3.amazonaws.com", false},
		{"notslack.com", false},
	} {
		if got := SlackHosts.PermitsBearer(c.host); got != c.want {
			t.Errorf("PermitsBearer(%q) = %v, want %v", c.host, got, c.want)
		}
		// No redirect tier at all on this leg, unlike a download.
		if got := SlackHosts.PermitsRedirect(c.host); got != c.want {
			t.Errorf("PermitsRedirect(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// The allowlist has to stop the request, not merely make its failure tidy. A
// host that answers perfectly well is the only way to tell the two apart: if
// the check were dropped, this upload would succeed.
func TestAnUnlistedHostReceivesNothingAtAll(t *testing.T) {
	t.Parallel()
	srv, rec := uploadHost(t, http.StatusOK)
	f := local(t, 64)

	// The server is on 127.0.0.1; these hosts are not.
	c := New(nil, slackhosts.Allow("slack.com"), 5*time.Second)
	err := c.transfer(context.Background(), srv.URL+"/u?"+canary, f)
	if err == nil {
		t.Fatal("transfer() = nil error, want the host refused")
	}
	if n := rec.hitCount(); n != 0 {
		t.Fatalf("the unlisted host received %d requests; the refusal must precede the request", n)
	}
	if !strings.Contains(err.Error(), "not a host Slack takes uploads at") {
		t.Fatalf("error = %q, want it to name the refusal", err)
	}
}

// The same for a redirect: refused, not followed. Two things make this
// assertion mean something. The target is a server that answers, so a hop that
// is taken is visible rather than merely failing somewhere else. And the status
// is 302, which Go rewrites to a GET and follows without needing to replay the
// body — a 307 cannot be followed at all with a body that has no GetBody, so it
// would pass whether or not any policy existed.
func TestARedirectedTransferReachesNoSecondHost(t *testing.T) {
	t.Parallel()
	second, landed := uploadHost(t, http.StatusOK)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/landing?"+canary, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	f := local(t, 64)
	err := client(t, 5*time.Second).transfer(context.Background(), redirector.URL+"/u?"+canary, f)
	if err == nil {
		t.Fatal("transfer() = nil error, want the redirect refused")
	}
	if n := landed.hitCount(); n != 0 {
		t.Fatalf("the redirect target received %d requests; the hop must not be followed", n)
	}
}

// The deadline has to bind on the transfer, not merely be computed.
//
// A client-wide timeout caps the whole request, so setting one from the base
// request timeout would silently override the size-aware bound and kill a large
// upload at the figure that suits a small JSON reply. This body arrives after
// the base timeout and well inside the transfer deadline, so it succeeds only
// if the right bound is the one in force.
func TestTheTransferBoundIsTheSizeAwareOneNotTheRequestTimeout(t *testing.T) {
	t.Parallel()
	const base = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(4 * base)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	f := local(t, 64)
	if d := TransferDeadline(base, f.Size); d <= 4*base {
		t.Fatalf("TransferDeadline(%v) = %v, want room for the reply", base, d)
	}
	err := client(t, base).transfer(context.Background(), srv.URL+"/u?"+canary, f)
	if err != nil {
		t.Fatalf("transfer() error = %v; the request timeout must not cap the transfer", err)
	}
}

// Slack documents 200 as the success. A 2xx that is not 200 marks the file
// staged and lets the one irreversible leg proceed on an assumption.
func TestOnlyHTTP200CountsAsATransfer(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			srv, _ := uploadHost(t, status)
			err := client(t, 5*time.Second).transfer(context.Background(), srv.URL+"/u?"+canary, local(t, 64))
			if err == nil {
				t.Fatalf("HTTP %d accepted as a transfer, want a failure", status)
			}
		})
	}
}

// Both TLS failure types arrive inside the *url.Error that http.Client returns;
// a wrong type in isTLS would report them as a plain network error.
func TestClassOfNamesWrappedTLSFailures(t *testing.T) {
	t.Parallel()
	cases := map[string]error{
		"record header": tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
		"certificate":   &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
	}
	for name, err := range cases {
		wrapped := &url.Error{Op: "Put", URL: "https://files.example/upload?" + canary, Err: err}
		if got := classOf(wrapped); got != "tls" {
			t.Errorf("%s: classOf = %q, want tls", name, got)
		}
	}
}
