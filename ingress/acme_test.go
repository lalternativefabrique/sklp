package ingress

import (
	"context"
	"testing"
	"time"
)

func TestMarshalUnmarshalACMEEnvRoundTrip(t *testing.T) {
	in := ACMEConfig{
		Email:              "ops@example.com",
		CADirURL:           "https://acme.example/dir",
		Staging:            true,
		StorageDir:         "/var/lib/sklp/acme",
		DNSProvider:        "ovh",
		DNSCredentials:     map[string]string{"OVH_APPLICATION_KEY": "ak", "OVH_CONSUMER_KEY": "ck"},
		PropagationTimeout: 90 * time.Second,
	}
	enc, err := MarshalACMEEnv(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if enc == "" {
		t.Fatal("expected non-empty env for a configured ACME")
	}
	out, ok, err := UnmarshalACMEEnv(enc)
	if err != nil || !ok {
		t.Fatalf("unmarshal: ok=%v err=%v", ok, err)
	}
	if out.Email != in.Email || out.DNSProvider != in.DNSProvider ||
		out.Staging != in.Staging || out.StorageDir != in.StorageDir ||
		out.CADirURL != in.CADirURL || out.PropagationTimeout != in.PropagationTimeout {
		t.Fatalf("round-trip mismatch: %+v vs %+v", out, in)
	}
	if out.DNSCredentials["OVH_APPLICATION_KEY"] != "ak" {
		t.Fatalf("creds not preserved: %+v", out.DNSCredentials)
	}
}

func TestUnmarshalACMEEnvEmpty(t *testing.T) {
	_, ok, err := UnmarshalACMEEnv("")
	if err != nil || ok {
		t.Fatalf("empty env should be (false,nil), got ok=%v err=%v", ok, err)
	}
}

func TestMarshalACMEEnvZeroIsEmpty(t *testing.T) {
	enc, err := MarshalACMEEnv(ACMEConfig{})
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if enc != "" {
		t.Fatalf("zero config should marshal to empty, got %q", enc)
	}
}

func TestNewACMEResolverValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  ACMEConfig
	}{
		{"no email", ACMEConfig{StorageDir: "/tmp/x", DNSProvider: "ovh"}},
		{"no storage", ACMEConfig{Email: "a@b.c", DNSProvider: "ovh"}},
		{"unknown provider", ACMEConfig{Email: "a@b.c", StorageDir: "/tmp/x", DNSProvider: "nope"}},
		{"missing creds", ACMEConfig{Email: "a@b.c", StorageDir: "/tmp/x", DNSProvider: "ovh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewACMEResolver(tc.cfg); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNewACMEResolverOK(t *testing.T) {
	r, err := NewACMEResolver(ACMEConfig{
		Email:       "ops@example.com",
		StorageDir:  t.TempDir(),
		DNSProvider: "ovh",
		DNSCredentials: map[string]string{
			"OVH_APPLICATION_KEY":    "ak",
			"OVH_APPLICATION_SECRET": "as",
			"OVH_CONSUMER_KEY":       "ck",
		},
	})
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	defer r.Stop()
	// A never-managed public host is served self-signed, not an ACME error.
	if r.isManaged("app.example.com") {
		t.Fatal("nothing managed yet")
	}
}

// TestQA_IsManagedCaseInsensitive proves a mixed-case route host is matched
// against the lower-case SNI a browser sends, so the ACME cert (not self-signed)
// is served. ManageDomains normalises on insert; isManaged normalises on lookup.
func TestQA_IsManagedCaseInsensitive(t *testing.T) {
	r, err := NewACMEResolver(ACMEConfig{
		Email: "ops@example.com", StorageDir: t.TempDir(), DNSProvider: "ovh",
		DNSCredentials: map[string]string{
			"OVH_APPLICATION_KEY": "ak", "OVH_APPLICATION_SECRET": "as", "OVH_CONSUMER_KEY": "ck",
		},
	})
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	defer r.Stop()
	r.manageAsync = func(context.Context, []string) error { return nil } // no CA round-trip
	// Manage a mixed-case host and a mixed-case wildcard.
	_ = r.ManageDomains(context.Background(), []string{"App.Example.COM", "*.Apps.Example.com"})
	for _, sni := range []string{"app.example.com", "svc.apps.example.com"} {
		if !r.isManaged(sni) {
			t.Errorf("SNI %q must match a mixed-case managed host", sni)
		}
	}
}

// TestQA_ManageDomainsEvictsUnmanaged proves a domain dropped from the route set
// is removed from the managed set (so it stops being served / renewed as ACME).
func TestQA_ManageDomainsEvictsUnmanaged(t *testing.T) {
	r, err := NewACMEResolver(ACMEConfig{
		Email: "ops@example.com", StorageDir: t.TempDir(), DNSProvider: "ovh",
		DNSCredentials: map[string]string{
			"OVH_APPLICATION_KEY": "ak", "OVH_APPLICATION_SECRET": "as", "OVH_CONSUMER_KEY": "ck",
		},
	})
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	defer r.Stop()
	r.manageAsync = func(context.Context, []string) error { return nil } // no CA round-trip
	_ = r.ManageDomains(context.Background(), []string{"a.example.com", "b.example.com"})
	_ = r.ManageDomains(context.Background(), []string{"a.example.com"}) // b dropped
	if !r.isManaged("a.example.com") {
		t.Error("a.example.com must stay managed")
	}
	if r.isManaged("b.example.com") {
		t.Error("b.example.com must be unmanaged after removal")
	}
}

func TestACMEResolverIsManagedWildcard(t *testing.T) {
	r := &ACMEResolver{managed: map[string]bool{"*.apps.example.com": true, "root.example.com": true}}
	cases := []struct {
		host string
		want bool
	}{
		{"a.apps.example.com", true},    // wildcard covers one label
		{"apps.example.com", false},     // wildcard does not cover the base
		{"a.b.apps.example.com", false}, // wildcard is single-label
		{"root.example.com", true},      // exact
		{"other.example.com", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := r.isManaged(tc.host); got != tc.want {
			t.Errorf("isManaged(%q)=%v want %v", tc.host, got, tc.want)
		}
	}
}
