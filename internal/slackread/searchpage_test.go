package slackread

import (
	"context"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// searchPageBody is the shape search.messages answers with, recorded from the
// real API with every value replaced: matches carry no thread_ts of their own,
// and the thread is named only in the permalink.
const searchPageBody = `{"ok":true,"query":"q","messages":{
  "total":250,
  "matches":[
    {"iid":"i1","type":"message","ts":"1700000300.000200","user":"U0000000002","username":"bob",
     "text":"a reply","team":"T0000000000","score":0,"no_reactions":true,"db_message":true,
     "channel":{"id":"C0000000001","name":"general","is_channel":true,"is_group":false,"is_im":false,
       "is_mpim":false,"is_private":false,"is_shared":false,"is_ext_shared":false,"is_org_shared":false,
       "is_archived":false,"is_pending_ext_shared":false,"pending_shared":[]},
     "permalink":"https://example.slack.test/archives/C0000000001/p1700000300000200?thread_ts=1700000000.000100&cid=C0000000001",
     "attachments":[{"text":"<@U0000000001> was named here"}],
     "blocks":[]},
    {"iid":"i2","type":"message","ts":"1700000200.000100","user":"U0000000003","username":"carol",
     "text":"in a group dm","team":"T0000000000",
     "channel":{"id":"G0000000001","name":"mpdm-alice--bob--carol-1","is_im":false,"is_mpim":true,"is_private":true},
     "permalink":"https://example.slack.test/archives/G0000000001/p1700000200000100"}
  ],
  "paging":{"count":2,"total":250,"page":2,"pages":125},
  "pagination":{"total_count":250,"page":2,"per_page":2,"page_count":125,"first":3,"last":4}
}}`

// SearchPage is search as a caller that pages needs it: newest first when asked,
// a page number, and the paging block that says whether there is more.
func TestSearchPageSendsSortAndPageAndDecodesPaging(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("search.messages", searchPageBody)

	got, err := client(t, s).SearchPage(context.Background(), SearchOptions{
		Query: "is:dm after:2023-11-12", Count: 100, Page: 2, Newest: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := s.Last()
	if got, want := req.QueryFields(), []string{"count", "page", "query", "sort", "sort_dir"}; !equal(got, want) {
		t.Fatalf("query fields = %v, want exactly %v", got, want)
	}
	q := req.Query
	if q.Get("query") != "is:dm after:2023-11-12" || q.Get("count") != "100" || q.Get("page") != "2" ||
		q.Get("sort") != "timestamp" || q.Get("sort_dir") != "desc" {
		t.Fatalf("query = %v", q)
	}
	if !got.PagingKnown || got.Paging != (SearchPaging{Count: 2, Total: 250, Page: 2, Pages: 125}) {
		t.Fatalf("paging = %+v", got.Paging)
	}
	if len(got.Matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(got.Matches))
	}
	m := got.Matches[0]
	if m.TS != "1700000300.000200" || m.User != "U0000000002" || m.Channel.ID != "C0000000001" {
		t.Fatalf("first match = %+v", m)
	}
	if m.Permalink == "" || string(m.Attachments) == "" {
		t.Fatalf("permalink or attachments lost: %+v", m)
	}
	g := got.Matches[1].Channel
	if !g.IsMPIM || g.IsIM || g.Name != "mpdm-alice--bob--carol-1" {
		t.Fatalf("group DM channel = %+v", g)
	}
}

// A page left unset and relevance order are Slack's defaults, so they are not
// sent: the request carries exactly the fields that were chosen.
func TestSearchPageOmitsWhatWasNotAskedFor(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("search.messages", `{"ok":true,"messages":{"matches":[]}}`)

	got, err := client(t, s).SearchPage(context.Background(), SearchOptions{Query: "x", Count: 20})
	if err != nil {
		t.Fatal(err)
	}
	if fields, want := s.Last().QueryFields(), []string{"count", "query"}; !equal(fields, want) {
		t.Fatalf("query fields = %v, want exactly %v", fields, want)
	}
	if got.Matches == nil {
		t.Fatal("matches = nil, want an empty list")
	}
	if got.PagingKnown || got.Paging != (SearchPaging{}) {
		t.Fatalf("paging = %+v (known %v), want zero and unknown when Slack sent none", got.Paging, got.PagingKnown)
	}
}

// The query is one field however it is written: the page and sort a caller
// chose cannot be overridden from inside it.
func TestSearchPageQueryCannotIntroduceAField(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("search.messages", `{"ok":true,"messages":{"matches":[]}}`)

	_, err := client(t, s).SearchPage(context.Background(), SearchOptions{
		Query: "a&page=9&sort_dir=asc&token=xoxp-stolen", Count: 10, Page: 1, Newest: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := s.Last().Query
	if q.Get("page") != "1" || q.Get("sort_dir") != "desc" || q.Has("token") {
		t.Fatalf("query = %v; the query string overrode a declared field", q)
	}
}
