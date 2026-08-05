// Package resolve turns a human target — "#eng", "@stepan", "C0123ABCD" — into
// the channel value an endpoint accepts.
//
// Resolution is capability-aware because the endpoints genuinely differ:
// chat.postMessage accepts a bare #name and opens a DM when handed a user id,
// while reactions.add needs a real conversation id. The difference is a
// declared property of the caller rather than something each caller rediscovers.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ledger-rocket/hersaid/internal/cache"
)

// Failure modes, typed so a command can word its own message without matching
// on strings.
var (
	// ErrNoSuchChannel is a #name the cache knows nothing about.
	ErrNoSuchChannel = errors.New("no such channel")
	// ErrUnknownChannel is a bare name that resolved to nothing and cannot be
	// passed through, because only a #name may be.
	ErrUnknownChannel = errors.New("unknown channel")
	// ErrNoSuchUser is an @handle matching nobody.
	ErrNoSuchUser = errors.New("no user matching")
	// ErrAmbiguousUser is an @prefix matching more than one person. It is an
	// error, never a guess: picking one would message the wrong person.
	ErrAmbiguousUser = errors.New("matches several people")
	// ErrNoOpenDM is a person with no existing 1:1 conversation, for a caller
	// whose endpoint cannot open one.
	ErrNoOpenDM = errors.New("no open DM")
)

// Slack ids are an uppercase-prefixed alphanumeric run. C/D/G are channel-like;
// U is a user, which only an endpoint that opens DMs may be given.
var (
	conversationID = regexp.MustCompile(`^[CDG][A-Z0-9]{7,}$`)
	userID         = regexp.MustCompile(`^[CDGU][A-Z0-9]{7,}$`)
)

// Capability declares what the calling endpoint can do with a target it cannot
// look up. Both fields are false for the strictest caller.
type Capability struct {
	// OpensDM means the endpoint accepts a U… id and opens the conversation
	// itself, so a person with no existing DM is still addressable.
	// chat.postMessage does; reactions.add does not.
	OpensDM bool
	// PassThroughName means the endpoint resolves a #name server-side, so a
	// cold or incomplete cache is not fatal. chat.postMessage does; reactions.add
	// does not.
	PassThroughName bool
}

// The three commands' capabilities.
var (
	// ForRead is `hear`: names and ids only, and it holds the credential that
	// can fill a cold cache.
	ForRead = Capability{}
	// ForPost is `sez`: chat.postMessage accepts a #name and opens DMs.
	ForPost = Capability{OpensDM: true, PassThroughName: true}
	// ForReact is `tap`: reactions.add needs a resolved conversation id.
	ForReact = Capability{}
	// ForEdit is `sez --edit`/`--delete`: chat.update and chat.delete address a
	// message by conversation id, so a #name passed through unresolved reaches
	// Slack as a channel name and is refused — after the render has already
	// succeeded, which is the confusion the read gate exists to remove.
	ForEdit = Capability{}
)

// Target is a resolved address.
type Target struct {
	// Input is the argument exactly as given, for messages and the audit log.
	Input string
	// ID is what goes on the wire as the channel field. It is a Slack id
	// unless Resolved is false, in which case it is the "#name" as given.
	ID string
	// Name is the channel name without its "#", when known.
	Name string
	// UserID is set when resolution went through a person.
	UserID string
	// Resolved is false only for a #name passed through unresolved.
	Resolved bool
}

// Catalog supplies the lists resolution needs. It is consulted lazily, so a raw
// id resolves without touching a cache the caller may not be able to fill.
// *cache.Store satisfies it.
type Catalog interface {
	Conversations(ctx context.Context) ([]cache.Conversation, error)
	Users(ctx context.Context) ([]cache.User, error)
}

// Resolve addresses target under can.
func Resolve(ctx context.Context, cat Catalog, can Capability, target string) (Target, error) {
	if strings.HasPrefix(target, "@") {
		return resolvePerson(ctx, cat, can, target)
	}
	return resolveChannel(ctx, cat, can, target)
}

func resolveChannel(ctx context.Context, cat Catalog, can Capability, target string) (Target, error) {
	bare := strings.TrimPrefix(target, "#")
	t := Target{Input: target, Name: bare}

	// A raw id needs no catalog at all. U… is only an id for an endpoint that
	// opens DMs; for the others it falls through to a name lookup and fails.
	if conversationID.MatchString(bare) || (can.OpensDM && userID.MatchString(bare)) {
		t.ID, t.Name, t.Resolved = bare, "", true
		if strings.HasPrefix(bare, "U") {
			t.UserID = bare
		}
		return t, nil
	}

	convs, err := cat.Conversations(ctx)
	if err != nil {
		// A cold cache is only fatal for a caller that cannot pass a name on.
		if !(errors.Is(err, cache.ErrCold) && can.PassThroughName) {
			return t, err
		}
	} else {
		// Case-sensitive, first duplicate wins. Slack
		// channel names are lowercase in practice, so folding case here would
		// only widen what a typo can hit.
		for _, c := range convs {
			if c.Name == bare {
				t.ID, t.Resolved = c.ID, true
				return t, nil
			}
		}
	}

	if !can.PassThroughName {
		return t, fmt.Errorf("%w: %s", ErrNoSuchChannel, bare)
	}
	// Only an explicit #name may be passed through: a bare word that matched
	// nothing is far more likely to be a typo than a channel.
	if !strings.HasPrefix(target, "#") {
		return t, fmt.Errorf("%w: %s", ErrUnknownChannel, target)
	}
	t.ID, t.Resolved = target, false
	return t, nil
}

func resolvePerson(ctx context.Context, cat Catalog, can Capability, target string) (Target, error) {
	handle := strings.TrimPrefix(target, "@")
	t := Target{Input: target}

	users, err := cat.Users(ctx)
	if err != nil {
		return t, err
	}
	uid, err := matchUser(users, handle)
	if err != nil {
		return t, err
	}
	t.UserID = uid

	convs, err := cat.Conversations(ctx)
	if err != nil {
		return t, err
	}
	for _, c := range convs {
		if c.IsIM && c.User == uid {
			t.ID, t.Resolved = c.ID, true
			return t, nil
		}
	}

	// No conversation exists yet. chat.postMessage opens one when given the
	// user id; reactions.add cannot, and reading needs a real D… channel.
	if !can.OpensDM {
		return t, fmt.Errorf("%w with %s (open one in Slack first)", ErrNoOpenDM, target)
	}
	t.ID, t.Resolved = uid, true
	return t, nil
}

// matchUser resolves a person: an exact handle wins over any
// prefix, a prefix is accepted only when unique, and deleted accounts are never
// matched. The comparison is over the handle, the display name and the real
// name, all case-folded.
func matchUser(users []cache.User, query string) (string, error) {
	q := strings.ToLower(query)
	var exact, prefix []string
	for _, u := range users {
		if u.Deleted {
			continue
		}
		for _, k := range []string{u.Name, u.Profile.DisplayName, u.RealName} {
			k = strings.ToLower(k)
			if k == q {
				exact = append(exact, u.ID)
				break
			}
		}
		for _, k := range []string{u.Name, u.Profile.DisplayName, u.RealName} {
			if strings.HasPrefix(strings.ToLower(k), q) {
				prefix = append(prefix, u.ID)
				break
			}
		}
	}
	candidates := exact
	if len(candidates) == 0 {
		candidates = prefix
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("%w @%s", ErrNoSuchUser, query)
	default:
		return "", fmt.Errorf("@%s %w — use the exact handle", query, ErrAmbiguousUser)
	}
}
