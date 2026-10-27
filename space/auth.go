package space

import (
	"context"

	"github.com/lalternativefabrique/sklp/internal/registry"
)

// authCtxKey carries deploy-supplied registry overrides through the pull path
// without widening every prepareImage/newImageResolver signature. The deployer
// attaches the creds to the context it passes to Start; the resolver reads them
// back. A context with no creds falls back to the stored credentials, so every
// non-deploy caller (CI, runner builds, push) is unaffected.
type authCtxKey struct{}

// WithRegistryAuths returns a context carrying per-host pull credentials that
// the image resolver prefers over the stored ones. Passing a nil/empty
// map is a no-op (the returned context behaves like the input). This is the
// daemonless equivalent of mounting a Kubernetes imagePullSecret: the creds live
// only in memory, on the request context, never on disk.
func WithRegistryAuths(ctx context.Context, auths map[string]RegistryAuth) context.Context {
	if len(auths) == 0 {
		return ctx
	}
	return context.WithValue(ctx, authCtxKey{}, auths)
}

// registryAuthsFrom extracts the deploy-supplied overrides from a context, or
// nil when none were attached.
func registryAuthsFrom(ctx context.Context) map[string]RegistryAuth {
	a, _ := ctx.Value(authCtxKey{}).(map[string]RegistryAuth)
	return a
}

// RegistryAuth is one registry's pull credential, supplied OUT OF BAND of the
// stored credentials — e.g. a deploy.yaml `registries:` block whose password
// is resolved from the environment (like a Kubernetes imagePullSecret). The
// host key is the registry hostname (e.g. "rg.fr-par.scw.cloud").
type RegistryAuth struct {
	Username string
	Password string
}

// registryAuths returns a credential function for containerd's resolver.
// `overrides` are deploy-supplied per-host creds that WIN over the stored
// ones; for any host they don't cover it falls back to the file
// `sklp registry login` writes. ~/.docker/config.json is never consulted: a
// credential sklp was not given does not exist, so a stale or revoked docker
// login cannot authenticate a pull or a push behind sklp's back.
func registryAuths(overrides map[string]RegistryAuth) func(host string) (string, string, error) {
	fileAuth := storedAuths()
	return func(host string) (string, string, error) {
		if a, ok := overrides[host]; ok && a.Username != "" {
			return a.Username, a.Password, nil
		}
		return fileAuth(host)
	}
}

// storedAuths is the file-backed half of registryAuths: it loads the sklp
// registries file once and returns a per-host lookup. A missing file, or a
// host it does not name, yields empty credentials — which is what a public
// image needs anyway.
func storedAuths() func(host string) (string, string, error) {
	empty := func(string) (string, string, error) { return "", "", nil }
	f, _, err := registry.Load()
	if err != nil {
		return empty
	}
	return func(host string) (string, string, error) {
		// A credential this run resolved wins: it is the value the pipeline
		// was told to push with, already materialised.
		if r, ok := registry.Remembered(host); ok {
			return r.Username, r.Password, nil
		}
		r, err := f.Resolve(host)
		if err != nil {
			// A host with no entry is not an error here: the resolver asks
			// about every host it touches, including public ones.
			return "", "", nil
		}
		return r.Username, r.Password, nil
	}
}
