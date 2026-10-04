package cache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// engName is the conversation name the cache tests store.
const engName = "eng"

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	return filepath.Join(dir, "hersaid")
}

// The on-disk shape is the Slack response verbatim, so the file can be read
// with the same expression that reads the API.
func TestOnDiskShapeIsTheAPIResponse(t *testing.T) {
	dir := isolate(t)
	err := Write(ConversationsFile, Conversations{Channels: []Conversation{
		{ID: "C0123ABCD", Name: engName},
		{ID: "D0123ABCD", IsIM: true, User: "U0123ABCD"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ConversationsFile))
	if err != nil {
		t.Fatal(err)
	}
	var shaped struct {
		Channels []map[string]any `json:"channels"`
	}
	err = json.Unmarshal(b, &shaped)
	if err != nil {
		t.Fatalf("cache is not {\"channels\":[…]}: %v\n%s", err, b)
	}
	if len(shaped.Channels) != 2 {
		t.Fatalf("channels = %v", shaped.Channels)
	}
	if shaped.Channels[0]["id"] != "C0123ABCD" || shaped.Channels[0]["name"] != engName {
		t.Fatalf("first channel = %v, want the id/name keys jq reads", shaped.Channels[0])
	}
	if shaped.Channels[1]["is_im"] != true || shaped.Channels[1]["user"] != "U0123ABCD" {
		t.Fatalf("DM entry = %v, want is_im/user", shaped.Channels[1])
	}
}

// An empty list is [], never null. `jq '.channels[]'` errors on null, and a
// fetch that legitimately returns nothing must not poison the cache for every
// reader after it.
func TestEmptyListsAreWrittenAsAnEmptyArray(t *testing.T) {
	dir := isolate(t)
	err := Write(ConversationsFile, Conversations{})
	if err != nil {
		t.Fatal(err)
	}
	err = Write(UsersFile, Users{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, want string }{
		{ConversationsFile, `{"channels":[]}`},
		{UsersFile, `{"members":[]}`},
	} {
		b, err := os.ReadFile(filepath.Join(dir, tc.name))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, b, tc.want)
		}
	}
}

// A container, or a daemon with a scrubbed environment, has neither
// XDG_CACHE_HOME nor HOME. Writing to a relative path there would put the cache
// in whatever directory the command happened to run from, where the next
// invocation would not find it and nobody would think to look.
func TestNoResolvableDirectoryIsAnErrorNotTheWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")

	dir, err := Dir()
	if err == nil {
		t.Fatalf("Dir() = %q with no XDG_CACHE_HOME and no HOME, want an error", dir)
	}
	err = Write(ConversationsFile, Conversations{})
	if err == nil {
		t.Fatal("Write() succeeded with nowhere to write to")
	}
	var c Conversations
	// Both halves matter: nil would mean Read invented a result for a file it
	// could not name, and ErrCold would mean "the cache is empty" where the
	// truth is "there is no cache directory" — a caller told the first fetches
	// and writes; told the second it explains what to fix.
	err = Read(ConversationsFile, &c)
	if err == nil || errors.Is(err, ErrCold) {
		t.Fatalf("Read() = %v with nowhere to read from, want an error that is not ErrCold", err)
	}
	err = Clear()
	if err == nil {
		t.Fatal("Clear() succeeded with no cache directory")
	}

	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the working directory gained %v; the cache must never land there", entries)
	}
}

// A cache file written by hand, in the shape the API returns, is read as is.
func TestACacheWrittenByHandIsRead(t *testing.T) {
	dir := isolate(t)
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	const written = `{"channels":[{"id":"C0123ABCD","name":"eng","is_channel":true,"is_archived":false}]}`
	err = os.WriteFile(filepath.Join(dir, ConversationsFile), []byte(written), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var c Conversations
	err = Read(ConversationsFile, &c)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Channels) != 1 || c.Channels[0].Name != engName {
		t.Fatalf("channels = %+v", c.Channels)
	}
}

// Written via a temp file and renamed, so a concurrent reader never observes a
// half-written cache and the file is never briefly world-readable.
func TestWriteIsAtomicAndPrivate(t *testing.T) {
	dir := isolate(t)
	err := Write(UsersFile, Users{Members: []User{{ID: "U1", Name: "stepan"}}})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, UsersFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache mode = %v, want 0600", got)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("cache dir mode = %v, want 0700", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("temp file %q survived the write", e.Name())
		}
	}
}

// Overwriting must leave a whole file behind, never a truncated one.
func TestOverwriteLeavesAWholeFile(t *testing.T) {
	isolate(t)
	big := Users{}
	for range 500 {
		big.Members = append(big.Members, User{ID: "U", RealName: strings.Repeat("n", 100)})
	}
	err := Write(UsersFile, big)
	if err != nil {
		t.Fatal(err)
	}
	err = Write(UsersFile, Users{Members: []User{{ID: "U1"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got Users
	err = Read(UsersFile, &got)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 {
		t.Fatalf("members = %d, want the second write to have replaced the first whole", len(got.Members))
	}
}

// A zero-length file is a cold cache, not a cache of nothing.
func TestMissingAndEmptyAreBothCold(t *testing.T) {
	dir := isolate(t)
	var c Conversations
	err := Read(ConversationsFile, &c)
	if !errors.Is(err, ErrCold) {
		t.Fatalf("missing file: error = %v, want ErrCold", err)
	}
	err = os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, ConversationsFile), nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = Read(ConversationsFile, &c)
	if !errors.Is(err, ErrCold) {
		t.Fatalf("empty file: error = %v, want ErrCold", err)
	}
}

// `hear refresh` removes every *.json, which is deliberately broader than the
// two files this package writes.
func TestClearRemovesEveryJSONFile(t *testing.T) {
	dir := isolate(t)
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ConversationsFile, UsersFile, "stale.json", "notes.txt"} {
		err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
	err = Clear()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "notes.txt" {
		t.Fatalf("after Clear the directory holds %v, want only notes.txt", entries)
	}
}

func TestClearOnAMissingDirectoryIsNotAnError(t *testing.T) {
	isolate(t)
	err := Clear()
	if err != nil {
		t.Fatalf("Clear() on a cold cache = %v, want nil", err)
	}
}

type fakeSource struct {
	convs []Conversation
	users []User
	calls int
}

func (f *fakeSource) ConversationsList(context.Context) ([]Conversation, error) {
	f.calls++
	return f.convs, nil
}

func (f *fakeSource) UsersList(context.Context) ([]User, error) {
	f.calls++
	return f.users, nil
}

// The write commands hold no credential that can fill the cache, so a cold one
// is an error they turn into "run hear channels first" — never a silent fetch.
func TestOfflineStoreNeverFetches(t *testing.T) {
	isolate(t)
	_, err := Offline().Conversations(context.Background())
	if !errors.Is(err, ErrCold) {
		t.Fatalf("error = %v, want ErrCold", err)
	}
	_, err = Offline().Users(context.Background())
	if !errors.Is(err, ErrCold) {
		t.Fatalf("error = %v, want ErrCold", err)
	}
}

func TestFillingStoreFetchesOnceAndWritesBack(t *testing.T) {
	isolate(t)
	src := &fakeSource{convs: []Conversation{{ID: "C0123ABCD", Name: engName}}}
	s := Filling(src)

	for range 3 {
		got, err := s.Conversations(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != engName {
			t.Fatalf("conversations = %+v", got)
		}
	}
	if src.calls != 1 {
		t.Fatalf("source called %d times, want 1 — the result must be memoised", src.calls)
	}

	// A second process must find the cache warm.
	var onDisk Conversations
	err := Read(ConversationsFile, &onDisk)
	if err != nil {
		t.Fatalf("the fetched list was not written back: %v", err)
	}
	if len(onDisk.Channels) != 1 {
		t.Fatalf("on disk = %+v", onDisk.Channels)
	}
}

// A warm cache is never refetched, which is what keeps `sez` usable with no
// read scope of its own.
func TestFillingStoreSkipsTheFetchWhenWarm(t *testing.T) {
	isolate(t)
	err := Write(UsersFile, Users{Members: []User{{ID: "U1", Name: "stepan"}}})
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{}
	got, err := Filling(src).Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if src.calls != 0 {
		t.Fatalf("source called %d times on a warm cache, want 0", src.calls)
	}
	if len(got) != 1 {
		t.Fatalf("members = %+v", got)
	}
}

func TestUserLabelPrefersDisplayName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		user User
		want string
	}{
		{User{ID: "U1", Name: "s", RealName: "Stepan S", Profile: Profile{DisplayName: "step"}}, "step"},
		{User{ID: "U1", Name: "s", RealName: "Stepan S"}, "Stepan S"},
		{User{ID: "U1", Name: "s"}, "s"},
		{User{ID: "U1"}, "U1"},
	} {
		if got := tc.user.Label(); got != tc.want {
			t.Errorf("Label() = %q, want %q", got, tc.want)
		}
	}
}
