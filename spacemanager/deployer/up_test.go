package deployer

import (
	"context"
	"testing"
)

// TestUpReturnsErrorNoExit asserts Up surfaces a non-nil error (never os.Exit)
// for a missing file. On the default (non-embedded) build this is the stub's
// ErrNoEmbeddedRuntime; under embedded_runtime it is the Load failure — both
// satisfy err != nil, keeping this test build-tag-agnostic.
func TestUpReturnsErrorNoExit(t *testing.T) {
	if err := Up(context.Background(), UpConfig{File: "/nonexistent/deploy.yaml"}); err == nil {
		t.Fatal("Up returned nil error for a missing deploy.yaml; want a non-nil error")
	}
}
