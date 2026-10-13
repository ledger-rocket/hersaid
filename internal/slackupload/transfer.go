// Package slackupload sends a file's bytes to Slack and shares what it sent.
//
// It is separate from slackwrite because the middle leg is not an API call: the
// bytes go to a URL Slack hands back, with no envelope to check and no
// credential to attach. slackwrite guarantees that every request it makes is a
// typed form POST answered by an envelope, and a streaming binary body to
// somebody else's host is neither.
//
// The rule the whole package exists to keep: the write credential never travels
// to the upload URL, and the upload URL never travels into an error, a log line
// or a terminal. That URL is pre-signed — its query string is a credential —
// so disclosing it is the same failure as disclosing the bearer, and it is one
// an ordinary %w would commit.
package slackupload

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"

	"github.com/ledger-rocket/hersaid/internal/slackhosts"
)

// Local is one file to send, held open since pre-flight.
//
// The descriptor is held rather than the path reopened: length is declared on
// leg 1 and the bytes are sent on leg 2, and between a stat and a later open a
// symlink can be retargeted or a path replaced. Holding one descriptor makes
// the file the render described and the file that is sent the same file. It
// does not freeze the contents — another process can still overwrite the same
// inode — which is why the size is checked again after the transfer.
type Local struct {
	// Path is absolute, for the render and the record.
	Path string
	// Name is what Slack is told, always a single path element.
	Name string
	// Size is from the held descriptor, and is what leg 1 declares.
	Size int64
	// Mimetype is the Content-Type of the transfer. Slack infers the file's
	// type from the name, so this is a statement about the request and not a
	// field Slack is asked to honour.
	Mimetype string
	// File is the open descriptor. The caller opened it and closes it.
	File *os.File
}

// transferError is a leg 2 failure. It carries a filename, a host and a class,
// and never a URL.
//
// It is constructed, never wrapped. Go returns transport failures as
// *url.Error, whose Error() contains the whole URL including the query string
// that authorises the upload — and sez writes err.Error() into posts.jsonl. A
// single %w anywhere on this path would persist a credential to disk.
type transferError struct {
	file  string
	host  string
	class string
}

func (e *transferError) Error() string {
	return fmt.Sprintf("%s: upload to %s failed (%s)", e.file, e.host, e.class)
}

// uploadURL parses and vets the URL Slack handed back, returning only its host.
//
// The host is all that comes back on purpose: it is the only part of a
// pre-signed URL that is safe to name. url.Parse's own error carries the whole
// string, so it is discarded rather than wrapped — the caller learns that the
// URL was unusable, not what it was.
func (c *Client) uploadURL(rawURL, file string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &transferError{file: file, host: "an unparseable url", class: "malformed"}
	}
	host := u.Hostname()
	if u.Scheme != "https" && !isLoopback(host) {
		return nil, &transferError{file: file, host: host, class: "refusing to send in clear"}
	}
	// The URL comes from Slack's own reply, but so would a hostile one if that
	// reply were ever not Slack's. A file is the sender's, and it goes where
	// Slack serves uploads or nowhere.
	if !c.hosts.PermitsBearer(host) {
		return nil, &transferError{file: file, host: host, class: "not a host Slack takes uploads at"}
	}
	return u, nil
}

// transfer is leg 2: the bytes, to the URL leg 1 returned, with no bearer.
//
// Nothing here calls Authorize. A pre-signed URL carries its own authorisation
// in the query string, so the bearer is both unnecessary and exactly what must
// not leak — and the host it would leak to is not one this code chose.
func (c *Client) transfer(ctx context.Context, rawURL string, f Local) error {
	u, err := c.uploadURL(rawURL, f.Name)
	if err != nil {
		return err
	}
	host := u.Hostname()

	// The bound lives here rather than on the client, so that it scales with
	// this file rather than with whatever the last caller set.
	ctx, cancel := context.WithTimeout(ctx, TransferDeadline(c.timeout, f.Size))
	defer cancel()

	_, err = f.File.Seek(0, io.SeekStart)
	if err != nil {
		return &transferError{file: f.Name, host: host, class: "could not be read"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, io.LimitReader(f.File, f.Size))
	if err != nil {
		// http.NewRequest quotes the URL back in its error, like url.Parse.
		return &transferError{file: f.Name, host: host, class: "request could not be built"}
	}
	// Set explicitly: a LimitReader has no length of its own, and without this
	// Go would send the body chunked, which some storage endpoints refuse.
	req.ContentLength = f.Size
	req.Header.Set("Content-Type", f.Mimetype)

	resp, err := c.hc.Do(req)
	if err != nil {
		return &transferError{file: f.Name, host: host, class: classOf(err)}
	}
	defer resp.Body.Close()
	// Drained under a bound so the connection can be reused, and discarded: the
	// body of an upload reply says nothing this code acts on, and reading it
	// into an error message is another way the URL could come back.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	// Slack documents 200 as the success, and anything else as a failure. A 202
	// treated as success would mark the file staged and go on to the one leg
	// that cannot be undone.
	if resp.StatusCode != http.StatusOK {
		return &transferError{file: f.Name, host: host, class: fmt.Sprintf("http %d", resp.StatusCode)}
	}

	// The descriptor fixes which file was sent, not what was in it. A file that
	// grew or shrank while it was being read was sent as a prefix of itself or
	// refused mid-body, and either way what Slack now holds is not what the
	// render described. Nothing has been shared yet, so this still fails before
	// anything is posted.
	info, err := f.File.Stat()
	if err != nil {
		return &transferError{file: f.Name, host: host, class: "could not be checked after sending"}
	} else if info.Size() != f.Size {
		return &transferError{file: f.Name, host: host, class: "changed while it was being sent"}
	}
	return nil
}

// classOf names the kind of a transport failure without quoting it. err is a
// *url.Error carrying the pre-signed URL, so it is examined and discarded.
func classOf(err error) string {
	var dns *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &dns):
		return "dns"
	case errors.Is(err, errRedirect):
		return "redirect refused"
	case isTLS(err):
		return "tls"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "network error"
}

func isTLS(err error) bool {
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return true
	}
	_, ok := errors.AsType[*tls.CertificateVerificationError](err)
	return ok
}

// errRedirect is the refusal to follow a hop on leg 2. It names nothing: the
// host it would name is the one in the Location, which on this leg is the part
// that must not be reported.
var errRedirect = errors.New("refusing a redirect")

func isLoopback(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// SlackHosts is the policy for where an upload may go: Slack's own hosts, and
// nowhere by redirect at all.
//
// There is no redirect tier here, unlike a download. A download follows Slack's
// hop to storage because that is how Slack serves bytes; an upload is a request
// this code makes to a URL Slack chose, and no evidence says Slack redirects
// one. If it turns out to, this loosens under the same two-tier rule rather
// than by following whatever arrives.
var SlackHosts = slackhosts.Allow("slack.com", "slack-files.com")
