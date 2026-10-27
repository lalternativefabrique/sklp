package plan

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// registryLoginWritten records that this process resolved a registry
// credential during this run. The stage that resolves and the stage that
// pushes are far apart, and threading a bool between them would widen four
// structs for one message. A push refused for a host no credential covered
// reads differently from one refused for a host that had one.
var registryLoginWritten atomic.Bool

// MarkRegistryLoginWritten is called by the login path once it has resolved a
// credential for this run.
func MarkRegistryLoginWritten() { registryLoginWritten.Store(true) }

// RegistryLoginWasWritten reports whether this run resolved a registry
// credential before pushing.
func RegistryLoginWasWritten() bool { return registryLoginWritten.Load() }

// registryAuthSymptoms are the substrings a registry uses to refuse a push it
// could not authenticate. Scaleway answers its token endpoint with 410 Gone
// rather than a 401, so matching on the status alone misses it; the wording
// below is what the containerd resolver surfaces in each case.
var registryAuthSymptoms = []string{
	"failed to authorize",
	"failed to fetch oauth token",
	"unauthorized",
	"401 unauthorized",
	"403 forbidden",
	"410 gone",
	"denied: requested access to the resource is denied",
	"insufficient_scope",
}

// imageHostOf returns the registry host of an image reference, or the
// reference itself when it carries no host, which is enough to name in a
// message even if it is not a host.
func imageHostOf(image string) string {
	image = strings.TrimSpace(image)
	if i := strings.Index(image, "/"); i > 0 {
		first := image[:i]
		if first == "localhost" || strings.ContainsAny(first, ".:") {
			return first
		}
	}
	return image
}

// looksLikeRegistryAuthFailure reports whether err is a registry refusing the
// credential rather than a transport or content problem. It reads the message
// because the resolver wraps the registry's answer in an opaque error.
func looksLikeRegistryAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, symptom := range registryAuthSymptoms {
		if strings.Contains(msg, symptom) {
			return true
		}
	}
	return false
}

// describePushAuthFailure turns a registry's refusal into the shortest note
// that names where to look. The registry's own message says the token request
// failed and nothing else: not which credential was used, not which file
// declared it, not that the run may have skipped its login because the auth
// file already held an entry. Each of those is a place the reader would
// otherwise have to guess.
//
// host is the registry the push targeted. resolvedLogin tells whether this
// run supplied a credential at all, which separates a rejected secret from a
// push that went out unauthenticated.
func describePushAuthFailure(host string, resolvedLogin bool, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "the registry refused the credential for %s\n", host)
	if resolvedLogin {
		b.WriteString("  this run supplied the credential `sklp registry login` stored for that host,\n")
		b.WriteString("  so the stored value is the one the registry refused\n")
		fmt.Fprintf(&b, "  --> sklp registry login %s   (the stored secret is stale or wrong)\n", host)
	} else {
		b.WriteString("  this run supplied no credential for that host\n")
		fmt.Fprintf(&b, "  --> sklp registry login %s\n", host)
	}
	fmt.Fprintf(&b, "  the registry answered: %v", err)
	return b.String()
}

// pushAuthHint is the one-line form for a caller that has no room for the
// full note, such as an aggregated failure list.
func pushAuthHint(host string, resolvedLogin bool) string {
	if resolvedLogin {
		return fmt.Sprintf("%s refused the stored credential — re-run `sklp registry login %s`", host, host)
	}
	return fmt.Sprintf("%s refused a push this run had no credential for — run `sklp registry login %s`", host, host)
}
