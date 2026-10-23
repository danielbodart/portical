// Package version says what this build calls itself.
package version

// Version is replaced at build time with
//
//	go build -ldflags "-X github.com/danielbodart/portical/internal/version.Version=<version>"
//
// The fallback is what you get from `go run .`, where there was no build step
// to bake anything in. It says so, rather than guessing a number that would
// then appear in a bug report as if it had been released.
var Version = "development"
