package spacemanager

import "testing"

func lalterLikeSpace() []Service {
	return []Service{
		{Name: "postgres", Image: "rg.fr-par.scw.cloud/platform-ops/postgres-pgbackrest-pgvector:20260923-0744-052cb8cc8525",
			Volumes: []string{"pgdata:/var/lib/postgresql/data", "backup:/backup"},
			Env:     []string{"POSTGRES_USER=app", "PGBACKREST_STANZA=altfab", "PGBACKREST_REPO1_S3_BUCKET=altfab-prod-backups"}},
		{Name: "pgbackrest", Image: "rg.fr-par.scw.cloud/platform-ops/pgbackrest-sidecar-pgvector:20260923-0744-052cb8cc8525",
			Volumes: []string{"pgdata:/var/lib/postgresql/data"}},
		{Name: "nats", Image: "nats:2.12-alpine", Volumes: []string{"nats-data:/data"},
			Backup: &ServiceBackup{Schedule: "15 3 * * *", Command: []string{"sh", "-c", "tar czf … && aws s3 cp …"}}},
		{Name: "core", Image: "rg.fr-par.scw.cloud/altfab/core:1"},
		{Name: "migrate", Image: "rg.fr-par.scw.cloud/altfab/migrate:1", Oneshot: true, Volumes: []string{"pgdata:/x"}},
	}
}

func storesByService(stores []DataStore) map[string]DataStore {
	m := map[string]DataStore{}
	for _, s := range stores {
		m[s.Service] = s
	}
	return m
}

func TestDataStoresGradesALalterLikeSpace(t *testing.T) {
	got := storesByService(DataStores(lalterLikeSpace()))
	if len(got) != 2 {
		t.Fatalf("stores = %v, want postgres and nats only (no sidecar, no job, no stateless service)", got)
	}
	pg := got["postgres"]
	if pg.Engine != EnginePostgres || pg.Protection != ProtectionBacked || pg.Detail != "pgbackrest stanza altfab → s3://altfab-prod-backups" {
		t.Fatalf("postgres = %+v", pg)
	}
	nats := got["nats"]
	if nats.Engine != EngineNATS || nats.Protection != ProtectionOpaque {
		t.Fatalf("nats = %+v", nats)
	}
}

func TestDataStoresFindsPgbackrestOnTheSidecarAlone(t *testing.T) {
	space := []Service{
		{Name: "db", Image: "postgres:16-alpine", Volumes: []string{"pgdata:/var/lib/postgresql/data"}},
		{Name: "pgbackrest", Image: "example/pgbackrest-sidecar:1", Volumes: []string{"pgdata:/var/lib/postgresql/data"},
			Env: []string{"PGBACKREST_STANZA=shop", "PGBACKREST_REPO1_S3_BUCKET=shop-backups"}},
	}
	if db := storesByService(DataStores(space))["db"]; db.Protection != ProtectionBacked {
		t.Fatalf("db = %+v, want backed through the sidecar sharing its volume", db)
	}
}

func TestDataStoresFlagsUnprotectedStores(t *testing.T) {
	space := []Service{
		{Name: "cache", Image: "redis:7-alpine", Volumes: []string{"redis-data:/data"}},
		{Name: "mail", Image: "stalwartlabs/stalwart:v0.13", Volumes: []string{"mail-data:/opt/stalwart"}},
		{Name: "legacy", Image: "mysql:8", Volumes: []string{"/srv/mysql:/var/lib/mysql"}},
	}
	got := storesByService(DataStores(space))
	if c := got["cache"]; c.Engine != EngineRedis || c.Protection != ProtectionNone {
		t.Fatalf("cache = %+v", c)
	}
	if m := got["mail"]; m.Engine != EngineUnknown || m.Protection != ProtectionNone {
		t.Fatalf("mail = %+v: data in a volume is a store even when the engine is unknown", m)
	}
	if _, ok := got["legacy"]; ok {
		t.Fatal("a host bind is the operator's to back up, not a store sklp speaks for")
	}
}

func TestEngineOfReadsTheRepositoryNotTheRegistry(t *testing.T) {
	for image, want := range map[string]DataEngine{
		"rg.fr-par.scw.cloud/platform-ops/postgres-pgbackrest-pg16:20260923": EnginePostgres,
		"timescale/timescaledb-ha:pg17":                                      EnginePostgres,
		"valkey/valkey:8":                                                    EngineRedis,
		"mariadb:11@sha256:abc":                                              EngineMariaDB,
		"bitnami/mongodb:7":                                                  EngineMongo,
		"registry.example:5000/team/api:1":                                   EngineUnknown,
		"rg.fr-par.scw.cloud/redis-team/api:1":                               EngineUnknown,
	} {
		if got := engineOf(image); got != want {
			t.Errorf("engineOf(%q) = %s, want %s", image, got, want)
		}
	}
}

func TestDataStoresCountsAScheduledBackupJobAimedAtTheStore(t *testing.T) {
	space := []Service{
		{Name: "nats", Image: "nats:2.12-alpine", Volumes: []string{"nats-data:/data"}},
		{Name: "nats-backup", Image: "rg.fr-par.scw.cloud/skalpai/platform/nats-backup:1", Oneshot: true, Schedule: "45 */6 * * *",
			Env: []string{"NATS_URL=nats://backup:pw@nats:4222"}},
		{Name: "migrate", Image: "app/migrate:1", Oneshot: true, Schedule: "0 * * * *", Env: []string{"DB=postgres://nats:4222"}},
	}
	got := storesByService(DataStores(space))["nats"]
	if got.Protection != ProtectionOpaque || got.Detail != "job nats-backup backs it up on schedule 45 */6 * * *, but the platform cannot read what it produces" {
		t.Fatalf("nats = %+v", got)
	}
}

func TestDataStoresMarksCachesDisposable(t *testing.T) {
	space := []Service{
		{Name: "tts", Image: "waves/tts:1", Volumes: []string{"tts-hf-cache:/home/tts/.cache/huggingface"}},
		{Name: "search", Image: "searxng/searxng:1", Volumes: []string{"searxng-config:/etc/searxng", "searxng-cache:/var/cache"}},
	}
	got := storesByService(DataStores(space))
	if got["tts"].Protection != ProtectionDisposable {
		t.Fatalf("tts = %+v", got["tts"])
	}
	if got["search"].Protection != ProtectionNone {
		t.Fatalf("search = %+v: one real volume among caches keeps the store a store", got["search"])
	}
}
