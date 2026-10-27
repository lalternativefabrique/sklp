package spacemanager

import (
	"net/url"
	"path"
	"slices"
	"strings"
)

type DataEngine string

const (
	EnginePostgres DataEngine = "postgres"
	EngineRedis    DataEngine = "redis"
	EngineMySQL    DataEngine = "mysql"
	EngineMariaDB  DataEngine = "mariadb"
	EngineMongo    DataEngine = "mongo"
	EngineNATS     DataEngine = "nats"
	EngineUnknown  DataEngine = "unknown"
)

// Protection grades what the platform can do for a store's data if the node
// is lost: nothing, run a backup it cannot read back, or read it back itself.
type Protection string

const (
	ProtectionNone       Protection = "none"
	ProtectionOpaque     Protection = "opaque"
	ProtectionBacked     Protection = "backed"
	ProtectionDisposable Protection = "disposable"
)

type DataStore struct {
	Service    string     `json:"service"`
	Engine     DataEngine `json:"engine"`
	Image      string     `json:"image"`
	Volumes    []string   `json:"volumes"`
	Protection Protection `json:"protection"`
	Detail     string     `json:"detail"`
}

// engineMarkers are matched against the image's repository name, most
// specific first: a pgvector or timescale image is a Postgres.
var engineMarkers = []struct {
	engine  DataEngine
	markers []string
}{
	{EnginePostgres, []string{"postgres", "pgvector", "timescale", "postgis"}},
	{EngineRedis, []string{"redis", "valkey", "keydb", "dragonfly"}},
	{EngineMariaDB, []string{"mariadb"}},
	{EngineMySQL, []string{"mysql", "percona"}},
	{EngineMongo, []string{"mongo"}},
	{EngineNATS, []string{"nats"}},
}

func engineOf(image string) DataEngine {
	repo := imageRepoName(image)
	for _, e := range engineMarkers {
		for _, m := range e.markers {
			if strings.Contains(repo, m) {
				return e.engine
			}
		}
	}
	return EngineUnknown
}

func imageRepoName(image string) string {
	ref, _, _ := strings.Cut(image, "@")
	if slash, colon := strings.LastIndex(ref, "/"), strings.LastIndex(ref, ":"); colon > slash {
		ref = ref[:colon]
	}
	return strings.ToLower(path.Base(ref))
}

// isBackupCompanion spots a service that only exists to back another one up
// (the pgbackrest sidecar mounts the database volume too): it holds no data
// of its own and must not be counted as a store.
func isBackupCompanion(svc Service) bool {
	repo := imageRepoName(svc.Image)
	return strings.Contains(repo, "sidecar") || strings.HasPrefix(repo, "pgbackrest")
}

// DataStores lists every long-running service that keeps data in a named
// volume and grades how well that data is protected.
func DataStores(services []Service) []DataStore {
	var stores []DataStore
	for _, svc := range services {
		if svc.Oneshot || isBackupCompanion(svc) {
			continue
		}
		var volumes []string
		for _, v := range svc.Volumes {
			if isNamedVolume(v) {
				volumes = append(volumes, volumeName(v))
			}
		}
		if len(volumes) == 0 {
			continue
		}
		store := DataStore{Service: svc.Name, Engine: engineOf(svc.Image), Image: svc.Image, Volumes: volumes}
		store.Protection, store.Detail = protection(svc, store, services)
		stores = append(stores, store)
	}
	return stores
}

func protection(svc Service, store DataStore, services []Service) (Protection, string) {
	if disposable(store.Volumes) {
		return ProtectionDisposable, "its volumes are named as caches: losing them loses nothing"
	}
	if store.Engine == EnginePostgres {
		if detail, ok := pgbackrestCoverage(svc, store.Volumes, services); ok {
			return ProtectionBacked, detail
		}
	}
	if store.Engine == EngineNATS {
		if dest, ok := declaredBackupDestination(svc); ok {
			return ProtectionBacked, "JetStream store archives → " + dest.String()
		}
	}
	if svc.Backup != nil {
		return ProtectionOpaque, "a backup command runs on schedule " + svc.Backup.Schedule + ", but the platform cannot read what it produces"
	}
	if job, ok := backupJobFor(svc, store.Volumes, services); ok {
		return ProtectionOpaque, "job " + job.Name + " backs it up on schedule " + job.Schedule + ", but the platform cannot read what it produces"
	}
	return ProtectionNone, "nothing backs this data up"
}

var disposableMarkers = []string{"cache", "tmp", "scratch"}

func disposable(volumes []string) bool {
	for _, v := range volumes {
		if !slices.ContainsFunc(disposableMarkers, func(m string) bool { return strings.Contains(strings.ToLower(v), m) }) {
			return false
		}
	}
	return true
}

// backupJobFor finds a scheduled backup job aimed at the store: one that
// mounts its volume, or whose environment addresses it by service name
// (nats://nats:4222, postgres://u:p@db:5432).
func backupJobFor(store Service, volumes []string, services []Service) (Service, bool) {
	for _, job := range services {
		if !job.Oneshot || job.Schedule == "" || !strings.Contains(imageRepoName(job.Image), "backup") {
			continue
		}
		if sharesVolume(job, volumes) {
			return job, true
		}
		for _, kv := range job.Env {
			_, v, _ := strings.Cut(kv, "=")
			if strings.Contains(v, "//"+store.Name+":") || strings.Contains(v, "@"+store.Name+":") {
				return job, true
			}
		}
	}
	return Service{}, false
}

// pgbackrestCoverage finds a pgbackrest configuration shipping this database:
// on the database service itself or on a companion mounting its volume.
func pgbackrestCoverage(db Service, volumes []string, services []Service) (string, bool) {
	for _, svc := range services {
		if svc.Name != db.Name && !sharesVolume(svc, volumes) {
			continue
		}
		env := envMap(svc.Env)
		stanza, bucket := env["PGBACKREST_STANZA"], env["PGBACKREST_REPO1_S3_BUCKET"]
		if stanza != "" && bucket != "" {
			return "pgbackrest stanza " + stanza + " → s3://" + bucket, true
		}
	}
	return "", false
}

func sharesVolume(svc Service, volumes []string) bool {
	for _, v := range svc.Volumes {
		if isNamedVolume(v) && slices.Contains(volumes, volumeName(v)) {
			return true
		}
	}
	return false
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// BackupToEnv names where a store's archives land, as an S3 URL in path style
// (https://<endpoint>/<bucket>/<prefix>/). Declaring it is what lets the
// platform read a backup back; the credentials stay the AWS_* the backup
// command already uses.
const BackupToEnv = "SKLP_BACKUP_TO"

type backupDestination struct {
	Endpoint, Bucket, Prefix string
}

func (d backupDestination) String() string {
	return d.Endpoint + "/" + d.Bucket + "/" + d.Prefix
}

func declaredBackupDestination(svc Service) (backupDestination, bool) {
	raw := envMap(svc.Env)[BackupToEnv]
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return backupDestination{}, false
	}
	bucket, prefix, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if bucket == "" {
		return backupDestination{}, false
	}
	return backupDestination{Endpoint: u.Scheme + "://" + u.Host, Bucket: bucket, Prefix: prefix}, true
}
