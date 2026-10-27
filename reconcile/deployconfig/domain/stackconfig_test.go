package domain

import "testing"

const mixedConfig = `
encryptionsalt: T5kqEkG2qjHYVyWLzmj01A==
encryptioniters: 600000
config:
    LALTER_HOST: altfab.fr
    JWT_SECRET:
        secure: JHNEG00VxH1h92nUGbGtGctsB68D0kDANInFGVgRsC5T
    LALTER_LLM_MODEL: deepseek-v4-flash-0731
`

// A sealed value is encrypted with a passphrase this process does not have,
// so reading it as if it were plain would hand a container the ciphertext.
func TestParsePlainConfigSkipsSealedValues(t *testing.T) {
	got := ParsePlainConfig([]byte(mixedConfig))

	if got["LALTER_HOST"] != "altfab.fr" || got["LALTER_LLM_MODEL"] != "deepseek-v4-flash-0731" {
		t.Fatalf("plain values = %v, want both read", got)
	}
	if _, ok := got["JWT_SECRET"]; ok {
		t.Fatal("a sealed value must never be read as plain — that ships ciphertext to the container")
	}
}

// A malformed file costs the ${VAR} it could have resolved, which surfaces
// clearly downstream; failing here would break a deployment that may not
// reference config at all.
func TestParsePlainConfigToleratesGarbage(t *testing.T) {
	if got := ParsePlainConfig([]byte("::: not yaml :::")); len(got) != 0 {
		t.Fatalf("got %v, want nothing", got)
	}
}

func TestConfigPathForSitsBesideTheManifest(t *testing.T) {
	for manifest, want := range map[string]string{
		".sklp/prod/deploy.yaml": ".sklp/prod/config.yaml",
		".sklp/www/deploy.yaml":  ".sklp/www/config.yaml",
	} {
		if got := ConfigPathFor(manifest); got != want {
			t.Errorf("ConfigPathFor(%q) = %q, want %q", manifest, got, want)
		}
	}
}
