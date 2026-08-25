// Package slackhosts says where a request may go and where the credential may
// go with it. The two are not the same question, and holding them apart is the
// whole of what this package is for.
//
// It exists because both directions of file transfer need the same rule. A
// download follows Slack's redirect to object storage; an upload sends bytes to
// a URL Slack hands back. Either way the far end is a host Slack named rather
// than one this code chose, and either way it carries its own authorisation in
// the query string — so it may receive the file and must never receive the
// bearer. One copy of that rule, used by both, cannot drift out of step with
// itself.
package slackhosts

import "strings"

// Hosts is where a transfer may go. The two roles are held apart on purpose: a
// host that may serve or receive bytes is not thereby a host the credential may
// be sent to.
type Hosts struct {
	bearer      []string
	viaRedirect []string
}

// Allow builds a Hosts whose suffixes may both receive the bearer and be
// redirected to. A host matches a suffix exactly or as a subdomain of it, never
// as a plain substring, so "not-slack.com" does not match "slack.com".
func Allow(suffixes ...string) Hosts { return Hosts{bearer: suffixes} }

// AndViaRedirect returns h extended with hosts a redirect may land on but a
// request carrying the bearer may never be sent to.
func (h Hosts) AndViaRedirect(suffixes ...string) Hosts {
	h.viaRedirect = append(append([]string(nil), h.viaRedirect...), suffixes...)
	return h
}

// PermitsBearer reports whether a request carrying the credential may be sent
// to host.
func (h Hosts) PermitsBearer(host string) bool { return matches(host, h.bearer) }

// PermitsRedirect reports whether a redirect may land on host. Everywhere the
// bearer may go is somewhere a redirect may go; the reverse does not hold.
func (h Hosts) PermitsRedirect(host string) bool {
	return matches(host, h.bearer) || matches(host, h.viaRedirect)
}

func matches(host string, suffixes []string) bool {
	if host == "" {
		return false
	}
	for _, s := range suffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}
