// Package version exposes build metadata injected at link time.
package version

import "runtime"

var (
	// Version is the semantic version, injected via -ldflags.
	Version = "dev"
	// Commit is the git SHA, injected via -ldflags.
	Commit = "unknown"
	// BuildDate is the RFC3339 build timestamp, injected via -ldflags.
	BuildDate = "unknown"
)

// Info is the payload served by GET /version.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
}

// Get returns the current build info.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
	}
}
