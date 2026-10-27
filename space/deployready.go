package space

import "context"

// spaceProber dials a container natively FROM the space bridge netns. The
// launcher supplies the space network as the implementation in space mode;
// it is nil for a standalone launcher (no bridge). It is the reliable net-probe
// path for deploy: no dependency on `nc`/`wget` existing in the service image.
type spaceProber interface {
	probeTCP(addr string) bool
	probeHTTP(url string) bool
}

// ProbeCoords carries the per-service coordinates the production probe backend
// needs to reach a running container. For a SPACE service the net probes use
// SpaceProbe (native dial from the bridge netns to ContainerIP) — no binary
// dependency in the image. For a STANDALONE service (no space) they fall back
// to `runc exec` into the container via the runc binary/state/id. File/log
// probes always read host-visible paths. The deploy launcher fills these in
// from what it already resolved at Start.
type ProbeCoords struct {
	RuncBin  string
	RuncRoot string
	ID       string // container id to exec net probes into (standalone fallback)
	LogPath  string // host-side captured stdout/stderr

	// SpaceProbe / ContainerIP drive the reliable space net probe: when both
	// are set, TCP/HTTP probes dial ContainerIP from the space netns instead of
	// exec'ing a binary inside the container. nil/"" => standalone runc-exec path.
	SpaceProbe  spaceProber
	ContainerIP string

	// Port is the service's declared listen port. A bare-path HTTP probe
	// ("/health") is built into http://<ContainerIP>:<Port><path>; a probe
	// target that already carries its own host/port ignores it. 0 = unknown.
	Port int
}

// WaitServiceReady is the exported readiness gate for the deploy agent. It is a
// thin wrapper over the package-internal waitReady. In SPACE mode it uses
// spaceProbeBackend (native dial from the bridge netns to the container IP),
// which does NOT need `nc`/`wget` in the image; otherwise it falls back to the
// runc-exec backend. File/log probes are identical in both. A nil probe returns
// immediately (launch-and-forget). ctx cancels the wait so the caller (the
// agent) can abort it when the container dies before becoming ready.
func WaitServiceReady(ctx context.Context, name string, r *ServiceReady, c ProbeCoords) error {
	return waitReady(ctx, name, r, probeBackendFor(ctx, c))
}

// CheckServiceReady runs the probe exactly once: nil when it passes, an error
// naming the probe otherwise. It is the periodic health check the agent runs
// on a Ready instance, so it never polls or waits on a timeout.
func CheckServiceReady(ctx context.Context, name string, r *ServiceReady, c ProbeCoords) error {
	return checkReady(name, r, probeBackendFor(ctx, c))
}

func probeBackendFor(ctx context.Context, c ProbeCoords) probeBackend {
	if c.SpaceProbe != nil && c.ContainerIP != "" {
		return spaceProbeBackend{
			prober:  c.SpaceProbe,
			ip:      c.ContainerIP,
			port:    c.Port,
			logPath: c.LogPath,
		}
	}
	return execProbeBackend{
		ctx:      ctx,
		runcBin:  c.RuncBin,
		runcRoot: c.RuncRoot,
		id:       c.ID,
		logPath:  c.LogPath,
	}
}
