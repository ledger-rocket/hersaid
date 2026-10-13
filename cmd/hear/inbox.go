package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cli"
	"github.com/ledger-rocket/hersaid/internal/render"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

// The inbox's bounds. Each one that is reached is reported as the reason a
// part of the answer is not complete, never applied silently.
const (
	defaultInboxWindow = 24 * time.Hour
	defaultMaxCalls    = 60
	maxMaxCalls        = 1000
	// searchCountPerPage is the most search.messages returns in one page.
	searchCountPerPage = 100
	maxSearchPages     = 10
	maxThreadMessages  = 1000
	// maxThreadPages bounds a thread read by pages as well as by messages: a
	// server that repeats one cursor and one page adds no new message, so the
	// message bound alone would never stop it.
	maxThreadPages = maxThreadMessages/pageSize + 1
	// maxInboxMessages is the ceiling `hear read --since` uses for one window,
	// applied to everything the inbox holds, a thread's parent included:
	// nothing prints until all of it is in memory.
	maxInboxMessages = maxWindow
	// maxOneWait and maxTotalWait bound how long a run waits out 429s, so a
	// rate limit reads as an incomplete answer rather than as a hang.
	maxOneWait   = 30 * time.Second
	maxTotalWait = 60 * time.Second
	// inboxDeadline bounds each of a run's two stages: loading the identity and
	// the user list, then everything else. They are bounded apart because a
	// cold user list in a large workspace is slow, and a fill cut short writes
	// nothing, so the second stage must not lose time to the first.
	inboxDeadline = 3 * time.Minute
	// afterMarginDays is how far beyond a range's ends the search dates are.
	// after: and before: are exclusive and read in the user's time zone, up to
	// 14 hours from UTC, so two days covers the range.
	afterMarginDays = 2
	// mentionLookback is how far before the window the inbox looks for threads
	// the reader was mentioned in. with:me does not match a thread like that,
	// so a new reply in it is found only by reading it.
	mentionLookback = 7 * day
	// probeShare is the part of --max-calls kept for reading those threads:
	// one in four, and at least one.
	probeShare = 4
)

// Why a part of the inbox is not complete.
const (
	reasonBudget       = "call budget"
	reasonPageLimit    = "page limit"
	reasonMessageLimit = "message limit"
	reasonRateLimited  = "rate limited"
	reasonDeadline     = "deadline"
	reasonNoThread     = "hit without a thread"
	reasonNoTimestamp  = "hit without a timestamp"
	reasonNoChannel    = "hit without a channel"
	reasonEmptyPage    = "empty page before the last"
	reasonProbeShare   = "share of the call budget"
)

// Thread read order: threads in DMs first, then threads holding a mention,
// then the rest, then threads the reader was mentioned in before the window.
const (
	classDM = iota
	classMention
	classOther
	classProbe
)

const (
	methodSearch  = "search.messages"
	methodReplies = "conversations.replies"
)

var (
	errMethodBlocked = errors.New("rate limited earlier in this run")
	errNotAUserID    = errors.New("the identity is not a Slack user id")
)

// limitError is a request refused because a stage of the run has spent the
// part of the budget it may use. Reason is what the output says about it.
type limitError struct{ reason string }

func (e limitError) Error() string { return e.reason }

// userIDShape is what a user id the inbox puts into a search looks like: U,
// or W on an Enterprise Grid, then capitals and digits.
var userIDShape = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

// clock is the time the inbox reads and the way it waits.
type clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (h *hear) inboxVerb(rest []string) int {
	if len(rest) > 0 {
		return h.usage("inbox takes no arguments")
	}
	maxCalls := h.maxCalls
	if maxCalls == 0 {
		maxCalls = defaultMaxCalls
	}
	window := h.since
	if window == 0 {
		window = defaultInboxWindow
	}
	budget := slackhttp.NewBudget(maxCalls)
	h.src.budget = budget

	setup, cancelSetup := context.WithTimeout(context.Background(), h.timing.setup)
	defer cancelSetup()
	r, err := h.newInboxRun(setup, budget, window)
	if err != nil {
		return h.fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timing.deadline)
	defer cancel()
	in, err := r.run(ctx)
	if err != nil {
		return h.fail(err)
	}
	if h.asJSON {
		return h.emit(in)
	}
	err = render.WriteInbox(h.stdout, in, render.Options{ShowTS: h.showTS})
	if err != nil {
		return h.fail(err)
	}
	return cli.ExitOK
}

// inboxRun is one inbox: the window, who "me" is, what has been found, and
// what the run has spent.
type inboxRun struct {
	c      *slackread.Client
	budget *slackhttp.Budget
	clock  clock
	names  render.Names
	me     string

	since, until   string
	sinceT, untilT time.Time

	waited  time.Duration
	blocked map[string]bool
	// limit, when above zero, is the budget count a stage may not pass, and
	// limitReason is what a request it refuses is reported as.
	limit       int
	limitReason string
	// ceiling is maxInboxMessages; a field so a test can reach it. held is
	// every message the inbox holds now, which the ceiling bounds; seen is
	// every message it has placed, held or since dropped, so a later sighting
	// of the same message is not placed twice.
	ceiling int
	held    map[string]bool
	seen    map[string]bool
	// known is every thread the first three searches named, kept or not, so
	// the mention lookback does not read again what was already found.
	known      map[string]bool
	probes     map[string]probe
	probeOrder []string
	convs      map[string]*inboxConv
	sources    []render.InboxSource
}

// newInboxRun establishes what every later step needs: the client, the
// window, the reader's identity and the user list. A failure here is fatal,
// since no search can be written or read without them.
func (h *hear) newInboxRun(ctx context.Context, budget *slackhttp.Budget, window time.Duration) (*inboxRun, error) {
	c, err := h.src.within(ctx)
	if err != nil {
		return nil, err
	}
	until := h.timing.clock.Now().Truncate(time.Second)
	// A window reaching before 1970 would give a negative timestamp, which
	// no Slack message has and nothing here can order.
	since := maxTime(until.Add(-window).Truncate(time.Second), time.Unix(0, 0))
	r := &inboxRun{
		c: c, budget: budget, clock: h.timing.clock,
		since: slackTime(since), until: slackTime(until), sinceT: since, untilT: until,
		blocked: map[string]bool{}, ceiling: maxInboxMessages,
		held: map[string]bool{}, seen: map[string]bool{}, known: map[string]bool{},
		probes: map[string]probe{}, convs: map[string]*inboxConv{},
	}
	r.me, err = h.inboxIdentity(ctx, c)
	if err != nil {
		return nil, err
	}
	// The id goes into a search query, so it is checked for shape first: a
	// cache file is not a place a query should be able to come from.
	if !userIDShape.MatchString(r.me) {
		return nil, fmt.Errorf("%w: %q", errNotAUserID, r.me)
	}
	users, err := h.store.Users(slackhttp.Uncharged(ctx))
	if err != nil {
		return nil, err
	}
	r.names = render.NamesOf(users)
	return r, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// within is get, bounded by ctx. Resolving a credential can block on a
// keyring with nobody to answer it; the inbox's setup deadline covers that
// too, rather than starting only once it returns. A resolution that outlives
// the deadline is left to finish on its own: the command exits without it.
func (s *source) within(ctx context.Context) (*slackread.Client, error) {
	done := make(chan struct{})
	go func() {
		s.resolve()
		close(done)
	}()
	select {
	case <-done:
		return s.client, s.err
	case <-ctx.Done():
		return nil, fmt.Errorf("resolving the read credential: %w", ctx.Err())
	}
}

// inboxIdentity is the reader's user id: the cached identity when it belongs to
// the credential in use, else auth.test, written through as whoami does.
func (h *hear) inboxIdentity(ctx context.Context, c *slackread.Client) (string, error) {
	var id cache.Identity
	err := cache.Read(cache.IdentityFile, &id)
	if err == nil && id.UserID != "" && id.CredentialKey == h.src.credentialKey() {
		return id.UserID, nil
	}
	resp, err := c.AuthTest(ctx)
	if err != nil {
		return "", err
	}
	h.rememberIdentity(resp)
	return resp.UserID, nil
}

// afterDate is the search modifier that covers everything after start.
func afterDate(start time.Time) string {
	return "after:" + start.UTC().AddDate(0, 0, -afterMarginDays).Format(time.DateOnly)
}

// beforeDate is the search modifier that covers everything before end.
func beforeDate(end time.Time) string {
	return "before:" + end.UTC().AddDate(0, 0, afterMarginDays).Format(time.DateOnly)
}

// run finds, reads and assembles the inbox. The error is fatal: the credential
// was refused, so nothing more can be asked and nothing found can be trusted
// to be the whole of it.
func (r *inboxRun) run(ctx context.Context) (render.Inbox, error) {
	err := r.discoverAll(ctx)
	if err != nil {
		return render.Inbox{}, err
	}
	r.prune()
	// The mention lookback's share is held back from the other reads when it
	// has anything to read, so a busy run cannot spend it first.
	share := max(1, r.budget.Max()/probeShare)
	if n := min(share, len(r.probeOrder)); n > 0 {
		r.setLimit(r.budget.Max()-n, reasonBudget)
	}
	for _, t := range r.targets() {
		err := r.read(ctx, t)
		if err != nil {
			return render.Inbox{}, err
		}
		// What a read showed is not news is let go at once, so it holds no
		// room the reads after it could use.
		r.prune()
	}
	r.setLimit(r.budget.Used()+share, reasonProbeShare)
	err = r.readProbes(ctx)
	if err != nil {
		return render.Inbox{}, err
	}
	r.setLimit(0, "")
	r.prune()
	r.markDMs()
	return r.assemble(), nil
}

// discoverAll runs the four searches in order. The mention lookback runs last,
// so the threads the others found are known and not read twice.
func (r *inboxRun) discoverAll(ctx context.Context) error {
	after := afterDate(r.sinceT)
	// The lookback starts no earlier than 1970, for the reason the window
	// does not.
	start := maxTime(r.sinceT.Add(-mentionLookback), time.Unix(0, 0))
	window := searchRange{from: r.since, to: r.until}
	// The lookback ends where the window starts, inclusive, since the window
	// excludes its own start: what the window's searches cover is not read
	// again on the lookback's share.
	lookback := searchRange{from: slackTime(start), to: r.since, toInclusive: true}
	for _, s := range []struct {
		name, query string
		rng         searchRange
		take        hitFunc
	}{
		{render.SearchDMs, "is:dm " + after, window, r.place},
		{render.SearchMentions, "<@" + r.me + "> " + after, window, r.place},
		{render.SearchThreads, "is:thread with:me " + after, window, r.place},
		{
			render.SearchMentionThreads,
			"<@" + r.me + "> is:thread " + afterDate(start) + " " + beforeDate(r.sinceT),
			lookback, r.collectProbe,
		},
	} {
		src, err := r.discover(ctx, s.name, s.query, s.rng, s.take)
		if err != nil {
			return err
		}
		r.sources = append(r.sources, src)
	}
	return nil
}

// markDMs says, on every DM and group DM, that its own messages may not all be
// here when the search that finds them stopped early.
func (r *inboxRun) markDMs() {
	dms := r.sources[0]
	if dms.Complete {
		return
	}
	for _, c := range r.convs {
		if c.kind != render.ConversationChannel {
			c.reason = render.SearchDMs + " search: " + dms.Reason
		}
	}
}

func (r *inboxRun) setLimit(n int, reason string) {
	r.limit, r.limitReason = n, reason
}

// call makes one request through fn, waiting out a 429 once when the wait is
// short enough and the run can afford it. A method refused twice, or asked for
// a wait it cannot have, is blocked for the rest of the run.
func (r *inboxRun) call(ctx context.Context, method string, fn func() error) error {
	if r.blocked[method] {
		return errMethodBlocked
	}
	if !r.withinLimit() {
		return limitError{reason: r.limitReason}
	}
	err := ctx.Err()
	if err != nil {
		return err
	}
	err = fn()
	var rl slackread.RateLimitedError
	if !errors.As(err, &rl) {
		return err
	}
	if !r.withinLimit() || r.budget.Remaining() == 0 {
		// The run's own bounds, not Slack, rule out a retry now; a later
		// stage with a share of its own may still ask.
		return err
	}
	if !r.mayWait(rl.RetryAfter) {
		r.blocked[method] = true
		return err
	}
	serr := r.clock.Sleep(ctx, rl.RetryAfter)
	if serr != nil {
		return serr
	}
	r.waited += rl.RetryAfter
	err = fn()
	if errors.As(err, &rl) {
		r.blocked[method] = true
	}
	return err
}

func (r *inboxRun) withinLimit() bool {
	return r.limit <= 0 || r.budget.Used() < r.limit
}

func (r *inboxRun) mayWait(d time.Duration) bool {
	return d >= time.Second && d <= maxOneWait && r.waited+d <= maxTotalWait
}

// reasonOf says why a request did not succeed, in the words the output uses.
// The run's own context decides "deadline": a single request that timed out
// carries the same error, and is not the run running out of time.
func reasonOf(ctx context.Context, err error) string {
	var rl slackread.RateLimitedError
	var le limitError
	switch {
	case errors.Is(err, slackhttp.ErrBudgetSpent):
		return reasonBudget
	case errors.As(err, &le):
		return le.reason
	case errors.Is(err, errMethodBlocked), errors.As(err, &rl):
		return reasonRateLimited
	case ctx.Err() != nil:
		return reasonDeadline
	default:
		return err.Error()
	}
}

// searchRange is the part of a search's results the inbox takes: after from,
// and before to, or at to too when the end is inclusive.
type searchRange struct {
	from, to    string
	toInclusive bool
}

// before reports whether ts is at or before the start of the range, which is
// where a newest-first search can stop.
func (g searchRange) before(ts string) bool { return !render.TSLess(g.from, ts) }

// beyond reports whether ts is past the end of the range.
func (g searchRange) beyond(ts string) bool {
	if g.toInclusive {
		return render.TSLess(g.to, ts)
	}
	return !render.TSLess(ts, g.to)
}

// hitFunc takes one hit inside a search's range. It names a flaw that keeps
// the search from being complete, or reasonMessageLimit when the inbox can hold
// no more, or returns empty.
type hitFunc func(m slackread.SearchMatch, source string) string

// discover runs one search, newest first, page by page, and hands every hit
// inside rng to take. It stops at the first page that reaches back to the
// start of rng, at Slack's last page, or at a bound, which it names.
func (r *inboxRun) discover(ctx context.Context, name, query string, rng searchRange, take hitFunc) (render.InboxSource, error) {
	src := render.InboxSource{Name: name, Query: query}
	flaw := ""
	for page := 1; page <= maxSearchPages; page++ {
		var res slackread.SearchResults
		err := r.call(ctx, methodSearch, func() error {
			var err error
			res, err = r.c.SearchPage(ctx, slackread.SearchOptions{
				Query: query, Count: searchCountPerPage, Page: page, Newest: true,
			})
			return err
		})
		if err != nil {
			if slackread.IsAuthError(err) {
				return src, err
			}
			src.Reason = reasonOf(ctx, err)
			return src, nil
		}
		done, stop := takePage(&src, res, rng, take, &flaw)
		if stop != "" {
			src.Reason = stop
			return src, nil
		}
		if done {
			src.Complete = flaw == ""
			src.Reason = flaw
			return src, nil
		}
	}
	src.Reason = reasonPageLimit
	return src, nil
}

// takePage hands one page of hits to take. It reports whether the search is
// done, or the reason it has to stop early.
func takePage(src *render.InboxSource, res slackread.SearchResults, rng searchRange,
	take hitFunc, flaw *string,
) (bool, string) {
	reachedFrom := false
	for _, m := range res.Matches {
		var f string
		switch {
		case !render.ValidTS(m.TS):
			f = reasonNoTimestamp
		case !linkConversation.MatchString(m.Channel.ID):
			f = reasonNoChannel
		case rng.before(m.TS):
			reachedFrom = true
			continue
		case rng.beyond(m.TS):
			continue
		default:
			src.Hits++
			f = take(m, src.Name)
		}
		if f == reasonMessageLimit {
			return false, f
		}
		if f != "" && *flaw == "" {
			*flaw = f
		}
	}
	return pageEnds(res, reachedFrom)
}

// pageEnds reports whether a page ends the search, or why the search has to
// stop although it has not ended.
//
// Ending needs evidence: a hit at or before the start of the range, Slack's
// own last page, or an empty result. A page that is empty while Slack says
// more follow is not the end of anything, and missing paging cannot say that
// this page was the last.
func pageEnds(res slackread.SearchResults, reachedFrom bool) (bool, string) {
	p := res.Paging
	switch {
	case reachedFrom, p.Pages > 0 && p.Page >= p.Pages:
		return true, ""
	case len(res.Matches) == 0 && res.PagingKnown && p.Total == 0:
		return true, ""
	case len(res.Matches) == 0:
		return false, reasonEmptyPage
	default:
		return false, ""
	}
}

// inboxConv is one conversation with something new in it.
type inboxConv struct {
	id, name, kind string
	msgs           map[string]slackread.Message
	threads        map[string]*inboxThread
	// reason is set when the conversation's own messages may not all be here.
	reason string
}

// inboxThread is one thread with something new in it.
type inboxThread struct {
	conv   *inboxConv
	ts     string
	class  int
	parent *slackread.Message
	msgs   map[string]slackread.Message
	read   bool
	reason string
}

func conversationKind(m slackread.SearchMatch) string {
	switch {
	case m.Channel.IsIM:
		return render.ConversationDM
	case m.Channel.IsMPIM:
		return render.ConversationGroupDM
	default:
		return render.ConversationChannel
	}
}

func (r *inboxRun) conv(m slackread.SearchMatch) *inboxConv {
	c, ok := r.convs[m.Channel.ID]
	if ok {
		return c
	}
	c = &inboxConv{
		id: m.Channel.ID, kind: conversationKind(m),
		msgs: map[string]slackread.Message{}, threads: map[string]*inboxThread{},
	}
	// A 1:1 DM has no name in Slack's model; search fills the field with
	// something else, so it is not taken.
	if c.kind != render.ConversationDM {
		c.name = m.Channel.Name
	}
	r.convs[c.id] = c
	return c
}

func (c *inboxConv) thread(ts string, class int) *inboxThread {
	t, ok := c.threads[ts]
	if !ok {
		t = &inboxThread{conv: c, ts: ts, class: class, msgs: map[string]slackread.Message{}}
		c.threads[ts] = t
	}
	t.class = min(t.class, class)
	return t
}

// threadOf is the thread a hit's permalink names in the hit's own
// conversation, or empty.
func threadOf(m slackread.SearchMatch) string {
	link, isLink, err := parsePermalink(m.Permalink)
	if !isLink || err != nil || link.channelID != m.Channel.ID {
		return ""
	}
	return link.threadTS
}

// place files one hit under its conversation, and under its thread when its
// permalink names one. A thread's parent is filed as its parent whichever of
// the two arrives first, so a thread whose read is refused still lists it once.
// A threads-search hit that names no thread is kept as a top-level message,
// and named as a flaw: it cannot be read as a thread.
func (r *inboxRun) place(m slackread.SearchMatch, source string) string {
	key := m.Channel.ID + "/" + m.TS
	fresh := !r.seen[key]
	if fresh && !r.hold(key) {
		return reasonMessageLimit
	}
	r.seen[key] = true
	c := r.conv(m)
	threadTS := threadOf(m)
	if threadTS != "" {
		r.known[c.id+"/"+threadTS] = true
		t := c.thread(threadTS, threadClass(c, source))
		if p, ok := c.msgs[threadTS]; ok && t.parent == nil {
			delete(c.msgs, threadTS)
			t.add(p)
		}
		if fresh {
			t.add(m.Message)
		}
		return ""
	}
	if fresh {
		if t, ok := c.threads[m.TS]; ok {
			t.add(m.Message)
		} else {
			c.msgs[m.TS] = m.Message
		}
	}
	if source == render.SearchThreads {
		return reasonNoThread
	}
	return ""
}

// hold counts key against the ceiling, and reports false when the inbox can
// hold no more. Holding a key already held costs nothing.
func (r *inboxRun) hold(key string) bool {
	if r.held[key] {
		return true
	}
	if len(r.held) >= r.ceiling {
		return false
	}
	r.held[key] = true
	return true
}

// release gives back what a dropped thread or message held.
func (r *inboxRun) release(channelID string, tss ...string) {
	for _, ts := range tss {
		delete(r.held, channelID+"/"+ts)
	}
}

func threadClass(c *inboxConv, source string) int {
	switch {
	case c.kind != render.ConversationChannel:
		return classDM
	case source == render.SearchMentions:
		return classMention
	default:
		return classOther
	}
}

// add files m as the thread's parent or as one of its replies.
func (t *inboxThread) add(m slackread.Message) {
	if m.TS == t.ts {
		t.parent = &m
		return
	}
	t.msgs[m.TS] = m
}

// heldTS is every timestamp the thread holds, its parent included.
func (t *inboxThread) heldTS() []string {
	out := make([]string, 0, len(t.msgs)+1)
	for ts := range t.msgs {
		out = append(out, ts)
	}
	if t.parent != nil {
		out = append(out, t.parent.TS)
	}
	return out
}

// prune drops what is not news: a thread whose messages in the window are all
// mine, and a conversation with nothing left that is not mine. A parent older
// than the window is context, never news. What is dropped stops counting
// against the ceiling.
func (r *inboxRun) prune() {
	for id, c := range r.convs {
		for ts, t := range c.threads {
			if !r.threadIsNews(t) {
				r.release(c.id, t.heldTS()...)
				delete(c.threads, ts)
			}
		}
		if len(c.threads) == 0 && !r.anyFromOthers(c.msgs) {
			for ts := range c.msgs {
				r.release(c.id, ts)
			}
			delete(r.convs, id)
		}
	}
}

func (r *inboxRun) threadIsNews(t *inboxThread) bool {
	if t.parent != nil && r.inWindow(t.parent.TS) && t.parent.User != r.me {
		return true
	}
	return r.anyFromOthers(t.msgs)
}

func (r *inboxRun) anyFromOthers(msgs map[string]slackread.Message) bool {
	for _, m := range msgs {
		if m.User != r.me && r.inWindow(m.TS) {
			return true
		}
	}
	return false
}

func (r *inboxRun) inWindow(ts string) bool {
	return render.TSLess(r.since, ts) && render.TSLess(ts, r.until)
}

// targets is every thread to read, in the order the budget is spent on them:
// class, then newest activity, then conversation id, then thread ts, so the
// order never depends on map iteration.
func (r *inboxRun) targets() []*inboxThread {
	var out []*inboxThread
	for _, c := range r.convs {
		for _, t := range c.threads {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.class != b.class {
			return a.class < b.class
		}
		if na, nb := a.newest(), b.newest(); na != nb {
			return render.TSLess(nb, na)
		}
		if a.conv.id != b.conv.id {
			return a.conv.id < b.conv.id
		}
		return render.TSLess(a.ts, b.ts)
	})
	return out
}

func (t *inboxThread) newest() string {
	n := t.ts
	for ts := range t.msgs {
		if render.TSLess(n, ts) {
			n = ts
		}
	}
	return n
}

// probe is a thread the reader was mentioned in before the window, which no
// other search found: whether it has anything new is known only once it is
// read.
type probe struct {
	hit    slackread.SearchMatch
	thread string
}

// collectProbe is the mention lookback's hitFunc: it notes each thread a hit
// names that the other searches did not find, newest mention first.
func (r *inboxRun) collectProbe(m slackread.SearchMatch, _ string) string {
	ts := threadOf(m)
	if ts == "" {
		return reasonNoThread
	}
	key := m.Channel.ID + "/" + ts
	if r.known[key] {
		return ""
	}
	if _, ok := r.probes[key]; !ok {
		// Only where the thread is matters until it is read; the message
		// itself is dropped so the lookback holds nothing the ceiling does not
		// count.
		m.Message = slackread.Message{}
		m.Permalink = ""
		r.probes[key] = probe{hit: m, thread: ts}
		r.probeOrder = append(r.probeOrder, key)
	}
	return ""
}

// readProbes reads each thread the mention lookback found, within its share
// of the budget. A thread with a reply from someone else inside the window is
// kept; the rest are dropped as not news. Every thread that could not be read
// to the end is listed on the source, so a caller can read it itself.
func (r *inboxRun) readProbes(ctx context.Context) error {
	src := &r.sources[len(r.sources)-1]
	var reasons []string
	note := func(reason string) {
		if !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}
	for _, key := range r.probeOrder {
		p := r.probes[key]
		t := r.conv(p.hit).thread(p.thread, classProbe)
		err := r.read(ctx, t)
		if err != nil {
			return err
		}
		if !t.read || t.reason != "" {
			src.Unread = append(src.Unread, render.InboxUnread{
				Channel: render.ChannelRef{ID: p.hit.Channel.ID}, ThreadTS: p.thread, Reason: t.reason,
			})
			note(t.reason)
		}
		r.prune()
	}
	if len(src.Unread) == 0 {
		return nil
	}
	summary := fmt.Sprintf("%d of %d threads not read: %s", len(src.Unread), len(r.probeOrder), strings.Join(reasons, ", "))
	if src.Reason != "" {
		summary = src.Reason + "; " + summary
	}
	src.Complete, src.Reason = false, summary
	return nil
}

// read fetches a thread's parent and every reply in the window, page by page.
// A whole read replaces what search found for the thread; a read cut short is
// merged with it, keeps what it got, and says why.
func (r *inboxRun) read(ctx context.Context, t *inboxThread) error {
	cursor := ""
	returned := map[string]bool{}
	for page := 0; ; page++ {
		if page >= maxThreadPages {
			t.reason = reasonPageLimit
			return nil
		}
		var p slackread.HistoryPage
		err := r.call(ctx, methodReplies, func() error {
			var err error
			p, err = r.c.Replies(ctx, t.conv.id, t.ts, slackread.HistoryOptions{
				Limit: pageSize, Oldest: r.since, Latest: r.until, Cursor: cursor,
			})
			return err
		})
		if err != nil {
			if slackread.IsAuthError(err) {
				return err
			}
			t.reason = reasonOf(ctx, err)
			return nil
		}
		t.read = true
		for _, m := range p.Messages {
			stop := r.merge(t, m)
			if stop != "" {
				t.reason = stop
				return nil
			}
			returned[m.TS] = true
		}
		if cursor = p.NextCursor; cursor == "" {
			r.keepOnly(t, returned)
			return nil
		}
	}
}

// keepOnly drops what a whole read did not return, the parent included.
// Search found it, but a read to the end of the window is the authority on
// what the thread holds now: a hit it left out was deleted, or edited out of
// the window, since the index saw it. A read cut short proves nothing of the
// kind, so it never gets here.
func (r *inboxRun) keepOnly(t *inboxThread, returned map[string]bool) {
	for ts := range t.msgs {
		if !returned[ts] {
			r.release(t.conv.id, ts)
			delete(t.msgs, ts)
		}
	}
	if t.parent != nil && !returned[t.ts] {
		r.release(t.conv.id, t.ts)
		t.parent = nil
	}
}

// merge files one message a read returned, or names why the read has to stop.
// The read's copy wins over the search hit for the same message, and a message
// that turns out to belong to the thread leaves the conversation's top level.
// The parent is context and is kept wherever it falls; any other message
// outside the window is not.
func (r *inboxRun) merge(t *inboxThread, m slackread.Message) string {
	switch {
	case !render.ValidTS(m.TS):
		return reasonNoTimestamp
	case m.TS != t.ts && !r.inWindow(m.TS):
		return ""
	}
	key := t.conv.id + "/" + m.TS
	if !r.held[key] {
		if m.TS != t.ts && len(t.msgs) >= maxThreadMessages {
			return reasonMessageLimit
		}
		if !r.hold(key) {
			return reasonMessageLimit
		}
	}
	r.seen[key] = true
	delete(t.conv.msgs, m.TS)
	t.add(m)
	return ""
}

// assemble builds the output from what was found and read, ordered as the spec
// says: conversations newest first, everything inside them oldest first.
func (r *inboxRun) assemble() render.Inbox {
	in := render.Inbox{
		Kind:     render.KindInbox,
		Since:    r.sinceT.UTC().Format(time.RFC3339),
		Until:    r.untilT.UTC().Format(time.RFC3339),
		UserID:   r.me,
		Complete: true,
		Calls: render.InboxCalls{
			Used: r.budget.Used(), Max: r.budget.Max(), Cache: r.budget.Cache(),
		},
		Sources:       r.sources,
		Conversations: []render.InboxConversation{},
	}
	for _, s := range r.sources {
		in.Complete = in.Complete && s.Complete
	}
	type ranked struct {
		conv   render.InboxConversation
		newest string
	}
	all := make([]ranked, 0, len(r.convs))
	for _, c := range r.convs {
		rc := r.conversation(c)
		in.Complete = in.Complete && rc.Complete
		all = append(all, ranked{conv: rc, newest: newestOf(rc)})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].newest != all[j].newest {
			return render.TSLess(all[j].newest, all[i].newest)
		}
		return all[i].conv.Channel.ID < all[j].conv.Channel.ID
	})
	for _, a := range all {
		in.Conversations = append(in.Conversations, a.conv)
	}
	return in
}

func (r *inboxRun) conversation(c *inboxConv) render.InboxConversation {
	out := render.InboxConversation{
		Channel:  render.ChannelRef{ID: c.id, Name: c.name},
		Kind:     c.kind,
		Source:   render.SourceSearch,
		Complete: c.reason == "",
		Reason:   c.reason,
		Messages: r.messages(c.msgs),
		Threads:  []render.InboxThread{},
	}
	tss := make([]string, 0, len(c.threads))
	for ts := range c.threads {
		tss = append(tss, ts)
	}
	sort.Slice(tss, func(i, j int) bool { return render.TSLess(tss[i], tss[j]) })
	for _, ts := range tss {
		t := r.thread(c.threads[ts])
		out.Complete = out.Complete && t.Complete
		out.Threads = append(out.Threads, t)
	}
	if c.kind == render.ConversationDM {
		out.User = r.otherPerson(c)
		out.Name = r.names[out.User]
	}
	return out
}

func (r *inboxRun) thread(t *inboxThread) render.InboxThread {
	out := render.InboxThread{
		ThreadTS: t.ts,
		Replies:  r.messages(t.msgs),
		Source:   render.SourceSearch,
		Complete: t.read && t.reason == "",
		Reason:   t.reason,
	}
	if t.read {
		out.Source = render.SourceReplies
	}
	if t.parent != nil {
		p := r.messages(map[string]slackread.Message{t.parent.TS: *t.parent})
		out.Parent = &p[0]
	}
	return out
}

// messages is msgs oldest first, in the transcript's structured form, each
// marked when it names the reader.
func (r *inboxRun) messages(msgs map[string]slackread.Message) []render.InboxMessage {
	list := make([]slackread.Message, 0, len(msgs))
	for _, m := range msgs {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return render.TSLess(list[i].TS, list[j].TS) })
	out := make([]render.InboxMessage, 0, len(list))
	for i, m := range render.Messages(list, r.names) {
		out = append(out, render.InboxMessage{Message: m, MentionsMe: mentionsMe(list[i], r.me)})
	}
	return out
}

// otherPerson is the other side of a 1:1 DM: the author of any message in it
// that is not the reader. A DM where only an app wrote has none.
func (r *inboxRun) otherPerson(c *inboxConv) string {
	var users []string
	collect := func(m slackread.Message) {
		if m.User != "" && m.User != r.me {
			users = append(users, m.User)
		}
	}
	for _, m := range c.msgs {
		collect(m)
	}
	for _, t := range c.threads {
		if t.parent != nil {
			collect(*t.parent)
		}
		for _, m := range t.msgs {
			collect(m)
		}
	}
	if len(users) == 0 {
		return ""
	}
	sort.Strings(users)
	return users[0]
}

func newestOf(c render.InboxConversation) string {
	n := ""
	consider := func(ts string) {
		if n == "" || render.TSLess(n, ts) {
			n = ts
		}
	}
	for _, m := range c.Messages {
		consider(m.TS)
	}
	for _, t := range c.Threads {
		for _, m := range t.Replies {
			consider(m.TS)
		}
		if t.Parent != nil {
			consider(t.Parent.TS)
		}
	}
	return n
}

// mentionsMe reports whether m names the reader exactly: in its text, in an
// attachment's text fields, or as a user element of a block. Blocks and
// attachments are decoded and walked rather than searched as raw text, so an id
// that merely occurs in other JSON does not count.
func mentionsMe(m slackread.Message, me string) bool {
	if namesUser(m.Text, me) || attachmentsNameUser(m.Attachments, me) {
		return true
	}
	var blocks any
	return json.Unmarshal(m.Blocks, &blocks) == nil && blockNamesUser(blocks, me)
}

// attachmentsNameUser reports whether an attachment's text fields name the
// user. Bot messages carry their mentions here rather than in the text.
func attachmentsNameUser(raw json.RawMessage, me string) bool {
	var attachments []struct {
		Text     string `json:"text"`
		Pretext  string `json:"pretext"`
		Fallback string `json:"fallback"`
		Title    string `json:"title"`
		Fields   []struct {
			Value string `json:"value"`
		} `json:"fields"`
	}
	if json.Unmarshal(raw, &attachments) != nil {
		return false
	}
	for _, a := range attachments {
		if namesUser(a.Text, me) || namesUser(a.Pretext, me) || namesUser(a.Fallback, me) || namesUser(a.Title, me) {
			return true
		}
		for _, f := range a.Fields {
			if namesUser(f.Value, me) {
				return true
			}
		}
	}
	return false
}

func namesUser(s, id string) bool {
	return strings.Contains(s, "<@"+id+">") || strings.Contains(s, "<@"+id+"|")
}

func blockNamesUser(v any, id string) bool {
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "user" && x["user_id"] == id {
			return true
		}
		for _, child := range x {
			if blockNamesUser(child, id) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if blockNamesUser(child, id) {
				return true
			}
		}
	}
	return false
}
