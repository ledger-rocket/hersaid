package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/resolve"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("HERSAID_WRITE_CHANNELS", "")
	err := os.Unsetenv("HERSAID_WRITE_CHANNELS")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "state", "hersaid", "posts.jsonl")
}

// warmCache is what `hear channels` would have left behind. tap has no read
// scope of its own, so without it a #name cannot resolve.
func warmCache(t *testing.T) {
	t.Helper()
	err := cache.Write(cache.ConversationsFile, cache.Conversations{Channels: []cache.Conversation{
		{ID: "C0123ABCD", Name: "eng"},
		{ID: "C0456EFGH", Name: "general"},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

type record map[string]any

func readLog(t *testing.T, path string) []record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []record
	for line := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r record
		err := json.Unmarshal([]byte(line), &r)
		if err != nil {
			t.Fatalf("audit line is not JSON: %v\n%s", err, line)
		}
		out = append(out, r)
	}
	return out
}

func phases(recs []record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		p, _ := r["phase"].(string)
		out = append(out, p)
	}
	return out
}

func tap(t *testing.T, s *slacktest.Server, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, s.URL)
	return code, stdout.String(), stderr.String()
}

// With no write credential nothing is applied and no request is issued at all.
// The gate is the same credential sez's is, so it must close the same way.
func TestCredentialGateIssuesNoRequest(t *testing.T) {
	isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "")
	s := slacktest.New(t)
	s.Reply("reactions.add", `{"ok":true}`)

	code, _, stderr := tap(t, s, "#eng", "1785712596.256909", "thumbsup")

	if code == 0 {
		t.Fatal("exit code = 0; a refused reaction must be non-zero")
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
	if !strings.Contains(stderr, "no write credential found") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// A kill mid-request must still leave evidence a reaction was attempted, so the
// intent record is written first. The assertion runs inside the request
// handler.
func TestIntentIsRecordedBeforeTheRequestBegins(t *testing.T) {
	log := isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	var atRequest []record
	s.ReplyFunc("reactions.add", func(w http.ResponseWriter, r *http.Request) {
		atRequest = readLog(t, log)
		w.Write([]byte(`{"ok":true}`))
	})

	if code, _, stderr := tap(t, s, "#eng", "1785712596.256909", "thumbsup"); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, stderr)
	}

	if got := phases(atRequest); !reflect.DeepEqual(got, []string{"intent"}) {
		t.Fatalf("audit at the moment of the request = %v, want [intent]", got)
	}
	if atRequest[0]["action"] != "react.add" || atRequest[0]["emoji"] != "thumbsup" {
		t.Fatalf("intent record = %v, want it to say what was about to happen", atRequest[0])
	}
	if got := phases(readLog(t, log)); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("audit phases = %v, want [intent outcome]", got)
	}
}

// A post-send transport failure is not a definite failure: Slack may have
// applied the reaction.
func TestPostSendTransportFailureIsRecordedAsUnknown(t *testing.T) {
	log := isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Drop("reactions.add")

	code, _, stderr := tap(t, s, "#eng", "1785712596.256909", "thumbsup")

	if code == 0 {
		t.Fatal("exit code = 0; an unknown outcome must be reported")
	}
	if !strings.Contains(stderr, "UNKNOWN") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := s.Hits(); got != 1 {
		t.Fatalf("Slack received %d requests, want exactly 1 — a write is never retried", got)
	}
	recs := readLog(t, log)
	if recs[1]["ok"] != nil {
		t.Fatalf("outcome ok = %v, want null, not false", recs[1]["ok"])
	}
}

// Idempotent outcomes: the end state is the one that was asked for, so they are
// successes carrying the code as context.
func TestIdempotentOutcomesSucceed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		method string
		code   string
		stdout string
	}{
		{"already reacted", []string{"#eng", "1.1", "thumbsup"}, "reactions.add", "already_reacted", "already on that message"},
		{"no reaction", []string{"--remove", "#eng", "1.1", "thumbsup"}, "reactions.remove", "no_reaction", "was not on that message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := isolate(t)
			warmCache(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			s.Reply(tc.method, `{"ok":false,"error":"`+tc.code+`"}`)

			code, stdout, stderr := tap(t, s, tc.args...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stdout, tc.stdout) {
				t.Fatalf("stdout = %q, want %q", stdout, tc.stdout)
			}
			recs := readLog(t, log)
			if recs[1]["ok"] != true {
				t.Fatalf("outcome ok = %v, want true", recs[1]["ok"])
			}
			if recs[1]["error"] != tc.code {
				t.Fatalf("outcome error = %v, want %q as context", recs[1]["error"], tc.code)
			}
		})
	}
}

// The exit code for a lost record, on the reaction lane. A reaction that was
// applied and whose outcome record was lost is not a success: a failed audit
// append is fatal (exit 3). The applied reaction is still reported.
//
// The log is closed while the request is in flight, which is the one moment
// that produces exactly this: a durable intent record, an applied reaction, and
// no way to write the record accounting for it.
func TestALostOutcomeRecordIsFatalAndStillReportsTheReaction(t *testing.T) {
	logPath := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	wcred, err := cred.ResolveWrite()
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	s := slacktest.New(t)
	s.ReplyFunc("reactions.add", func(w http.ResponseWriter, r *http.Request) {
		err := log.Close()
		if err != nil {
			t.Errorf("closing the log: %v", err)
		}
		w.Write([]byte(`{"ok":true}`))
	})

	var stdout, stderr bytes.Buffer
	code := react(&stdout, &stderr, log,
		audit.Reaction{
			Action: audit.Add, Channel: "#eng", ChannelID: "C0123ABCD",
			TS: "1785712596.256909", Emoji: "thumbsup",
		},
		s.URL, wcred, false,
		resolve.Target{Input: "#eng", ID: "C0123ABCD", Name: "eng", Resolved: true},
		"1785712596.256909", "thumbsup")

	if code != 3 {
		t.Fatalf("exit code = %d, want 3 — a reaction nobody can account for is not a success", code)
	}
	if !strings.Contains(stdout.String(), "add :thumbsup:") {
		t.Fatalf("stdout = %q, want the reaction still reported", stdout.String())
	}
	if !strings.Contains(stderr.String(), "AUDIT WRITE FAILED") {
		t.Fatalf("stderr = %q, want it to name the lost record", stderr.String())
	}
	if got := phases(readLog(t, logPath)); !reflect.DeepEqual(got, []string{"intent"}) {
		t.Fatalf("audit phases = %v, want [intent] — the outcome is the record that was lost", got)
	}
}

// The intent-before-request ordering on the reaction lane, seen from the
// failing side: when the intent record cannot be written, no reaction is
// applied at all.
//
// The refusal comes before the request, and exit 1 says the command refused
// rather than 3, which means "it happened and was lost".
//
// The log is closed before react() is called: the same instrument the
// lost-outcome test uses, moved one record earlier.
func TestAFailedIntentRecordRefusesToReact(t *testing.T) {
	logPath := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	wcred, err := cred.ResolveWrite()
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	err = log.Close()
	if err != nil {
		t.Fatal(err)
	}

	s := slacktest.New(t)
	// Stubbed so that a regression shows up as an applied reaction rather than
	// as an unstubbed-endpoint complaint.
	s.Reply("reactions.add", `{"ok":true}`)

	var stdout, stderr bytes.Buffer
	code := react(&stdout, &stderr, log,
		audit.Reaction{
			Action: audit.Add, Channel: "#eng", ChannelID: "C0123ABCD",
			TS: "1785712596.256909", Emoji: "thumbsup",
		},
		s.URL, wcred, false,
		resolve.Target{Input: "#eng", ID: "C0123ABCD", Name: "eng", Resolved: true},
		"1785712596.256909", "thumbsup")

	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0 — no write may begin without a durable intent record", got)
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 — the command refused; nothing was left unaccounted for", code)
	}
	if !strings.Contains(stderr.String(), "refusing to send") {
		t.Fatalf("stderr = %q, want it to say the reaction was refused", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing reported as applied", stdout.String())
	}
	if recs := readLog(t, logPath); len(recs) != 0 {
		t.Fatalf("audit records = %v, want none — the log is the thing that failed", recs)
	}
}

// The record shape must stay the reaction one: an action and an emoji, and none
// of the post-only fields.
func TestReactionRecordShape(t *testing.T) {
	log := isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("reactions.remove", `{"ok":true}`)

	if code, _, stderr := tap(t, s, "--remove", "#eng", "1785712596.256909", ":+1:"); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, stderr)
	}
	rec := readLog(t, log)[1]
	if rec["action"] != "react.remove" {
		t.Fatalf("action = %v", rec["action"])
	}
	// The colons are stripped: Slack wants the bare name.
	if rec["emoji"] != "+1" {
		t.Fatalf("emoji = %v, want the bare name", rec["emoji"])
	}
	for _, f := range []string{"thread_ts", "permalink", "text", "slack_text"} {
		if _, present := rec[f]; present {
			t.Errorf("reaction record gained the post-only field %q", f)
		}
	}
}

// reactions.add needs a real conversation id, so unlike chat.postMessage a bare
// #name cannot be passed through — the cache hear builds is required.
func TestColdCacheIsRefusedWithNoRequest(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("reactions.add", `{"ok":true}`)

	code, _, stderr := tap(t, s, "#eng", "1785712596.256909", "thumbsup")
	if code == 0 {
		t.Fatal("exit code = 0; a #name cannot be passed to reactions.add")
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
	if !strings.Contains(stderr, "cache") {
		t.Fatalf("stderr = %q, want it to name the cold cache", stderr)
	}
}

func TestAllowlistRefusesATargetOutsideItWithNoRequest(t *testing.T) {
	log := isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	t.Setenv("HERSAID_WRITE_CHANNELS", "#eng")
	s := slacktest.New(t)
	s.Reply("reactions.add", `{"ok":true}`)

	code, _, stderr := tap(t, s, "#general", "1785712596.256909", "thumbsup")

	if code == 0 {
		t.Fatal("exit code = 0; a target outside the allowlist must be refused")
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
	if !strings.Contains(stderr, "HERSAID_WRITE_CHANNELS") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := phases(readLog(t, log)); !reflect.DeepEqual(got, []string{"refused"}) {
		t.Fatalf("audit phases = %v, want [refused]", got)
	}
}

// A dry run resolves and validates without calling Slack, and is still logged.
func TestDryRunSendsNothingAndIsLogged(t *testing.T) {
	log := isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, stdout, stderr := tap(t, s, "--dry-run", "#eng", "1785712596.256909", "thumbsup")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, stderr)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
	if !strings.Contains(stdout, "#eng -> C0123ABCD") {
		t.Fatalf("stdout = %q, want the resolution shown", stdout)
	}
	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"dry-run"}) {
		t.Fatalf("audit phases = %v, want [dry-run]", got)
	}
	if recs[0]["dry_run"] != true {
		t.Fatalf("dry_run = %v, want true", recs[0]["dry_run"])
	}
}

// A mode the audit log cannot be given is reported, as sez reports it.
//
// Both commands append to the same file, so which of them happened to open it
// is not something the person reading the warning chose; a log that cannot be
// kept at 0600 is worth saying either way.
//
// Provoking it needs a file this process can write to but cannot chmod, and the
// only portable one is a file it does not own: /dev/null. Writing a record to
// that file then fails as well, so the exit code and the log contents say
// nothing useful here and the warning is the whole assertion.
func TestAnUnenforceableLogModeIsReported(t *testing.T) {
	logPath := isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	err := os.MkdirAll(filepath.Dir(logPath), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink(os.DevNull, logPath)
	if err != nil {
		t.Fatal(err)
	}
	// As root — a container's default — chmod succeeds and there is no warning
	// to report, which would leave this asserting nothing at all.
	probe, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	if len(probe.Warnings()) == 0 {
		t.Skipf("this environment can chmod %s, so the warning cannot be provoked", os.DevNull)
	}

	s := slacktest.New(t)
	_, _, stderr := tap(t, s, "--dry-run", "#eng", "1785712596.256909", "thumbsup")

	if !strings.Contains(stderr, "tap: warning") || !strings.Contains(stderr, "cannot enforce 0600") {
		t.Fatalf("stderr = %q, want the unenforceable mode reported as sez reports it", stderr)
	}
}

// --help and --version leave no trace at all.
//
// Asking a tool what it does must not create a file or reach a keychain — on
// macOS that is a modal unlock prompt, and on Linux a D-Bus call where there
// may be no Secret Service.
func TestHelpAndVersionTouchNothing(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "--version"} {
		t.Run(flag, func(t *testing.T) {
			home := isolateHome(t)
			consulted := watchCredential(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			code := run([]string{flag}, &stdout, &stderr, s.URL)

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
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Errorf("left %v behind under HOME; state, cache and config must be untouched", entries)
			}
		})
	}
}

func TestHelpDocumentsEveryFlag(t *testing.T) {
	isolateHome(t)
	watchCredential(t)
	s := slacktest.New(t)

	var help, version bytes.Buffer
	run([]string{"--help"}, &help, io.Discard, s.URL)
	run([]string{"--version"}, &version, io.Discard, s.URL)

	for _, want := range []string{
		"--remove", "--dry-run", "--help", "--version",
		"HERSAID_WRITE_CHANNELS", "HERSAID_TIMEOUT", "SLACK_WRITE_TOKEN",
	} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("--help does not mention %q", want)
		}
	}
	if !strings.HasPrefix(version.String(), "tap ") {
		t.Errorf("--version = %q, want it to name the command", version.String())
	}
}

// A usage error is the other case: a diagnostic on stderr, exit 2.
func TestUsageErrorsAreDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--rmeove", "#eng", "1.1", "thumbsup"}},
		{"too few arguments", []string{"#eng", "1785712596.256909"}},
		{"too many arguments", []string{"#eng", "1785712596.256909", "thumbsup", "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr, s.URL)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want the diagnostic on stderr", stdout.String())
			}
			if !strings.Contains(stderr.String(), "usage:") {
				t.Errorf("stderr = %q, want the usage text", stderr.String())
			}
			if got := s.Hits(); got != 0 {
				t.Errorf("Slack received %d requests", got)
			}
		})
	}
}

// sentinelToken is what a credential resolved behind the seam's back is worth.
const sentinelToken = "xoxp-sentinel-no-test-may-resolve-a-real-credential"

// isolateHome points every directory the commands use, including HOME itself,
// inside one temp directory — so anything created anywhere is visible as an
// entry in it.
//
// It also plants the sentinel in both token variables. watchCredential stubs
// the resolveWrite variable, which covers this file's code as written; code
// that called cred.ResolveWrite directly would walk straight past the stub to
// the keyring, and on a developer's machine — or anywhere SLACK_WRITE_TOKEN is
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
	prev := resolveWrite
	resolveWrite = func() (cred.Write, error) {
		consulted = true
		return cred.Write{}, cred.ErrNotFound
	}
	t.Cleanup(func() { resolveWrite = prev })
	return &consulted
}

// Validation happens before the credential is even resolved, so a malformed
// argument can never reach the network.
func TestMalformedArgumentsAreRefusedWithNoRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"not a timestamp", []string{"#eng", "yesterday", "thumbsup"}},
		{"timestamp carrying a separator", []string{"#eng", "1.1&channel=C0", "thumbsup"}},
		{"emoji carrying a separator", []string{"#eng", "1.1", "thumbs&name=fire"}},
		{"uppercase emoji", []string{"#eng", "1.1", "ThumbsUp"}},
		{"channel that looks like a flag", []string{"--", "-rf", "1.1", "thumbsup"}},
		{"wrong argument count", []string{"#eng", "1.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			warmCache(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			s.Reply("reactions.add", `{"ok":true}`)

			if code, _, _ := tap(t, s, tc.args...); code == 0 {
				t.Fatal("exit code = 0; the invocation must be refused")
			}
			if got := s.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests, want 0", got)
			}
		})
	}
}

// A cold cache is not a dead end, and the message has to say so: this command
// holds a write credential only, so the list it needs is one hear runs away and
// nothing it does by itself will fill it.
func TestAColdCacheNamesTheCommandThatFillsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{"channel", "#eng", "hear channels"},
		{"person", "@sam", "hear dms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t) // no warmCache: the cache is cold
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			s.Reply("reactions.add", `{"ok":true}`)

			code, _, stderr := tap(t, s, tc.target, "1785712596.256909", "thumbsup")
			if code == 0 {
				t.Fatal("exit code = 0; nothing could be resolved")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr = %q, want it to name %q", stderr, tc.want)
			}
			if got := s.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests, want 0", got)
			}
		})
	}
}

// missing_scope is the one Slack refusal with a fix the user can act on, and
// the bare code names neither the scope nor where to add it.
func TestMissingScopeExplainsTheFix(t *testing.T) {
	isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("reactions.add", `{"ok":false,"error":"missing_scope"}`)

	code, _, stderr := tap(t, s, "#eng", "1785712596.256909", "thumbsup")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	for _, want := range []string{"missing_scope", "reactions:write", "hersaid-setup"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to mention %q", stderr, want)
		}
	}
}

// Only that refusal gets the extra paragraph. Printing it for every failure
// would send someone editing OAuth scopes over a message that was refused for a
// reason scopes have nothing to do with.
func TestOtherRefusalsDoNotSuggestAScopeChange(t *testing.T) {
	isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("reactions.add", `{"ok":false,"error":"message_not_found"}`)

	_, _, stderr := tap(t, s, "#eng", "1785712596.256909", "thumbsup")
	if strings.Contains(stderr, "reactions:write") {
		t.Fatalf("stderr = %q, want no scope advice for an unrelated refusal", stderr)
	}
}

// The guard is released after the outcome record, never before it: a signal in
// between would leave a reaction Slack had applied with nothing but an intent
// line to account for it. What the log held at the moment of release is the
// assertion, because everything inside that window is local — there is no round
// trip here to hold open and signal into, as there is in sez.
func TestTheAbortGuardOutlivesTheOutcomeRecord(t *testing.T) {
	log := isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("reactions.add", `{"ok":true}`)

	var atRelease []string
	prev := onAbort
	onAbort = func(func(os.Signal)) func() {
		return func() { atRelease = phases(readLog(t, log)) }
	}
	t.Cleanup(func() { onAbort = prev })

	if code, _, stderr := tap(t, s, "#eng", "1785712596.256909", "thumbsup"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if want := []string{"intent", "outcome"}; !reflect.DeepEqual(atRelease, want) {
		t.Fatalf("the log held %v when the guard was released, want %v", atRelease, want)
	}
}
