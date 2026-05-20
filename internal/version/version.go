// Package version exposes the binary version string. Production
// builds inject this via `-ldflags="-X
// github.com/stubbedev/treeman/internal/version.Version=vX.Y.Z"` at
// release time (see justfile's release-{patch,minor,major} recipes).
// Dev builds get the placeholder below.
package version

// Version is the semantic version of this binary. Replaced at link
// time by the release recipes. Dev builds keep the placeholder.
var Version = "0.0.0-dev"
