package devstack

// BuildCacheDir is the host dev-stack build cache (go mod / build artefacts
// shared across run: services). Exported so the `sklp cache` hub can clean it.
func BuildCacheDir() string { return devBuildCacheDir() }

// ToolsCacheDir is the host skt toolchain cache (~/.cache/skt). Exported so the
// `sklp cache` hub can clean it.
func ToolsCacheDir() string { return devToolsCacheDir() }
