// Package traefik is the traefik GatewayDriver: a single host-level traefik
// owning :443/:80 + acme.json, reading a shared file-provider dyn dir into which
// sklp writes one router file per (space, service). It is the FIRST driver behind
// deploy.Gateway — kept in its OWN package so package deploy (the agent/reconcile
// core) carries ZERO vendor/traefik code. The driver imports deploy one-way and
// registers itself via deploy.RegisterGatewayDriver from init(); the CLI edge
// pulls it in with a blank import.
package traefik

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// imageTag pins the concrete traefik image the singleton runs. Pinned (not
// :latest) so a host upgrade is an explicit, reviewable change of this constant.
const imageTag = "traefik:v3.3"

// Ports 9080/9443 (unprivileged) so the rootless container can bind without
// CAP_NET_BIND_SERVICE. On a public host redirect :80→:9080 / :443→:9443 via
// nftables — `sklp gateway install-redirect` writes those rules.
const (
	entryWeb       = "web"
	entryWebsecure = "websecure"
	certResolver   = "le"

	portWeb       = ":9080"
	portWebsecure = ":9443"
)

// sanitizeFile makes a name safe as a file stem. A tiny local copy of the deploy
// helper of the same name, duplicated here on purpose so this package does not
// widen deploy's exported API just to share one trivial mapping.
func sanitizeFile(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// routerName is the deterministic traefik router/service id for a (space,
// service): "<space>-<service>", matching the namespaced dyn-dir filename so two
// spaces routing the same service never collide.
func routerName(space, service string) string {
	return sanitizeFile(space) + "-" + sanitizeFile(service)
}

// route is the minimal per-route view renderRoutes needs, decoupled from the
// spacemanager.Domain type so render stays a pure string function the unit tests
// drive directly.
type route struct {
	Host     string
	Path     string // optional path prefix; "" = host root
	Backends []string
	TLS      bool

	RedirectApex    bool
	SecurityHeaders bool
	Compress        bool

	// Rate/Burst are the per-client-IP limit. traefik's rateLimit middleware
	// averages over a period rather than holding a token bucket, so the
	// enforcement is not bit-identical to the native L7's — the declared
	// budget is.
	Rate  float64
	Burst int

	// Timeout is the per-request cap. traefik has no per-router request
	// timeout, so it is rendered as the backend serversTransport's forwarding
	// timeouts — the closest equivalent it offers.
	Timeout time.Duration

	// MaxBody is the per-request body cap in bytes, rendered as the buffering
	// middleware's maxRequestBodyBytes. 0 = unbounded (traefik's own default).
	MaxBody int64

	// BackendHost, when non-empty, is sent upstream as the Host header in
	// place of the public one — a mesh route's backend is the remote node's
	// controller, which routes by internal name.
	BackendHost string

	Passthrough bool
}

// renderRoutes emits the traefik file-provider config for ALL routes a
// (space, service) is exposed on, as one document. Pure (no FS): the driver
// writes the result atomically into the dyn dir. Each route gets its own router
// and service block named "<space>-<service>" plus a per-route suffix so a
// service on several hosts/paths never collides. The rule is Host(`h`), plus
// && PathPrefix(`/p`) when a path is set; TLS routes carry the cert resolver.
func renderRoutes(space, service string, routes []route) string {
	base := routerName(space, service)

	var routers, services, middlewares, transports, tcpRouters, tcpServices, tcpMiddlewares strings.Builder
	for i, r := range routes {
		name := base
		if len(routes) > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		if r.Passthrough {
			renderPassthrough(name, r, &tcpRouters, &tcpServices, &tcpMiddlewares)
			continue
		}
		entry := entryWeb
		if r.TLS {
			entry = entryWebsecure
		}
		rule := fmt.Sprintf("Host(`%s`)", r.Host)
		if r.Path != "" {
			rule += fmt.Sprintf(" && PathPrefix(`%s`)", r.Path)
		}
		mws := middlewaresFor(name, r, &middlewares)
		fmt.Fprintf(&routers, "    %s:\n", name)
		fmt.Fprintf(&routers, "      rule: \"%s\"\n", rule)
		fmt.Fprintf(&routers, "      service: %s\n", name)
		fmt.Fprintf(&routers, "      entryPoints: [%s]\n", entry)
		if len(mws) > 0 {
			fmt.Fprintf(&routers, "      middlewares: [%s]\n", strings.Join(mws, ", "))
		}
		if r.TLS {
			routers.WriteString("      tls:\n")
			fmt.Fprintf(&routers, "        certResolver: %s\n", certResolver)
		}

		// The apex redirect needs its own router: it matches the www host, which
		// the route's own rule does not. It points at the same service because a
		// traefik router requires one, but the redirect middleware answers before
		// the backend is ever reached.
		if r.RedirectApex && !strings.HasPrefix(r.Host, "www.") {
			rname := name + "-apex"
			fmt.Fprintf(&routers, "    %s:\n", rname)
			fmt.Fprintf(&routers, "      rule: \"Host(`www.%s`)\"\n", r.Host)
			fmt.Fprintf(&routers, "      service: %s\n", name)
			fmt.Fprintf(&routers, "      entryPoints: [%s]\n", entry)
			fmt.Fprintf(&routers, "      middlewares: [%s]\n", rname)
			if r.TLS {
				routers.WriteString("      tls:\n")
				fmt.Fprintf(&routers, "        certResolver: %s\n", certResolver)
			}
			fmt.Fprintf(&middlewares, "    %s:\n      redirectRegex:\n", rname)
			// The scheme is CAPTURED, not templated: redirectRegex substitutes
			// capture groups only, so a ${scheme} placeholder would end up in the
			// Location header verbatim.
			fmt.Fprintf(&middlewares, "        regex: \"^(https?)://www\\\\.%s/(.*)\"\n", regexpEscapeHost(r.Host))
			fmt.Fprintf(&middlewares, "        replacement: \"${1}://%s/${2}\"\n", r.Host)
			middlewares.WriteString("        permanent: true\n")
		}

		// Deterministic backend order so an identical set produces an identical
		// file (no spurious file-provider reload).
		sorted := append([]string(nil), r.Backends...)
		sort.Strings(sorted)
		fmt.Fprintf(&services, "    %s:\n      loadBalancer:\n", name)
		// A per-request cap becomes a serversTransport on THIS service: traefik
		// has no per-router request timeout, so the forwarding timeouts toward
		// the backend are the closest equivalent it offers. Named per service so
		// two routes of different services never share one.
		if r.Timeout > 0 {
			transport := name + "-transport"
			fmt.Fprintf(&services, "        serversTransport: %s\n", transport)
			fmt.Fprintf(&transports, "    %s:\n      forwardingTimeouts:\n", transport)
			fmt.Fprintf(&transports, "        dialTimeout: %s\n", durationSeconds(r.Timeout))
			fmt.Fprintf(&transports, "        responseHeaderTimeout: %s\n", durationSeconds(r.Timeout))
		}
		services.WriteString("        servers:\n")
		for _, addr := range sorted {
			fmt.Fprintf(&services, "          - url: \"http://%s\"\n", addr)
		}
	}

	var b strings.Builder
	if routers.Len() > 0 {
		b.WriteString("http:\n  routers:\n")
		b.WriteString(routers.String())
		b.WriteString("  services:\n")
		b.WriteString(services.String())
		if middlewares.Len() > 0 {
			b.WriteString("  middlewares:\n")
			b.WriteString(middlewares.String())
		}
		if transports.Len() > 0 {
			b.WriteString("  serversTransports:\n")
			b.WriteString(transports.String())
		}
	}
	if tcpRouters.Len() > 0 {
		b.WriteString("tcp:\n  routers:\n")
		b.WriteString(tcpRouters.String())
		b.WriteString("  services:\n")
		b.WriteString(tcpServices.String())
		b.WriteString("  middlewares:\n")
		b.WriteString(tcpMiddlewares.String())
	}
	return b.String()
}

// passthroughConnsPerIP caps concurrent passthrough connections per client IP:
// traefik cannot see inside the TLS stream, so this is the only flood guard.
const passthroughConnsPerIP = 100

func renderPassthrough(name string, r route, routers, services, middlewares *strings.Builder) {
	limit := name + "-conns"
	fmt.Fprintf(routers, "    %s:\n", name)
	fmt.Fprintf(routers, "      rule: \"HostSNI(`%s`)\"\n", r.Host)
	fmt.Fprintf(routers, "      service: %s\n", name)
	fmt.Fprintf(routers, "      entryPoints: [%s]\n", entryWebsecure)
	fmt.Fprintf(routers, "      middlewares: [%s]\n", limit)
	fmt.Fprintf(middlewares, "    %s:\n      inFlightConn:\n        amount: %d\n", limit, passthroughConnsPerIP)
	routers.WriteString("      tls:\n        passthrough: true\n")
	sorted := append([]string(nil), r.Backends...)
	sort.Strings(sorted)
	fmt.Fprintf(services, "    %s:\n      loadBalancer:\n        servers:\n", name)
	for _, addr := range sorted {
		fmt.Fprintf(services, "          - address: \"%s\"\n", addr)
	}
}

// durationSeconds renders a duration the way traefik's forwardingTimeouts
// expect it. Sub-second values round UP to 1s: traefik accepts a smaller unit,
// but a timeout under a second on a network hop is a misconfiguration, and
// rounding down to "0s" would mean NO timeout — the inverse of the request.
func durationSeconds(d time.Duration) string {
	secs := int64(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%ds", secs)
}

// middlewaresFor emits the middleware blocks a route asked for and returns the
// names to attach to its router. The definitions are per-router rather than
// shared: a shared name would have to be declared in a file this driver does
// not own, and PruneRoutes only keeps the files it generates.
//
// The apex redirect is emitted as a SEPARATE router (see apexRedirectRouter),
// not as a middleware on this one — the www host has to be matched first.
func middlewaresFor(name string, r route, out *strings.Builder) []string {
	var names []string
	if r.SecurityHeaders {
		mw := name + "-headers"
		fmt.Fprintf(out, "    %s:\n      headers:\n", mw)
		if r.TLS {
			// HSTS over plain http is meaningless; traefik emits it only when the
			// request is served over TLS anyway, but declaring it per-scheme keeps
			// the generated file honest about what it does.
			out.WriteString("        stsSeconds: 31536000\n")
			out.WriteString("        stsIncludeSubdomains: true\n")
		}
		out.WriteString("        frameDeny: true\n")
		out.WriteString("        contentTypeNosniff: true\n")
		out.WriteString("        referrerPolicy: \"strict-origin-when-cross-origin\"\n")
		names = append(names, mw)
	}
	if r.Compress {
		mw := name + "-compress"
		fmt.Fprintf(out, "    %s:\n      compress:\n        minResponseBodyBytes: %d\n", mw, compressMinSize)
		names = append(names, mw)
	}
	if r.Rate > 0 {
		mw := name + "-ratelimit"
		avg, period := rateLimitAverage(r.Rate)
		fmt.Fprintf(out, "    %s:\n      rateLimit:\n", mw)
		fmt.Fprintf(out, "        average: %d\n", avg)
		fmt.Fprintf(out, "        burst: %d\n", r.Burst)
		if period != "" {
			fmt.Fprintf(out, "        period: %s\n", period)
		}
		names = append(names, mw)
	}
	if r.MaxBody > 0 {
		mw := name + "-maxbody"
		fmt.Fprintf(out, "    %s:\n      buffering:\n        maxRequestBodyBytes: %d\n", mw, r.MaxBody)
		// memRequestBodyBytes bounds how much traefik holds in MEMORY before
		// spilling the rest to a temp file; it is NOT the cap. Left at the
		// smaller of 1 MiB (traefik's own default) and the cap itself, so a cap
		// below 1 MiB never reserves more memory than it can possibly need.
		if mem := int64(bufferingMemBytes); r.MaxBody < mem {
			fmt.Fprintf(out, "        memRequestBodyBytes: %d\n", r.MaxBody)
		}
		names = append(names, mw)
	}
	if r.BackendHost != "" {
		mw := name + "-meshhost"
		fmt.Fprintf(out, "    %s:\n      headers:\n        customRequestHeaders:\n          Host: %q\n", mw, r.BackendHost)
		names = append(names, mw)
	}
	return names
}

// rateLimitAverage expresses a requests-per-second rate as traefik's integer
// `average` over a `period`. traefik takes no fractional average, so a rate
// below 1 rps is scaled up into a longer period (0.5 rps -> 1 per 2s) instead
// of truncating to 0, which traefik reads as "no limit" — the exact inverse of
// what the declaration asked for. An empty period means traefik's default (1s).
func rateLimitAverage(rps float64) (average int64, period string) {
	if rps >= 1 {
		return int64(rps), ""
	}
	// A rate small enough to overflow int64 would render a NEGATIVE period,
	// which traefik rejects — and a rejected middleware drops the whole file,
	// unrouting every route of the service. Clamp to a day instead.
	secs := math.Ceil(1 / rps)
	if secs > maxRateLimitPeriodSeconds {
		secs = maxRateLimitPeriodSeconds
	}
	return 1, fmt.Sprintf("%ds", int64(secs))
}

// maxRateLimitPeriodSeconds caps the scaled period at 24h: below roughly one
// request per day the distinction stops being meaningful, and the cap keeps
// the rendered value inside int64.
const maxRateLimitPeriodSeconds = 86400

// bufferingMemBytes is traefik's own memRequestBodyBytes default: the body is
// held in memory up to this size and spilled to a temp file beyond it. Named
// here because a route capping the body BELOW it should not reserve more.
const bufferingMemBytes = 1 << 20

// compressMinSize matches the native L7's floor, so the same declaration
// compresses the same responses on either driver.
const compressMinSize = 1024

// renderStaticConfig emits the traefik STATIC config the singleton boots with.
// Pure (no FS): unit-tested so the config is covered without running traefik.
//
//   - entryPoints: web :80 (ACME HTTP-01 challenge + plain routes) and websecure
//     :443 (TLS).
//   - providers.file: the dyn dir (mounted read-only into the container at
//     dynMountPath), watched for hot reload.
//   - certificatesResolvers.le.acme: storage = acmeMountPath, contact email,
//     checkNewVersion/dashboard off. Challenge is dnsChallenge (dnsProvider
//     non-empty — the only challenge that issues wildcard certs, solved by
//     traefik's own built-in lego provider, e.g. "ovh") or httpChallenge on the
//     web entrypoint (dnsProvider empty — the pre-existing default).
//
// dynMountPath and acmeMountPath are the IN-CONTAINER paths the dyn dir and
// acme.json are mounted at (decided by the launcher), not host paths.
func renderStaticConfig(dynMountPath, acmeMountPath, logMountPath, email, dnsProvider string) string {
	if email == "" {
		// A non-empty contact is required by some ACME servers; a placeholder keeps
		// traefik bootable offline/in dev. TODO: source from gateway config.
		email = "admin@example.com"
	}
	var b strings.Builder
	b.WriteString("global:\n")
	b.WriteString("  checkNewVersion: false\n")
	b.WriteString("  sendAnonymousUsage: false\n")
	b.WriteString("api:\n")
	b.WriteString("  dashboard: false\n")
	// Persist traefik's own log to the bind-mounted config dir: the singleton runs
	// detached with its stdout piped to a now-exited launcher, so file logging is
	// the only way to read ACME/cert diagnostics after the fact.
	fmt.Fprintf(&b, "log:\n  level: INFO\n  filePath: %q\n", logMountPath)
	b.WriteString("entryPoints:\n")
	// Every plain-HTTP request is redirected to HTTPS at the entrypoint —
	// without this, http://<host>/ answered a bare 404 (routers only attach
	// to websecure). Traefik keeps serving the ACME HTTP-01 challenge on the
	// web entrypoint regardless: the challenge handler takes priority over
	// entrypoint redirections by design.
	fmt.Fprintf(&b, "  %s:\n    address: %q\n", entryWeb, portWeb)
	b.WriteString("    http:\n      redirections:\n        entryPoint:\n")
	// ":443" (the PUBLIC port), not the websecure entrypoint name: naming the
	// entrypoint makes traefik emit its own bind port in the Location header
	// (https://host:9443/ — the unprivileged port behind the NAT), which the
	// outside world cannot reach.
	b.WriteString("          to: \":443\"\n          scheme: https\n          permanent: true\n")
	fmt.Fprintf(&b, "  %s:\n    address: %q\n", entryWebsecure, portWebsecure)
	b.WriteString("providers:\n")
	b.WriteString("  file:\n")
	fmt.Fprintf(&b, "    directory: %q\n", dynMountPath)
	b.WriteString("    watch: true\n")
	b.WriteString("certificatesResolvers:\n")
	fmt.Fprintf(&b, "  %s:\n    acme:\n", certResolver)
	fmt.Fprintf(&b, "      email: %q\n", email)
	fmt.Fprintf(&b, "      storage: %q\n", acmeMountPath)
	if dnsProvider != "" {
		fmt.Fprintf(&b, "      dnsChallenge:\n        provider: %s\n", dnsProvider)
	} else {
		fmt.Fprintf(&b, "      httpChallenge:\n        entryPoint: %s\n", entryWeb)
	}
	return b.String()
}

// regexpEscapeHost escapes the dots of a host so the redirect regex matches
// that host and not any single character in its place.
func regexpEscapeHost(host string) string {
	return strings.ReplaceAll(host, ".", "\\\\.")
}
