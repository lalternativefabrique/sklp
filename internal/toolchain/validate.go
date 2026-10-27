package toolchain

import (
	"fmt"
	"regexp"
)

// validate.go constrains the (name, version) pair before either value is used
// to build a filesystem path or a download URL.
//
// Both come from pipeline YAML (`tools: {go: "1.26.5"}`), which is authored on
// a branch and can be dispatched remotely — so they are untrusted input, not
// operator constants. Install joins them straight into
// <cacheDir>/<name>/<version>/<os>-<arch>, and the parent of the resulting
// binary is prepended to the PATH of every subsequent step. A version of
// "../../.." therefore writes outside the tool cache and can shadow go/kubectl/
// helm for every later run on the host, across repositories.
//
// The allowed shapes below cover semver with the usual prerelease/build
// suffixes, which is what every real tool pin looks like. Anything carrying a
// path separator, a parent reference, or a shell metacharacter is refused.

var (
	// toolNameRe matches a registry tool name: lowercase alphanumeric with
	// internal dashes. Registry lookup already restricts this, but the name is
	// also a path segment, so it is validated in its own right.
	toolNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

	// toolVersionRe matches a version pin: dot-separated numbers with an
	// optional prerelease (-rc.1, -beta2) and build metadata (+build.5).
	toolVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*(-[0-9A-Za-z.]+)?(\+[0-9A-Za-z.]+)?$`)

	// sktVersionRe matches a version handed to skt, which accepts partial pins
	// ("22", "10.5") and resolves them upstream. It must start alphanumeric, so
	// a value can never be mistaken for a flag in the exec argv.
	sktVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.\-+_]*$`)

	// maxVersionLen bounds the version before it reaches a path; a unix path
	// segment is capped at 255 bytes and no real pin comes close.
	maxVersionLen = 64
)

// ValidateToolRef rejects a (name, version) pair that must not be used to build
// a path or a URL. It is the single gate every native install goes through.
func ValidateToolRef(name, version string) error {
	if !toolNameRe.MatchString(name) {
		return fmt.Errorf("toolchain: invalid tool name %q: want lowercase alphanumeric with dashes", name)
	}
	if version == "" {
		return fmt.Errorf("toolchain: version required for %q", name)
	}
	if len(version) > maxVersionLen {
		return fmt.Errorf("toolchain: version for %q is %d bytes, max %d", name, len(version), maxVersionLen)
	}
	if !toolVersionRe.MatchString(version) {
		return fmt.Errorf("toolchain: invalid version %q for %q: want a numeric version like 1.26.5, optionally -rc.1 or +build", version, name)
	}
	return nil
}

// ValidateSktRef is the looser gate for the skt delegation path. skt resolves
// PARTIAL pins ("22", "10.5") to the latest matching release, so the strict
// numeric shape above would reject legitimate values. The version never becomes
// a path segment here — skt owns its own cache layout — but it is passed as
// "<name>@<version>" in an exec argv, so a leading dash would be read as a flag
// and any separator or metacharacter has no business in a version.
func ValidateSktRef(name, version string) error {
	if !toolNameRe.MatchString(name) {
		return fmt.Errorf("toolchain: invalid tool name %q: want lowercase alphanumeric with dashes", name)
	}
	if version == "" {
		return fmt.Errorf("toolchain: version required for %q", name)
	}
	if len(version) > maxVersionLen {
		return fmt.Errorf("toolchain: version for %q is %d bytes, max %d", name, len(version), maxVersionLen)
	}
	if !sktVersionRe.MatchString(version) {
		return fmt.Errorf("toolchain: invalid version %q for %q: want an alphanumeric version like 22 or 1.26.5-rc.1", version, name)
	}
	return nil
}
