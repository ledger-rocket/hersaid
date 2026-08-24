package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
}

// The identity output is a documented interface: it is what tells a user which
// account a token belongs to, and its format — including the two spaces before
// the id — is what people read.
func TestWhoamiFormatIsStable(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_READ_TOKEN", "xoxp-fake-read-token")
	s := slacktest.New(t)
	s.Reply("auth.test", `{"ok":true,"user":"stepan","team":"LedgerRocket","user_id":"U0123ABCD"}`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"whoami"}, &stdout, &stderr, testEndpoints(s.URL)); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := "stepan @ LedgerRocket  (U0123ABCD)\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if got := s.Last().Auth; got != "Bearer xoxp-fake-read-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := s.Last().Endpoint; got != "auth.test" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestWhoamiWithNoCredential(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_READ_TOKEN", "")
	s := slacktest.New(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"whoami"}, &stdout, &stderr, testEndpoints(s.URL)); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests with no credential, want 0", got)
	}
	if !strings.Contains(stderr.String(), "no read token found") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// The timeout rule at the command boundary: a non-positive timeout is refused
// rather than honoured: zero would disable the bound entirely, and it is the
// one value someone would set meaning "wait longer".
func TestNonPositiveTimeoutIsRejected(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_READ_TOKEN", "xoxp-fake-read-token")
	t.Setenv("HERSAID_TIMEOUT", "0")
	s := slacktest.New(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"whoami"}, &stdout, &stderr, testEndpoints(s.URL)); code == 0 {
		t.Fatal("exit code = 0; HERSAID_TIMEOUT=0 must be refused")
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests with an invalid timeout, want 0", got)
	}
	if !strings.Contains(stderr.String(), "positive") {
		t.Fatalf("stderr = %q, want it to explain the refusal", stderr.String())
	}
}

// --help and --version leave no trace at all: no cache, no config directory,
// and above all no credential — asking a read tool what it does must not raise
// a keychain prompt on macOS or a D-Bus call on a host with no Secret Service.
func TestHelpAndVersionTouchNothing(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "--version"} {
		t.Run(flag, func(t *testing.T) {
			home := isolateHome(t)
			consulted := watchCredential(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			code := run([]string{flag}, &stdout, &stderr, testEndpoints(s.URL))

			if code != 0 {
				t.Errorf("exit code = %d, want 0 — it was asked for, so it is output", code)
			}
			if stdout.Len() == 0 {
				t.Error("nothing on stdout")
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want the answer on stdout alone", stderr.String())
			}
			if *consulted {
				t.Error("a credential was resolved; --help must never prompt a keychain")
			}
			if got := s.Hits(); got != 0 {
				t.Errorf("Slack received %d requests", got)
			}
			if entries, err := os.ReadDir(home); err != nil {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Errorf("left %v behind under HOME; cache and config must be untouched", entries)
			}
		})
	}
}

func TestHelpDocumentsTheCommandsAndFlags(t *testing.T) {
	isolateHome(t)
	watchCredential(t)
	s := slacktest.New(t)

	var help, version bytes.Buffer
	run([]string{"--help"}, &help, io.Discard, testEndpoints(s.URL))
	run([]string{"--version"}, &version, io.Discard, testEndpoints(s.URL))

	for _, want := range []string{"whoami", "--help", "--version", "SLACK_READ_TOKEN", "HERSAID_TIMEOUT"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("--help does not mention %q", want)
		}
	}
	if !strings.HasPrefix(version.String(), "hear ") {
		t.Errorf("--version = %q, want it to name the command", version.String())
	}
}

// `hear whoami --help` must answer as a misuse, not by resolving a credential.
//
// cli.Intercept reads the first argument only, which is deliberate — only a
// parser knows that in `--thread --help` the second word is a timestamp — so
// the guarantee that asking a tool what it does never prompts for a credential
// rests on each verb refusing arguments it has no use for: a verb that ignored
// the rest would answer `hear whoami --help` by calling the API.
func TestWhoamiRejectsTrailingArguments(t *testing.T) {
	for _, args := range [][]string{{"whoami", "--help"}, {"whoami", "-h"}, {"whoami", "stepan"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateHome(t)
			consulted := watchCredential(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr, testEndpoints(s.URL))

			if *consulted {
				t.Error("a credential was resolved for an invocation the command cannot honour")
			}
			if got := s.Hits(); got != 0 {
				t.Errorf("Slack received %d requests, want 0", got)
			}
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want the diagnostic on stderr", stdout.String())
			}
			if !strings.Contains(stderr.String(), "usage:") {
				t.Errorf("stderr = %q, want the usage text", stderr.String())
			}
		})
	}
}

// A usage error is the other case: a diagnostic on stderr, exit 2.
func TestUsageErrorsAreDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"post"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr, testEndpoints(s.URL)); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want the diagnostic on stderr", stdout.String())
			}
			if !strings.Contains(stderr.String(), "usage:") {
				t.Errorf("stderr = %q, want the usage text", stderr.String())
			}
		})
	}
}

// A plain `go build` records the VCS revision by itself, so a binary nobody
// versioned still names the commit it came from rather than "unknown". That is
// the case that matters: a colleague's checkout, not a release.
func TestAnUnversionedBuildStillNamesItsRevision(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "hear")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, out)
	}
	rev, err := exec.Command("git", "rev-parse", "--short=7", "HEAD").Output()
	if err != nil {
		t.Skipf("no git revision to compare against: %v", err)
	}
	if !strings.Contains(string(out), strings.TrimSpace(string(rev))) {
		t.Fatalf("--version = %q, want it to name revision %s", out, rev)
	}
}

// The release flags reach the variables they name.
//
// This cannot be checked by running --version on a stamped binary: the linker
// silently ignores an -X whose target it cannot resolve, and the build stamp
// then supplies a perfectly plausible answer. Renaming any of the three
// variables would leave that check green while every released binary reported a
// version nobody set. Comparing the exact output against sentinels is what
// makes the failure visible.
func TestReleaseLinkerFlagsReachTheirVariables(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	const pkg = "github.com/ledger-rocket/hersaid/internal/version"
	bin := filepath.Join(t.TempDir(), "hear")
	ldflags := strings.Join([]string{
		"-X " + pkg + ".version=v9.8.7",
		"-X " + pkg + ".commit=0123456789abcdef0123456789abcdef01234567",
		"-X " + pkg + ".date=2026-08-03T12:00:00Z",
	}, " ")
	if out, err := exec.Command("go", "build", "-ldflags", ldflags, "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	if got, want := strings.TrimSpace(string(out)), "hear v9.8.7 (0123456, 2026-08-03)"; got != want {
		t.Fatalf("--version = %q, want %q — the -X flags did not reach internal/version", got, want)
	}
}

// sentinelToken is what a credential resolved behind the seam's back is worth.
const sentinelToken = "xoxp-sentinel-no-test-may-resolve-a-real-credential"

// isolateHome points every directory the command uses, including HOME itself,
// inside one temp directory — so anything created anywhere is visible as an
// entry in it.
//
// It also plants the sentinel in both token variables. watchCredential stubs
// the resolveRead variable, which covers this file's code as written; code that
// called cred.ResolveRead directly would walk straight past the stub to the
// keyring, and on a developer's machine — or anywhere SLACK_READ_TOKEN is
// exported — that would resolve, prompt, and quietly pass. The env var is the
// first source cred consults, so the sentinel wins instead: nothing real is
// reached, and the bypass surfaces as a request the assertions can see.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("SLACK_READ_TOKEN", sentinelToken)
	t.Setenv("SLACK_WRITE_TOKEN", sentinelToken)
	return home
}

// watchCredential reports whether the command resolved a credential. It answers
// as an unenrolled machine would rather than delegating: if the behaviour under
// test regresses, the test must fail, not open the developer's keychain.
func watchCredential(t *testing.T) *bool {
	t.Helper()
	var consulted bool
	prev := resolveRead
	resolveRead = func() (cred.Read, error) {
		consulted = true
		return cred.Read{}, cred.ErrNotFound
	}
	t.Cleanup(func() { resolveRead = prev })
	return &consulted
}

// The primary artifact-boundary check: write code is not in this binary's
// import graph, so no argument to hear can reach a write endpoint. A method set
// could not achieve this — any package can construct a client from a package it
// already imports — which is why slackread and slackwrite are separate packages.
func TestImportGraphExcludesWriteCode(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "slackwrite") {
			t.Fatalf("cmd/hear depends on %s", line)
		}
		if strings.Contains(line, "internal/allowlist") {
			// allowlist imports nothing from slackwrite today, but it is write
			// policy: pulling it in here would be a sign the boundary moved.
			t.Fatalf("cmd/hear depends on %s, which is write policy", line)
		}
	}
}

// The secondary check, on the artifact rather than the source.
//
// The build is deliberately unstripped: -ldflags="-s -w" erases every Go symbol
// on both darwin and linux, so `go tool nm` on a release artifact reports
// nothing and would pass vacuously. The raw byte scan is the half that survives
// stripping, because endpoint paths are string data rather than symbols.
func TestBuiltBinaryHasNoWriteEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "hear")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	forbidden := []string{"chat.postMessage", "chat.getPermalink", "reactions.add", "reactions.remove"}

	nm, err := exec.Command("go", "tool", "nm", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm: %v\n%s", err, nm)
	}
	for _, f := range append(forbidden, "slackwrite") {
		if strings.Contains(string(nm), f) {
			t.Errorf("symbol table names %q", f)
		}
	}

	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range forbidden {
		if bytes.Contains(raw, []byte(f)) {
			t.Errorf("binary contains the string %q", f)
		}
	}

	// Without a control the scan proves nothing, and here that is not
	// hypothetical: the linker drops the string constant of any endpoint the
	// binary does not call, so absence alone is weak evidence. auth.test is
	// reachable from whoami, so finding it shows the scan can see an endpoint
	// that is genuinely compiled in.
	if !bytes.Contains(raw, []byte("auth.test")) {
		t.Error("the byte scan cannot see a reachable endpoint, so it proves nothing")
	}
}
