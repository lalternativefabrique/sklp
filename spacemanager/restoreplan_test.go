package spacemanager

import (
	"slices"
	"strings"
	"testing"
)

func lalterProduction() []Service {
	return []Service{
		{Name: "postgres", Image: "rg.fr-par.scw.cloud/platform-ops/postgres-pgbackrest-pgvector:20260923-0744",
			Volumes: []string{"pgdata:/var/lib/postgresql/data"},
			Env: []string{"POSTGRES_USER=app", "PGBACKREST_STANZA=altfab", "PGBACKREST_REPO1_S3_BUCKET=altfab-prod-backups",
				"PGBACKREST_REPO1_S3_KEY=AK", "PGBACKREST_REPO1_S3_KEY_SECRET=SK"}},
		{Name: "pgbackrest", Image: "rg.fr-par.scw.cloud/platform-ops/pgbackrest-sidecar-pgvector:20260923-0744",
			Volumes: []string{"pgdata:/var/lib/postgresql/data"},
			Env: []string{"PGBACKREST_STANZA=altfab", "PGBACKREST_REPO1_S3_BUCKET=altfab-prod-backups",
				"PGBACKREST_REPO1_S3_ENDPOINT=s3.gra.io.cloud.ovh.net", "PGBACKREST_REPO1_S3_REGION=gra"}},
		{Name: "nats", Image: "nats:2.12-alpine", Volumes: []string{"nats-data:/data"}, Backup: &ServiceBackup{Schedule: "15 3 * * *"}},
		{Name: "cache", Image: "redis:7", Volumes: []string{"redis:/data"}},
	}
}

func TestPlanRestoresDerivesADrillFromTheProductionSpace(t *testing.T) {
	jobs, targets := PlanRestores("altfab-production", lalterProduction(), "/home/architect/.local/bin/sklet")
	if len(jobs) != 1 || len(targets) != 1 {
		t.Fatalf("jobs = %+v: only the pgbackrest-backed postgres can be read back", jobs)
	}
	job := jobs[0]
	if job.Name != "drill-postgres" || job.Image != lalterProduction()[0].Image || !job.Oneshot || job.Schedule == "" {
		t.Fatalf("job = %+v", job)
	}
	if !slices.Equal(job.Command, []string{RestoreSkletPath, "drill"}) {
		t.Fatalf("command = %v", job.Command)
	}
	if env := envMap(job.Env); env["PGBACKREST_STANZA"] != "altfab" || env["PGBACKREST_REPO1_S3_ENDPOINT"] != "s3.gra.io.cloud.ovh.net" {
		t.Fatalf("env = %v: the production image's entrypoint needs the pgbackrest settings as they are", job.Env)
	}
	if got := UnbackedVolumes([]Service{job}); len(got) != 0 {
		t.Fatalf("the drill scratch volume was reported as unprotected data: %v", got)
	}
	if !slices.Contains(job.Volumes, "/home/architect/.local/bin/sklet:"+RestoreSkletPath+":ro") {
		t.Fatalf("volumes = %v: the node's sklet must be mounted read-only", job.Volumes)
	}
	env := envMap(job.Env)
	want := map[string]string{
		"DRILL_ENGINE": "postgres", "DRILL_SPACE": "altfab-production", "PG_STANZA": "altfab", "PG_USER": "app",
		"S3_ENDPOINT": "https://s3.gra.io.cloud.ovh.net", "S3_REGION": "gra", "S3_BUCKET": "altfab-prod-backups",
		"S3_ACCESS_KEY": "AK", "S3_SECRET_KEY": "SK", "PG_REPO_PATH": "/pgbackrest",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q (merged from the database and its sidecar)", k, env[k], v)
		}
	}
	if targets[0] != (RestoreTarget{Space: "altfab-production", Service: "postgres", Engine: EnginePostgres, Job: "drill-postgres"}) {
		t.Fatalf("target = %+v", targets[0])
	}
}

func TestPlanRestoresCoversEveryDatabaseOfASpace(t *testing.T) {
	space := append(lalterProduction(), Service{Name: "techtuel-postgres", Image: "postgres:17-alpine",
		Volumes: []string{"techtuel-pgdata:/var/lib/postgresql/data"},
		Env:     []string{"PGBACKREST_STANZA=techtuel", "PGBACKREST_REPO1_S3_BUCKET=techtuel-prod-backups"}})
	jobs, _ := PlanRestores("acme-production", space, "/x/sklet")
	if len(jobs) != 2 || jobs[1].Name != "drill-techtuel-postgres" || envMap(jobs[1].Env)["PG_STANZA"] != "techtuel" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Schedule == jobs[1].Schedule {
		t.Log("two stores may share a slot; the node still runs them in sequence")
	}
}

func TestRestoreScheduleIsStableAndAfterTheBackups(t *testing.T) {
	a, b := restoreSchedule("altfab-production", "postgres"), restoreSchedule("altfab-production", "postgres")
	if a != b {
		t.Fatalf("schedule moved between calls: %s vs %s", a, b)
	}
	for _, s := range []string{a, restoreSchedule("x", "y"), restoreSchedule("acme-production", "techtuel-postgres")} {
		f := strings.Fields(s)
		if f[1] != "4" && f[1] != "5" {
			t.Fatalf("schedule %q is outside 04:00–05:59", s)
		}
		if _, err := ParseCron(s); err != nil {
			t.Fatal(err)
		}
	}
}

func natsWithDestination(dest string) Service {
	return Service{Name: "nats", Image: "nats:2.12-alpine", Volumes: []string{"nats-data:/data"},
		Backup: &ServiceBackup{Schedule: "15 3 * * *", Command: []string{"sh", "-c", "tar … && aws s3 cp …"}},
		Env:    []string{"SKLP_BACKUP_TO=" + dest, "AWS_ACCESS_KEY_ID=AK", "AWS_SECRET_ACCESS_KEY=SK", "AWS_DEFAULT_REGION=gra"}}
}

func TestPlanRestoresDrillsANATSStoreThatDeclaresWhereItsArchivesGo(t *testing.T) {
	space := []Service{natsWithDestination("https://s3.gra.io.cloud.ovh.net/tornad-prod-backups/nats/")}
	stores := DataStores(space)
	if len(stores) != 1 || stores[0].Protection != ProtectionBacked {
		t.Fatalf("stores = %+v: a declared destination makes the store readable back", stores)
	}
	jobs, targets := PlanRestores("tornad-production", space, "/x/sklet")
	if len(jobs) != 1 || targets[0].Engine != EngineNATS || jobs[0].Image != "nats:2.12-alpine" {
		t.Fatalf("jobs = %+v", jobs)
	}
	env := envMap(jobs[0].Env)
	for k, v := range map[string]string{
		"DRILL_ENGINE": "nats", "S3_ENDPOINT": "https://s3.gra.io.cloud.ovh.net", "S3_BUCKET": "tornad-prod-backups",
		"NATS_PREFIX": "nats/", "S3_REGION": "gra", "S3_ACCESS_KEY": "AK", "S3_SECRET_KEY": "SK",
	} {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

func TestPlanRestoresLeavesAnUndeclaredNATSBackupOpaque(t *testing.T) {
	for _, dest := range []string{"", "s3://bucket/nats/", "https://s3.example/"} {
		space := []Service{natsWithDestination(dest)}
		if got := DataStores(space)[0].Protection; got != ProtectionOpaque {
			t.Errorf("destination %q graded %s, want opaque", dest, got)
		}
		if jobs, _ := PlanRestores("s", space, "/x/sklet"); len(jobs) != 0 {
			t.Errorf("destination %q planned a drill", dest)
		}
	}
}

func TestPlanRestoresLetsEveryDrillReportToSkalpai(t *testing.T) {
	services := append(lalterProduction(), Service{Name: "core", Image: "core:1",
		Env: []string{"SKALPAI_ENDPOINT=https://api.skalpai.dev", "SKALPAI_API_KEY=sk_live"}})
	services[2].Env = []string{"SKLP_BACKUP_TO=https://s3.gra.io.cloud.ovh.net/altfab-prod-backups/nats", "AWS_ACCESS_KEY_ID=AK", "AWS_SECRET_ACCESS_KEY=SK"}
	jobs, _ := PlanRestores("altfab-production", services, "/sklet")
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v, want a postgres and a nats drill", jobs)
	}
	for _, job := range jobs {
		env := envMap(job.Env)
		if env["SKALPAI_ENDPOINT"] != "https://api.skalpai.dev" || env["SKALPAI_API_KEY"] != "sk_live" {
			t.Errorf("%s env = %v: the verdict never reaches the Health screen", job.Name, job.Env)
		}
	}
}

func TestPlanRestoresWithoutSkalpaiCredentialsStaysLocal(t *testing.T) {
	jobs, _ := PlanRestores("altfab-production", lalterProduction(), "/sklet")
	for _, kv := range jobs[0].Env {
		if strings.HasPrefix(kv, "SKALPAI_") {
			t.Fatalf("env = %v", jobs[0].Env)
		}
	}
}
