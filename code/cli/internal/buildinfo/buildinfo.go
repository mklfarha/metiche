// Package buildinfo carries the version stamped into a release build with
// -ldflags -X (see .goreleaser.yaml). A plain go build reports "dev".
package buildinfo

import "runtime"

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Platform is goos/goarch, as the User-Agent and `metiche version` print it.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// UserAgent is sent on every request; whoami's recent_requests evidence tells
// the CLI's own requests from a client's by this prefix (docs/CLI.md §1.1).
func UserAgent() string { return "metiche-cli/" + Version + " (" + Platform() + ")" }
