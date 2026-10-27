package spacemanager

import (
	"fmt"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// validateCapabilities rejects an unknown capability at parse time. A typo left
// to the launcher would be granted as nothing at all: the service would start,
// then fail at the one operation the capability was added for, which is a far
// worse place to discover it than the deploy that introduced it.
//
// A known capability that reaches past the userns (see
// ociproc.ManifestGrantableCapability) is refused with its own message: the
// author spelled it right and must learn it is a node decision, not a typo.
func validateCapabilities(kind, name string, caps []string) error {
	for _, c := range caps {
		if err := ValidateManifestCapability(c); err != nil {
			return fmt.Errorf("%s %q: %w", kind, name, err)
		}
	}
	return nil
}

// ValidateManifestCapability is the per-entry rule validateCapabilities and the
// apply edge share, so a pre-resolved spec cannot carry what YAML would refuse.
func ValidateManifestCapability(c string) error {
	if ociproc.NormalizeCapability(c) == "" {
		return fmt.Errorf("%q is not a capability this node grants", c)
	}
	if ociproc.ManifestGrantableCapability(c) == "" {
		return fmt.Errorf("%q cannot be granted by a manifest: it reaches past the user namespace onto the node", c)
	}
	return nil
}

// normalizedCapabilities returns caps in OCI spelling, dropping anything
// unknown. validate() runs first, so by the time a service resolves every entry
// is valid.
func normalizedCapabilities(caps []string) []string {
	if len(caps) == 0 {
		return nil
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if n := ociproc.NormalizeCapability(c); n != "" {
			out = append(out, n)
		}
	}
	return out
}
