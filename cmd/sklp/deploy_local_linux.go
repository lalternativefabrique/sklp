package main

import "github.com/lalternativefabrique/sklp/spacemanager/control"

func localSpaceOwner(space string) (string, bool) {
	_, meta, err := control.LoadManifest(space)
	if err != nil || meta.Stack == "" {
		return "", false
	}
	return meta.Stack, true
}

func localSpaceToken(space string) (string, error) {
	return control.EnsureToken(space)
}

func localSpaceFingerprint(space string) (string, error) {
	_, fp, err := control.EnsureServerCert(space)
	return fp, err
}
