package spacemanager

// ResolveSource is the node-side twin of deployer.RemoteApply for a manifest
// that never touched this machine: the raw deploy.yaml plus every ${VAR} value
// the caller already resolved, so nothing is read from this process's env.
func ResolveSource(raw []byte, vars map[string]string, space, stack string) ([]Service, SpaceMeta, error) {
	lookup := func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
	cfg, err := ParseWith(raw, lookup)
	if err != nil {
		return nil, SpaceMeta{}, err
	}
	services, meta, err := cfg.Resolve(space, "")
	if err != nil {
		return nil, SpaceMeta{}, err
	}
	meta.Stack = stack
	meta.Registries = ResolveRegistryPasswordsWith(meta.Registries, lookup)
	return services, meta, nil
}
