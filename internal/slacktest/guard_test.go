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
			if name := info.Name(); name == ".git" || name == "bin" {
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

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
