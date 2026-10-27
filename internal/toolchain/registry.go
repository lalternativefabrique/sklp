// Package toolchain is sklp's native installer for the handful of
// tools that the vast majority of pipelines depend on (go, kubectl,
// helm today; node, pnpm in a follow-up). It downloads binaries from
// upstream sources, verifies their sha256, and extracts them into a
// caller-provided cache directory.
//
// Why a native registry instead of always shelling out to skt:
//   - Speed: curl + sha256 in ~2s; a subprocess hop adds latency.
//   - Auditability: kubectl touches prod. The install path must be a
//     few lines of Go reading from dl.k8s.io, not an opaque plugin.
//   - No dependency: the four native tools resolve without a second
//     binary in the loop.
//
// Tools outside this registry are handled by skt (see skt.go), the
// standalone toolchain manager baked into the runner image. The
// pipeline executor's install loop routes to either based solely on
// Get() returning a Tool or not.
package toolchain

import "sync"

// Tool is the contract every native installer satisfies. Each
// implementation is tiny on purpose: URL pattern, checksum source,
// and an extract step that drops a runnable binary at a known path.
type Tool interface {
	// Name returns the tool's canonical name as used in `tools:` —
	// "go", "kubectl", "helm", … Must match the map key in the registry
	// or Get() will silently miss.
	Name() string

	// URL returns the download URL for (version, os, arch). os matches
	// runtime.GOOS values ("linux", "darwin"); arch matches GOARCH
	// ("amd64", "arm64"). Errors are reserved for "this combination is
	// not published" — typos in version strings flow through to the
	// HTTP layer where they surface as 404s.
	URL(version, os, arch string) (string, error)

	// SHA256 returns the expected hex-encoded sha256 of the downloaded
	// artifact for (version, os, arch). Implementations either embed
	// the value (rare — only for offline-friendly tools) or fetch it
	// from a sibling file the upstream publishes (the common case).
	SHA256(version, os, arch string) (string, error)

	// Extract takes the verified archive (or raw binary) at archivePath
	// and materialises the runnable binary inside destDir. Returns the
	// absolute path of the binary the caller should prepend to PATH.
	// Implementations are responsible for chmod 0755 when the upstream
	// artifact lacks the executable bit.
	Extract(archivePath, destDir string) (binPath string, err error)
}

// registry maps tool name -> implementation. Pre-populated at package
// init time by each tools/<name>.go file's init() function. Lookup is
// case-sensitive — the schema documents tool names as lowercase.
var (
	regMu    sync.RWMutex
	registry = map[string]Tool{}
)

// Register adds t to the registry. Called from each tool's init().
// Panics on duplicate registration — that always indicates two files
// claiming the same Name(), which is a build-time mistake we want to
// catch immediately, not on first install.
func Register(t Tool) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[t.Name()]; dup {
		panic("toolchain: duplicate tool registered: " + t.Name())
	}
	registry[t.Name()] = t
}

// Get returns the Tool implementation for name and whether it exists.
// Callers use the boolean to decide between native install and the
// skt fallback path.
func Get(name string) (Tool, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	t, ok := registry[name]
	return t, ok
}

// Names returns every registered tool name. Used by the CLI's
// `sklp tools list` and by tests that want to enumerate the registry.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}
