// Package cache owns the conversation and user lists on disk.
//
// The on-disk shape is the Slack API response verbatim — {"channels":[…]} and
// {"members":[…]} — so the file can be read with the same expression that reads
// the API. The Slack endpoints therefore return these types directly rather
// than a parallel set.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// File names under the cache directory.
const (
	ConversationsFile = "chans.json"
	UsersFile         = "users.json"
	IdentityFile      = "identity.json"
)

// Identity is who the read token belongs to, from auth.test. It is never
// fetched on demand — a read's call count is a tested property — so the
// commands that already call auth.test write it through and reads only look at
// what is already on disk.
type Identity struct {
	UserID string `json:"user_id"`
	User   string `json:"user"`
	Team   string `json:"team"`
	// CredentialKey is cred.Read.Key for the credential this came from. A
	// SLACK_READ_TOKEN override or a re-enrolment switches accounts without
	// clearing the cache, and a reader that trusted the file regardless would
	// mark another account's reaction as its own — worse than no marker.
	CredentialKey string `json:"credential_key,omitempty"`
}

// ErrCold reports that the cache holds nothing for the requested list. Only the
// read command has a credential that can fill it, so the write commands turn
// this into "run hear channels first" rather than fetching.
var ErrCold = errors.New("cache is empty")

// Conversation is one entry of conversations.list, narrowed to the fields the
// commands use. A 1:1 DM carries no Name — only User, the other party's id.
type Conversation struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	IsIM   bool   `json:"is_im,omitempty"`
	User   string `json:"user,omitempty"`
	IsMPIM bool   `json:"is_mpim,omitempty"`
}

// Conversations is the file body of chans.json.
type Conversations struct {
	Channels []Conversation `json:"channels"`
}

// MarshalJSON writes an empty list as [], never null. `jq '.channels[]'` errors
// on null, and a workspace whose visible list is momentarily empty must not
// poison a cache the next command reads.
func (c Conversations) MarshalJSON() ([]byte, error) {
	if c.Channels == nil {
		c.Channels = []Conversation{}
	}
	type wire Conversations
	return json.Marshal(wire(c))
}

// Profile is the part of a user profile the commands read.
type Profile struct {
	DisplayName string `json:"display_name"`
}

// User is one entry of users.list, narrowed to the fields the commands use.
type User struct {
	ID       string  `json:"id"`
	Name     string  `json:"name,omitempty"`
	RealName string  `json:"real_name,omitempty"`
	Deleted  bool    `json:"deleted,omitempty"`
	Profile  Profile `json:"profile"`
}

// Label is how a user is shown and matched: the display name when set, else the
// real name, else the handle, else the id.
func (u User) Label() string {
	switch {
	case u.Profile.DisplayName != "":
		return u.Profile.DisplayName
	case u.RealName != "":
		return u.RealName
	case u.Name != "":
		return u.Name
	default:
		return u.ID
	}
}

// Users is the file body of users.json.
type Users struct {
	Members []User `json:"members"`
}

// MarshalJSON writes an empty list as [], never null, for the same reason
// Conversations does.
func (u Users) MarshalJSON() ([]byte, error) {
	if u.Members == nil {
		u.Members = []User{}
	}
	type wire Users
	return json.Marshal(wire(u))
}

// Dir is the cache directory, $XDG_CACHE_HOME/hersaid or ~/.cache/hersaid.
//
// It fails when neither is resolvable — a container, or a daemon with a
// scrubbed environment, has no home directory. Returning a relative path there
// would put the cache in whatever directory the command happened to run from,
// where the next invocation would not find it and nobody would think to look.
func Dir() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cache: no cache directory: %w", err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "hersaid"), nil
}

// Path names a file inside the cache directory.
func Path(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Write serialises v to name atomically: a temp file in the same directory,
// chmod 0600, then rename. A reader never observes a half-written cache, and
// the file is never briefly world-readable.
func Write(name string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cache %s: %w", name, err)
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cache %s: %w", name, err)
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return fmt.Errorf("cache %s: %w", name, err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename has succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("cache %s: %w", name, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("cache %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cache %s: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("cache %s: %w", name, err)
	}
	return nil
}

// Read decodes name into v. A missing or empty file is ErrCold: a zero-length
// file is a cold cache, not a cache of nothing.
func Read(name string, v any) error {
	path, err := Path(name)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrCold, name)
		}
		return fmt.Errorf("cache %s: %w", name, err)
	}
	if len(b) == 0 {
		return fmt.Errorf("%w: %s", ErrCold, name)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("cache %s: %w", name, err)
	}
	return nil
}

// Clear removes every *.json in the cache directory, deliberately broader than
// the files this package writes: anything left there is cache too.
func Clear() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Source fetches the lists from Slack when the cache is cold.
// *slackread.Client satisfies it; the write commands have no such client.
type Source interface {
	ConversationsList(ctx context.Context) ([]Conversation, error)
	UsersList(ctx context.Context) ([]User, error)
}

// Store reads the two lists, optionally filling a cold cache from src.
//
// Both accessors are lazy and memoised: resolving a raw channel id touches
// neither list, so a write command never forces a load it cannot satisfy.
type Store struct {
	src   Source
	convs *Conversations
	users *Users
}

// Offline reads only what is already on disk. A cold cache is ErrCold.
func Offline() *Store { return &Store{} }

// Filling reads the cache, fetching from src and writing the result back when a
// list is cold.
func Filling(src Source) *Store { return &Store{src: src} }

// Conversations returns the conversation list.
func (s *Store) Conversations(ctx context.Context) ([]Conversation, error) {
	if s.convs != nil {
		return s.convs.Channels, nil
	}
	var c Conversations
	err := Read(ConversationsFile, &c)
	if errors.Is(err, ErrCold) && s.src != nil {
		list, ferr := s.src.ConversationsList(ctx)
		if ferr != nil {
			return nil, ferr
		}
		c = Conversations{Channels: list}
		if werr := Write(ConversationsFile, c); werr != nil {
			return nil, werr
		}
		err = nil
	}
	if err != nil {
		return nil, err
	}
	s.convs = &c
	return c.Channels, nil
}

// Users returns the member list.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	if s.users != nil {
		return s.users.Members, nil
	}
	var u Users
	err := Read(UsersFile, &u)
	if errors.Is(err, ErrCold) && s.src != nil {
		list, ferr := s.src.UsersList(ctx)
		if ferr != nil {
			return nil, ferr
		}
		u = Users{Members: list}
		if werr := Write(UsersFile, u); werr != nil {
			return nil, werr
		}
		err = nil
	}
	if err != nil {
		return nil, err
	}
	s.users = &u
	return u.Members, nil
}
