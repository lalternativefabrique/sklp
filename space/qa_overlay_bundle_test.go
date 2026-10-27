package space

import "testing"

// TestWriteServiceBundleNet_OverlayRoot proves the overlay mutations survive
// the JSON round-trip into the on-disk config.json (the in-memory applyOverlayRoot
// test asserts Go maps; this asserts what runc actually reads). QA-added (#695).
func TestWriteServiceBundleNet_OverlayRoot(t *testing.T) {
	bundle := t.TempDir()
	join := &joinNamespaces{UsernsPath: "/proc/9/ns/user", PrivateNetns: true}
	if err := writeServiceBundleNet(bundle, "/merged", Service{Name: "db", Image: "x"},
		[]string{"mariadbd"}, nil, nil, "", join, "/tmp/r.conf", "/tmp/hosts", nil, true); err != nil {
		t.Fatalf("writeServiceBundleNet: %v", err)
	}
	spec := readBundleSpec(t, bundle)

	if root := spec["root"].(map[string]any); root["readonly"] != false {
		t.Errorf("root.readonly = %v, want false", root["readonly"])
	}
	for _, d := range []string{"/tmp", "/run", "/var/run"} {
		if mountByDest(spec, d) != nil {
			t.Errorf("mount %s present in on-disk spec, want dropped", d)
		}
	}
	// resolv/hosts bind mounts still applied after the overlay filter.
	if mountByDest(spec, "/etc/resolv.conf") == nil {
		t.Error("/etc/resolv.conf mount missing — overlay filter dropped a bind mount")
	}
	linux := spec["linux"].(map[string]any)
	for _, ns := range linux["namespaces"].([]any) {
		if ns.(map[string]any)["type"] == "user" {
			t.Error("user namespace present in on-disk spec, want dropped")
		}
	}
	if _, ok := linux["uidMappings"]; ok {
		t.Error("uidMappings present in on-disk spec, want dropped")
	}
}
