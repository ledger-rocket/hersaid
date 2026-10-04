package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

var errExecDbusLaunchExecutable = errors.New("exec: \"dbus-launch\": executable file not found in $PATH")

// The tokens the tests paste. They are distinctive so that any output
// containing one is unmistakable, and long enough to exercise the full
// fingerprint rather than its short-secret branch.
const (
	readToken  = "xoxp-1111111111-2222222222-fake-read-token-aa01"
	writeToken = "xoxp-3333333333-4444444444-fake-write-token-bb02"
	botToken   = "xoxb-5555555555-6666666666-fake-bot-token-cc03"
	envToken   = "xoxp-7777777777-8888888888-fake-env-token-dd04"
)

// The scope sets the two Slack apps grant. The read app's list is what a real
// read-only app carries; the write app's differs by exactly chat:write, which
// is the whole basis of the split.
const (
	readScopes  = "channels:history,channels:read,groups:history,im:read,search:read,users:read"
	writeScopes = "channels:read,chat:write,users:read"
)

// TestMain installs go-keyring's in-memory provider for the whole binary. No
// test may reach the developer's real keychain — and because cred reads through
// the same provider, a credential this binary stores can be resolved back
// through cred without a Secret Service, which is what the round-trip tests
// need on a CI runner.
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

// isolate points every credential source at a temp directory and an empty
// keyring, so neither a real enrolment nor another test can influence this one.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	// Enrolment seeds the identity cache, so this must be pinned inside
	// the temp home: a developer with XDG_CACHE_HOME set would otherwise have
	// a test run overwrite their real identity.json.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("USER", "tester")
	unsetEnv(t, "SLACK_READ_TOKEN")
	unsetEnv(t, "SLACK_WRITE_TOKEN")
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	return home
}

// unsetEnv removes a variable for the duration of the test. t.Setenv is called
// first purely for its cleanup, which restores the developer's real value.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	err := os.Unsetenv(key)
	if err != nil {
		t.Fatal(err)
	}
}

// noKeyring is a host with no Secret Service: go-keyring fails every call, as
// it does on a headless Linux box, in a container and on a CI runner. Both this
// binary and cred then use the file, which is why the fallback is a first-class
// path rather than a degraded one.
func noKeyring(t *testing.T) {
	t.Helper()
	keyring.MockInitWithError(errExecDbusLaunchExecutable)
	t.Cleanup(keyring.MockInit)
}

// script drives the prompts from a string through the same console a terminal
// uses: raw is nil, so no terminal mode is changed, but the masking loop, the
// backspace handling and everything the prompts echo are the production ones.
func script(t *testing.T, input string) {
	t.Helper()
	prev := newConsole
	newConsole = func(_ io.Reader, out io.Writer) *console {
		return &console{in: bufio.NewReader(strings.NewReader(input)), out: out}
	}
	t.Cleanup(func() { newConsole = prev })
}

// enrolment is one scripted answer to one prompt: yes, then a pasted token.
func enrolment(token string) string { return "y\n" + token + "\n" }

// slack is a fake Slack whose auth.test answers with the given scope header and
// body. An empty header is omitted entirely, which is the case the scope check
// has to fail closed on.
func slack(t *testing.T, scopes, body string) *slacktest.Server {
	t.Helper()
	s := slacktest.New(t)
	s.ReplyFunc("auth.test", func(w http.ResponseWriter, r *http.Request) {
		if scopes != "" {
			w.Header().Set("X-Oauth-Scopes", scopes)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
	return s
}

const okBody = `{"ok":true,"user":"stepan","team":"LedgerRocket","user_id":"U0123ABCD"}`

// storedToken reads a role's credential back without going through this
// package's own lookup, so that a test asserting "it was stored" cannot be
// satisfied by two matching bugs. The service name, the account key and the
// path are spelled out here on purpose: they are the contract with cred.
func storedToken(t *testing.T, r role) (string, bool) {
	t.Helper()
	v, err := keyring.Get(r.service, os.Getenv("USER"))
	if err == nil && v != "" {
		return v, true
	}
	b, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid", r.fileName))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func assertNothingStored(t *testing.T) {
	t.Helper()
	for _, r := range []role{readRole, writeRole} {
		if v, ok := storedToken(t, r); ok {
			t.Fatalf("a %s credential was stored (%d chars); nothing should have been", r.name, len(v))
		}
	}
}

// The point of the whole binary: what enrolment writes, the three commands can
// find. The service name, the account key and the file path are duplicated from
// cred, which does not export them, so this is what holds the two in agreement.
func TestStoredReadCredentialIsResolvableByCred(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}

	c, err := cred.ResolveRead()
	if err != nil {
		t.Fatalf("cred.ResolveRead after enrolment: %v", err)
	}
	if got := authorization(t, c.Authorize); got != "Bearer "+readToken {
		t.Fatalf("cred resolved %q, want the token that was just enrolled", got)
	}
}

func TestStoredWriteCredentialIsResolvableByCred(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken)+enrolment(writeToken))
	s := slacktest.New(t)
	// The read prompt and the write prompt need different scope sets, and they
	// are the same endpoint, so the header is chosen per call by which token
	// asked.
	s.ReplyFunc("auth.test", func(w http.ResponseWriter, r *http.Request) {
		scopes := readScopes
		if r.Header.Get("Authorization") == "Bearer "+writeToken {
			scopes = writeScopes
		}
		w.Header().Set("X-Oauth-Scopes", scopes)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okBody)
	})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--with-write"}, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}

	c, err := cred.ResolveWrite()
	if err != nil {
		t.Fatalf("cred.ResolveWrite after enrolment: %v", err)
	}
	if got := authorization(t, c.Authorize); got != "Bearer "+writeToken {
		t.Fatalf("cred resolved %q, want the token that was just enrolled", got)
	}
}

// The same round trip on a host with no keyring, which is every CI runner and
// most containers. The file is the only source left, and cred has to find it.
func TestCredentialStoredInAFileIsResolvableByCred(t *testing.T) {
	isolate(t)
	noKeyring(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}

	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid", "read.token")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no token file at %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
	if !strings.Contains(stdout.String(), path) {
		t.Errorf("stdout did not name the file it wrote:\n%s", stdout.String())
	}

	c, err := cred.ResolveRead()
	if err != nil {
		t.Fatalf("cred.ResolveRead after a file enrolment: %v", err)
	}
	if got := authorization(t, c.Authorize); got != "Bearer "+readToken {
		t.Fatalf("cred resolved %q, want the token that was just enrolled", got)
	}
}

func authorization(t *testing.T, set func(*http.Request)) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	set(req)
	return req.Header.Get("Authorization")
}

// Read-only by default. Without --with-write there is no write prompt at all,
// so posting stays impossible until somebody deliberately asks for it — and the
// scripted input holding a second answer proves the prompt was never reached
// rather than merely declined.
func TestWriteCredentialNeedsTheFlag(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken)+enrolment(writeToken))
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if _, ok := storedToken(t, writeRole); ok {
		t.Fatal("a write credential was stored without --with-write")
	}
	_, err := cred.ResolveWrite()
	if !errors.Is(err, cred.ErrNotFound) {
		t.Fatalf("cred.ResolveWrite error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(stdout.String(), "posting is disabled") {
		t.Errorf("stdout did not say posting is off:\n%s", stdout.String())
	}
	if got := s.Hits(); got != 1 {
		t.Errorf("Slack was called %d times, want 1 — only the read token is verified", got)
	}
}

// A bot token carries chat:write and would pass the scope check, but posts as
// an app rather than as you, and nothing about the message says so until
// somebody sees it in Slack. The prefix is enough to refuse it, so no request
// is made at all.
func TestBotTokenIsRefusedByItsPrefix(t *testing.T) {
	isolate(t)
	script(t, enrolment(botToken))
	s := slack(t, writeScopes, okBody)

	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1 — a refusal is a failure", code)
	}
	assertNothingStored(t)
	if got := s.Hits(); got != 0 {
		t.Errorf("Slack was called %d times; a bot token is refused before any request", got)
	}
	if !strings.Contains(stderr.String(), "BOT token") {
		t.Errorf("stderr did not explain the refusal:\n%s", stderr.String())
	}
}

// The other half of the same refusal: a token that does not announce itself as
// a bot in its prefix, but whose auth.test reply carries bot_id.
func TestBotTokenIsRefusedByBotIDInTheReply(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, `{"ok":true,"user":"some-app","team":"LedgerRocket","bot_id":"B0123ABCD"}`)

	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	assertNothingStored(t)
	if !strings.Contains(stderr.String(), "BOT token") {
		t.Errorf("stderr did not explain the refusal:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "some-app") {
		t.Errorf("stderr did not name the app the message would come from:\n%s", stderr.String())
	}
}

// The scope check in both directions. It is what mechanically enforces the
// two-app split: without it the two credentials are two copies of one token,
// and every other separation in the tool is decoration.
func TestScopeCheckRefusesTheWrongTokenForTheRole(t *testing.T) {
	tests := []struct {
		name string
		args []string
		// script answers the prompts. The write case declines the read prompt
		// first, so the write role is the only one that could store anything.
		script string
		scopes string
		want   string
	}{
		{
			name:   "a read token that can post",
			script: enrolment(readToken),
			scopes: writeScopes,
			want:   "carries chat:write",
		},
		{
			name:   "a write token that cannot post",
			args:   []string{"--with-write"},
			script: "n\n" + enrolment(writeToken),
			scopes: readScopes,
			want:   "does not carry chat:write",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			script(t, tc.script)
			s := slack(t, tc.scopes, okBody)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr, s.URL)

			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stdout %q)", code, stdout.String())
			}
			assertNothingStored(t)
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr did not say %q:\n%s", tc.want, stderr.String())
			}
		})
	}
}

// Fail closed. Storing a token whose scopes could not be read is exactly how a
// read token carrying chat:write ends up enrolled as a read credential, the one
// outcome the check exists to prevent. Not knowing is a refusal.
func TestUnknownScopesAreARefusal(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		token  string
		script string
		scopes string
	}{
		{name: "read role, header absent", token: readToken, script: enrolment(readToken)},
		{name: "read role, header empty", token: readToken, script: enrolment(readToken), scopes: " , "},
		{
			name:   "write role, header absent",
			args:   []string{"--with-write"},
			token:  writeToken,
			script: "n\n" + enrolment(writeToken),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			script(t, tc.script)
			s := slack(t, tc.scopes, okBody)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr, s.URL)

			if code != 1 {
				t.Fatalf("exit code = %d, want 1 — an unreadable scope list is a refusal", code)
			}
			assertNothingStored(t)
			if !strings.Contains(stderr.String(), "x-oauth-scopes") {
				t.Errorf("stderr did not say why it refused:\n%s", stderr.String())
			}
		})
	}
}

// Verification precedes storage, so a token Slack will not accept never becomes
// a credential the commands would then fail with.
func TestATokenSlackRejectsIsNotStored(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, `{"ok":false,"error":"invalid_auth"}`)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	assertNothingStored(t)
	if !strings.Contains(stderr.String(), "invalid_auth") {
		t.Errorf("stderr did not quote Slack's reason:\n%s", stderr.String())
	}
}

func TestVerificationNamesTheAccount(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "verified as stepan @ LedgerRocket") {
		t.Errorf("stdout did not name the account:\n%s", stdout.String())
	}
}

// The token is never printed, on any path. What is printed is a fingerprint,
// and the assertion is deliberately blunt: no window of the token longer than
// the fingerprint's own may appear anywhere in either stream, so an accidental
// echo of a middle slice fails as loudly as printing the whole thing.
func TestTheTokenIsNeverPrinted(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		scopes string
		body   string
	}{
		{name: "stored", token: readToken, scopes: readScopes, body: okBody},
		{name: "refused for its scopes", token: readToken, scopes: writeScopes, body: okBody},
		{name: "refused for unknown scopes", token: readToken, body: okBody},
		{name: "refused as a bot", token: botToken, scopes: readScopes, body: okBody},
		{name: "rejected by Slack", token: readToken, scopes: readScopes, body: `{"ok":false,"error":"invalid_auth"}`},
		{
			name:   "refused for bot_id",
			token:  readToken,
			scopes: readScopes,
			body:   `{"ok":true,"user":"some-app","team":"T","bot_id":"B1"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			script(t, enrolment(tc.token))
			s := slack(t, tc.scopes, tc.body)

			var stdout, stderr bytes.Buffer
			run(nil, strings.NewReader(""), &stdout, &stderr, s.URL)

			out := stdout.String() + stderr.String()
			// 13 characters is one more than the fingerprint discloses: nine
			// leading and four trailing, which never appear adjacent.
			for i := 0; i+13 <= len(tc.token); i++ {
				if window := tc.token[i : i+13]; strings.Contains(out, window) {
					t.Fatalf("output contains %d characters of the token at offset %d:\n%s", len(window), i, out)
				}
			}
		})
	}
}

// The masked prompt echoes asterisks and nothing else, which is what makes a
// paste visible without putting it on screen.
func TestThePromptEchoesOnlyAsterisks(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "*"); got != len(readToken) {
		t.Errorf("echoed %d asterisks, want one per character (%d)", got, len(readToken))
	}
}

func TestFingerprintIsPrinted(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	want := "received: " + fingerprint(readToken)
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout does not contain %q:\n%s", want, stdout.String())
	}
}

func TestFingerprint(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{token: "xoxp-1234567890abcdefab12", want: "xoxp-1234...ab12 (25 chars)"},
		{token: readToken, want: "xoxp-1111...aa01 (47 chars)"},
		// Below the threshold nothing but the length: the two windows would
		// otherwise print almost the whole string.
		{token: "xoxp-short", want: "(10 chars)"},
		{token: "", want: "(0 chars)"},
	}
	for _, tc := range tests {
		if got := fingerprint(tc.token); got != tc.want {
			t.Errorf("fingerprint(%d chars) = %q, want %q", len(tc.token), got, tc.want)
		}
	}
}

// Enrolment needs a terminal, and with stdin redirected there is none: nothing
// is asked, nothing is stored, no request is made, and the exit status is clean
// so that a script running the installer does not fail on it.
func TestNonInteractiveStoresNothing(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devNull.Close() })

	// A file, a pipe and /dev/null: none is a terminal, and /dev/null is the
	// one a mode-bit check would wrongly call one.
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipeR.Close(); pipeW.Close() })

	regular, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { regular.Close() })

	for _, tc := range []struct {
		name  string
		stdin io.Reader
	}{
		{name: "/dev/null", stdin: devNull},
		{name: "a pipe", stdin: pipeR},
		{name: "a regular file", stdin: regular},
		{name: "not a file at all", stdin: strings.NewReader(enrolment(readToken))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			code := run([]string{"--with-write"}, tc.stdin, &stdout, &stderr, s.URL)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
			}
			assertNothingStored(t)
			if got := s.Hits(); got != 0 {
				t.Errorf("Slack was called %d times with no terminal, want 0", got)
			}
			if !strings.Contains(stdout.String(), "not a terminal") {
				t.Errorf("stdout did not say why nothing happened:\n%s", stdout.String())
			}
		})
	}
}

// --help and --version leave no trace: no terminal opened, no request, no file
// under HOME, and above all no credential looked at. Asking a tool what it does
// must not enrol anyone in anything or raise a keychain prompt.
func TestHelpAndVersionTouchNothing(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "--version"} {
		t.Run(flag, func(t *testing.T) {
			home := isolate(t)
			opened := watchConsole(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			code := run([]string{flag}, strings.NewReader(enrolment(readToken)), &stdout, &stderr, s.URL)

			if code != 0 {
				t.Errorf("exit code = %d, want 0 — it was asked for, so it is output", code)
			}
			if stdout.Len() == 0 {
				t.Error("nothing on stdout")
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want the answer on stdout alone", stderr.String())
			}
			if *opened {
				t.Error("a prompt was opened; --help must never ask for a token")
			}
			if got := s.Hits(); got != 0 {
				t.Errorf("Slack was called %d times", got)
			}
			assertNothingStored(t)
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Errorf("left %v behind under HOME", entries)
			}
		})
	}
}

// watchConsole reports whether a prompt was opened, and answers as an
// unattended machine would: if the behaviour under test regresses, the test
// fails rather than blocking on a terminal that is not there.
func watchConsole(t *testing.T) *bool {
	t.Helper()
	var opened bool
	prev := newConsole
	newConsole = func(io.Reader, io.Writer) *console {
		opened = true
		return nil
	}
	t.Cleanup(func() { newConsole = prev })
	return &opened
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "an unknown option", args: []string{"--with-read"}},
		{name: "a stray argument", args: []string{"xoxp-token"}},
		{name: "two options", args: []string{"--with-write", "--help"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			opened := watchConsole(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			if code := run(tc.args, strings.NewReader(""), &stdout, &stderr, s.URL); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want the diagnostic on stderr alone", stdout.String())
			}
			if *opened {
				t.Error("a prompt was opened for a misuse")
			}
			assertNothingStored(t)
		})
	}
}

// Declining, pasting nothing and cancelling all leave the machine as it was —
// and none of them is a failure, because nothing was wrong. That is the
// distinction the exit status carries: a refusal is 1, a choice is 0.
func TestChoosingNotToEnrolIsNotAFailure(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{name: "declined", script: "n\n", want: "skipped"},
		{name: "declined by default", script: "\n", want: "skipped"},
		{name: "nothing pasted", script: "y\n\n", want: "nothing received"},
		{name: "cancelled with ctrl-c", script: "y\n" + string(ctrlC), want: "cancelled"},
		{name: "cancelled with ctrl-d", script: "y\n" + string(ctrlD), want: "cancelled"},
		{name: "input ran out", script: "", want: "cancelled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			script(t, tc.script)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
			}
			assertNothingStored(t)
			if got := s.Hits(); got != 0 {
				t.Errorf("Slack was called %d times, want 0", got)
			}
			if out := stdout.String() + stderr.String(); !strings.Contains(out, tc.want) {
				t.Errorf("output does not contain %q:\n%s", tc.want, out)
			}
		})
	}
}

// A credential already in place is named and confirmed before it is replaced,
// so re-running enrolment cannot quietly swap the token a working install is
// using for one somebody pasted by mistake.
func TestExistingCredentialIsNotReplacedWithoutConfirmation(t *testing.T) {
	tests := []struct {
		name    string
		confirm string
		want    string
	}{
		{name: "declined", confirm: "n\n", want: "xoxp-already-enrolled-token-0000"},
		{name: "confirmed", confirm: "y\n", want: readToken},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			const existing = "xoxp-already-enrolled-token-0000"
			err := keyring.Set(readRole.service, os.Getenv("USER"), existing)
			if err != nil {
				t.Fatal(err)
			}
			script(t, enrolment(readToken)+tc.confirm)
			s := slack(t, readScopes, okBody)

			var stdout, stderr bytes.Buffer
			if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
			}
			got, ok := storedToken(t, readRole)
			if !ok {
				t.Fatal("the existing credential disappeared")
			}
			if got != tc.want {
				t.Fatalf("stored token is %d chars, want the %s one", len(got), tc.name)
			}
			if !strings.Contains(stdout.String(), "already stored") {
				t.Errorf("stdout did not say a credential was already there:\n%s", stdout.String())
			}
			// Named, not disclosed: the prompt describes what it would replace
			// by type and length only.
			if strings.Contains(stdout.String()+stderr.String(), existing) {
				t.Error("the existing token was printed")
			}
		})
	}
}

// A token pasted with a stray newline or a leading space is the same token.
// cred strips whitespace from every source it reads, so enrolment has to strip
// it too, or what is stored is a token nothing can match.
func TestWhitespaceIsStrippedFromThePastedToken(t *testing.T) {
	isolate(t)
	script(t, "y\n  "+readToken+" \t\n")
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got := s.Last().Auth; got != "Bearer "+readToken {
		t.Errorf("Authorization = %q, want the stripped token", got)
	}
	got, ok := storedToken(t, readRole)
	if !ok {
		t.Fatal("nothing was stored")
	}
	if got != readToken {
		t.Errorf("stored %q, want the stripped token", got)
	}
}

// Storage happens once, at the end. A refusal on the write role must not undo
// the read credential that was already stored, and must still be reported.
func TestOneRoleRefusedDoesNotUndoTheOther(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken)+enrolment(writeToken))
	// Both tokens answer with the read app's scopes, so the write role is
	// refused for lacking chat:write while the read role passes.
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--with-write"}, strings.NewReader(""), &stdout, &stderr, s.URL); code != 1 {
		t.Fatalf("exit code = %d, want 1 — the write role was refused", code)
	}
	if _, ok := storedToken(t, readRole); !ok {
		t.Error("the read credential was not stored")
	}
	if _, ok := storedToken(t, writeRole); ok {
		t.Error("the write credential was stored despite being refused")
	}
}

// cred resolves $SLACK_READ_TOKEN before anything stored, and by presence
// rather than by value — so an exported variable, even an empty one, makes a
// freshly enrolled credential unreachable. Enrolment cannot unset it, but it
// must not report success as though the commands will now use what it stored.
func TestAnEnvironmentVariableThatOutranksTheStoredCredentialIsReported(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "set to another token", value: "xoxp-something-else-0000", want: "will use it, not what was just stored"},
		{name: "set but empty", value: "", want: "no read credential at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("SLACK_READ_TOKEN", tc.value)
			script(t, enrolment(readToken))
			s := slack(t, readScopes, okBody)

			var stdout, stderr bytes.Buffer
			if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "SLACK_READ_TOKEN is set here") {
				t.Errorf("stderr did not warn about the variable:\n%s", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr did not say %q:\n%s", tc.want, stderr.String())
			}
		})
	}
}

// A token pasted from a CRLF source ends the masked line with a byte to spare.
// Left in the buffer it becomes the answer to the next prompt — which is the
// one that decides whether an existing credential is replaced, so the token
// just verified would be discarded and nobody would have chosen that.
func TestACRLFPasteDoesNotAnswerTheNextPrompt(t *testing.T) {
	isolate(t)
	err := keyring.Set(readRole.service, os.Getenv("USER"), "xoxp-already-enrolled-token-0000")
	if err != nil {
		t.Fatal(err)
	}
	script(t, "y\r\n"+readToken+"\r\ny\r\n")
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	got, ok := storedToken(t, readRole)
	if !ok {
		t.Fatal("nothing was stored")
	}
	if got != readToken {
		t.Fatalf("stored a %d-char token; the replacement was answered by a leftover newline", len(got))
	}
}

// Enrolment names the production host through slackread's constant, so the read
// client is in this binary's import graph. The write client is not, and must not
// be: enrolling a write credential is not the same as being able to use one, and
// a binary that could do both would make "posting stays impossible until someone
// asks for it" a claim about argument parsing rather than about what is linked.
func TestImportGraphExcludesWriteCode(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, "slackwrite") {
			t.Fatalf("cmd/hersaid-setup depends on %s", line)
		}
	}
}

// Enrolment already calls auth.test to validate the token and check its scopes,
// so the id in that reply is free. Writing it through means a fresh install has
// the reaction marker from its first read rather than after whoami — the
// difference between a feature that works and one that ships dark.
func TestEnrolmentSeedsTheIdentity(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}

	var id cache.Identity
	err := cache.Read(cache.IdentityFile, &id)
	if err != nil {
		t.Fatalf("identity after enrolment: %v, want it seeded", err)
	}
	if id.UserID != "U0123ABCD" {
		t.Fatalf("UserID = %q, want U0123ABCD", id.UserID)
	}
	// Bound to the credential hear will actually resolve, not merely to the
	// function enrolment happened to call: the two agree only while enrolment
	// and cred normalise the token identically, and a divergence there is the
	// silent-dark failure this seeding exists to prevent.
	c, err := cred.ResolveRead()
	if err != nil {
		t.Fatalf("cred.ResolveRead after enrolment: %v", err)
	}
	if id.CredentialKey != c.Key() {
		t.Fatalf("CredentialKey = %q, want the resolved credential's key %q", id.CredentialKey, c.Key())
	}
	if strings.Contains(id.CredentialKey, readToken) {
		t.Fatalf("CredentialKey = %q, want a digest rather than the token", id.CredentialKey)
	}
}

// A token Slack refuses is not stored, and must not leave an identity behind
// either — the marker would then name an account whose credential nothing holds.
func TestARefusedTokenSeedsNoIdentity(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, `{"ok":false,"error":"invalid_auth"}`)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code == 0 {
		t.Fatalf("exit code = 0, want failure (stdout %q)", stdout.String())
	}
	var id cache.Identity
	err := cache.Read(cache.IdentityFile, &id)
	if err == nil {
		t.Fatalf("identity = %+v, want none after a refused token", id)
	}
}

// The write credential belongs to the other app and is never what hear reads
// with, so enrolling it must not overwrite the identity the read role seeded.
func TestTheWriteRoleDoesNotOverwriteTheIdentity(t *testing.T) {
	isolate(t)
	script(t, enrolment(readToken)+enrolment(writeToken))
	s := slacktest.New(t)
	s.ReplyFunc("auth.test", func(w http.ResponseWriter, r *http.Request) {
		scopes, body := readScopes, okBody
		if strings.Contains(r.Header.Get("Authorization"), writeToken) {
			scopes = writeScopes
			body = `{"ok":true,"user":"other","team":"LedgerRocket","user_id":"UOTHER999"}`
		}
		w.Header().Set("X-Oauth-Scopes", scopes)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--with-write"}, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}

	var id cache.Identity
	err := cache.Read(cache.IdentityFile, &id)
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "U0123ABCD" {
		t.Fatalf("UserID = %q, want the read role's id kept", id.UserID)
	}
}

// An env var outranks anything stored (cred resolves it first), so hear reads
// with that credential and not the one being enrolled. Seeding here would
// replace an identity that credential warmed with a key nothing will match,
// silently turning a working marker off — worse than leaving the newly stored
// credential to be warmed by a whoami later.
func TestEnrolmentLeavesTheIdentityAloneWhenTheEnvVarOutranksIt(t *testing.T) {
	isolate(t)
	warm := cache.Identity{UserID: "UENVACCT", User: "env", CredentialKey: cred.KeyFor(envToken)}
	err := cache.Write(cache.IdentityFile, warm)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLACK_READ_TOKEN", envToken)
	script(t, enrolment(readToken))
	s := slack(t, readScopes, okBody)

	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, s.URL); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}

	var id cache.Identity
	err = cache.Read(cache.IdentityFile, &id)
	if err != nil {
		t.Fatal(err)
	}
	if id != warm {
		t.Fatalf("identity = %+v, want the warm one left alone: %+v", id, warm)
	}
}
