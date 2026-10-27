package spacemanager

import (
	"fmt"
	"strconv"
	"strings"
)

// defaultServicePidsMax is the fork-bomb backstop every deployed service gets
// when its resources block names no pids cap. The pids controller is delegated
// even where cpu/memory are not, so this applies on hosts the other caps cannot.
const defaultServicePidsMax = 4096

// ResourcesSpec is the YAML `resources:` block of a service. Every field is
// optional; an unset field means no cap for that controller.
type ResourcesSpec struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
	Pids   int64  `yaml:"pids,omitempty"`

	// Require fails the launch when a requested cap cannot be enforced (no
	// cgroups v2 delegation) instead of running the service unbounded.
	Require bool `yaml:"require,omitempty"`
}

// ServiceResources is the parsed, launcher-facing form of ResourcesSpec.
type ServiceResources struct {
	CPUCores    float64
	MemoryBytes int64
	PidsMax     int64
	Require     bool
}

// Any reports whether any cap is requested.
func (r ServiceResources) Any() bool {
	return r.CPUCores > 0 || r.MemoryBytes > 0 || r.PidsMax > 0
}

func (s *ResourcesSpec) parse() (*ServiceResources, error) {
	if s == nil {
		return nil, nil
	}
	out := ServiceResources{PidsMax: s.Pids, Require: s.Require}
	if s.Pids < 0 {
		return nil, fmt.Errorf("pids must be >= 0, got %d", s.Pids)
	}
	if s.CPU != "" {
		cores, err := parseCPU(s.CPU)
		if err != nil {
			return nil, err
		}
		out.CPUCores = cores
	}
	if s.Memory != "" {
		bytes, err := parseMemory(s.Memory)
		if err != nil {
			return nil, err
		}
		out.MemoryBytes = bytes
	}
	return &out, nil
}

// parseCPU accepts a core count ("1.5") or the Kubernetes milli-core form
// ("1500m"), both meaning the same cap.
func parseCPU(v string) (float64, error) {
	v = strings.TrimSpace(v)
	if milli, ok := strings.CutSuffix(v, "m"); ok {
		n, err := strconv.ParseFloat(milli, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu %q: %w", v, err)
		}
		return n / 1000, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("cpu %q: %w", v, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("cpu %q must be > 0", v)
	}
	return n, nil
}

var memoryUnits = []struct {
	suffix string
	mult   int64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	{"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}, {"T", 1000 * 1000 * 1000 * 1000},
}

// parseMemory accepts a byte count ("536870912") or a binary/decimal suffix
// ("512Mi", "2Gi", "512MB"), matching the Kubernetes quantity vocabulary
// operators already know.
func parseMemory(v string) (int64, error) {
	v = strings.TrimSpace(v)
	for _, u := range memoryUnits {
		num, ok := strings.CutSuffix(v, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
		if err != nil {
			return 0, fmt.Errorf("memory %q: %w", v, err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("memory %q must be > 0", v)
		}
		return int64(n * float64(u.mult)), nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory %q: want bytes or a Ki/Mi/Gi suffix: %w", v, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("memory %q must be > 0", v)
	}
	return n, nil
}

// ApplyDefaultResources fills the limits every service gets when its manifest
// sets none, whichever path the manifest came in through.
func ApplyDefaultResources(r *ServiceResources) {
	if r.PidsMax == 0 {
		r.PidsMax = defaultServicePidsMax
	}
}
