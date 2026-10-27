package spacemanager

import (
	"fmt"
	"os"
	"strings"

	"github.com/distribution/reference"
)

// TrustedRegistriesEnv lists the registry hosts a node pulls from, comma
// separated (e.g. "rg.fr-par.scw.cloud,docker.io"). Unset means any registry:
// a node that has not stated a policy keeps working as before.
const TrustedRegistriesEnv = "SKLP_TRUSTED_REGISTRIES"

// RequireImageDigestEnv, when "1", refuses an image that is not pinned by
// digest: a tag is a mutable pointer the registry owner can repoint.
const RequireImageDigestEnv = "SKLP_REQUIRE_IMAGE_DIGEST"

// ImagePolicy is what a node accepts in a manifest's image references. It is a
// node property, never a manifest one — the manifest is the thing being judged.
type ImagePolicy struct {
	TrustedRegistries []string
	RequireDigest     bool
}

func ImagePolicyFromEnv() ImagePolicy {
	var p ImagePolicy
	for _, h := range strings.Split(os.Getenv(TrustedRegistriesEnv), ",") {
		if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
			p.TrustedRegistries = append(p.TrustedRegistries, h)
		}
	}
	p.RequireDigest = os.Getenv(RequireImageDigestEnv) == "1"
	return p
}

// Check judges one image reference. "alpine:3.20" resolves to docker.io the
// way the puller resolves it, so a short name is judged by where it would
// actually be pulled from.
func (p ImagePolicy) Check(image string) error {
	if image == "" {
		return nil
	}
	named, err := reference.ParseDockerRef(image)
	if err != nil {
		return fmt.Errorf("image %q: %w", image, err)
	}
	if len(p.TrustedRegistries) > 0 {
		host := strings.ToLower(reference.Domain(named))
		trusted := false
		for _, t := range p.TrustedRegistries {
			if host == t {
				trusted = true
				break
			}
		}
		if !trusted {
			return fmt.Errorf("image %q: registry %q is not trusted by this node (%s=%s)",
				image, host, TrustedRegistriesEnv, strings.Join(p.TrustedRegistries, ","))
		}
	}
	if p.RequireDigest {
		if _, ok := named.(reference.Digested); !ok {
			return fmt.Errorf("image %q: this node requires images pinned by digest (%s=1)", image, RequireImageDigestEnv)
		}
	}
	return nil
}
