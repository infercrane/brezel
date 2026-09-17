package buildinfo

import (
	"runtime/debug"
	"strings"
)

// These values can be overridden with -ldflags for packaged builds. Source
// builds fall back to the VCS metadata embedded by the Go toolchain.
var (
	Version  = "dev"
	Revision = "unknown"
	BuiltAt  = "unknown"
)

// Info is the stable, content-free build identity exposed by Brezel binaries.
type Info struct {
	Version  string `json:"version"`
	Revision string `json:"revision"`
	BuiltAt  string `json:"built_at"`
	Modified bool   `json:"modified"`
}

// Current returns package metadata when supplied and otherwise derives it from
// the VCS settings recorded by the Go toolchain.
func Current() Info {
	info := Info{Version: Version, Revision: Revision, BuiltAt: BuiltAt}
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	if info.Version == "dev" && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Revision == "unknown" && setting.Value != "" {
				info.Revision = setting.Value
			}
		case "vcs.modified":
			info.Modified = strings.EqualFold(setting.Value, "true")
		}
	}
	return info
}
