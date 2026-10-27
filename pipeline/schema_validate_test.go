package pipeline

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadPipeline_ServicesValidation covers the cross-field checks
// validatePipeline runs after `extends:` resolution: unknown service
// references, bind-mount volumes, and the happy path where a step
// declares a service that exists.
func TestLoadPipeline_ServicesValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string // substring; "" means must succeed
	}{
		{
			name: "happy_path_step_with_known_service",
			yaml: `
version: 1
kind: pipeline
services:
  postgres:
    image: postgres:16-alpine
    ports: ["5432:5432"]
steps:
  - name: integration
    run: go test ./...
    services: [postgres]
`,
		},
		{
			name: "no_services_block_works",
			yaml: `
version: 1
kind: pipeline
steps:
  - name: unit
    run: go test ./...
`,
		},
		{
			name: "step_references_unknown_service",
			yaml: `
version: 1
kind: pipeline
services:
  postgres:
    image: postgres:16-alpine
steps:
  - name: integration
    run: go test ./...
    services: [redis]
`,
			wantErr: `references unknown service "redis"`,
		},
		{
			name: "bind_mount_volume_rejected",
			yaml: `
version: 1
kind: pipeline
services:
  data:
    image: busybox
    volumes: ["./host:/data"]
steps:
  - name: run
    run: 'true'
`,
			wantErr: "bind mounts are not supported",
		},
		{
			name: "absolute_bind_mount_volume_rejected",
			yaml: `
version: 1
kind: pipeline
services:
  data:
    image: busybox
    volumes: ["/var/data:/data"]
steps:
  - name: run
    run: 'true'
`,
			wantErr: "bind mounts are not supported",
		},
		{
			name: "named_volume_accepted",
			yaml: `
version: 1
kind: pipeline
services:
  pg:
    image: postgres:16-alpine
    volumes: ["pg-data:/var/lib/postgresql/data"]
steps:
  - name: run
    run: 'true'
    services: [pg]
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"ci.yaml": tc.yaml})
			_, err := LoadPipeline(filepath.Join(dir, "ci.yaml"))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
