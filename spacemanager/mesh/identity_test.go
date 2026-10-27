package mesh

import "testing"

func TestEnsureIdentityIsStableAcrossCalls(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	first, err := EnsureIdentity()
	if err != nil {
		t.Fatalf("first EnsureIdentity: %v", err)
	}
	second, err := EnsureIdentity()
	if err != nil {
		t.Fatalf("second EnsureIdentity: %v", err)
	}

	if first.NodeID != second.NodeID {
		t.Fatalf("node id changed across calls: %q != %q", first.NodeID, second.NodeID)
	}
	if first.PrivateKey.String() != second.PrivateKey.String() {
		t.Fatalf("private key changed across calls")
	}
}

func TestEnsureIdentityGeneratesDistinctIDs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, err := EnsureIdentity()
	if err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	b, err := EnsureIdentity()
	if err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}

	if a.NodeID == b.NodeID {
		t.Fatalf("two independent identities got the same node id: %q", a.NodeID)
	}
	if a.PrivateKey.String() == b.PrivateKey.String() {
		t.Fatalf("two independent identities got the same private key")
	}
}
