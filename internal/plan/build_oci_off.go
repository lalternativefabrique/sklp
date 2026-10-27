//go:build !linux

package plan

// On a build WITHOUT -tags embedded_runtime on Linux, the native builder is
// unavailable; nativeBuildFn keeps its package default (a loud error). The real
// implementation lives in docker_build_native_linux.go (tagged), which assigns
// nativeBuildFn in its init().
