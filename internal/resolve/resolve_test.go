package resolve

import (
	"context"
	"errors"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/cache"
)

// catalog is a fixed pair of lists. cold makes both lists report ErrCold, which
// is what a write command sees before anyone has run `hear channels`.
type catalog struct {
	convs []cache.Conversation
	users []cache.User
	cold  bool
}

func (c catalog) Conversations(context.Context) ([]cache.Conversation, error) {
	if c.cold {
		return nil, cache.ErrCold
	}
	return c.convs, nil
}

func (c catalog) Users(context.Context) ([]cache.User, error) {
	if c.cold {
		return nil, cache.ErrCold
	}
	return c.users, nil
}

func fixture() catalog {
	return catalog{
		convs: []cache.Conversation{
			{ID: "C0000ENG1", Name: "eng"},
			{ID: "C0000ENG2", Name: "eng"}, // duplicate name; the first must win
			{ID: "C0000ENGX", Name: "eng-standup"},
			{ID: "C0000UPPER", Name: "ENG"},
			{ID: "D0000STEP", IsIM: true, User: "U0000STEP"},
			{ID: "D0000DELE", IsIM: true, User: "U0000DELE"},
		},
		users: []cache.User{
			{
				ID: "U0000STEP", Name: "stepan", RealName: "Stepan Marks",
				Profile: cache.Profile{DisplayName: "step"},
			},
			{ID: "U0000STEV", Name: "steve", RealName: "Steve Smith"},
			{ID: "U0000DELE", Name: "gone", RealName: "Gone Away", Deleted: true},
			{ID: "U0000NODM", Name: "nodm", RealName: "No Dm"},
		},
	}
}

func mustResolve(t *testing.T, cat Catalog, can Capability, target string) Target {
	t.Helper()
	got, err := Resolve(context.Background(), cat, can, target)
	if err != nil {
		t.Fatalf("Resolve(%q) error = %v", target, err)
	}
	return got
}

func TestChannelNameResolvesFromTheCache(t *testing.T) {
	got := mustResolve(t, fixture(), ForRead, "#eng")
	if got.ID != "C0000ENG1" {
		t.Fatalf("ID = %q, want the first duplicate C0000ENG1", got.ID)
	}
	if got.Name != "eng" || !got.Resolved || got.Input != "#eng" {
		t.Fatalf("target = %+v", got)
	}
}

// A leading '#' is optional, so both spellings resolve.
func TestBareNameResolvesLikeAHashName(t *testing.T) {
	if got := mustResolve(t, fixture(), ForRead, "eng"); got.ID != "C0000ENG1" {
		t.Fatalf("ID = %q, want C0000ENG1", got.ID)
	}
}

// Case-sensitive: folding case would only widen what a typo can hit.
func TestChannelNameMatchingIsCaseSensitive(t *testing.T) {
	if got := mustResolve(t, fixture(), ForRead, "#ENG"); got.ID != "C0000UPPER" {
		t.Fatalf("#ENG resolved to %q, want the literally-named C0000UPPER", got.ID)
	}
}

// A prefix of a real channel is not a channel.
func TestChannelNameIsNotPrefixMatched(t *testing.T) {
	_, err := Resolve(context.Background(), fixture(), ForRead, "#eng-stand")
	if !errors.Is(err, ErrNoSuchChannel) {
		t.Fatalf("error = %v, want ErrNoSuchChannel", err)
	}
}

// A raw id resolves without consulting the catalog at all, so it works with a
// cold cache.
func TestRawConversationIDBypassesTheCatalog(t *testing.T) {
	for _, id := range []string{"C0123ABCD", "D0123ABCD", "G0123ABCD"} {
		got := mustResolve(t, catalog{cold: true}, ForReact, id)
		if got.ID != id || !got.Resolved {
			t.Fatalf("Resolve(%q) = %+v", id, got)
		}
	}
}

// The capability difference, declared rather than incidental: chat.postMessage
// opens a DM when handed a user id, reactions.add cannot.
func TestUserIDIsOnlyATargetForAnEndpointThatOpensDMs(t *testing.T) {
	got := mustResolve(t, catalog{cold: true}, ForPost, "U0123ABCD")
	if got.ID != "U0123ABCD" || got.UserID != "U0123ABCD" {
		t.Fatalf("sez: %+v, want the user id accepted as a target", got)
	}

	// For tap it is not an id, so it falls through to a name lookup and fails.
	_, err := Resolve(context.Background(), fixture(), ForReact, "U0123ABCD")
	if err == nil {
		t.Fatal("tap accepted a raw user id; reactions.add cannot open a DM")
	}
}

func TestPersonResolvesToTheOpenDM(t *testing.T) {
	got := mustResolve(t, fixture(), ForReact, "@stepan")
	if got.ID != "D0000STEP" || got.UserID != "U0000STEP" {
		t.Fatalf("target = %+v", got)
	}
}

// Handle, display name and real name are all matched, case-insensitively.
func TestPersonMatchesEveryNameField(t *testing.T) {
	for _, q := range []string{"@stepan", "@STEPAN", "@step", "@Stepan Marks"} {
		if got := mustResolve(t, fixture(), ForReact, q); got.UserID != "U0000STEP" {
			t.Fatalf("Resolve(%q) = %q, want U0000STEP", q, got.UserID)
		}
	}
}

// An exact handle wins over any number of prefix matches, so a person whose
// handle is a prefix of someone else's is still addressable.
func TestExactHandleBeatsPrefix(t *testing.T) {
	cat := fixture()
	cat.users = append(cat.users, cache.User{ID: "U0000STEPX", Name: "stepanx"})
	cat.convs = append(cat.convs, cache.Conversation{ID: "D0000STEX", IsIM: true, User: "U0000STEPX"})

	if got := mustResolve(t, cat, ForReact, "@stepan"); got.UserID != "U0000STEP" {
		t.Fatalf("UserID = %q, want the exact handle U0000STEP", got.UserID)
	}
}

// Ambiguity is an error, never a guess: picking one would message the wrong
// person, and the mistake is only visible in Slack afterwards.
func TestAmbiguousPrefixIsAnError(t *testing.T) {
	_, err := Resolve(context.Background(), fixture(), ForReact, "@ste")
	if !errors.Is(err, ErrAmbiguousUser) {
		t.Fatalf("error = %v, want ErrAmbiguousUser", err)
	}
}

// A deactivated account keeps its name and its DM channel. Matching it would
// address a conversation nobody reads.
func TestDeletedUsersAreExcluded(t *testing.T) {
	_, err := Resolve(context.Background(), fixture(), ForReact, "@gone")
	if !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("error = %v, want ErrNoSuchUser", err)
	}
}

func TestUnknownPersonIsAnError(t *testing.T) {
	_, err := Resolve(context.Background(), fixture(), ForPost, "@nobody")
	if !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("error = %v, want ErrNoSuchUser", err)
	}
}

// With no conversation open, sez may address the person by user id and let
// chat.postMessage create it; tap cannot.
func TestPersonWithNoOpenDM(t *testing.T) {
	got := mustResolve(t, fixture(), ForPost, "@nodm")
	if got.ID != "U0000NODM" {
		t.Fatalf("sez: ID = %q, want the user id", got.ID)
	}

	_, err := Resolve(context.Background(), fixture(), ForReact, "@nodm")
	if !errors.Is(err, ErrNoOpenDM) {
		t.Fatalf("tap: error = %v, want ErrNoOpenDM", err)
	}
}

// sez has no read scope of its own, so a cold cache must not stop it posting to
// a channel it can name — chat.postMessage resolves a #name server-side.
func TestColdCachePassesAHashNameThrough(t *testing.T) {
	got := mustResolve(t, catalog{cold: true}, ForPost, "#eng")
	if got.ID != "#eng" || got.Resolved {
		t.Fatalf("target = %+v, want the #name passed through unresolved", got)
	}
	if got.Name != "eng" {
		t.Fatalf("Name = %q, want eng so the allowlist can still match it", got.Name)
	}
}

// A #name the warm cache does not know is passed through too: the cache can be
// stale, and only Slack is authoritative.
func TestUnknownHashNamePassesThroughOnAWarmCache(t *testing.T) {
	got := mustResolve(t, fixture(), ForPost, "#brand-new")
	if got.ID != "#brand-new" || got.Resolved {
		t.Fatalf("target = %+v", got)
	}
}

// A bare word that matched nothing is far more likely to be a typo than a
// channel, so only an explicit #name may be passed through.
func TestBareNameIsNotPassedThrough(t *testing.T) {
	_, err := Resolve(context.Background(), catalog{cold: true}, ForPost, "eng")
	if !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("error = %v, want ErrUnknownChannel", err)
	}
}

// tap needs a resolved id, so a cold cache is fatal and the command turns this
// into "run hear channels first".
func TestColdCacheIsFatalForReactions(t *testing.T) {
	_, err := Resolve(context.Background(), catalog{cold: true}, ForReact, "#eng")
	if !errors.Is(err, cache.ErrCold) {
		t.Fatalf("error = %v, want cache.ErrCold", err)
	}
}

// A person cannot be resolved without the user list, whatever the capability.
func TestColdCacheIsFatalForAPerson(t *testing.T) {
	_, err := Resolve(context.Background(), catalog{cold: true}, ForPost, "@stepan")
	if !errors.Is(err, cache.ErrCold) {
		t.Fatalf("error = %v, want cache.ErrCold", err)
	}
}

// A display name is attacker-controlled — an external Slack Connect guest sets
// their own — so a name that looks like a pattern or a format verb must be
// matched as literal data.
func TestHostileDisplayNamesAreMatchedAsData(t *testing.T) {
	cat := catalog{
		users: []cache.User{
			{ID: "U0000EVIL", Name: "evil", Profile: cache.Profile{DisplayName: `s/x/y/ ${x} $1 %s .*`}},
		},
		convs: []cache.Conversation{{ID: "D0000EVIL", IsIM: true, User: "U0000EVIL"}},
	}
	got := mustResolve(t, cat, ForReact, `@s/x/y/ ${x} $1 %s .*`)
	if got.ID != "D0000EVIL" {
		t.Fatalf("target = %+v", got)
	}
	// The metacharacters must not have matched anything else.
	_, err := Resolve(context.Background(), cat, ForReact, "@anything")
	if !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("error = %v; the display name was treated as a pattern", err)
	}
}

// ForUpload is files.completeUploadExternal's capability: channel_id is a
// conversation id, so neither a #name Slack would resolve nor a U… whose DM it
// would open is any use. It is a name of its own rather than a reuse of
// ForReact because it is a different endpoint's reason for the same strictness.
func TestForUploadResolvesNothingItCannotName(t *testing.T) {
	cat := fixture()
	for _, c := range []struct {
		target string
		want   error
	}{
		{"#nosuch", ErrNoSuchChannel},
		{"@nodm", ErrNoOpenDM},
		{"U0000NODM", ErrNoOpenDM},
	} {
		_, err := Resolve(context.Background(), cat, ForUpload, c.target)
		if !errors.Is(err, c.want) {
			t.Errorf("Resolve(%q) error = %v, want %v", c.target, err, c.want)
		}
	}
}

// A #name must never be passed through unresolved: completion would receive a
// channel name where an id belongs, after every byte had already been staged.
func TestForUploadNeverPassesANameThrough(t *testing.T) {
	got, err := Resolve(context.Background(), fixture(), ForUpload, "#eng")
	if err != nil {
		t.Fatalf("Resolve(#eng) error = %v", err)
	}
	if !got.Resolved || got.ID != "C0000ENG1" {
		t.Fatalf("Resolve(#eng) = %+v, want the resolved id", got)
	}
}

// A bare U… is one of the three forms the usage advertises. For a caller that
// does not open DMs it used to fall past the id check into channel-name
// matching and report "no such channel", even with that person's DM in the
// cache. It resolves through the IM instead.
func TestABareUserIDResolvesThroughTheOpenDM(t *testing.T) {
	for _, can := range []struct {
		name string
		cap  Capability
	}{
		{"ForUpload", ForUpload},
		{"ForReact", ForReact},
		{"ForEdit", ForEdit},
	} {
		got, err := Resolve(context.Background(), fixture(), can.cap, "U0000STEP")
		if err != nil {
			t.Fatalf("%s: Resolve(U0000STEP) error = %v", can.name, err)
		}
		if got.ID != "D0000STEP" {
			t.Errorf("%s: ID = %q, want the DM id D0000STEP", can.name, got.ID)
		}
		if !got.Resolved {
			t.Errorf("%s: Resolved = false, want true", can.name)
		}
		if got.UserID != "U0000STEP" {
			t.Errorf("%s: UserID = %q, want the person it went through", can.name, got.UserID)
		}
	}
}

// The capability that opens DMs still hands the user id straight to Slack, which
// opens the conversation itself. Resolving it to a D… here would be harmless but
// it is not this change's business, and chat.postMessage has always taken U….
func TestForPostStillPassesABareUserIDStraightThrough(t *testing.T) {
	got, err := Resolve(context.Background(), fixture(), ForPost, "U0000NODM")
	if err != nil {
		t.Fatalf("Resolve(U0000NODM) error = %v", err)
	}
	if got.ID != "U0000NODM" {
		t.Fatalf("ID = %q, want the user id passed through", got.ID)
	}
}

// A channel whose name merely begins with U is not a user id. The id form is
// ^[CDGU][A-Z0-9]{7,}$ and nothing looser: a prefix test would send #UPDATES
// hunting for a DM with a person who does not exist, and refuse a channel that
// resolves perfectly well. Uppercase channel names exist — the shared fixture
// carries one — so this is not hypothetical.
func TestAChannelNamedLikeAUserIDStillResolves(t *testing.T) {
	cat := catalog{
		convs: []cache.Conversation{
			{ID: "C0000UPD1", Name: "UPDATES"},
			{ID: "D0000STEP", IsIM: true, User: "U0000STEP"},
		},
		users: []cache.User{{ID: "U0000STEP", Name: "stepan"}},
	}
	for _, can := range []struct {
		name string
		cap  Capability
	}{
		{"ForUpload", ForUpload},
		{"ForReact", ForReact},
		{"ForEdit", ForEdit},
		{"ForRead", ForRead},
	} {
		for _, target := range []string{"#UPDATES", "UPDATES"} {
			got, err := Resolve(context.Background(), cat, can.cap, target)
			if err != nil {
				t.Errorf("%s: Resolve(%q) error = %v, want the channel", can.name, target, err)
				continue
			}
			if got.ID != "C0000UPD1" {
				t.Errorf("%s: Resolve(%q) ID = %q, want C0000UPD1", can.name, target, got.ID)
			}
		}

		// The name the catalog does not have. This is the case that tells a
		// prefix test from the id form: the channel lookup misses either way,
		// and only a prefix test then goes looking for a DM and reports that
		// none is open — for something that was never a person.
		_, err := Resolve(context.Background(), cat, can.cap, "#UNKNOWN")
		if !errors.Is(err, ErrNoSuchChannel) {
			t.Errorf("%s: Resolve(#UNKNOWN) error = %v, want %v", can.name, err, ErrNoSuchChannel)
		}
	}
}
