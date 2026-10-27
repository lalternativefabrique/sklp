//go:build !linux

package space

import "fmt"

func (s *Space) MigrateVolumeOwner(bool) ([]string, error) {
	return nil, fmt.Errorf("volume owner migration requires linux")
}
