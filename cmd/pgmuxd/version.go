package main

import "runtime/debug"

// Set with -ldflags "-X main.version=... -X main.commit=... -X main.buildDate=...".
// With the exec driver pinning artifacts by checksum, the running binary has to
// be able to say which one it is.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

// resolveVersion fills in from the VCS stamps Go embeds, so a plain `go build`
// still reports something useful.
func resolveVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if commit == "none" && setting.Value != "" {
				commit = setting.Value
			}
		case "vcs.time":
			if buildDate == "unknown" && setting.Value != "" {
				buildDate = setting.Value
			}
		}
	}
}
