// Package slacktest is a fake Slack for tests.
//
// It exists so that no test can reach the real Slack. Every write test in this
// module points at a Server; a test that names the real API is a bug, and a
// request to an endpoint nobody stubbed fails the test loudly rather than
// silently returning nothing.
package slacktest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Request is one recorded call.
type Request struct {
	// Endpoint is the Slack method name, e.g. "chat.postMessage".
	Endpoint string
	// HTTPMethod is GET or POST.
	HTTPMethod string
	// Query is the URL query string, decoded.
	Query url.Values
	// Body is the form-encoded request body, decoded.
	Body url.Values
	// Auth is the Authorization header exactly as received.
	Auth string
}

// BodyFields is the sorted set of field names in the request body. Asserting
// against it, rather than against one field's value, is what catches a value
// that smuggled a second field in alongside itself.
func (r Request) BodyFields() []string { return names(r.Body) }

// QueryFields is the sorted set of field names in the query string.
func (r Request) QueryFields() []string { return names(r.Query) }

func names(v url.Values) []string {
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Server is a fake Slack API.
type Server struct {
	// URL is the base URL to hand to a client constructor.
	URL string

	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	requests []Request
}

// New starts a fake Slack and stops it when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{t: t, handlers: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

// Reply makes endpoint answer with a fixed JSON body.
func (s *Server) Reply(endpoint, body string) {
	s.ReplyFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
}

// ReplyFunc installs an arbitrary handler for endpoint.
func (s *Server) ReplyFunc(endpoint string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[endpoint] = h
}

// Pages makes endpoint answer with bodies in turn, one per call. It is how a
// cursor-paginated endpoint is stubbed: give each page its own next_cursor and
// the last one none.
func (s *Server) Pages(endpoint string, bodies ...string) {
	var n int
	var mu sync.Mutex
	s.ReplyFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := n
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, bodies[i])
	})
}

// Drop records the request and then closes the connection without replying: the
// request left the client and no answer came back, which is the only situation
// in which a delivered write can look like a failure.
func (s *Server) Drop(endpoint string) {
	s.ReplyFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			s.t.Error("ResponseWriter is not a Hijacker; cannot simulate a dropped connection")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			s.t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	})
}

// Hits is the number of requests received, including unstubbed ones.
func (s *Server) Hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// Requests returns every recorded call, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Last returns the most recent call and fails the test if there was none.
func (s *Server) Last() Request {
	s.t.Helper()
	reqs := s.Requests()
	if len(reqs) == 0 {
		s.t.Fatal("no request was received")
	}
	return reqs[len(reqs)-1]
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	endpoint := strings.TrimPrefix(r.URL.Path, "/")

	// Parsed before the handler runs, so a request is recorded even when the
	// handler drops the connection.
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	form, _ := url.ParseQuery(string(body))

	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Endpoint:   endpoint,
		HTTPMethod: r.Method,
		Query:      r.URL.Query(),
		Body:       form,
		Auth:       r.Header.Get("Authorization"),
	})
	h, stubbed := s.handlers[endpoint]
	s.mu.Unlock()

	if !stubbed {
		s.t.Errorf("slacktest: unstubbed endpoint %q was called", endpoint)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":false,"error":"unstubbed_endpoint"}`)
		return
	}
	h(w, r)
}
