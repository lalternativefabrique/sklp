package plan

import (
	"errors"
	"strings"
	"testing"
)

func TestLooksLikeRegistryAuthFailure_RecognisesScalewaysAnswer(t *testing.T) {
	// The wording a real run produced: Scaleway refuses the token request
	// with 410 Gone rather than a 401, so a status-only match misses it.
	err := errors.New(`push blob sha256:abc: failed to authorize: failed to fetch oauth token: ` +
		`unexpected status from GET request to https://api.scaleway.com/registry-internal/v1/` +
		`regions/fr-par/tokens?scope=repository%3Aurbangate%2Fkratos%3Apull&service=registry: 410 Gone`)
	if !looksLikeRegistryAuthFailure(err) {
		t.Error("want a 410 Gone token failure to read as an auth failure")
	}
}

func TestLooksLikeRegistryAuthFailure_IgnoresAnUnrelatedError(t *testing.T) {
	for _, msg := range []string{
		"connection reset by peer",
		"no space left on device",
		"manifest invalid",
	} {
		if looksLikeRegistryAuthFailure(errors.New(msg)) {
			t.Errorf("%q must not read as an auth failure", msg)
		}
	}
	if looksLikeRegistryAuthFailure(nil) {
		t.Error("nil is not a failure")
	}
}

func TestDescribePushAuthFailure_PointsAtTheLoginWhenNoCredentialWasSupplied(t *testing.T) {
	got := describePushAuthFailure("rg.fr-par.scw.cloud", false, errors.New("410 Gone"))
	for _, want := range []string{
		"rg.fr-par.scw.cloud",
		"supplied no credential",
		"sklp registry login rg.fr-par.scw.cloud",
		"410 Gone",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the note must mention %q:\n%s", want, got)
		}
	}
}

func TestDescribePushAuthFailure_PointsAtTheStoredSecretWhenTheRunSuppliedIt(t *testing.T) {
	got := describePushAuthFailure("ghcr.io", true, errors.New("401 Unauthorized"))
	if !strings.Contains(got, "supplied the credential `sklp registry login` stored") {
		t.Errorf("must say the run supplied the stored credential:\n%s", got)
	}
	if !strings.Contains(got, "sklp registry login ghcr.io") {
		t.Errorf("must name the verb that replaces it:\n%s", got)
	}
	// ~/.docker/config.json is not an input any more, so it is never a place
	// to send the reader.
	if strings.Contains(got, ".docker/config.json") {
		t.Errorf("must not mention the docker config, which sklp does not read:\n%s", got)
	}
}

func TestImageHostOf(t *testing.T) {
	for image, want := range map[string]string{
		"rg.nl-ams.scw.cloud/team/web": "rg.nl-ams.scw.cloud",
		"ghcr.io/org/app":              "ghcr.io",
		"localhost:5000/app":           "localhost:5000",
		"alpine":                       "alpine",
		"library/alpine":               "library/alpine",
	} {
		if got := imageHostOf(image); got != want {
			t.Errorf("imageHostOf(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestRegistryLoginMarker(t *testing.T) {
	registryLoginWritten.Store(false)
	if RegistryLoginWasWritten() {
		t.Error("must start false")
	}
	MarkRegistryLoginWritten()
	if !RegistryLoginWasWritten() {
		t.Error("must report the write")
	}
	registryLoginWritten.Store(false)
}
