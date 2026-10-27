//go:build !linux

package publicnic

import (
	"context"
	"errors"
)

var errLinuxOnly = errors.New("public NIC reconciler is Linux-only")

func NewHost() (Host, error) { return nil, errLinuxOnly }

func Events(context.Context) (<-chan struct{}, error) { return nil, errLinuxOnly }
