// Package version holds the build-time version string injected via ldflags.
package version

import "os"

// Version is set at build time via:
//
//	go build -ldflags "-X '...version.Version=v2.1.0'"
//
// Defaults to "dev" for local development. Note this is the cwa-manager component version (image tag).
var Version = "dev" //nolint:gochecknoglobals // set via ldflags at build time

// Agent returns the overall agent release version reported to the control plane.
// The release version is computed at release time and injected at deploy time via AGENT_VERSION (env).
func Agent() string {
	v := os.Getenv("AGENT_VERSION")
	if v == "" {
		v = Version
	}

	return normalizeRelease(v)
}

// normalizeRelease canonicalizes a release version to a leading "v" (e.g. "v2.5").
func normalizeRelease(v string) string {
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}

	return v
}
