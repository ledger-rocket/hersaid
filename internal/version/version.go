// Package version reports which build of a command is running.
//
// A release sets the values at link time. A build that does not — `go build`,
// `go install`, a colleague's checkout — falls back to the VCS stamp the Go
// toolchain records by itself, so a locally built binary still names the commit
// it came from rather than "unknown". That fallback is the point: the version
// string is what a bug report quotes, and it has to be right in the case where
// nobody was thinking about versions.
package version

import (
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

// Set at link time. The exact flags, which goreleaser and the CI build pass:
//
//	pkg=github.com/ledger-rocket/hersaid/internal/version
//	go build -ldflags "\
//	  -X $pkg.version=$(git describe --tags --dirty 2>/dev/null || true) \
//	  -X $pkg.commit=$(git rev-parse HEAD) \
//	  -X $pkg.date=$(git log -1 --format=%cI)" ./cmd/hear
//
// version is deliberately left empty when no tag describes the commit: this
// package then reports (devel) beside the revision, which says more than a
// version that is the same commit printed twice.
//
// They are unexported: only this package decides what a build says about
// itself, and nothing at runtime can change it.
var (
	version string
	commit  string
	date    string
)

// devel is what the toolchain calls a build with no module version.
const devel = "(devel)"

// pseudoVersion matches the synthetic module version the toolchain derives from
// a commit, e.g. v0.0.0-20260803093803-b6010131d76c or the +dirty form of it.
var pseudoVersion = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]{12}(\+dirty)?$`)

// Info is what a build knows about itself.
type Info struct {
	// Name is the command, not the module: three binaries share this package.
	Name string
	// Version is a semver tag, or "(devel)" for a build made from a checkout.
	Version string
	// Commit is the full revision; String abbreviates it.
	Commit string
	// Date is the commit date in RFC 3339; String reduces it to a day.
	Date string
	// Modified reports a build made from a dirty tree.
	Modified bool
}

// Current reports the running build of name, preferring the link-time values
// and filling any gap from the toolchain's own stamp.
func Current(name string) Info {
	build, _ := debug.ReadBuildInfo() // nil outside a Go build, which is fine
	return current(name, Info{Version: version, Commit: commit, Date: date}, build)
}

// current is Current with the build info injected, so that every combination of
// "what the linker set" and "what the toolchain stamped" can be exercised.
func current(name string, link Info, build *debug.BuildInfo) Info {
	i := link
	i.Name = name
	// The dirty marker belongs on the commit, not glued to the version:
	// goreleaser spells it -dirty and the toolchain +dirty.
	i.Version, i.Modified = splitDirty(i.Version)

	if build == nil {
		return i.withDefaults()
	}

	var stamp Info
	for _, s := range build.Settings {
		switch s.Key {
		case "vcs.revision":
			stamp.Commit = s.Value
		case "vcs.time":
			stamp.Date = s.Value
		case "vcs.modified":
			stamp.Modified = s.Value == "true"
		}
	}

	// Field by field, so a partial set of -X flags does not discard the rest.
	// The stamp's dirty marker is taken only alongside its revision: it
	// describes the tree that commit was built from, and saying it of a
	// different, linker-supplied commit would be a claim about a tree nobody saw.
	if i.Commit == "" {
		i.Commit = stamp.Commit
		i.Modified = i.Modified || stamp.Modified
	}
	if i.Date == "" {
		i.Date = stamp.Date
	}
	if i.Version == "" {
		v, dirty := splitDirty(build.Main.Version)
		// A pseudo-version restates the commit and date printed beside it — but
		// only where those were recovered. A build from the module cache
		// (`go install …@abc123`) carries no VCS stamp at all, and there the
		// pseudo-version is the only provenance there is.
		if !pseudoVersion.MatchString(v) || i.Commit == "" {
			i.Version = v
			i.Modified = i.Modified || dirty
		}
	}
	return i.withDefaults()
}

// splitDirty removes the marker either producer appends for a modified tree.
func splitDirty(v string) (string, bool) {
	for _, marker := range []string{"-dirty", "+dirty"} {
		if before, ok := strings.CutSuffix(v, marker); ok {
			return before, true
		}
	}
	return v, false
}

// String is the one line --version prints: "hear v0.1.0 (abc1234, 2026-08-03)".
// A part nobody recorded is left out rather than printed as "unknown", which
// reads like a value.
func (i Info) String() string {
	s := i.Name + " " + i.Version

	var parts []string
	if c := shortCommit(i.Commit); c != "" {
		if i.Modified {
			c += "-dirty"
		}
		parts = append(parts, c)
	} else if i.Modified {
		parts = append(parts, "dirty")
	}
	if d := day(i.Date); d != "" {
		parts = append(parts, d)
	}
	if len(parts) == 0 {
		return s
	}
	return s + " (" + strings.Join(parts, ", ") + ")"
}

func (i Info) withDefaults() Info {
	if i.Version == "" {
		i.Version = devel
	}
	return i
}

// String reports the running build of name.
func String(name string) string { return Current(name).String() }

// shortCommit abbreviates a full revision to the customary seven characters.
// Anything that is not a plain hex revision is passed through untouched — all
// of it, since truncating "abcdef0-release" to "abcdef0" would turn a label
// someone chose into something that looks like a revision.
func shortCommit(c string) string {
	if len(c) <= 7 {
		return c
	}
	for _, r := range c {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return c
		}
	}
	return c[:7]
}

// day reduces a timestamp to the date. A build date is used to answer "roughly
// how old is this binary", which the time of day does not help with.
func day(d string) string {
	if d == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02"} {
		t, err := time.Parse(layout, d)
		if err == nil {
			// In the offset it was recorded in, not UTC: `git log` shows the
			// former, and a commit made just after midnight should not appear to
			// be from the day before.
			return t.Format("2006-01-02")
		}
	}
	// An unparseable stamp is still better than none: it came from a build flag
	// someone set on purpose.
	return d
}
