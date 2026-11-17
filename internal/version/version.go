// Package version holds the build version and the GitHub repo used by the
// self-updater. Release builds set Version via
//
//	-ldflags "-X github.com/miwidot/darkbot/internal/version.Version=0.2.0"
//
// (the release workflow derives it from the git tag).
package version

// Version is the running build's version (semver, no leading "v").
// "dev" (local builds) is never considered older than a release, so a
// developer build never replaces itself with a downloaded one.
var Version = "dev"

// GitHub repo the self-updater checks for releases.
const (
	RepoOwner = "miwidot"
	RepoName  = "darkbot"
)
