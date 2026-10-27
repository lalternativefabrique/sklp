package controller

import (
	"context"
	"errors"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"os"
	"strings"
	"testing"
)

// defaults must fill the feedback streams so a space whose caller omitted them
// does not nil-deref when runSpace writes a warning.
func TestDefaultsFillStreams(t *testing.T) {
	o := defaults(SpaceOptions{})
	if o.Out != os.Stdout {
		t.Errorf("Out default = %v, want os.Stdout", o.Out)
	}
	if o.ErrOut != os.Stderr {
		t.Errorf("ErrOut default = %v, want os.Stderr", o.ErrOut)
	}
}

// defaults must not override explicitly-provided streams.
func TestDefaultsKeepProvidedStreams(t *testing.T) {
	r, _, _ := os.Pipe()
	o := defaults(SpaceOptions{Out: r, ErrOut: r})
	if o.Out != r || o.ErrOut != r {
		t.Error("defaults overwrote caller-provided streams")
	}
}

// proxyDynDir must stay identical to the previous inline path
// (os.TempDir()/sklp-deploy/<space>/traefik) so the extraction is parity.
func TestProxyDynDirParity(t *testing.T) {
	got := proxyDynDir("acme")
	if !strings.HasSuffix(got, "/sklp-deploy/acme/traefik") {
		t.Errorf("proxyDynDir = %q, want .../sklp-deploy/acme/traefik", got)
	}
	if !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("proxyDynDir = %q, want under TempDir %q", got, os.TempDir())
	}
}

func TestWrapListenersSignalsReadyForThePurgeListener(t *testing.T) {
	var called, ready bool
	opts := wrapListeners(SpaceOptions{
		NetListenerWithPurge: func(context.Context, string, control.Supervisor, func(), func()) { called = true },
	}, func(error) { ready = true })
	if opts.NetListenerWithPurge == nil {
		t.Fatal("the purge listener must stay the one runSpace calls")
	}
	opts.NetListenerWithPurge(context.Background(), "s", nil, func() {}, func() {})
	if !called || !ready {
		t.Fatalf("called=%v ready=%v, want both: AddSpace waits on this signal", called, ready)
	}
}

func TestWrapListenersSignalsReadyForThePlainListener(t *testing.T) {
	var ready bool
	opts := wrapListeners(SpaceOptions{}, func(error) { ready = true })
	opts.NetListener(context.Background(), "s", nil, func() {})
	if !ready {
		t.Fatal("a space with no caller listener must still signal ready")
	}
}

func TestRequireTrustedImages(t *testing.T) {
	svcs := []spacemanager.Service{{Name: "www", Image: "ghcr.io/x/y:1"}}
	t.Setenv(spacemanager.TrustedRegistriesEnv, "")
	if err := requireTrustedImages(svcs); err != nil {
		t.Fatalf("no policy must accept: %v", err)
	}
	t.Setenv(spacemanager.TrustedRegistriesEnv, "rg.fr-par.scw.cloud")
	err := requireTrustedImages(svcs)
	if err == nil || !errors.Is(err, spacemanager.ErrConfig) {
		t.Fatalf("want ErrConfig refusal, got %v", err)
	}
}
