package slackhttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgeted is a client built as the commands build one, with b charged for
// every request it sends.
func budgeted(b *Budget) *http.Client {
	c := New(2 * time.Second)
	c.Transport = b.Wrap(c.Transport)
	return c
}

func send(t *testing.T, c *http.Client, url string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// The budget is a ceiling on what reaches Slack, so it is counted where a
// request leaves: every request is charged, and the one past the limit is
// refused before it is sent.
func TestBudgetRefusesTheRequestPastItsLimitWithoutSendingIt(t *testing.T) {
	t.Parallel()
	srv, hits := counting(t, http.StatusOK, nil)
	b := NewBudget(2)
	c := budgeted(b)

	for i := range 2 {
		err := send(t, c, srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	err := send(t, c, srv.URL)
	if !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("third request: err = %v, want ErrBudgetSpent", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want 2: the refused one must not be sent", got)
	}
	if b.Used() != 2 || b.Max() != 2 || b.Remaining() != 0 {
		t.Fatalf("used/max/remaining = %d/%d/%d, want 2/2/0", b.Used(), b.Max(), b.Remaining())
	}
}

// A request that fails on the way out was still sent, so it is still charged:
// what the budget bounds is how often Slack is asked, not how often it answers.
func TestBudgetChargesARequestThatFailed(t *testing.T) {
	t.Parallel()
	srv, _ := counting(t, http.StatusTooManyRequests, nil)
	b := NewBudget(5)
	c := budgeted(b)

	err := send(t, c, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = send(t, c, "http://127.0.0.1:1/unreachable")
	if err == nil {
		t.Fatal("a request to a closed port succeeded")
	}
	if got := b.Used(); got != 2 {
		t.Fatalf("used = %d, want 2", got)
	}
	if got := b.Remaining(); got != 3 {
		t.Fatalf("remaining = %d, want 3", got)
	}
}

// A refusal is not a request, so it is not charged, and asking again after the
// budget is spent keeps being refused rather than wrapping around.
func TestBudgetDoesNotChargeARefusal(t *testing.T) {
	t.Parallel()
	srv, hits := counting(t, http.StatusOK, nil)
	b := NewBudget(1)
	c := budgeted(b)

	_ = send(t, c, srv.URL)
	for range 3 {
		err := send(t, c, srv.URL)
		if !errors.Is(err, ErrBudgetSpent) {
			t.Fatalf("err = %v, want ErrBudgetSpent", err)
		}
	}
	if b.Used() != 1 || hits.Load() != 1 {
		t.Fatalf("used = %d, hits = %d, want 1 and 1", b.Used(), hits.Load())
	}
}

// The RoundTripper contract says a transport closes the request body even when
// it returns an error; a refusal that leaked it would hold a file or a pipe open.
func TestBudgetRefusalClosesTheRequestBody(t *testing.T) {
	t.Parallel()
	b := NewBudget(0)
	rt := b.Wrap(http.DefaultTransport)
	body := &closeTracker{Reader: strings.NewReader("x")}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://127.0.0.1:1/", body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("err = %v, want ErrBudgetSpent", err)
	}
	if !body.closed {
		t.Fatal("the refused request's body was not closed")
	}
}

// Concurrent requests share one budget and never overrun it.
func TestBudgetHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	srv, hits := counting(t, http.StatusOK, nil)
	b := NewBudget(10)
	c := budgeted(b)

	var wg sync.WaitGroup
	var mu sync.Mutex
	refused := 0
	for range 40 {
		wg.Go(func() {
			err := send(t, c, srv.URL)
			if errors.Is(err, ErrBudgetSpent) {
				mu.Lock()
				refused++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if hits.Load() != 10 || b.Used() != 10 || refused != 30 {
		t.Fatalf("hits = %d, used = %d, refused = %d, want 10, 10, 30", hits.Load(), b.Used(), refused)
	}
}

// A negative limit is a budget of nothing, not an unlimited one.
func TestBudgetNegativeLimitIsZero(t *testing.T) {
	t.Parallel()
	b := NewBudget(-3)
	if b.Max() != 0 || b.Remaining() != 0 {
		t.Fatalf("max/remaining = %d/%d, want 0/0", b.Max(), b.Remaining())
	}
}

type closeTracker struct {
	io.Reader

	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}

// A cache fill is paid once and then shared by every command, so it is counted
// apart from the budget and never refused by it.
func TestBudgetCountsUnchargedRequestsApart(t *testing.T) {
	t.Parallel()
	srv, hits := counting(t, http.StatusOK, nil)
	b := NewBudget(1)
	c := budgeted(b)

	for range 3 {
		err := sendCtx(t, c, Uncharged(context.Background()), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := send(t, c, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = send(t, c, srv.URL)
	if !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("err = %v, want ErrBudgetSpent", err)
	}
	if b.Cache() != 3 || b.Used() != 1 || hits.Load() != 4 {
		t.Fatalf("cache = %d, used = %d, hits = %d, want 3, 1 and 4", b.Cache(), b.Used(), hits.Load())
	}
}

// Uncharged marks a context, not a client: a request without it is charged as
// usual even on a client that has sent uncharged ones.
func TestUnchargedDoesNotLeak(t *testing.T) {
	t.Parallel()
	if isUncharged(context.Background()) {
		t.Fatal("a plain context reads as uncharged")
	}
	ctx, cancel := context.WithCancel(Uncharged(context.Background()))
	defer cancel()
	if !isUncharged(ctx) {
		t.Fatal("a context derived from an uncharged one lost the mark")
	}
}

func sendCtx(t *testing.T, c *http.Client, ctx context.Context, url string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}
