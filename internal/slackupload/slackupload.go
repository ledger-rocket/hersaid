package slackupload

import (
	"net/http"
	"time"

	"github.com/ledger-rocket/hersaid/internal/slackhosts"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackwrite"
)

// Client stages files with Slack and shares what it staged.
//
// It holds no credential. The two API legs go through the slackwrite client,
// which has one; leg 2 has nothing to authorise with, so the rule that it must
// not carry the bearer is a property of what this struct contains rather than
// of what its code remembers not to do.
type Client struct {
	api     *slackwrite.Client
	hc      *http.Client
	hosts   slackhosts.Hosts
	timeout time.Duration
}

// New returns a client that transfers to hosts under timeout.
//
// The client is built here rather than taken, because leg 2 needs a redirect
// policy of its own: slackhttp.New refuses every redirect with a message naming
// the target, and on this leg the target is the part that must not be named. It
// shares slackhttp's transport so the no-proxy and TLS-floor ground rules are
// the same ones, rather than a second set kept in step by hand.
func New(api *slackwrite.Client, hosts slackhosts.Hosts, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = slackhttp.DefaultTimeout
	}
	return &Client{
		api:     api,
		hosts:   hosts,
		timeout: timeout,
		hc: &http.Client{
			// No client-wide timeout: it caps the whole request, so it would
			// override the size-aware deadline transfer applies and kill a
			// large upload at the bound that suits a small JSON reply. The
			// per-transfer context is what bounds this client.
			Transport: slackhttp.Transport(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return errRedirect
			},
		},
	}
}

// TransferDeadline is how long a body of size bytes is allowed.
//
// A flat multiple of the request timeout cannot serve both: ten times the
// twenty-second default is two hundred seconds, which moves the largest
// permitted file only above about 40 Mbit/s, so the multiple meant to stop
// large uploads failing on size alone is what would fail them. The floor rate
// is deliberately slow enough to be nearly always true.
func TransferDeadline(timeout time.Duration, size int64) time.Duration {
	const minRate = 1_000_000 // bytes per second
	flat := 10 * timeout
	// timeout is capped at a day by slackhttp, and size at 1e9 by pre-flight,
	// so neither the multiply nor the add can overflow.
	sized := timeout + time.Duration((size+minRate-1)/minRate)*time.Second
	if sized > flat {
		return sized
	}
	return flat
}

// MaxSize is the largest file this will send.
//
// Slack documents its limit as "1 GB" without saying which one. The decimal
// reading is the smaller, so it is the one that cannot let through a file Slack
// will refuse.
const MaxSize = 1_000_000_000

// MaxFiles bounds one message's attachments. Slack documents no limit; this
// bounds an accident, not a need.
const MaxFiles = 10
