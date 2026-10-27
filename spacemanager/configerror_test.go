package spacemanager

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A config error must be recognisable through wrapping: the supervisor sees it
// only after the controller has added its own context, and misreading it means
// crashlooping the unit against a fix only a human can apply.
func TestConfigError_SurvivesWrapping(t *testing.T) {
	err := ConfigError(errors.New("no data root"))
	wrapped := fmt.Errorf("space %q: %w", "production", err)

	if !errors.Is(wrapped, ErrConfig) {
		t.Error("a wrapped config error is no longer recognised as one")
	}
	if got := wrapped.Error(); got == "" || !strings.Contains(got, "no data root") {
		t.Errorf("the original message was lost: %q", got)
	}
}

// A transient failure must NOT be mistaken for a config error, or the unit would
// stop retrying something a retry would have fixed.
func TestConfigError_DoesNotSwallowOtherErrors(t *testing.T) {
	if errors.Is(errors.New("connection refused"), ErrConfig) {
		t.Error("an ordinary error was reported as a configuration error")
	}
	if ConfigError(nil) != nil {
		t.Error("ConfigError(nil) must stay nil")
	}
}
