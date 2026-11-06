//go:build !linux

package plan

import "os/exec"

func stepSliceBudget() int { return 0 }

func wrapInStepSlice(*exec.Cmd) {}
