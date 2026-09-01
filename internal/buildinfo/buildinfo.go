// Package buildinfo carries the version stamp injected at build time via
// -ldflags -X. All nylon binaries share it.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

var (
	Version = "dev"  // set via -ldflags -X
	Commit  = "none" // set via -ldflags -X
)

// String returns "Version: <Version> (commit <Commit>)"; when Commit == "none",
// it falls back to debug.ReadBuildInfo vcs.revision (go builds inside git).
func String() string {
	commit := Commit
	if commit == "none" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" && setting.Value != "" {
					commit = setting.Value
					if len(commit) > 8 {
						commit = commit[:8]
					}
					break
				}
			}
		}
	}
	return fmt.Sprintf("Version: %s (commit %s)", Version, commit)
}
