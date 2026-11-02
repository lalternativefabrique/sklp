//go:build linux

package plan

import (
	"context"
	"errors"
	"testing"
)

func TestBuildLimits(t *testing.T) {
	cases := []struct {
		cpus      int
		wantCores float64
	}{{20, 10}, {4, 2}, {3, 2}, {1, 1}}
	for _, c := range cases {
		got := buildLimits(c.cpus, 32)
		if got.CPUCores != c.wantCores || got.MemoryBytes != 16 || got.PidsMax != buildPidsLimit {
			t.Errorf("buildLimits(%d, 32) = %+v", c.cpus, got)
		}
	}
}

func TestImageBuildSlotAdmitsOneBuild(t *testing.T) {
	release, err := acquireImageBuildSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireImageBuildSlot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("second build admitted: err = %v", err)
	}
}
