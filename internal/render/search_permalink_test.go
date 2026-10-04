package render_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/render"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

// A search hit is only chainable if it carries the address of what it found.
// Everything else in a hit describes the message; the permalink is the one
// field a caller can hand straight back to `hear read`.
func TestASearchHitCarriesItsPermalink(t *testing.T) {
	t.Parallel()
	const link = "https://example.slack.test/archives/C1/p1782660796826639"
	var m slackread.SearchMatch
	m.TS = "1782660796.826639"
	m.Text = "found"
	m.Username = "stepan"
	m.Channel.ID = "C1"
	m.Channel.Name = "eng"
	m.Permalink = link

	hits := render.Hits([]slackread.SearchMatch{m}, render.Names{})
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if hits[0].Permalink != link {
		t.Fatalf("Permalink = %q, want %q", hits[0].Permalink, link)
	}

	b, err := json.Marshal(hits[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"permalink":"`+link+`"`) {
		t.Fatalf("marshalled hit = %s; want a permalink field", b)
	}
}

// Absent rather than empty: a hit Slack gave no permalink for should not carry
// a field a consumer would then have to test for emptiness.
func TestAHitWithNoPermalinkOmitsTheField(t *testing.T) {
	t.Parallel()
	var m slackread.SearchMatch
	m.TS = "1.1"
	m.Text = "found"
	m.Channel.ID = "C1"

	hits := render.Hits([]slackread.SearchMatch{m}, render.Names{})
	b, err := json.Marshal(hits[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "permalink") {
		t.Fatalf("marshalled hit = %s; want no permalink field", b)
	}
}
