package sklet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
	"github.com/lalternativefabrique/sklp/restore/natsjs"
	"github.com/lalternativefabrique/sklp/restore/objstore"
	"github.com/lalternativefabrique/sklp/restore/postgres"
	"github.com/lalternativefabrique/sklp/restore/proc"
	"github.com/lalternativefabrique/sklp/restore/report"
)

// The verdict is delivered once printed: the node reads it back from the
// job's log, so a failed drill exits 0 and the node never re-runs it.
const (
	exitDelivered = 0
	exitMisconfig = 3
)

// RunDrill restores one store from the environment the node composed and
// prints the report; the node reads it back from the job's log.
func RunDrill() int {
	env := drillEnv{}
	engineName := env.required("DRILL_ENGINE")
	space := env.fallback("DRILL_SPACE", os.Getenv("SKALPAI_SPACE"))
	maxRPO := env.duration("DRILL_MAX_RPO", 26*time.Hour)
	timeout := env.duration("DRILL_TIMEOUT", 2*time.Hour)
	scratch := env.fallback("DRILL_SCRATCH", "/scratch")
	endpoint, apiKey := os.Getenv("SKALPAI_ENDPOINT"), os.Getenv("SKALPAI_API_KEY")
	s3 := objstore.S3Config{
		Endpoint:  env.required("S3_ENDPOINT"),
		Region:    env.fallback("S3_REGION", "us-east-1"),
		Bucket:    env.required("S3_BUCKET"),
		AccessKey: env.required("S3_ACCESS_KEY"),
		SecretKey: env.required("S3_SECRET_KEY"),
	}

	var eng drill.Engine
	switch engineName {
	case "postgres":
		id, err := proc.Lookup("postgres")
		if err != nil {
			env.errs = append(env.errs, err.Error())
		}
		eng = postgres.New(postgres.Config{
			Stanza:     env.required("PG_STANZA"),
			Repo:       postgres.Repo{Endpoint: strings.TrimPrefix(strings.TrimPrefix(s3.Endpoint, "https://"), "http://"), Region: s3.Region, Bucket: s3.Bucket, Path: env.fallback("PG_REPO_PATH", "/pgbackrest"), AccessKey: s3.AccessKey, SecretKey: s3.SecretKey},
			User:       env.fallback("PG_USER", "postgres"),
			Scratch:    scratch,
			ProcessMax: env.integer("PG_PROCESS_MAX", 0),
			Replay:     env.duration("PG_REPLAY_TIMEOUT", time.Hour),
			KeepData:   env.fallback("PG_KEEP_DATA", "true") == "true",
			Identity:   id,
		})
	case "nats":
		store, err := objstore.NewS3(s3)
		if err != nil {
			env.errs = append(env.errs, err.Error())
		}
		eng = natsjs.New(natsjs.Config{Store: store, Bucket: s3.Bucket, Prefix: os.Getenv("NATS_PREFIX"), Scratch: scratch})
	default:
		env.errs = append(env.errs, fmt.Sprintf("DRILL_ENGINE=%q: supported engines are postgres and nats", engineName))
	}
	if space == "" {
		env.errs = append(env.errs, "DRILL_SPACE (or SKALPAI_SPACE) is required: the report is filed under it")
	}
	if len(env.errs) > 0 {
		fmt.Fprintln(os.Stderr, "restore-drill: invalid configuration:\n  "+strings.Join(env.errs, "\n  "))
		return exitMisconfig
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	drillCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rep := drill.Run(drillCtx, eng, drill.Options{Space: space, MaxRPO: maxRPO})
	rep.Render(os.Stdout)
	if raw, err := json.Marshal(rep); err == nil {
		fmt.Printf("%s%s\n", drill.ReportMarker, raw)
	}
	fmt.Println("restore-drill:", rep.Summary())

	if endpoint != "" && apiKey != "" {
		sendCtx, cancelSend := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancelSend()
		if err := (report.Client{Endpoint: endpoint, APIKey: apiKey, Attempts: 5, Backoff: 2 * time.Second}).Send(sendCtx, rep); err != nil {
			fmt.Fprintln(os.Stderr, "restore-drill: Skalpai not notified:", err)
		}
	}
	return exitDelivered
}

type drillEnv struct{ errs []string }

func (e *drillEnv) required(key string) string {
	v := os.Getenv(key)
	if v == "" {
		e.errs = append(e.errs, key+" is required")
	}
	return v
}

func (e *drillEnv) fallback(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (e *drillEnv) duration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Sprintf("%s=%q: %v", key, v, err))
	}
	return d
}

func (e *drillEnv) integer(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Sprintf("%s=%q: %v", key, v, err))
	}
	return n
}
