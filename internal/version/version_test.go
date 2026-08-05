package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

// The string is what a bug report quotes, so a part nobody recorded is left out
// rather than printed as "unknown" — which reads like a value someone set.
func TestStringOmitsWhatNobodyRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		info Info
		want string
	}{
		{
			"a release build",
			Info{Name: "hear", Version: "v0.1.0", Commit: "abc1234def5678901234567890abcdef12345678", Date: "2026-08-03T09:41:00Z"},
			"hear v0.1.0 (abc1234, 2026-08-03)",
		},
		{
			"no date",
			Info{Name: "sez", Version: "v0.1.0", Commit: "abc1234def5678901234567890abcdef12345678"},
			"sez v0.1.0 (abc1234)",
		},
		{
			"no commit",
			Info{Name: "tap", Version: "v0.1.0", Date: "2026-08-03T09:41:00Z"},
			"tap v0.1.0 (2026-08-03)",
		},
		{
			"nothing but a development build",
			Info{Name: "hear", Version: devel},
			"hear (devel)",
		},
		{
			"built from a dirty tree",
			Info{Name: "sez", Version: "v0.1.0", Commit: "abc1234def5678901234567890abcdef12345678", Date: "2026-08-03T09:41:00Z", Modified: true},
			"sez v0.1.0 (abc1234-dirty, 2026-08-03)",
		},
		{
			"a short or non-hex revision is passed through",
			Info{Name: "tap", Version: "v0.1.0", Commit: "release-42"},
			"tap v0.1.0 (release-42)",
		},
		{
			// Truncating this to "abcdef0" would turn a label someone chose into
			// something indistinguishable from a revision.
			"a label that merely starts like a revision is not abbreviated",
			Info{Name: "tap", Version: "v0.1.0", Commit: "abcdef0-release"},
			"tap v0.1.0 (abcdef0-release)",
		},
		{
			// git log --format=%cI records an offset; a commit made just after
			// midnight must not be reported as the day before.
			"a commit date in its own offset",
			Info{Name: "hear", Version: "v0.1.0", Date: "2026-08-03T00:30:00+02:00"},
			"hear v0.1.0 (2026-08-03)",
		},
		{
			"an unparseable date is passed through rather than dropped",
			Info{Name: "hear", Version: "v0.1.0", Date: "release-day"},
			"hear v0.1.0 (release-day)",
		},
	} {
		if got := tc.info.String(); got != tc.want {
			t.Errorf("%s: String() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The link-time values are what a release carries, so they win over the stamp
// the toolchain recorded for the same build.
func TestLinkTimeValuesWin(t *testing.T) {
	setBuild(t, "v1.2.3", "0123456789abcdef0123456789abcdef01234567", "2026-07-01T12:00:00Z")

	if got, want := String("hear"), "hear v1.2.3 (0123456, 2026-07-01)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// goreleaser's version template ends in -dirty for a tagged build made from a
// dirty tree; that belongs on the commit, not glued to the version.
func TestDirtyVersionBecomesTheCommitMarker(t *testing.T) {
	setBuild(t, "v1.2.3-dirty", "0123456789abcdef0123456789abcdef01234567", "2026-07-01T12:00:00Z")

	got := Current("sez")
	if !got.Modified {
		t.Fatal("a -dirty version did not mark the build as modified")
	}
	if want := "sez v1.2.3 (0123456-dirty, 2026-07-01)"; got.String() != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// A build nobody versioned still has to say something true. Whatever the
// toolchain stamped, the answer names the command and never invents a value.
func TestUnstampedBuildStillNamesItself(t *testing.T) {
	setBuild(t, "", "", "")

	got := Current("hear")
	if got.Version == "" {
		t.Fatal("Version is empty; it must fall back to the module version or (devel)")
	}
	s := got.String()
	if !strings.HasPrefix(s, "hear ") {
		t.Fatalf("String() = %q, want it to start with the command name", s)
	}
	if strings.Contains(s, "unknown") {
		t.Fatalf("String() = %q; an unrecorded field is omitted, not called unknown", s)
	}
}

// Where each field comes from when the linker set some of them and the
// toolchain stamped others.
func TestProvenance(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	stamped := func(settings ...[2]string) *debug.BuildInfo {
		b := &debug.BuildInfo{}
		for _, s := range settings {
			b.Settings = append(b.Settings, debug.BuildSetting{Key: s[0], Value: s[1]})
		}
		return b
	}
	withMain := func(v string, b *debug.BuildInfo) *debug.BuildInfo {
		b.Main.Version = v
		return b
	}

	for _, tc := range []struct {
		name  string
		link  Info
		build *debug.BuildInfo
		want  string
	}{
		{
			"a release: the link-time values are the whole answer",
			Info{Version: "v1.2.3", Commit: sha, Date: "2026-07-01T12:00:00Z"},
			stamped([2]string{"vcs.revision", "ffffffffffffffffffffffffffffffffffffffff"},
				[2]string{"vcs.modified", "true"}),
			"hear v1.2.3 (0123456, 2026-07-01)",
		},
		{
			// A -X'd commit is a claim about a tree this build never saw, so
			// the local tree's dirtiness is not attributed to it.
			"a linker-supplied commit is not labelled with the local tree's state",
			Info{Commit: sha},
			stamped([2]string{"vcs.modified", "true"}, [2]string{"vcs.time", "2026-07-01T12:00:00Z"}),
			"hear (devel) (0123456, 2026-07-01)",
		},
		{
			"a plain go build: everything comes from the stamp",
			Info{},
			withMain(devel, stamped([2]string{"vcs.revision", sha},
				[2]string{"vcs.time", "2026-07-01T12:00:00Z"},
				[2]string{"vcs.modified", "true"})),
			"hear (devel) (0123456-dirty, 2026-07-01)",
		},
		{
			// go install …@abc123 builds from the module cache, which is not a
			// repository: there is no stamp, and the pseudo-version is the only
			// provenance there is.
			"a module-cache build keeps its pseudo-version",
			Info{},
			withMain("v0.0.0-20260701120000-0123456789ab", stamped()),
			"hear v0.0.0-20260701120000-0123456789ab",
		},
		{
			"a stamped build drops the pseudo-version that restates it",
			Info{},
			withMain("v0.0.0-20260701120000-0123456789ab",
				stamped([2]string{"vcs.revision", sha}, [2]string{"vcs.time", "2026-07-01T12:00:00Z"})),
			"hear (devel) (0123456, 2026-07-01)",
		},
		{
			"a real installed version survives",
			Info{},
			withMain("v1.4.0", stamped()),
			"hear v1.4.0",
		},
		{
			"the toolchain's +dirty marker moves to the commit",
			Info{},
			withMain("v1.4.0+dirty", stamped([2]string{"vcs.revision", sha})),
			"hear v1.4.0 (0123456-dirty)",
		},
		{
			"no build info at all",
			Info{},
			nil,
			"hear (devel)",
		},
	} {
		if got := current("hear", tc.link, tc.build).String(); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// The toolchain's pseudo-version spells out the same commit and date that are
// reported next to it, so it is not repeated as a version.
func TestPseudoVersionIsNotAVersion(t *testing.T) {
	for _, v := range []string{
		"v0.0.0-20260803093803-b6010131d76c",
		"v0.0.0-20260803093803-b6010131d76c+dirty",
		"v1.2.4-0.20260803093803-b6010131d76c",
	} {
		if !pseudoVersion.MatchString(v) {
			t.Errorf("%q is a pseudo-version and was not recognised as one", v)
		}
	}
	// A version someone chose is kept, which is what `go install …@v1.2.3` records.
	for _, v := range []string{"v1.2.3", "v0.1.0-rc.1", devel} {
		if pseudoVersion.MatchString(v) {
			t.Errorf("%q was mistaken for a pseudo-version", v)
		}
	}
}

// setBuild stands in for the -X flags, and restores whatever this binary was
// actually built with.
func setBuild(t *testing.T, v, c, d string) {
	t.Helper()
	pv, pc, pd := version, commit, date
	version, commit, date = v, c, d
	t.Cleanup(func() { version, commit, date = pv, pc, pd })
}
