package spacemanager

import (
	"strings"
	"testing"
)

// parseBackup is a small helper: parse a deploy.yaml body, expecting success.
func parseBackup(t *testing.T, body string) *DeployConfig {
	t.Helper()
	cfg, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func TestBackup_ParsedOntoService(t *testing.T) {
	cfg := parseBackup(t, `
services:
  db:
    image: postgres:16
    volumes: ["pgdata:/var/lib/postgresql/data", "backups:/backups"]
    backup:
      command: ["sh", "-c", "pg_dump -Fc app > /backups/db.dump"]
      schedule: "0 3 * * *"
`)
	spec := cfg.Services["db"]
	if spec.Backup == nil {
		t.Fatal("backup block not parsed")
	}
	if spec.Backup.Schedule != "0 3 * * *" {
		t.Errorf("schedule: got %q", spec.Backup.Schedule)
	}
	if len(spec.Backup.Command) != 3 {
		t.Errorf("command: got %v", spec.Backup.Command)
	}
	// Must be carried onto the runnable Service.
	svc := spec.toService("db")
	if svc.Backup == nil || svc.Backup.Schedule != "0 3 * * *" {
		t.Errorf("backup not carried onto Service: %+v", svc.Backup)
	}
}

func TestBackup_NoBlockIsValid(t *testing.T) {
	cfg := parseBackup(t, `
services:
  db:
    image: postgres:16
`)
	if cfg.Services["db"].Backup != nil {
		t.Error("absent backup should stay nil")
	}
}

func TestBackup_RequiresBothFields(t *testing.T) {
	cases := map[string]string{
		"missing schedule": `
services:
  db:
    image: postgres:16
    backup:
      command: ["pg_dump"]
`,
		"missing command": `
services:
  db:
    image: postgres:16
    backup:
      schedule: "0 3 * * *"
`,
		"empty block": `
services:
  db:
    image: postgres:16
    backup: {}
`,
		"bad cron": `
services:
  db:
    image: postgres:16
    backup:
      command: ["pg_dump"]
      schedule: "not a cron"
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(body))
			if err == nil {
				t.Fatalf("want validation error for %s", name)
			}
			if !strings.Contains(err.Error(), "backup") {
				t.Errorf("error should mention backup, got: %v", err)
			}
		})
	}
}

func TestJobSchedule_ParsedOntoJob(t *testing.T) {
	cfg := parseBackup(t, `
services:
  db:
    image: postgres:17-alpine
jobs:
  ship:
    image: amazon/aws-cli:2.28.9
    schedule: "*/15 * * * *"
    command: ["sh", "-c", "aws s3 sync /backups s3://bucket/"]
`)
	spec := cfg.Jobs["ship"]
	if spec.Schedule != "*/15 * * * *" {
		t.Errorf("schedule: got %q", spec.Schedule)
	}
	svc := spec.toRunnable("ship", true)
	if !svc.Oneshot {
		t.Error("a jobs: entry must resolve to Oneshot")
	}
	if svc.Schedule != "*/15 * * * *" {
		t.Errorf("schedule not carried onto the runnable Service: %q", svc.Schedule)
	}
}

func TestJobSchedule_RunOnceJobHasNoSchedule(t *testing.T) {
	cfg := parseBackup(t, `
services:
  db:
    image: postgres:17-alpine
jobs:
  migrate:
    image: migrate:1
`)
	if got := cfg.Jobs["migrate"].toRunnable("migrate", true).Schedule; got != "" {
		t.Errorf("a job without a schedule must stay run-once, got %q", got)
	}
}

func TestJobSchedule_Rejected(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"on a service": {`
services:
  web:
    image: nginx
    schedule: "0 3 * * *"
`, "only valid on a job"},
		"bad cron": {`
services:
  db:
    image: postgres:17-alpine
jobs:
  ship:
    image: aws-cli
    schedule: "every 15 minutes"
`, "want 5 fields"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
