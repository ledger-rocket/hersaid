package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The goldens below pin the field set, the key order, the nullability and the
// tri-state ok. A single struct shared by both record types would silently
// change all four.
const (
	goldenPostIntent = `{"at":"2026-08-03T09:15:00Z","by":"stepan","phase":"intent","action":null,"channel":"#eng","channel_id":"C0123ABCD","thread_ts":null,"dry_run":false,"ok":null,"error":null,"ts":null,"permalink":null,"text":"hello","slack_text":null,"previous_text":null,"files":null}`

	goldenPostOutcome = `{"at":"2026-08-03T09:15:01Z","by":"stepan","phase":"outcome","action":null,"channel":"#eng","channel_id":"C0123ABCD","thread_ts":"1785712596.256909","dry_run":false,"ok":true,"error":null,"ts":"1785712599.000100","permalink":"https://example.invalid/archives/C/p1","text":"hello","slack_text":"hello","previous_text":null,"files":null}`

	// An edit, which is the only record that carries both an action and the
	// wording it replaced — the two fields the other goldens leave at their
	// zero, and so the only place their position in the key order is pinned.
	goldenPostEdit = `{"at":"2026-08-03T09:15:03Z","by":"stepan","phase":"outcome","action":"message.update","channel":"#eng","channel_id":"C0123ABCD","thread_ts":null,"dry_run":false,"ok":true,"error":null,"ts":"1785712599.000100","permalink":null,"text":"after","slack_text":"after","previous_text":"before","files":null}`

	goldenReactionOutcome = `{"at":"2026-08-03T09:15:02Z","by":"stepan","phase":"outcome","action":"react.add","channel":"#eng","channel_id":"C0123ABCD","ts":"1785712596.256909","emoji":"thumbsup","dry_run":false,"ok":true,"error":null}`
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestPostRecordFieldSetAndOrder(t *testing.T) {
	got, err := json.Marshal(Post{
		At: ts("2026-08-03T09:15:00Z"), By: "stepan", Phase: Intent,
		Channel: "#eng", ChannelID: "C0123ABCD", OK: Unknown, Text: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenPostIntent {
		t.Fatalf("post intent record\n got: %s\nwant: %s", got, goldenPostIntent)
	}

	got, err = json.Marshal(Post{
		At: ts("2026-08-03T09:15:01Z"), By: "stepan", Phase: Outcome,
		Channel: "#eng", ChannelID: "C0123ABCD", ThreadTS: "1785712596.256909",
		OK: Succeeded, TS: "1785712599.000100",
		Permalink: "https://example.invalid/archives/C/p1",
		Text:      "hello", SlackText: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenPostOutcome {
		t.Fatalf("post outcome record\n got: %s\nwant: %s", got, goldenPostOutcome)
	}
}

func TestReactionRecordFieldSetAndOrder(t *testing.T) {
	got, err := json.Marshal(Reaction{
		At: ts("2026-08-03T09:15:02Z"), By: "stepan", Action: Add,
		Channel: "#eng", ChannelID: "C0123ABCD", TS: "1785712596.256909",
		Emoji: "thumbsup", OK: Succeeded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenReactionOutcome {
		t.Fatalf("reaction record\n got: %s\nwant: %s", got, goldenReactionOutcome)
	}
}

// The two classes must not converge on one field set: a post record has no
// action or emoji, a reaction record has no thread_ts, permalink, text or
// slack_text. Asserting the difference is what stops a future refactor folding
// them together behind omitempty.
func TestRecordClassesHaveDifferentFieldSets(t *testing.T) {
	// Both carry an action, so the fixtures set one: a post record's is nullable,
	// and asserting against an unset one would pin the fixture rather than the
	// type.
	post := fields(t, Post{Channel: "#eng", Action: Send})
	reaction := fields(t, Reaction{Channel: "#eng", Action: Add})

	for _, f := range []string{"thread_ts", "permalink", "text", "slack_text"} {
		if !post[f] {
			t.Errorf("post record is missing %q", f)
		}
		if reaction[f] {
			t.Errorf("reaction record gained %q", f)
		}
	}
	if post["emoji"] {
		t.Error(`post record gained "emoji"`)
	}
	if !reaction["emoji"] {
		t.Error(`reaction record is missing "emoji"`)
	}
	for name, set := range map[string]map[string]bool{"post": post, "reaction": reaction} {
		if !set["action"] {
			t.Errorf("%s record is missing %q", name, "action")
		}
	}
}

func fields(t *testing.T, rec any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// An intent line has no outcome yet, so ok is tri-state on both record types.
// Pin every spelling.
func TestTristateMarshalling(t *testing.T) {
	for _, tc := range []struct {
		state Tristate
		want  string
	}{{Unknown, "null"}, {Failed, "false"}, {Succeeded, "true"}} {
		b, err := json.Marshal(tc.state)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("Tristate(%d) = %s, want %s", tc.state, b, tc.want)
		}
	}
}

// A message is attacker-controlled text, never a format string and
// never markup. Display names and message bodies arrive from anyone in the
// workspace, including an external Slack Connect guest, so the payloads here are
// the ones that have broken a shell pipeline before: sed metacharacters, Go
// replacement metacharacters, control bytes and a mention that looks like markup.
func TestUntrustedTextRoundTripsUnchanged(t *testing.T) {
	const nasty = "<@U0123ABCD> said \"a & b\" — \n\ttab\x1f and 日本語 ${x} $1 %s %!v(MISSING) s/x/y/"

	b, err := json.Marshal(Post{Channel: "#eng", Text: nasty, SlackText: nasty})
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Text      string `json:"text"`
		SlackText string `json:"slack_text"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Text != nasty || back.SlackText != nasty {
		t.Fatalf("text round-tripped as %q, want %q", back.Text, nasty)
	}
}

// A record must serialise the same way for the log as for any other caller,
// so a Phase 2 test can assert against json.Marshal and mean the file.
func TestLoggedBytesEqualMarshalledBytes(t *testing.T) {
	l, path := openTemp(t)
	rec := Post{
		At: ts("2026-08-03T09:15:00Z"), By: "stepan", Phase: Outcome,
		Channel: "#eng", ChannelID: "C0123ABCD", OK: Succeeded,
		Text: "<b> & \"quoted\"",
	}
	if err := l.Post(rec); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want)+"\n" {
		t.Fatalf("log line\n got: %s\nwant: %s\n", got, want)
	}
}

func openTemp(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "posts.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

// The log holds credentials-adjacent history of everything the user said, so
// its mode is not advisory.
func TestLogFileIsPrivate(t *testing.T) {
	l, path := openTemp(t)
	if err := l.Post(Post{Channel: "#eng", Phase: DryRun, OK: Succeeded}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %v, want 0600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("state dir mode = %v, want 0700", got)
	}
}

// Open must tighten a log left permissive by an earlier version or another tool.
func TestOpenTightensAnExistingLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posts.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %v, want 0600 after Open", got)
	}
}

// The log under concurrency: parallel writers interleave whole lines, never
// fragments. Each record is composed in full and then written once.
func TestConcurrentWritersInterleaveWholeLines(t *testing.T) {
	l, path := openTemp(t)

	const writers, each = 8, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := l.Post(Post{
					Channel: "#eng",
					Phase:   Outcome,
					OK:      Succeeded,
					Text:    strings.Repeat("x", 200+w),
				}); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	lines := 0
	for sc.Scan() {
		lines++
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %d is not whole JSON: %v\n%s", lines, err, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if want := writers * each; lines != want {
		t.Fatalf("wrote %d lines, want %d", lines, want)
	}
}

// A record that could not be written must be reported, never swallowed: a write
// that happened without its record is the thing the log exists to prevent.
func TestWriteFailureIsReported(t *testing.T) {
	l, _ := openTemp(t)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	err := l.Post(Post{Channel: "#eng", Phase: Outcome, OK: Succeeded})
	if err == nil {
		t.Fatal("Post on a closed log returned nil; a lost record must be an error")
	}
	if !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("error = %v, want it to wrap fs.ErrClosed", err)
	}
}

// The record is a single unbuffered write, so there is nothing for os.Exit to
// discard: the bytes are on disk before Post returns, readable by another
// process without this one closing the file.
func TestRecordIsDurableBeforePostReturns(t *testing.T) {
	l, path := openTemp(t)
	if err := l.Post(Post{Channel: "#eng", Phase: Intent, OK: Unknown}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"phase":"intent"`) {
		t.Fatalf("log holds %q; the record must be readable before Close", b)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatal("record is not newline-terminated; the next append would join it")
	}
}

func TestActorFallsBackWhenUserIsUnset(t *testing.T) {
	t.Setenv("USER", "")
	if err := os.Unsetenv("USER"); err != nil {
		t.Fatal(err)
	}
	if Actor() == "" {
		t.Fatal("Actor() is empty; an unset USER must not lose the record's author")
	}
}

func TestDefaultPathFollowsXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state")
	if got, want := DefaultPath(), "/tmp/state/hersaid/posts.jsonl"; got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

// The action and previous_text keys must sit where the wire struct puts them,
// which no other golden shows: every one of them leaves both at their zero.
func TestAnEditRecordPinsItsShape(t *testing.T) {
	got, err := json.Marshal(Post{
		At: ts("2026-08-03T09:15:03Z"), By: "stepan",
		Phase: Outcome, Action: Update,
		Channel: "#eng", ChannelID: "C0123ABCD",
		OK: Succeeded, TS: "1785712599.000100",
		Text: "after", SlackText: "after", PreviousText: "before",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenPostEdit {
		t.Fatalf("edit record\n got: %s\nwant: %s", got, goldenPostEdit)
	}
}
