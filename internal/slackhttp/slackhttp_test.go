package slackhttp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func counting(t *testing.T, status int, headers map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// A redirect can move an Authorization header to a host chosen by
// whoever controls the response.
func TestRefusesRedirects(t *testing.T) {
	final, finalHits := counting(t, http.StatusOK, nil)
	start, _ := counting(t, http.StatusFound, map[string]string{"Location": final.URL})

	resp, err := New(2 * time.Second).Get(start.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("Get() succeeded; a redirect must be refused")
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("error = %v, want it to name the refusal", err)
	}
	if got := finalHits.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests, want 0", got)
	}
}

// HTTP_PROXY must not route a request carrying a bearer token.
//
// This test does NOT establish that. Go's proxy resolution exempts loopback
// addresses and a test server is on loopback, so the counters below read zero
// whether or not proxying is enabled — verified by mutation, and the reason
// TestTransportProxyIsNil exists. What this does establish is the weaker,
// still-worth-having property that the four variables do not break the request,
// and the control at the end shows the proxy server counts what reaches it.
func TestProxyEnvironmentDoesNotBreakARequest(t *testing.T) {
	target, targetHits := counting(t, http.StatusOK, nil)
	proxy, proxyHits := counting(t, http.StatusOK, nil)

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)

	resp, err := New(2 * time.Second).Get(target.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resp.Body.Close()

	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("proxy received %d requests, want 0", got)
	}
	if got := targetHits.Load(); got != 1 {
		t.Fatalf("target received %d requests, want 1", got)
	}

	// The proxy server does count what reaches it, so the zero above is a fact
	// about this run rather than a broken counter.
	proxied := &http.Client{Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(proxy.URL) },
	}}
	resp, err = proxied.Get(target.URL)
	if err != nil {
		t.Fatalf("control request error = %v", err)
	}
	resp.Body.Close()
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("control: proxy received %d requests, want 1", got)
	}
}

// Every request has a timeout.
func TestRequestsTimeOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := New(50 * time.Millisecond).Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("Get() succeeded; the request must time out")
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("took %v; the timeout was not applied", elapsed)
	}
}

// The proxy assertion that has to be structural. Go's env proxy resolution
// exempts loopback addresses, and a test server is on loopback, so a test that
// counts hits on a loopback proxy passes even with proxying fully enabled —
// TestIgnoresProxyEnvironment above demonstrates that with its control. This is
// the assertion that actually holds.
func TestTransportProxyIsNil(t *testing.T) {
	if p := newTransport().Proxy; p != nil {
		t.Fatal("transport Proxy is set; it must be nil, not ProxyFromEnvironment")
	}
}

// CheckRedirect belongs to the Client, not the Transport: a Transport has no
// such field, so setting it there would compile and do nothing.
func TestRedirectRefusalLivesOnTheClient(t *testing.T) {
	if New(time.Second).CheckRedirect == nil {
		t.Fatal("Client.CheckRedirect is nil; redirects would be followed")
	}
}

// The timeout comes from the environment, and a non-positive value is refused —
// zero would disable the bound entirely, and it is the one value someone would
// set meaning "wait longer".
func TestTimeoutFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		unset   bool
		want    time.Duration
		wantErr bool
	}{
		{name: "unset", unset: true, want: DefaultTimeout},
		{name: "empty", value: "", want: DefaultTimeout},
		{name: "seconds", value: "45", want: 45 * time.Second},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "not a number", value: "20s", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TimeoutEnvVar, tc.value)
			if tc.unset {
				err := os.Unsetenv(TimeoutEnvVar)
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := TimeoutFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("TimeoutFromEnv() = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("TimeoutFromEnv() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("TimeoutFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A non-positive timeout handed straight to New is clamped rather than honoured,
// so no caller can build an unbounded client by accident.
func TestNewClampsANonPositiveTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if got := New(d).Timeout; got != DefaultTimeout {
			t.Errorf("New(%v).Timeout = %v, want %v", d, got, DefaultTimeout)
		}
	}
}

// The parser is the only place that can bound HERSAID_TIMEOUT, because every
// consumer multiplies it again: hear takes ten times it for a download, sez
// takes twice it for the hear subprocess it runs, and an upload takes ten times
// it as a floor. A cap that only kept the seconds-to-Duration conversion in
// range would still let all three overflow into a negative duration, and a
// negative timeout fails every request instantly.
func TestTimeoutFromEnvIsCappedSoItsConsumersCannotOverflow(t *testing.T) {
	t.Setenv(TimeoutEnvVar, "86400")
	ceiling, err := TimeoutFromEnv()
	if err != nil {
		t.Fatalf("TimeoutFromEnv() at the cap error = %v, want it accepted", err)
	}
	if ceiling != 24*time.Hour {
		t.Fatalf("TimeoutFromEnv() = %v, want 24h", ceiling)
	}
	for _, factor := range []int64{2, 10} {
		if scaled := time.Duration(int64(ceiling) * factor); scaled <= 0 {
			t.Errorf("%v × %d = %v; every consumer's multiple must stay positive",
				ceiling, factor, scaled)
		}
	}

	t.Setenv(TimeoutEnvVar, "86401")
	_, err = TimeoutFromEnv()
	if err == nil {
		t.Fatal("TimeoutFromEnv() one second above the cap = nil error, want a refusal")
	}
}

// The value that used to overflow silently. int64 seconds this large wraps when
// multiplied by time.Second, and the result was a negative timeout that failed
// every request before it was made.
func TestTimeoutFromEnvRefusesAValueThatWouldWrap(t *testing.T) {
	t.Setenv(TimeoutEnvVar, "9223372036854")
	got, err := TimeoutFromEnv()
	if err == nil {
		t.Fatalf("TimeoutFromEnv() = %v, nil error; want a refusal", got)
	}
	if got > 0 {
		t.Fatalf("TimeoutFromEnv() = %v alongside its error, want the zero duration", got)
	}
}
