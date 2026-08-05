package allowlist

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/resolve"
)

func resolved(id, name string) resolve.Target {
	return resolve.Target{Input: "#" + name, ID: id, Name: name, Resolved: true}
}

// unsetEnv removes a variable for the duration of the test. t.Setenv is called
// first purely for its cleanup, which restores the developer's real value — a
// bare Unsetenv would clobber their HERSAID_WRITE_CHANNELS for every test after
// this one in the same process.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// Unset means unrestricted. The allowlist is opt-in: defaulting to closed would
// lock out everyone who never asked for it, which is how a safety feature gets
// removed rather than adopted.
func TestUnsetPermitsEverything(t *testing.T) {
	unsetEnv(t, EnvVar)
	l := FromEnv()
	if l.Active() {
		t.Fatal("an unset allowlist is active")
	}
	if !l.Permits(resolved("C0123ABCD", "anything")) {
		t.Fatal("an unset allowlist refused a target")
	}
}

// An empty or whitespace-only value is the same as unset — it is what an
// `export HERSAID_WRITE_CHANNELS=` leaves behind.
func TestEmptyValueIsInactive(t *testing.T) {
	for _, v := range []string{"", "  ", ",", " , ,"} {
		if l := Parse(v); l.Active() {
			t.Errorf("Parse(%q) is active", v)
		}
	}
}

func TestPermitsByID(t *testing.T) {
	l := Parse("C0123ABCD,D0456EFGH")
	if !l.Active() {
		t.Fatal("list is not active")
	}
	if !l.Permits(resolve.Target{Input: "C0123ABCD", ID: "C0123ABCD", Resolved: true}) {
		t.Fatal("an allowlisted id was refused")
	}
	if l.Permits(resolve.Target{Input: "C9999ZZZZ", ID: "C9999ZZZZ", Resolved: true}) {
		t.Fatal("an id outside the list was permitted")
	}
}

// Both spellings of a name are accepted in the variable, because both are what
// a user types on the command line.
func TestPermitsByName(t *testing.T) {
	for _, spec := range []string{"#eng-standup", "eng-standup"} {
		l := Parse(spec)
		if !l.Permits(resolved("C0123ABCD", "eng-standup")) {
			t.Errorf("Parse(%q) refused #eng-standup", spec)
		}
	}
}

// A resolved #name is permitted by its name even though the wire value is an
// id: they are the same conversation, which is why the check runs after
// resolution rather than on the raw argument.
func TestNameEntryPermitsAResolvedID(t *testing.T) {
	l := Parse("#eng")
	if !l.Permits(resolved("C0123ABCD", "eng")) {
		t.Fatal("a name entry did not permit the id it resolves to")
	}
}

// sez may pass a #name through unresolved; the allowlist must still bind.
func TestNameEntryPermitsAPassThroughTarget(t *testing.T) {
	l := Parse("#eng")
	passthrough := resolve.Target{Input: "#eng", ID: "#eng", Name: "eng", Resolved: false}
	if !l.Permits(passthrough) {
		t.Fatal("an unresolved #name was refused by its own name")
	}
}

// Exact match only. #eng must not open #eng-standup, #engineering or #eng2.
func TestNameMatchingIsExact(t *testing.T) {
	l := Parse("#eng")
	for _, name := range []string{"eng-standup", "engineering", "eng2", "en", "ENG"} {
		if l.Permits(resolved("C0123ABCD", name)) {
			t.Errorf("#eng permitted #%s", name)
		}
	}
}

// Fail closed: naming a channel in the list and then passing its raw id is
// refused, because this package cannot know the two are the same conversation.
func TestRawIDIsNotPermittedByANameEntry(t *testing.T) {
	l := Parse("#eng")
	rawID := resolve.Target{Input: "C0123ABCD", ID: "C0123ABCD", Resolved: true}
	if l.Permits(rawID) {
		t.Fatal("a name entry permitted a bare id; the fail-closed direction is refusal")
	}
}

// A person allowlisted by user id stays allowed once a DM exists and resolution
// starts yielding the D… id instead.
func TestUserIDEntryPermitsTheResolvedDM(t *testing.T) {
	l := Parse("U0000STEP")
	dm := resolve.Target{Input: "@stepan", ID: "D0000STEP", UserID: "U0000STEP", Resolved: true}
	if !l.Permits(dm) {
		t.Fatal("a user-id entry did not permit that person's DM")
	}
}

func TestMixedEntriesAndWhitespace(t *testing.T) {
	l := Parse(" D0123ABCD , #eng-standup ,, plain ")
	for _, tc := range []struct {
		target resolve.Target
		want   bool
	}{
		{resolve.Target{Input: "@x", ID: "D0123ABCD", Resolved: true}, true},
		{resolved("C1", "eng-standup"), true},
		{resolved("C2", "plain"), true},
		{resolved("C3", "other"), false},
	} {
		if got := l.Permits(tc.target); got != tc.want {
			t.Errorf("Permits(%+v) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

func TestCheckWrapsErrRefused(t *testing.T) {
	l := Parse("#eng")
	if err := l.Check(resolved("C1", "eng")); err != nil {
		t.Fatalf("Check on a permitted target = %v, want nil", err)
	}
	err := l.Check(resolved("C2", "secrets"))
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("error = %v, want it to wrap ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "#secrets") {
		t.Fatalf("error = %q, want it to name the refused target", err)
	}
}

func TestFromEnvReadsTheVariable(t *testing.T) {
	t.Setenv(EnvVar, "#eng")
	l := FromEnv()
	if !l.Active() || !l.Permits(resolved("C1", "eng")) {
		t.Fatalf("FromEnv() = %+v", l)
	}
	if l.Permits(resolved("C2", "other")) {
		t.Fatal("FromEnv() permitted a channel outside the variable")
	}
}
