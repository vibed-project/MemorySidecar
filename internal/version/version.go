package version

import "runtime/debug"

// Set via -ldflags by the Makefile, the Dockerfile and GoReleaser.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func init() {
	// `go install github.com/vibed-project/mindD/cmd/mindd@v0.1.0` applies no
	// ldflags, so without this a tagged install reports "dev (unknown,
	// unknown)". The toolchain stamps the module version and VCS metadata into
	// the build info, so prefer that over the placeholders when it is present.
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "unknown" && s.Value != "" {
				Commit = s.Value
			}
		case "vcs.time":
			if BuildDate == "unknown" && s.Value != "" {
				BuildDate = s.Value
			}
		}
	}
}

func String() string {
	return Version + " (" + Commit + ", " + BuildDate + ")"
}
