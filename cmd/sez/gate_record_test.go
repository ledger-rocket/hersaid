package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// The log holds every attempt, and a refused send is an attempt: the caller
// asked for delivery and was told no. Without that line, a caller reading the
// log to answer "did anything try to go out" gets the wrong answer.
//
// The gate itself is not in question — no request is issued either way. What is
// in question is whether the refusal is discoverable afterwards.
func TestAGateRefusalIsRecorded(t *testing.T) {
	logPath := isolate(t)
	s := slacktest.New(t)
	watchCredential(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--send", "#eng", "hello"}, strings.NewReader(""), &stdout, &stderr, s.URL)

	if code == 0 {
		t.Fatal("exit = 0; a refused send must fail")
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests; the gate must precede the client", s.Hits())
	}

	recs := readLog(t, logPath)
	if len(recs) != 1 {
		t.Fatalf("audit lines = %d, want 1; a refused attempt must leave a record", len(recs))
	}
	r := recs[0]
	if p, _ := r["phase"].(string); p != "abort" && p != "refused" {
		t.Fatalf("phase = %v, want abort or refused", r["phase"])
	}
	if ok, _ := r["ok"].(bool); ok {
		t.Fatalf("ok = %v, want false; the send did not happen", r["ok"])
	}
	if e, _ := r["error"].(string); !strings.Contains(e, "credential") {
		t.Fatalf("error = %v, want it to name the missing credential", r["error"])
	}
	if txt, _ := r["text"].(string); txt != "hello" {
		t.Fatalf("text = %v, want the message that was refused", r["text"])
	}
}
