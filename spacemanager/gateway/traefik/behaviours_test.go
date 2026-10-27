package traefik

import (
	"strings"
	"testing"
)

func renderOne(r route) string {
	return renderRoutes("production", "web", []route{r})
}

// TestNoBehavioursEmitsNoMiddlewares: a plain route keeps the file it always
// had, so enabling nothing changes nothing.
func TestNoBehavioursEmitsNoMiddlewares(t *testing.T) {
	out := renderOne(route{Host: "example.com", Backends: []string{"10.0.0.2:80"}})
	if strings.Contains(out, "middlewares") {
		t.Fatalf("a plain route emitted middlewares:\n%s", out)
	}
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	out := renderOne(route{Host: "example.com", Backends: []string{"10.0.0.2:80"}, TLS: true, SecurityHeaders: true})
	for _, want := range []string{
		"production-web-headers:",
		"frameDeny: true",
		"contentTypeNosniff: true",
		"stsSeconds: 31536000",
		"middlewares: [production-web-headers]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// TestSecurityHeadersSkipsHSTSWithoutTLS: HSTS over plain http is meaningless,
// and it is a one-way door for a domain — it is only ever emitted on a TLS route.
func TestSecurityHeadersSkipsHSTSWithoutTLS(t *testing.T) {
	out := renderOne(route{Host: "internal.local", Backends: []string{"10.0.0.2:80"}, SecurityHeaders: true})
	if strings.Contains(out, "stsSeconds") {
		t.Fatalf("HSTS emitted on a non-TLS route:\n%s", out)
	}
	if !strings.Contains(out, "frameDeny: true") {
		t.Fatalf("the other headers should still be set:\n%s", out)
	}
}

func TestCompressMiddleware(t *testing.T) {
	out := renderOne(route{Host: "example.com", Backends: []string{"10.0.0.2:80"}, Compress: true})
	if !strings.Contains(out, "compress:") || !strings.Contains(out, "minResponseBodyBytes: 1024") {
		t.Fatalf("compress middleware missing:\n%s", out)
	}
}

// TestApexRedirectEmitsItsOwnRouter: the www host matches no rule of the apex
// route, so the redirect needs a router of its own or it would 404.
func TestApexRedirectEmitsItsOwnRouter(t *testing.T) {
	out := renderOne(route{Host: "example.com", Backends: []string{"10.0.0.2:80"}, TLS: true, RedirectApex: true})
	for _, want := range []string{
		"production-web-apex:",
		"Host(`www.example.com`)",
		"redirectRegex:",
		"permanent: true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// TestApexRedirectCapturesScheme: redirectRegex substitutes capture groups
// only. A ${scheme} placeholder would land in the Location header verbatim and
// send every visitor to a broken URL.
func TestApexRedirectCapturesScheme(t *testing.T) {
	out := renderOne(route{Host: "example.com", Backends: []string{"10.0.0.2:80"}, TLS: true, RedirectApex: true})
	if strings.Contains(out, "${scheme}") {
		t.Fatalf("redirectRegex cannot template a scheme:\n%s", out)
	}
	if !strings.Contains(out, `regex: "^(https?)://www\\.example\\.com/(.*)"`) {
		t.Fatalf("the scheme must be captured:\n%s", out)
	}
	if !strings.Contains(out, `replacement: "${1}://example.com/${2}"`) {
		t.Fatalf("the captured scheme must be replayed:\n%s", out)
	}
}

// TestApexRedirectEscapesDots: an unescaped dot matches any character, so
// "wwwXexample.com" would redirect too.
func TestApexRedirectEscapesDots(t *testing.T) {
	out := renderOne(route{Host: "my.example.com", Backends: []string{"10.0.0.2:80"}, RedirectApex: true})
	if !strings.Contains(out, `www\\.my\\.example\\.com`) {
		t.Fatalf("host dots not escaped in the regex:\n%s", out)
	}
}

// TestApexRedirectSkippedOnWWWHost: declaring it on a www route would build a
// redirect from www.www.<host>.
func TestApexRedirectSkippedOnWWWHost(t *testing.T) {
	out := renderOne(route{Host: "www.example.com", Backends: []string{"10.0.0.2:80"}, RedirectApex: true})
	if strings.Contains(out, "www.www.") {
		t.Fatalf("built a redirect from a doubled www host:\n%s", out)
	}
}
