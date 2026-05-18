// Package version holds the build-time version string injected via ldflags.
package version

// Version is set at build time via:
//
//	go build -ldflags "-X '...version.Version=v2.1.0'"
//
// Defaults to "dev" for local development.
var Version = "dev" //nolint:gochecknoglobals // set via ldflags at build time
