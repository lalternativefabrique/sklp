package main

import (
	"io"

	"github.com/lalternativefabrique/sklp/sklet"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

// deployDownLocal resolves the manifest at path for the named space and tears
// the deploy down on THIS host, the node-side path shared with sklet's own
// supervisor.
func deployDownLocal(path, space string, volumes bool, out, errOut io.Writer) error {
	cfg, err := spacemanager.Load(path)
	if err != nil {
		return err
	}
	// GatewayMode is irrelevant to a teardown (Down never reads it) — "" avoids
	// implying this call knows the node's authoritative mode.
	services, meta, err := cfg.Resolve(space, "")
	if err != nil {
		return err
	}
	return sklet.DownWithVolumes(meta, services, volumes, out, errOut)
}
