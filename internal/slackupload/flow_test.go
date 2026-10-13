package slackupload

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slackhosts"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
	"github.com/ledger-rocket/hersaid/internal/slackwrite"
)

func writeCred(t *testing.T, token string) cred.Write {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SLACK_WRITE_TOKEN", token)
	c, err := cred.ResolveWrite()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// flowClient wires a real slackwrite client at the fake Slack, and points the
// upload host at a sink both can reach.
func flowClient(t *testing.T, s *slacktest.Server, hosts slackhosts.Hosts) *Client {
	t.Helper()
	api := slackwrite.New(writeCred(t, "xoxp-fake-write-token"), slackhttp.New(2*time.Second), s.URL)
	return New(api, hosts, 2*time.Second)
}

func TestStageReportsEachFileAsItLands(t *testing.T) {
	s := slacktest.New(t)
	up, _ := uploadHost(t, http.StatusOK)
	s.Pages("files.getUploadURLExternal",
		`{"ok":true,"file_id":"F1","upload_url":"`+up.URL+`/u?`+canary+`"}`,
		`{"ok":true,"file_id":"F2","upload_url":"`+up.URL+`/u?`+canary+`"}`)

	var seen []string
	staged, err := flowClient(t, s, loopback).Stage(context.Background(),
		[]Local{local(t, 16), local(t, 32)},
		func(st Staged) { seen = append(seen, st.ID) })
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 2 || staged[0].ID != "F1" || staged[1].ID != "F2" {
		t.Fatalf("staged = %+v, want F1 then F2", staged)
	}
	// The callback is what makes a signal handler's view true during the send
	// rather than only after it.
	if strings.Join(seen, ",") != "F1,F2" {
		t.Fatalf("callback saw %v, want each file as it landed", seen)
	}
}

// The ids of what did stage come back with the error. Nothing is visible, but
// an interrupted attempt has to be reconcilable, and this is the only record.
func TestStageReturnsWhatStagedAlongsideItsFailure(t *testing.T) {
	s := slacktest.New(t)
	up, _ := uploadHost(t, http.StatusOK)
	s.Pages("files.getUploadURLExternal",
		`{"ok":true,"file_id":"F1","upload_url":"`+up.URL+`/u?`+canary+`"}`,
		`{"ok":false,"error":"file_uploads_disabled"}`)

	staged, err := flowClient(t, s, loopback).Stage(context.Background(),
		[]Local{local(t, 16), local(t, 32)}, nil)
	if err == nil {
		t.Fatal("Stage() = nil error, want the second file's refusal")
	}
	if len(staged) != 1 || staged[0].ID != "F1" {
		t.Fatalf("staged = %+v, want the one file that did land", staged)
	}
	if !strings.Contains(err.Error(), "trace.pdf") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

// A dropped connection while staging stages nothing and returns its error. The
// classification that follows from it — failed, never unknown — is the command's
// to make and is asserted there, in cmd/sez, where the record is written.
func TestAStagingFailureStagesNothingAndReturnsItsError(t *testing.T) {
	s := slacktest.New(t)
	s.Drop("files.getUploadURLExternal")

	staged, err := flowClient(t, s, loopback).Stage(context.Background(),
		[]Local{local(t, 16)}, nil)
	if err == nil {
		t.Fatal("Stage() = nil error, want the dropped connection")
	}
	if len(staged) != 0 {
		t.Fatalf("staged = %+v, want none", staged)
	}
}

func shareOf(t *testing.T, reply string, files []Staged, channel string) (Shared, error) {
	t.Helper()
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal", reply)
	return flowClient(t, s, loopback).Share(context.Background(),
		ShareRequest{Files: files, ChannelID: channel}, ShareBoundary{})
}

func staged(ids ...string) []Staged {
	out := make([]Staged, 0, len(ids))
	for _, id := range ids {
		out = append(out, Staged{Name: id + ".pdf", ID: id})
	}
	return out
}

func TestShareTakesTheTimestampEveryShareAgreesOn(t *testing.T) {
	got, err := shareOf(t,
		`{"ok":true,"files":[{"id":"F1","shares":{"private":{"C1":[{"ts":"9.9"}]}}},`+
			`{"id":"F2","shares":{"private":{"C1":[{"ts":"9.9"}]}}}]}`,
		staged("F1", "F2"), "C1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TS != "9.9" {
		t.Fatalf("TS = %q, want 9.9", got.TS)
	}
	if strings.Join(got.IDs, ",") != "F1,F2" {
		t.Fatalf("IDs = %v, want the requested ids in order", got.IDs)
	}
}

// Two timestamps mean the files did not land in one message. Picking one would
// record a message holding some of them and link to it as though it were all.
func TestSharesThatDisagreeAreNotAMessage(t *testing.T) {
	_, err := shareOf(t,
		`{"ok":true,"files":[{"id":"F1","shares":{"public":{"C1":[{"ts":"1.1"}]}}},`+
			`{"id":"F2","shares":{"public":{"C1":[{"ts":"2.2"}]}}}]}`,
		staged("F1", "F2"), "C1")
	if !errors.Is(err, ErrManyMessages) {
		t.Fatalf("error = %v, want ErrManyMessages", err)
	}
	if ClassifyShare(err) != slackwrite.Ambiguous {
		t.Fatalf("ClassifyShare = %v, want Ambiguous", ClassifyShare(err))
	}
}

// The documented minimal reply. No timestamp is a success whose anchor is the
// file ids, not a failure.
func TestAReplyWithNoSharesIsStillASuccess(t *testing.T) {
	got, err := shareOf(t, `{"ok":true,"files":[{"id":"F1"}]}`, staged("F1"), "C1")
	if err != nil {
		t.Fatalf("Share() error = %v, want a success", err)
	}
	if got.TS != "" {
		t.Fatalf("TS = %q, want empty", got.TS)
	}
	if strings.Join(got.IDs, ",") != "F1" {
		t.Fatalf("IDs = %v, want F1", got.IDs)
	}
}

func TestAReplyThatDoesNotMatchWhatWasStagedIsUnreconciled(t *testing.T) {
	for _, c := range []struct {
		name, reply string
		want        string
	}{
		{"omits one", `{"ok":true,"files":[{"id":"F1"}]}`, "no answer for F2"},
		{"repeats one", `{"ok":true,"files":[{"id":"F1"},{"id":"F1"},{"id":"F2"}]}`, "appears 2 times"},
		{"invents one", `{"ok":true,"files":[{"id":"F1"},{"id":"F2"},{"id":"F9"}]}`, "F9 was never staged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := shareOf(t, c.reply, staged("F1", "F2"), "C1")
			if !errors.Is(err, ErrUnreconciled) {
				t.Fatalf("error = %v, want ErrUnreconciled", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to say %q", err, c.want)
			}
			if ClassifyShare(err) != slackwrite.Ambiguous {
				t.Fatalf("ClassifyShare = %v, want Ambiguous: Slack answered ok, so what it did is unknown",
					ClassifyShare(err))
			}
		})
	}
}

// The boundaries fire inside the call, which is the whole reason they exist.
func TestShareAnnouncesBothBoundaries(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal",
		`{"ok":true,"files":[{"id":"F1","shares":{"public":{"C1":[{"ts":"7.7"}]}}}]}`)

	var order []string
	var decided Shared
	_, err := flowClient(t, s, loopback).Share(context.Background(),
		ShareRequest{Files: staged("F1"), ChannelID: "C1"},
		ShareBoundary{
			Issued:  func() { order = append(order, "issued") },
			Decided: func(sh Shared, err error) { order = append(order, "decided"); decided = sh },
		})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "issued,decided" {
		t.Fatalf("boundaries = %v, want issued then decided", order)
	}
	// Decided carries the outcome, so a caller interrupted after it can record
	// what Slack actually said instead of "unknown".
	if decided.TS != "7.7" {
		t.Fatalf("Decided saw TS %q, want the timestamp Slack returned", decided.TS)
	}
}

// Decided fires for a refusal too, carrying the error: an interruption after
// Slack has answered must record the answer, not an unknown.
func TestDecidedFiresOnARefusalWithItsError(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal", `{"ok":false,"error":"posting_to_channel_denied"}`)

	var got error
	var fired bool
	_, err := flowClient(t, s, loopback).Share(context.Background(),
		ShareRequest{Files: staged("F1"), ChannelID: "C1"},
		ShareBoundary{Decided: func(_ Shared, e error) { fired, got = true, e }})
	if err == nil {
		t.Fatal("Share() = nil error, want the refusal")
	}
	if !fired {
		t.Fatal("Decided did not fire on a refusal")
	}
	if got == nil || slackwrite.Code(got) != "posting_to_channel_denied" {
		t.Fatalf("Decided saw %v, want the refusal", got)
	}
}

// The classification table, with the reason for each entry. A code is a proven
// refusal only when its own meaning forecloses the share.
func TestClassifyShareIsUnknownUnlessTheCodeProvesOtherwise(t *testing.T) {
	for _, c := range []struct {
		code string
		want slackwrite.Outcome
	}{
		{"missing_scope", slackwrite.Rejected},
		{"posting_to_channel_denied", slackwrite.Rejected},
		// Says one ticket was not found, and nothing about the others.
		{"file_not_found", slackwrite.Ambiguous},
		{"internal_error", slackwrite.Ambiguous},
		{"fatal_error", slackwrite.Ambiguous},
		{"file_update_failed", slackwrite.Ambiguous},
		{"something_new_slack_added", slackwrite.Ambiguous},
	} {
		err := error(&slackwrite.APIError{Method: "files.completeUploadExternal", Code: c.code})
		if got := ClassifyShare(err); got != c.want {
			t.Errorf("ClassifyShare(%s) = %v, want %v", c.code, got, c.want)
		}
	}
	if got := ClassifyShare(nil); got != slackwrite.Delivered {
		t.Errorf("ClassifyShare(nil) = %v, want Delivered", got)
	}
	// A preflight failure means no request left the process, so nothing was shared.
	pre := fmt.Errorf("share: %w", &slackwrite.PreflightError{Method: "files.completeUploadExternal", Err: slackwrite.ErrNoCredential})
	if got := ClassifyShare(pre); got != slackwrite.Rejected {
		t.Errorf("ClassifyShare(preflight) = %v, want Rejected", got)
	}
	// A transport failure on the completion leg is the one case that genuinely
	// is unknown: the request left and no answer came back.
	s := slacktest.New(t)
	s.Drop("files.completeUploadExternal")
	_, err := flowClient(t, s, loopback).Share(context.Background(),
		ShareRequest{Files: staged("F1"), ChannelID: "C1"}, ShareBoundary{})
	if got := ClassifyShare(err); got != slackwrite.Ambiguous {
		t.Fatalf("ClassifyShare(transport) = %v, want Ambiguous", got)
	}
}
