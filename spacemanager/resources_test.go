package spacemanager

import "testing"

func TestParseCPU(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"1", 1},
		{"1.5", 1.5},
		{"0.5", 0.5},
		{"1500m", 1.5},
		{"500m", 0.5},
	}
	for _, c := range cases {
		got, err := parseCPU(c.in)
		if err != nil {
			t.Fatalf("parseCPU(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseCPU(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-1"} {
		if _, err := parseCPU(bad); err == nil {
			t.Errorf("parseCPU(%q): want error", bad)
		}
	}
}

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"512", 512},
		{"512Mi", 512 << 20},
		{"2Gi", 2 << 30},
		{"1Ki", 1 << 10},
		{"512MB", 512 * 1000 * 1000},
		{"1G", 1000 * 1000 * 1000},
	}
	for _, c := range cases {
		got, err := parseMemory(c.in)
		if err != nil {
			t.Fatalf("parseMemory(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseMemory(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-1", "512Xi"} {
		if _, err := parseMemory(bad); err == nil {
			t.Errorf("parseMemory(%q): want error", bad)
		}
	}
}

func TestResourcesSpecParse(t *testing.T) {
	if got, err := (*ResourcesSpec)(nil).parse(); err != nil || got != nil {
		t.Fatalf("nil spec: got %v, %v", got, err)
	}
	spec := &ResourcesSpec{CPU: "1.5", Memory: "512Mi", Pids: 2048, Require: true}
	got, err := spec.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := ServiceResources{CPUCores: 1.5, MemoryBytes: 512 << 20, PidsMax: 2048, Require: true}
	if *got != want {
		t.Errorf("parse = %+v, want %+v", *got, want)
	}
	if !got.Any() {
		t.Error("Any() = false, want true")
	}
	if (ServiceResources{}).Any() {
		t.Error("zero Any() = true, want false")
	}
	if _, err := (&ResourcesSpec{Pids: -1}).parse(); err == nil {
		t.Error("negative pids: want error")
	}
}

func TestToRunnableAppliesResources(t *testing.T) {
	spec := ServiceSpec{Image: "img", Resources: &ResourcesSpec{CPU: "2", Memory: "1Gi"}}
	svc := spec.toService("api")
	if svc.Resources.CPUCores != 2 || svc.Resources.MemoryBytes != 1<<30 {
		t.Fatalf("resources = %+v", svc.Resources)
	}
	if svc.Resources.PidsMax != defaultServicePidsMax {
		t.Errorf("PidsMax = %d, want default %d", svc.Resources.PidsMax, defaultServicePidsMax)
	}
}

func TestToRunnableDefaultPidsWithoutBlock(t *testing.T) {
	svc := ServiceSpec{Image: "img"}.toService("api")
	if svc.Resources.PidsMax != defaultServicePidsMax {
		t.Errorf("PidsMax = %d, want default %d", svc.Resources.PidsMax, defaultServicePidsMax)
	}
	if svc.Resources.CPUCores != 0 || svc.Resources.MemoryBytes != 0 {
		t.Errorf("cpu/memory should stay unbounded: %+v", svc.Resources)
	}
}

func TestValidateRejectsBadResources(t *testing.T) {
	c := &DeployConfig{Services: map[string]ServiceSpec{
		"api": {Image: "img", Resources: &ResourcesSpec{Memory: "nope"}},
	}}
	if err := c.validate(); err == nil {
		t.Fatal("want error for unparsable memory")
	}
}
