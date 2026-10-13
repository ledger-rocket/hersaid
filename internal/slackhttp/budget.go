package slackhttp

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// ErrBudgetSpent is returned by a Budget-wrapped RoundTripper once the budget
// has no requests left. It is a sentinel so that a caller can tell a refusal
// from a network error with errors.Is.
var ErrBudgetSpent = errors.New("slackhttp: budget spent")

// Budget is a ceiling on how many requests a RoundTripper sends. It is counted
// where a request leaves, so a request that fails on the way out still counts:
// what is bounded is how often Slack is asked, not how often it answers.
type Budget struct {
	mu    sync.Mutex
	max   int
	used  int
	cache int
}

// NewBudget returns a budget that allows maxRequests requests. A negative
// limit is a budget of nothing rather than an unlimited one.
func NewBudget(maxRequests int) *Budget {
	if maxRequests < 0 {
		maxRequests = 0
	}
	return &Budget{max: maxRequests}
}

// Used reports how many requests the budget has charged.
func (b *Budget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Max reports the ceiling the budget was built with.
func (b *Budget) Max() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max
}

// Cache reports how many requests were sent uncharged.
func (b *Budget) Cache() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cache
}

// Remaining reports how many requests the budget can still charge.
func (b *Budget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max - b.used
}

// Wrap returns a RoundTripper that charges one request before sending it and
// refuses the ones past the ceiling. A refusal is not a request, so it is not
// charged, and the request body is closed as the RoundTripper contract requires.
func (b *Budget) Wrap(next http.RoundTripper) http.RoundTripper {
	return &budgetRoundTripper{budget: b, next: next}
}

// budgetRoundTripper is the RoundTripper Wrap returns. It is a separate type so
// that the budget itself stays a plain counter.
type budgetRoundTripper struct {
	budget *Budget
	next   http.RoundTripper
}

func (r *budgetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.budget.mu.Lock()
	switch {
	case isUncharged(req.Context()):
		r.budget.cache++
	case r.budget.used >= r.budget.max:
		r.budget.mu.Unlock()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrBudgetSpent
	default:
		r.budget.used++
	}
	r.budget.mu.Unlock()
	return r.next.RoundTrip(req)
}

// unchargedKey marks a context whose requests a Budget counts apart.
type unchargedKey struct{}

// Uncharged returns a context whose requests a Budget counts in Cache rather
// than charging. It is for a cache fill: one that is paid once and then shared
// by every command, and that writes nothing unless it finishes, so a ceiling
// meant for one command's own requests would make it fail on every run.
func Uncharged(ctx context.Context) context.Context {
	return context.WithValue(ctx, unchargedKey{}, true)
}

func isUncharged(ctx context.Context) bool {
	v, _ := ctx.Value(unchargedKey{}).(bool)
	return v
}
