// Package logship ships one space's container logs to Skalpai as OTLP
// LogRecords, the way an application already ships its own OTLP logs — a
// space just cannot instrument itself, so the supervisor does it on its
// containers' behalf.
//
// It reuses the SAME on-disk captured logs (service.log per instance) that
// `sklp deploy logs` reads: no new capture path, no filelog-style directory
// tailing, no journald. A tailer per live instance follows its file the way
// the reporter's own logStreamer does; each line becomes one LogRecord,
// batched and exported via otlploghttp.
//
// Scope: ONE process supervises ONE space (the existing 1 Supervisor : 1
// space : 1 process model), so this ships exactly that space's logs. A node
// running several spaces gets several shippers, one per supervising process
// — no cross-process aggregation here.
package logship

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otlploghttp "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// EnvIngestKey names the Skalpai ingest-source key (sklp_<hex>, created via
// "Add infrastructure" > Single machine) this node ships its logs under.
// Absent = shipping is disabled; a node need not ship logs to run.
const EnvIngestKey = "SKLP_LOG_INGEST_KEY"

// Supervisor is the subset of control.Supervisor a Shipper needs: the desired
// service names of the space it ships, and each one's captured log files.
// Kept minimal (not control.Supervisor itself) so this package does not import
// spacemanager and stays usable from a plain test double.
type Supervisor interface {
	DesiredServiceNames() []string
	ServiceLogSources(name string) []LogSource
}

// LogSource mirrors spacemanager.LogSource (label + captured file path) so
// this package takes no dependency on spacemanager's concrete type.
type LogSource struct {
	Label string
	Path  string
}

// Configured reports whether EnvIngestKey is set. Callers gate Start on it so
// an unconfigured node ships nothing rather than failing to boot.
func Configured() bool { return os.Getenv(EnvIngestKey) != "" }

// Shipper tails a space's service log files and exports each line as an OTLP
// LogRecord. One Shipper per supervised space (see package doc).
type Shipper struct {
	lp       *sdklog.LoggerProvider
	logger   otellog.Logger
	space    string
	tailer   *tailer
	sup      Supervisor
	stopPoll context.CancelFunc
}

// Start builds the exporter from EnvIngestKey / OTEL_EXPORTER_OTLP_ENDPOINT
// (standard OTLP env, same as the rest of sklp — see otel_observer.go) and
// begins tailing sup's desired services, discovering new ones as they start.
// Returns (nil, nil) when logship is not configured: a no-op the caller need
// not special-case. The returned shutdown stops every tail and flushes the
// exporter under a bounded timeout.
func Start(ctx context.Context, space, nodeName string, sup Supervisor) (*Shipper, func(), error) {
	if !Configured() {
		return nil, func() {}, nil
	}

	exp, err := otlploghttp.New(ctx,
		otlploghttp.WithHeaders(map[string]string{
			"x-api-key": os.Getenv(EnvIngestKey),
		}),
	)
	if err != nil {
		return nil, nil, err
	}

	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.HostName(nodeName),
	))
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		sdklog.WithResource(res),
	)

	s := &Shipper{
		lp:     lp,
		logger: lp.Logger("github.com/lalternativefabrique/sklp/sklet/logship"),
		space:  space,
		sup:    sup,
	}
	s.tailer = newTailer(s.emit)

	pollCtx, cancel := context.WithCancel(ctx)
	s.stopPoll = cancel
	go s.pollLoop(pollCtx)

	return s, s.shutdown, nil
}

// pollDiscoveryInterval bounds how quickly a newly-started service's log
// starts shipping: short enough that "sklp deploy" feels live, long enough
// that discovery is not a hot loop.
const pollDiscoveryInterval = 2 * time.Second

// pollLoop re-derives the live log sources of every desired service on a
// timer and reconciles the tailer's active set against it. Polling (not an
// event feed) keeps this package decoupled from the agent's internal
// lifecycle events — it only needs the two read methods on Supervisor.
func (s *Shipper) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(pollDiscoveryInterval)
	defer ticker.Stop()
	s.reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcile()
		}
	}
}

// reconcile asks the supervisor for every desired service's current log
// sources and tells the tailer to track exactly that set — instances that
// vanished (service removed, instance replaced) stop being tailed, and any
// new one starts.
func (s *Shipper) reconcile() {
	var sources []taggedSource
	for _, name := range s.sup.DesiredServiceNames() {
		for _, ls := range s.sup.ServiceLogSources(name) {
			sources = append(sources, taggedSource{service: name, LogSource: ls})
		}
	}
	s.tailer.sync(sources)
}

// emit converts one tailed line into an OTLP LogRecord, tagged with the
// owning service and space — the attributes the Skalpai Logs page groups by
// (service.name is the existing "Service" column; space.name lets a stack
// with several spaces tell them apart).
func (s *Shipper) emit(service, line string) {
	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetBody(attribute.StringValue(line))
	rec.AddAttributes(
		attribute.String("service.name", service),
		attribute.String("space.name", s.space),
	)
	s.logger.Emit(context.Background(), rec)
}

// shutdown stops every tail goroutine, cancels discovery polling, then
// flushes + closes the log provider under a short bounded timeout so a
// slow/absent collector can never hang shutdown.
func (s *Shipper) shutdown() {
	if s == nil {
		return
	}
	s.stopPoll()
	s.tailer.stopAll()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.lp.Shutdown(ctx)
}
