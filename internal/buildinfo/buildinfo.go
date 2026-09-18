// Package buildinfo holds version information set at build time with
// -ldflags "-X scenegit.org/forgesync/internal/buildinfo.Version=...".
package buildinfo

// Set by the linker; the defaults apply to `go run` and plain `go build`.
var (
	Version = "dev"
	Commit  = "unknown"
)

// String returns "version (commit)".
func String() string {
	return Version + " (" + Commit + ")"
}
