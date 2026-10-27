//go:build !linux

package limarelay

import "testing"

// An unbuilt native backend falls through to a VM manager rather than failing,
// so a machine that has not run `sklp vz build` yet still works — but never
// silently: which Linux a run happened on is announced on stderr.
func TestPickProviderFallsThroughWhenNativeUnbuilt(t *testing.T) {
	t.Setenv("SKLP_VM_PROVIDER", "")
	prev := newNativeProvider
	t.Cleanup(func() { newNativeProvider = prev })

	newNativeProvider = func() (provider, error) { return nil, errUnbuilt{} }
	p, err := pickProvider()
	if err != nil {
		t.Skipf("no VM manager installed to fall through to: %v", err)
	}
	if p.label() == "vz" {
		t.Error("an unbuilt native backend must not be selected")
	}
}

func TestPickProviderPrefersNativeWhenReady(t *testing.T) {
	t.Setenv("SKLP_VM_PROVIDER", "")
	prev := newNativeProvider
	t.Cleanup(func() { newNativeProvider = prev })

	newNativeProvider = func() (provider, error) { return fakeNative{}, nil }
	p, err := pickProvider()
	if err != nil {
		t.Fatalf("pickProvider: %v", err)
	}
	if p.label() != "vz" {
		t.Errorf("label = %q, want the native backend to win auto-detection", p.label())
	}
}

// An explicit choice still wins, so a user can pin lima while vz is available.
func TestPickProviderHonoursForcedUnknown(t *testing.T) {
	t.Setenv("SKLP_VM_PROVIDER", "nope")
	if _, err := pickProvider(); err == nil {
		t.Fatal("an unknown SKLP_VM_PROVIDER must be reported, not ignored")
	}
}

type errUnbuilt struct{}

func (errUnbuilt) Error() string { return "artifacts missing" }

type fakeNative struct{}

func (fakeNative) label() string                { return "vz" }
func (fakeNative) status() string               { return "" }
func (fakeNative) create() error                { return nil }
func (fakeNative) start() error                 { return nil }
func (fakeNative) guestPath(h string) string    { return h }
func (fakeNative) rootExec(string) error        { return nil }
func (fakeNative) userOut(string) string        { return "" }
func (fakeNative) install(string, string) error { return nil }
func (fakeNative) execArgv(string, []string) (string, []string) {
	return "", nil
}
