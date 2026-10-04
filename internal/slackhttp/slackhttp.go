// Package slackhttp builds the only HTTP client the hersaid commands use.
//
// http.DefaultClient and http.DefaultTransport are never used: DefaultTransport
// honours HTTP_PROXY/HTTPS_PROXY and the default client follows redirects, so
// ambient configuration could point an Authorization header at a host of
// someone else's choosing.
package slackhttp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

var (
	errSlackhttpRefusingRedirect = errors.New("slackhttp: refusing redirect")
	errNotWholeNumberSeconds     = errors.New("not a whole number of seconds")
	errRequestTimeoutPositive    = errors.New("a request timeout must be positive")
	errSeconds                   = errors.New("seconds")
)

// DefaultTimeout bounds every request.
const DefaultTimeout = 20 * time.Second

// TimeoutEnvVar overrides DefaultTimeout, in whole seconds.
const TimeoutEnvVar = "HERSAID_TIMEOUT"

// New returns a client whose requests cannot be redirected or proxied. A
// non-positive timeout is replaced by DefaultTimeout rather than honoured:
// zero would mean no bound at all, which is never what was wanted.
func New(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: newTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("%w to %s", errSlackhttpRefusingRedirect, req.URL.Redacted())
		},
	}
}

// Transport returns the transport every client in this module is built on: no
// proxy, a TLS 1.2 floor, and a bounded dial and handshake.
//
// New is what a caller wants. This exists for the one client that cannot use
// New — a file download, which must be allowed to follow a redirect under
// bounds of its own — so that its ground rules are still these ones rather than
// a second set that has to be kept in step by hand.
func Transport() *http.Transport { return newTransport() }

// maxTimeoutSeconds bounds HERSAID_TIMEOUT. A day is chosen so that every
// multiple taken downstream — hear's ten times for a download, sez's twice for
// its subprocess, an upload's ten times as a floor — stays far inside range,
// not because a day is a sensible request timeout.
const maxTimeoutSeconds = 86400

// TimeoutFromEnv resolves HERSAID_TIMEOUT, in whole seconds. An unset variable
// yields DefaultTimeout; a value that is not a positive integer is an error
// rather than a silently disabled timeout.
func TimeoutFromEnv() (time.Duration, error) {
	v, ok := os.LookupEnv(TimeoutEnvVar)
	if !ok || v == "" {
		return DefaultTimeout, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", TimeoutEnvVar, v, errNotWholeNumberSeconds)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s=%d: %w", TimeoutEnvVar, n, errRequestTimeoutPositive)
	}
	if n > maxTimeoutSeconds {
		return 0, fmt.Errorf("%s=%d: a request timeout is capped at %d %w",
			TimeoutEnvVar, n, maxTimeoutSeconds, errSeconds)
	}
	return time.Duration(n) * time.Second, nil
}

// keepAlivePeriod is the TCP keep-alive interval; idleConnTimeout is how long an
// idle pooled connection is kept. They are separate knobs that happen to agree.
const (
	keepAlivePeriod = 30 * time.Second
	idleConnTimeout = 30 * time.Second
)

func newTransport() *http.Transport {
	return &http.Transport{
		// nil, not ProxyFromEnvironment: no environment variable may route a
		// request carrying a bearer token through a host of its choosing.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: keepAlivePeriod,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}
