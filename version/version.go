// Package version holds gosmo's own version metadata. Commit/Date are
// filled in automatically at build time — no ldflags or Makefile required
// — but init reads them from a different place depending on how gosmo
// ends up in the running binary:
//
//   - Built as the main module (gosmo's own tests, examples, or a
//     standalone gosmo binary): the Go toolchain's VCS stamp (go help
//     buildvcs) already describes gosmo's own repo, so init reads
//     Commit/Date straight from debug.BuildInfo.Settings.
//   - Embedded as a dependency of another program (gossms, in practice):
//     that same VCS stamp describes the *host* program instead, not
//     gosmo — Settings is one global stamp for the whole binary, not
//     per-module. init instead finds gosmo's own entry in
//     debug.BuildInfo.Deps and, if its Version is a Go pseudo-version
//     (vX.Y.Z-yyyymmddhhmmss-abcdefabcdef), decodes the commit and time
//     embedded in the version string itself — that part Go's module
//     system does track per-dependency, regardless of who is the main
//     module. A plain semver tag ("v0.0.3") or a local filesystem
//     replace ("(devel)") carries no such info, so Commit/Date stay
//     "unknown" in that case; there's genuinely nothing to decode.
//
// Version/Commit/Date remain vars, not consts, so a packaging build can
// still override them at link time via:
//
//	-ldflags "-X github.com/radix29/gosmo/version.Version=... \
//	          -X github.com/radix29/gosmo/version.Commit=...  \
//	          -X github.com/radix29/gosmo/version.Date=..."
package version

import (
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const Name = "gosmo"

// modulePath is gosmo's own import path — the only way to pick gosmo's
// entry out of debug.BuildInfo.Deps, which has no other "is this me" flag.
const modulePath = "github.com/radix29/gosmo"

var (
	Version = "v0.0.3"
	Commit  = "unknown"
	Date    = "unknown"
)

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if info.Main.Path == modulePath {
		fillFromVCS(info.Settings)
		return
	}
	fillFromDep(info.Deps)
}

// fillFromVCS reads Commit/Date from the toolchain's global VCS stamp —
// only accurate when gosmo itself is the main module being built.
func fillFromVCS(settings []debug.BuildSetting) {
	var revision string
	var dirty bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			if Date == "unknown" {
				Date = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision != "" && Commit == "unknown" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if dirty {
			revision += "-dirty"
		}
		Commit = revision
	}
}

// fillFromDep reads Commit/Date out of gosmo's own module version string,
// as recorded by the Go module system for the binary that embeds it —
// correct regardless of who the main module is, unlike the whole-binary
// VCS stamp fillFromVCS reads.
func fillFromDep(deps []*debug.Module) {
	for _, m := range deps {
		if m.Path != modulePath {
			continue
		}
		for m.Replace != nil {
			m = m.Replace
		}
		if commit, date, ok := parsePseudoVersion(m.Version); ok {
			if Commit == "unknown" {
				Commit = commit
			}
			if Date == "unknown" {
				Date = date
			}
		}
		return
	}
}

// parsePseudoVersion extracts the commit hash and commit time embedded in
// a Go pseudo-version — vX.0.0-yyyymmddhhmmss-abcdefabcdef when there's no
// earlier tagged version, vX.Y.Z-0.yyyymmddhhmmss-abcdefabcdef when there
// is (the "0." infix distinguishes a pseudo-version from that tag), either
// with an optional "+incompatible" suffix. This is the form go.mod uses
// for a dependency pinned to a commit rather than a tag. Returns ok=false
// for a plain semver tag ("v0.0.3") or a local filesystem replace
// ("(devel)"), neither of which carries commit info in the string itself.
func parsePseudoVersion(v string) (commit, date string, ok bool) {
	v = strings.TrimSuffix(v, "+incompatible")
	parts := strings.Split(v, "-")
	if len(parts) < 2 {
		return "", "", false
	}
	hash := parts[len(parts)-1]
	ts := parts[len(parts)-2]
	if i := strings.LastIndex(ts, "."); i >= 0 {
		ts = ts[i+1:]
	}
	if len(hash) != 12 || len(ts) != 14 {
		return "", "", false
	}
	if _, err := strconv.ParseUint(ts, 10, 64); err != nil {
		return "", "", false
	}
	t, err := time.Parse("20060102150405", ts)
	if err != nil {
		return "", "", false
	}
	return hash, t.UTC().Format(time.RFC3339), true
}

// Runtime returns the "GOOS/GOARCH" pair the binary was built for.
func Runtime() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
