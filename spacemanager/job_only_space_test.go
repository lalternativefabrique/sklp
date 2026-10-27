package spacemanager

import "testing"

func TestJobOnlySpaceResolvesToItsScheduledJob(t *testing.T) {
	cfg, err := Parse([]byte(`
version: 1
kind: deploy
name: restore
jobs:
  restore-check:
    image: rg.fr-par.scw.cloud/platform-ops/postgres-pgbackrest:20260923-1510-be49997204a8
    schedule: "0 5 * * *"
    volumes: ["restore-scratch:/restore"]
spaces:
  app-restore: {}
`))
	if err != nil {
		t.Fatalf("a restore space holding only jobs was refused: %v", err)
	}
	svcs, _, err := cfg.Resolve("app-restore", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	entries := scheduleEntries(svcs)
	if len(entries) != 1 || entries[0].service != "restore-check" || !entries[0].job {
		t.Fatalf("scheduled = %+v, want the restore-check job", entries)
	}
}

func TestSpaceWithNoWorkloadIsRefused(t *testing.T) {
	if _, err := Parse([]byte("version: 1\nkind: deploy\nname: empty\n")); err == nil {
		t.Fatal("a deploy file with neither services nor jobs was accepted")
	}
}
