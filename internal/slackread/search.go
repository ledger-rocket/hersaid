package slackread

import (
	"context"
	"net/url"
	"strconv"
)

// SearchOptions bounds a search.messages request. Page and Newest are left
// unset to mean Slack's defaults: relevance order, first page.
type SearchOptions struct {
	// Query is the search expression, exactly as a person would type it.
	Query string
	// Count is how many matches to ask for in this page.
	Count int
	// Page is the page number to ask for. Zero means the first page.
	Page int
	// Newest asks for matches newest first rather than by relevance.
	Newest bool
}

// SearchPaging is search.messages' paging block, which says where this page
// sits in the result.
type SearchPaging struct {
	Count int `json:"count"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Pages int `json:"pages"`
}

// SearchResults is one page of search.messages. PagingKnown says Slack sent a
// paging block at all: without one, a zero Paging is no evidence that this
// was the last page.
type SearchResults struct {
	Matches     []SearchMatch
	Paging      SearchPaging
	PagingKnown bool
}

// SearchPage runs a workspace message search as a caller that pages needs it:
// a page number, newest-first when asked, and the paging block that says
// whether there is more.
func (c *Client) SearchPage(ctx context.Context, opts SearchOptions) (SearchResults, error) {
	params := url.Values{}
	params.Set("query", opts.Query)
	params.Set("count", strconv.Itoa(opts.Count))
	if opts.Page > 0 {
		params.Set("page", strconv.Itoa(opts.Page))
	}
	if opts.Newest {
		params.Set("sort", "timestamp")
		params.Set("sort_dir", "desc")
	}
	var out struct {
		envelope

		Messages struct {
			Matches []SearchMatch `json:"matches"`
			Paging  *SearchPaging `json:"paging"`
		} `json:"messages"`
	}
	err := c.get(ctx, "search.messages", params, &out)
	if err != nil {
		return SearchResults{}, err
	}
	// Empty, not nil: a caller that pages over the result treats an empty page
	// as the end, and a nil slice would marshal as null.
	matches := out.Messages.Matches
	if matches == nil {
		matches = []SearchMatch{}
	}
	res := SearchResults{Matches: matches}
	if p := out.Messages.Paging; p != nil {
		res.Paging, res.PagingKnown = *p, true
	}
	return res, nil
}
