package plan

import "testing"

func TestNativeBuildConfigForwardsBuildArgs(t *testing.T) {
	spec := &PublishSpec{
		Image:      "rg.example/platform-ops/postgres-pgbackrest-pgvector",
		Tag:        "t",
		Context:    "images",
		Dockerfile: "images/Dockerfile.postgres",
		BuildArgs:  map[string]string{"BASE_IMAGE": "pgvector/pgvector:pg17"},
	}
	cfg := nativeBuildConfig("/repo", spec)
	if got := cfg.BuildArgs["BASE_IMAGE"]; got != "pgvector/pgvector:pg17" {
		t.Fatalf("BuildArgs not forwarded to the native builder: got %q", got)
	}
	if cfg.Dockerfile != "/repo/images/Dockerfile.postgres" || cfg.ContextDir != "/repo/images" {
		t.Fatalf("paths: %q %q", cfg.Dockerfile, cfg.ContextDir)
	}
}
