package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// A refused reaction is an attempt, so the log has to hold a record of it. sez had
// the same gap: the gate sat before the log opened, so the one path that most
// wants explaining left nothing to explain it.
func TestAGateRefusalIsRecorded(t *testing.T) {
	logPath := isolate(t)
	s := slacktest.New(t)
	watchCredential(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"#eng", "1785712596.256909", ":+1:"}, &stdout, &stderr, s.URL)

	if code == 0 {
		t.Fatal("exit = 0; a refused reaction must fail")
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests; the gate must precede the client", s.Hits())
	}
	recs := readLog(t, logPath)
	if len(recs) != 1 {
		t.Fatalf("audit lines = %d, want 1; a refused attempt must leave a record", len(recs))
	}
	if ok, _ := recs[0]["ok"].(bool); ok {
		t.Fatalf("ok = %v, want false", recs[0]["ok"])
	}
	if e, _ := recs[0]["error"].(string); !strings.Contains(e, "credential") {
		t.Fatalf("error = %v, want it to name the missing credential", recs[0]["error"])
	}
}
