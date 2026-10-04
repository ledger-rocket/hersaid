// Package audit appends one JSON line per write attempt to
// ~/.local/state/hersaid/posts.jsonl.
//
// Post and Reaction are separate types because they are genuinely different
// records: a reaction record always carries phase "outcome" and none
// of the post-only fields, and a post record's action is nullable where a
// reaction's is always set. Folding them into one struct would silently change
// both shapes, so each marshals its own field set, in the order jq emitted it.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// privateDirPerm and privateFilePerm keep what hersaid writes readable by its
// owner only.
const (
	privateDirPerm  = 0o700
	privateFilePerm = 0o600
)

var (
	errAuditNoLogPath  = errors.New("audit: no log path")
	errAuditShortWrite = errors.New("audit: short write")
)

// Phase distinguishes the lines of one attempt. A consumer reading outcomes
// must require phase == "outcome": an intent line's ok is null because the
// outcome is not yet known.
type Phase string

const (
	// Intent is written before the request leaves. It is the one record whose
	// existence survives SIGKILL, which is why it precedes the network.
	Intent Phase = "intent"
	// Outcome is the result of an attempt that reached the API.
	Outcome Phase = "outcome"
	// DryRun is a render or a validation that deliberately sent nothing.
	DryRun Phase = "dry-run"
	// Abort is any other non-zero exit, including a caught signal.
	Abort Phase = "abort"
	// Refused is a target the write allowlist excluded. It is a distinct phase
	// from Abort so a policy refusal is greppable; consumers filtering on
	// "outcome" are unaffected.
	Refused Phase = "refused"
)

// Tristate is the audit log's ok field. Unknown is not a failure: a transport
// error after the request left cannot distinguish "not delivered" from
// "delivered and the reply was lost", and recording it as failed would invite a
// retry that duplicates the message.
type Tristate uint8

const (
	// Unknown marshals to null.
	Unknown Tristate = iota
	// Failed marshals to false.
	Failed
	// Succeeded marshals to true.
	Succeeded
)

// MarshalJSON emits null, false or true.
func (t Tristate) MarshalJSON() ([]byte, error) {
	switch t {
	case Failed:
		return []byte("false"), nil
	case Succeeded:
		return []byte("true"), nil
	default:
		return []byte("null"), nil
	}
}

// stamp is UTC, whole seconds — the resolution a log is read at.
const stamp = "2006-01-02T15:04:05Z"

// Actor names who ran the command. $USER first, with a fallback because an
// unset USER must not lose the record.
func Actor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	u, err := user.Current()
	if err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

// Post is one line of a message attempt.
type Post struct {
	// At defaults to now, in UTC.
	At time.Time
	// By defaults to Actor().
	By        string
	Phase     Phase
	Action    Action
	Channel   string
	ChannelID string
	ThreadTS  string
	DryRun    bool
	OK        Tristate
	Error     string
	// TS is the timestamp Slack assigned to the delivered message.
	TS        string
	Permalink string
	Text      string
	// SlackText is what Slack actually stored, which can differ from what was
	// submitted: it sanitises and truncates. Logging both makes the difference
	// auditable.
	SlackText string
	// PreviousText is the wording an edit replaced, read before the change was
	// made. An edit destroys the previous wording as surely as a delete
	// destroys the message, and it is the only record of what was there.
	PreviousText string
	// Files are the attachments an upload carried, or the ones a delete removed
	// from the message it destroyed. Empty for every other kind of record.
	Files []File
}

// File is one attachment, as an upload record names it.
//
// It is an object rather than a bare id because the intent line is written
// before any id exists and must still say what was about to go out. ID fills in
// as each file is staged.
//
// Path holds the bytes of the path only insofar as they are valid UTF-8: a Unix
// path is a byte string and a JSON string is Unicode, so encoding/json replaces
// an invalid sequence with U+FFFD. That loss is accepted rather than worked
// around, a second base64 field on every record costing more than it returns.
type File struct {
	Path     string
	Name     string
	Size     int64
	Mimetype string
	// ID is Slack's file id, known only once the file has been staged.
	ID string
}

// fileWire pins one attachment's field set and key order, as postWire does for
// the record around it.
type fileWire struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Mimetype string  `json:"mimetype"`
	ID       *string `json:"id"`
}

// files renders the attachment list, or nil for a record with none — which
// marshals to null, like every other absent post field.
func files(in []File) []fileWire {
	if len(in) == 0 {
		return nil
	}
	out := make([]fileWire, 0, len(in))
	for _, f := range in {
		out = append(out, fileWire{
			Path: f.Path, Name: f.Name, Size: f.Size,
			Mimetype: f.Mimetype, ID: null(f.ID),
		})
	}
	return out
}

// nullAction renders an unset action as JSON null, matching how every other
// absent post field is written.
func nullAction(a Action) *Action {
	if a == "" {
		return nil
	}
	return &a
}

// postWire pins the field set and the key order of a post record.
type postWire struct {
	At    string `json:"at"`
	By    string `json:"by"`
	Phase Phase  `json:"phase"`
	// Nullable rather than omitted, like every other post field: a consumer
	// reads the same keys on every line. A record sez writes always names an
	// action; one built without one says so rather than going quiet.
	Action    *Action  `json:"action"`
	Channel   string   `json:"channel"`
	ChannelID *string  `json:"channel_id"`
	ThreadTS  *string  `json:"thread_ts"`
	DryRun    bool     `json:"dry_run"`
	OK        Tristate `json:"ok"`
	Error     *string  `json:"error"`
	TS        *string  `json:"ts"`
	Permalink *string  `json:"permalink"`
	Text      *string  `json:"text"`
	SlackText *string  `json:"slack_text"`
	// Nullable rather than omitted: a post record's field set is deliberately
	// fixed, so a consumer reads the same keys on every line.
	PreviousText *string `json:"previous_text"`
	// Files is null on every record that carries none, for the same reason.
	Files []fileWire `json:"files"`
}

// MarshalJSON emits a post record's field set, in order.
func (p Post) MarshalJSON() ([]byte, error) {
	return json.Marshal(postWire{
		At:           at(p.At),
		By:           by(p.By),
		Phase:        phase(p.Phase),
		Action:       nullAction(p.Action),
		Channel:      p.Channel,
		ChannelID:    null(p.ChannelID),
		ThreadTS:     null(p.ThreadTS),
		DryRun:       p.DryRun,
		OK:           p.OK,
		Error:        null(p.Error),
		TS:           null(p.TS),
		Permalink:    null(p.Permalink),
		Text:         null(p.Text),
		SlackText:    null(p.SlackText),
		PreviousText: null(p.PreviousText),
		Files:        files(p.Files),
	})
}

// Reaction is one line of a reaction attempt.
type Reaction struct {
	At time.Time
	By string
	// Phase defaults to Outcome. An Intent line is written before the request
	// so that a kill mid-flight still leaves evidence one was attempted.
	Phase     Phase
	Action    Action
	Channel   string
	ChannelID string
	// TS is the target message's timestamp.
	TS     string
	Emoji  string
	DryRun bool
	OK     Tristate
	Error  string
}

// Action names the operation a record describes.
type Action string

const (
	// Add is reactions.add.
	Add Action = "react.add"
	// Remove is reactions.remove.
	Remove Action = "react.remove"
	// Send is a chat.postMessage, the record's default.
	Send Action = "message.send"
	// Update is a chat.update, a rewrite in place.
	Update Action = "message.update"
	// Delete is a chat.delete, a destruction.
	Delete Action = "message.delete"
	// Upload is a file upload completed as a message.
	Upload Action = "message.upload"
)

// reactionWire pins the field set and the key order of a reaction record. It
// has no thread_ts, permalink, text or slack_text, and it carries an action —
// exactly the difference from postWire.
type reactionWire struct {
	At        string   `json:"at"`
	By        string   `json:"by"`
	Phase     Phase    `json:"phase"`
	Action    Action   `json:"action"`
	Channel   string   `json:"channel"`
	ChannelID *string  `json:"channel_id"`
	TS        *string  `json:"ts"`
	Emoji     *string  `json:"emoji"`
	DryRun    bool     `json:"dry_run"`
	OK        Tristate `json:"ok"`
	Error     *string  `json:"error"`
}

// MarshalJSON emits a reaction record's field set, in order.
func (r Reaction) MarshalJSON() ([]byte, error) {
	return json.Marshal(reactionWire{
		At:        at(r.At),
		By:        by(r.By),
		Phase:     phase(r.Phase),
		Action:    r.Action,
		Channel:   r.Channel,
		ChannelID: null(r.ChannelID),
		TS:        null(r.TS),
		Emoji:     null(r.Emoji),
		DryRun:    r.DryRun,
		OK:        r.OK,
		Error:     null(r.Error),
	})
}

func at(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(stamp)
}

func by(s string) string {
	if s == "" {
		return Actor()
	}
	return s
}

func phase(p Phase) Phase {
	if p == "" {
		return Outcome
	}
	return p
}

func null(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// DefaultPath is $XDG_STATE_HOME/hersaid/posts.jsonl, else
// ~/.local/state/hersaid/posts.jsonl.
func DefaultPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "hersaid", "posts.jsonl")
}

// Log appends records to the audit file.
//
// Every record is one unbuffered write(2) to a file opened O_APPEND, so there
// is no buffer for os.Exit to discard and concurrent invocations interleave
// whole lines rather than fragments.
type Log struct {
	mu       sync.Mutex
	f        *os.File
	warnings []string
}

// Open prepares the log at path, creating the directory 0700 and the file 0600.
func Open(path string) (*Log, error) {
	if path == "" {
		return nil, errAuditNoLogPath
	}
	err := os.MkdirAll(filepath.Dir(path), privateDirPerm)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, privateFilePerm)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	l := &Log{f: f}
	// An existing log may predate this, or have been created by another tool.
	// Warned about rather than refused: losing the record is worse than a
	// permissive mode on a file the user already owns.
	err = f.Chmod(privateFilePerm)
	if err != nil {
		l.warnings = append(l.warnings,
			fmt.Sprintf("cannot enforce 0600 on %s: %v", path, err))
	}
	return l, nil
}

// Warnings reports non-fatal problems noticed while opening.
func (l *Log) Warnings() []string { return l.warnings }

// Post appends a message record.
func (l *Log) Post(r Post) error { return l.append(r) }

// Reaction appends a reaction record.
func (l *Log) Reaction(r Reaction) error { return l.append(r) }

// Close releases the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// append encodes the whole record first and then issues exactly one write. A
// partial line would be indistinguishable from a truncated log, so a short
// write fails closed rather than being retried into a duplicated fragment.
func (l *Log) append(rec any) error {
	// json.Marshal, not an Encoder with SetEscapeHTML(false): a record must
	// serialise the same way here as it does for any other caller, and the
	// outer encoder re-escapes a MarshalJSON result anyway. Go spells < > & as
	// \u003c \u003e \u0026 where jq spells them literally; jq -r decodes both
	// to the same string, so the README's examples are unaffected.
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	body = append(body, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.f.Write(body)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if n != len(body) {
		return fmt.Errorf("%w, %d of %d bytes", errAuditShortWrite, n, len(body))
	}
	// One fsync per record, and a command writes at most three. It is not
	// needed for another process to see the line, only for power loss, but at
	// this rate it costs nothing worth reasoning about.
	err = l.f.Sync()
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// abortSignals are the ones a run can be cut short by.
var abortSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// exit is a variable so the signal path can be exercised without ending the
// test binary. Production always uses os.Exit.
var exit = os.Exit

// OnAbort runs f for the first of SIGINT, SIGTERM or SIGHUP and then exits with
// the conventional 128+n code, so a signal during the send window still leaves
// a record. The guarantee is one-sided: SIGKILL cannot be caught, so what
// survives it is the intent record, which is why intent precedes the request.
//
// The returned function stops handling and must be called once the write is
// done and recorded.
func OnAbort(f func(sig os.Signal)) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, abortSignals...)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			f(sig)
			exit(exitCode(sig))
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// signalExitBase is the shell convention: a process killed by signal n exits
// with 128+n.
const signalExitBase = 128

func exitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return signalExitBase + int(s)
	}
	return 1
}
