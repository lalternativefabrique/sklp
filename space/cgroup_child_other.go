//go:build !linux

package space

import "github.com/lalternativefabrique/sklp/space/ociproc"

func applyChildLimits(string, int, ociproc.Limits) error { return nil }
func clearChildCgroup(string)                            {}
