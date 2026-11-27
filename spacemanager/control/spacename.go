package control

import (
	"fmt"
	"regexp"
)

var spaceNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ValidSpaceName gates a space name before it is joined into a filesystem path
// (config dir, manifest, RemoveAll on purge): the multi-mux sees the raw URL
// path, so ".." would otherwise reach those joins.
func ValidSpaceName(name string) error {
	if !spaceNameRE.MatchString(name) {
		return fmt.Errorf("invalid space name %q: must match %s", name, spaceNameRE.String())
	}
	return nil
}
