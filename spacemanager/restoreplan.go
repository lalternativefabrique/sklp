package spacemanager

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// RestoreSkletPath is where a drill job finds the node's own sklet: the node
// mounts it read-only into the production database image, so the drill runs
// with exactly the engine version and extensions production runs.
const RestoreSkletPath = "/opt/sklp/sklet"

type RestoreTarget struct {
	Space   string     `json:"space"`
	Service string     `json:"service"`
	Engine  DataEngine `json:"engine"`
	Job     string     `json:"job"`
}

func RestoreSpaceName(space string) string { return space + "-restore" }

func restoreJobName(service string) string { return "drill-" + service }

// PlanRestores derives one drill job per store the platform can read back.
// Nothing in it is declared: the backup destination, the credentials and the
// image all come from the production services themselves.
func PlanRestores(space string, services []Service, skletPath string) ([]Service, []RestoreTarget) {
	var jobs []Service
	var targets []RestoreTarget
	byName := map[string]Service{}
	for _, s := range services {
		byName[s.Name] = s
	}
	reporting := skalpaiReporting(services)
	for _, store := range DataStores(services) {
		if store.Protection != ProtectionBacked {
			continue
		}
		if store.Engine == EngineNATS {
			if job, ok := natsDrill(space, byName[store.Service], skletPath); ok {
				job.Env = append(job.Env, reporting...)
				jobs = append(jobs, job)
				targets = append(targets, RestoreTarget{Space: space, Service: store.Service, Engine: store.Engine, Job: job.Name})
			}
			continue
		}
		if store.Engine != EnginePostgres {
			continue
		}
		db := byName[store.Service]
		env := pgbackrestEnv(db, store.Volumes, services)
		job := Service{
			Name:     restoreJobName(db.Name),
			Image:    db.Image,
			Command:  []string{RestoreSkletPath, "drill"},
			Oneshot:  true,
			Schedule: restoreSchedule(space, db.Name),
			Volumes:  []string{restoreJobName(db.Name) + "-scratch:/scratch", skletPath + ":" + RestoreSkletPath + ":ro"},
			Env: append(append(passThrough(env), []string{
				"DRILL_ENGINE=postgres",
				"DRILL_SPACE=" + space,
				"PG_STANZA=" + env["PGBACKREST_STANZA"],
				"PG_USER=" + firstNonEmpty(envMap(db.Env)["POSTGRES_USER"], "postgres"),
				"PG_REPO_PATH=" + firstNonEmpty(env["PGBACKREST_REPO1_PATH"], "/pgbackrest"),
				"S3_ENDPOINT=" + withScheme(env["PGBACKREST_REPO1_S3_ENDPOINT"]),
				"S3_REGION=" + env["PGBACKREST_REPO1_S3_REGION"],
				"S3_BUCKET=" + env["PGBACKREST_REPO1_S3_BUCKET"],
				"S3_ACCESS_KEY=" + env["PGBACKREST_REPO1_S3_KEY"],
				"S3_SECRET_KEY=" + env["PGBACKREST_REPO1_S3_KEY_SECRET"],
			}...), reporting...),
			Resources: ServiceResources{CPUCores: 1, MemoryBytes: 1 << 30, PidsMax: defaultServicePidsMax},
		}
		jobs = append(jobs, job)
		targets = append(targets, RestoreTarget{Space: space, Service: db.Name, Engine: store.Engine, Job: job.Name})
	}
	return jobs, targets
}

// skalpaiReporting lends the drill the Skalpai credentials a production
// service already carries, so the verdict reaches the Health screen.
func skalpaiReporting(services []Service) []string {
	for _, svc := range services {
		env := envMap(svc.Env)
		if env["SKALPAI_ENDPOINT"] != "" && env["SKALPAI_API_KEY"] != "" {
			return []string{"SKALPAI_ENDPOINT=" + env["SKALPAI_ENDPOINT"], "SKALPAI_API_KEY=" + env["SKALPAI_API_KEY"]}
		}
	}
	return nil
}

// pgbackrestEnv merges the pgbackrest settings of the database and of every
// companion sharing its volume: the sidecar usually holds the endpoint while
// the database holds the stanza, and neither alone describes the repository.
func pgbackrestEnv(db Service, volumes []string, services []Service) map[string]string {
	out := map[string]string{}
	for _, svc := range append([]Service{db}, services...) {
		if svc.Name != db.Name && !sharesVolume(svc, volumes) {
			continue
		}
		for k, v := range envMap(svc.Env) {
			if strings.HasPrefix(k, "PGBACKREST_") && v != "" && out[k] == "" {
				out[k] = v
			}
		}
	}
	return out
}

// natsDrill restores the newest store archive into the production NATS image,
// whose nats-server loads it.
func natsDrill(space string, svc Service, skletPath string) (Service, bool) {
	dest, ok := declaredBackupDestination(svc)
	if !ok {
		return Service{}, false
	}
	env := envMap(svc.Env)
	return Service{
		Name:     restoreJobName(svc.Name),
		Image:    svc.Image,
		Command:  []string{RestoreSkletPath, "drill"},
		Oneshot:  true,
		Schedule: restoreSchedule(space, svc.Name),
		Volumes:  []string{restoreJobName(svc.Name) + "-scratch:/scratch", skletPath + ":" + RestoreSkletPath + ":ro"},
		Env: []string{
			"DRILL_ENGINE=nats",
			"DRILL_SPACE=" + space,
			"NATS_PREFIX=" + dest.Prefix,
			"S3_ENDPOINT=" + dest.Endpoint,
			"S3_REGION=" + firstNonEmpty(env["AWS_DEFAULT_REGION"], env["AWS_REGION"]),
			"S3_BUCKET=" + dest.Bucket,
			"S3_ACCESS_KEY=" + env["AWS_ACCESS_KEY_ID"],
			"S3_SECRET_KEY=" + env["AWS_SECRET_ACCESS_KEY"],
		},
		Resources: ServiceResources{CPUCores: 0.5, MemoryBytes: 512 << 20, PidsMax: defaultServicePidsMax},
	}, true
}

// passThrough hands the production pgbackrest settings to the drill as they
// are: the production image's entrypoint refuses to start without them.
func passThrough(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// restoreSchedule spreads the node's drills over 04:00–05:59, after the 03:00
// backups, with a slot fixed per store so a restart never moves it.
func restoreSchedule(space, service string) string {
	h := fnv.New32a()
	h.Write([]byte(space + "/" + service))
	slot := h.Sum32() % 120
	return fmt.Sprintf("%d %d * * *", slot%60, 4+slot/60)
}

func withScheme(endpoint string) string {
	if endpoint == "" || strings.Contains(endpoint, "://") {
		return endpoint
	}
	return "https://" + endpoint
}

func firstNonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
