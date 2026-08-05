package slacktest_test

import (
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// host is spelled apart from any path so this file, which the guard also
// walks, never contains a contiguous endpoint of its own.
const (
	host     = "slack" + ".com"
	constRef = "slackread" + ".SlackAPI"
)

// The guard exists so no test can reach production Slack. Banning the mere
// string "slack.com" achieved that but also banned describing it: a doc comment
// explaining which hosts a permalink may carry could not name one. What matters
// is whether a file names an ENDPOINT, not whether it says the word.
//
// The commands reach production through the exported constant
// (slackread.SlackAPI), which is reviewable and greppable. A raw endpoint
// literal is not, so that is what is refused.
func TestNamesRealEndpointDistinguishesUseFromMention(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "prose naming the host is not an endpoint",
			src:  "// A permalink is https://<workspace>." + host + "/archives/<id>/p<ts>.",
			want: false,
		},
		{
			name: "prose naming the bare host is not an endpoint",
			src:  "// The host varies; " + host + " is only the common case.",
			want: false,
		},
		{
			name: "an endpoint literal is an endpoint",
			src:  `u := "https://` + host + `/api/chat.postMessage"`,
			want: true,
		},
		{
			name: "the api root as a literal is an endpoint",
			src:  `const base = "https://` + host + `/api"`,
			want: true,
		},
		{
			name: "the sanctioned constant is not a literal endpoint",
			src:  "run(args, os.Stdout, os.Stderr, " + constRef + ")",
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := slacktest.NamesRealEndpoint([]byte(tc.src), false); got != tc.want {
				t.Fatalf("slacktest.NamesRealEndpoint(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// A test file is held to the stricter rule: it may not reach production by any
// route, constant included. A command must name production — that is its job —
// but nothing under test ever should, and the constant was a hole the old
// string check never covered either.
func TestATestFileMayNotNameProductionEvenByConstant(t *testing.T) {
	viaConstant := "c := slackread.New(cred, hc, " + constRef + ")"
	if !slacktest.NamesRealEndpoint([]byte(viaConstant), true) {
		t.Fatal("a test file naming SlackAPI must be refused; it points at production")
	}
	if slacktest.NamesRealEndpoint([]byte(viaConstant), false) {
		t.Fatal("a command naming SlackAPI must be allowed; that is how it reaches Slack")
	}
}

// A raw string is not prose. It is a string literal a client can be constructed
// with, so an endpoint inside one has to count as use — the narrowing must not
// become an escape hatch the plain substring check would have closed.
func TestAnEndpointInARawStringIsStillUse(t *testing.T) {
	src := "c := New(cred, hc, `https://" + host + "/api`)"
	if !slacktest.NamesRealEndpoint([]byte(src), false) {
		t.Fatalf("raw-string endpoint not refused: %s", src)
	}
}

// The same string, this time genuinely prose, stays allowed: a comment cannot
// construct anything.
func TestAnEndpointNamedInACommentIsNotUse(t *testing.T) {
	src := "// the base is https://" + host + "/api, reached via the constant"
	if slacktest.NamesRealEndpoint([]byte(src), false) {
		t.Fatalf("comment refused: %s", src)
	}
}

// The carve-out that must not exist: a test file is exactly where a raw-string
// endpoint would do harm, since the guard exists so nothing under test reaches
// production. Prose stays prose; a string stays a string.
func TestARawStringEndpointInATestFileIsAlsoUse(t *testing.T) {
	src := "c := New(cred, hc, `https://" + host + "/api`)"
	if !slacktest.NamesRealEndpoint([]byte(src), true) {
		t.Fatalf("raw-string endpoint in a test file not refused: %s", src)
	}
}
