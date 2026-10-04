package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"

	"github.com/zalando/go-keyring"
)

// privateDirPerm and privateFilePerm keep what hersaid writes readable by its
// owner only.
const (
	privateDirPerm  = 0o700
	privateFilePerm = 0o600
)

var errKeychainWouldNotTake = errors.New("the keychain would not take the token, and it already holds")

// configDir is the directory under XDG_CONFIG_HOME that holds the file
// credential.
const configDir = "hersaid"

// keyringSet and keyringGet are variables so that a test can present a keyring
// that reads but refuses to write. go-keyring's own mock fails both at once,
// and it is precisely the split case — an item still readable while a write is
// declined — in which a file fallback would be written and never read.
var (
	keyringSet = keyring.Set
	keyringGet = keyring.Get
)

// store puts the token where cred will look for it: the keyring when the host
// has one, else a 0600 file.
//
// where says which, in words meant to be read out — a location is not a secret,
// and it is what somebody needs in order to revoke the credential later, or to
// explain why a command is still using a token they thought they had replaced.
// shadowed names credential material that cred ranks below what was just
// stored, and so will never read again.
func store(r role, token string) (where string, shadowed []string, err error) {
	acct := keyringAccount()

	// Any keyring error means the file, not only "not found": on a headless
	// host go-keyring returns the raw D-Bus or exec failure unwrapped, and cred
	// falls through on the same basis when it reads the credential back.
	err = keyringSet(r.service, acct, token)
	if err == nil {
		return fmt.Sprintf("keychain (service: %s)", r.service), outranked(r, true), nil
	}

	// The file is a fallback for a host with no keyring, not for a keyring that
	// declined one write. cred reads every keyring tier before any file, so if
	// an item is still readable the file would never be seen — and reporting a
	// stored credential that nothing resolves is worse than reporting nothing.
	_, err = keyringGet(r.service, acct)
	if err == nil {
		return "", nil, fmt.Errorf(
			"%w a %s credential under service %s that would be read in preference to a file; remove that item and try again",
			errKeychainWouldNotTake, r.name, r.service)
	}

	path, err := tokenPath(configDir, r)
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Dir(path)
	err = os.MkdirAll(dir, privateDirPerm)
	if err != nil {
		return "", nil, err
	}
	// MkdirAll applies its mode only to directories it creates, so the chmod is
	// unconditional: one somebody made group-readable years ago would otherwise
	// stay that way.
	err = os.Chmod(dir, privateDirPerm)
	if err != nil {
		return "", nil, err
	}
	err = writeTokenFile(path, token)
	if err != nil {
		return "", nil, err
	}
	return path + " (mode 0600)", outranked(r, false), nil
}

// writeTokenFile writes the token atomically: a temp file in the same directory,
// chmod 0600, then rename. Writing in place would put the token into whatever
// mode the previous file had — os.WriteFile applies its own only when it
// creates the file — and would leave a truncated token behind if the process
// died mid-write. It is the idiom cache.Write already uses in this module.
func writeTokenFile(path, token string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has succeeded
	err = tmp.Chmod(privateFilePerm)
	if err != nil {
		tmp.Close()
		return err
	}
	_, err = tmp.WriteString(token)
	if err != nil {
		tmp.Close()
		return err
	}
	err = tmp.Close()
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// outranked lists credential material for r that cred now ranks below what was
// just stored: the resolution order is env, keyring, file, so enrolling into
// the keyring buries a token file that nothing will read again and nobody has
// revoked. Naming it is the point.
func outranked(r role, inKeyring bool) []string {
	// A file enrolment outranks nothing: it is the last tier cred consults.
	if !inKeyring {
		return nil
	}
	var out []string
	for _, dir := range []string{configDir} {
		path, err := tokenPath(dir, r)
		if err != nil {
			continue
		}
		_, err = os.Stat(path)
		if err == nil {
			out = append(out, path)
		}
	}
	return out
}

// stored describes a credential already sitting where this enrolment would
// write, so that replacing one is a decision somebody made rather than a side
// effect of re-running the installer.
type stored struct {
	where     string
	described string
}

// lookup reports what is already stored for a role.
//
// It looks only where store writes, not at $SLACK_READ_TOKEN. The question here
// is narrow: is this about to overwrite something? An environment variable is
// not overwritten by anything done here, so naming it in a prompt headed
// "Replace it?" would misdescribe what the answer does. It is reported after
// the fact, by reportWhatOutranksIt, which is what it actually is.
func lookup(r role) (stored, bool) {
	v, err := keyringGet(r.service, keyringAccount())
	if err == nil {
		if t := strip(v); t != "" {
			return stored{
				where:     fmt.Sprintf("the keychain (service: %s)", r.service),
				described: describe(t),
			}, true
		}
	}
	path, err := tokenPath(configDir, r)
	if err != nil {
		return stored{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return stored{}, false
	}
	if t := strip(string(b)); t != "" {
		return stored{where: path, described: describe(t)}, true
	}
	return stored{}, false
}

// tokenPrefix is the shape of a Slack token: xoxp-, xoxb-, xoxe- and so on.
var tokenPrefix = regexp.MustCompile(`^xox[a-z]-`)

// describe identifies a credential that is already stored, and says less than
// fingerprint does. There is no paste to check for truncation here, so the
// leading bytes buy nothing beyond the token type — which is public — and the
// length is enough to answer the only question being asked: is the new token a
// different one.
func describe(token string) string {
	if p := tokenPrefix.FindString(token); p != "" {
		return fmt.Sprintf("%s… (%d chars)", p, len([]rune(token)))
	}
	return fmt.Sprintf("(%d chars)", len([]rune(token)))
}

// tokenPath restates cred's file location. See the note on role: the two have
// to agree exactly, and a round-trip test is what holds them together.
func tokenPath(dir string, r role) (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("no home directory to store a token in: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, dir, r.fileName), nil
}

// keyringAccount is the keyring account key. $USER first, because that is what
// cred looks under, and the two have to agree.
func keyringAccount() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	u, err := user.Current()
	if err == nil {
		return u.Username
	}
	return ""
}
