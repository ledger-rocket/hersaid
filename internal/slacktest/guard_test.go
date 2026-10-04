package slacktest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// Never send a real Slack message or reaction. The structural guarantee is that
// every client takes its base URL as a constructor parameter, but nothing stops
// a future test passing the production one — so no test file may name the real
// API at all. Only the endpoint packages, which define the constant, may.
func TestNoTestNamesTheRealSlackAPI(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	allowed := map[string]bool{
		filepath.Join("internal", "slackread", "slackread.go"):   true,
		filepath.Join("internal", "slackwrite", "slackwrite.go"): true,
		filepath.Join("internal", "slacktest", "guard_test.go"):  true,
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if allowed[rel] {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if slacktest.NamesRealEndpoint(b, strings.HasSuffix(rel, "_test.go")) {
			t.Errorf("%s reaches production Slack; writes must reach a slacktest server only", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// skipDir names a directory that is not this module's source.
//
// Go itself ignores a directory whose name begins with "." or "_", so nothing
// under one is built or tested as part of this module — and the guard walks the
// filesystem rather than the package list, so it has to say the same. The case
// that made this matter is a nested checkout: the harness puts agent worktrees
// under .claude, and walking one means reading another copy of this repo,
// where the endpoint constants live exactly where they are supposed to.
func skipDir(name string) bool {
	if name == "bin" {
		return true
	}
	return len(name) > 1 && (name[0] == '.' || name[0] == '_')
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// The guard walks the filesystem, so it has to know what is not this module.
func TestTheGuardSkipsWhatIsNotThisModulesSource(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".git", ".claude", ".rb", ".github", "_ignored", "bin"} {
		if !skipDir(name) {
			t.Errorf("skipDir(%q) = false, want it skipped", name)
		}
	}
	// filepath.Walk yields real entries, never "." or ".." — so the names that
	// have to be walked are the ones a directory can actually be called.
	for _, name := range []string{"internal", "cmd", "docs", "slacktest", "testdata"} {
		if skipDir(name) {
			t.Errorf("skipDir(%q) = true, want it walked", name)
		}
	}
}
