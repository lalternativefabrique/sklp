//go:build !linux

package main

import (
	"fmt"
	"io"
	"runtime"
)

func errLocalNode() error {
	return fmt.Errorf("this acts on a space running on THIS machine, which needs a Linux node (running on %s); target a node with -context instead", runtime.GOOS)
}

func localSpaceOwner(string) (string, bool) { return "", false }

func localSpaceToken(string) (string, error) { return "", errLocalNode() }

func localSpaceFingerprint(string) (string, error) { return "", errLocalNode() }

func deployDownLocal(_, _ string, _ bool, _, _ io.Writer) error { return errLocalNode() }
