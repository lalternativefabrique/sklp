package imagebuild

import "testing"

// the real acme core Dockerfile (multi-stage go build, secret, COPY --from).
const synthizCoreDockerfile = `FROM golang:1.25 AS builder
WORKDIR /src
ENV GONOSUMCHECK=github.com/digstack/skalpai
COPY lib/transcribe/ ./lib/transcribe/
COPY apps/core/go.mod apps/core/go.sum ./apps/core/
WORKDIR /src/apps/core
RUN --mount=type=secret,id=github_token \
    echo "machine github.com login oauth2 password $(cat /run/secrets/github_token)" > /root/.netrc && \
    go mod download && \
    rm /root/.netrc
COPY apps/core/ .
RUN CGO_ENABLED=0 go build -o /out/core .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates ffmpeg
RUN adduser -D -u 1000 appuser
COPY --from=builder /out/core /app/core
WORKDIR /app
USER appuser
EXPOSE 4000
ENTRYPOINT ["/app/core"]
`

func TestParse_SynthizCore_TwoStages(t *testing.T) {
	df, err := Parse(synthizCoreDockerfile)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(df.Stages) != 2 {
		t.Fatalf("got %d stages, want 2", len(df.Stages))
	}
	if df.Stages[0].Name != "builder" || df.Stages[0].BaseImage != "golang:1.25" {
		t.Errorf("stage 0 = name=%q base=%q, want builder/golang:1.25", df.Stages[0].Name, df.Stages[0].BaseImage)
	}
	if df.Stages[1].BaseImage != "alpine:3.20" {
		t.Errorf("stage 1 base = %q, want alpine:3.20", df.Stages[1].BaseImage)
	}
	// Target is the alpine stage.
	if df.Target().BaseImage != "alpine:3.20" {
		t.Errorf("target base = %q, want alpine:3.20", df.Target().BaseImage)
	}
}

// TestParse_CopyFrom resolves COPY --from=builder to the builder stage.
func TestParse_CopyFrom(t *testing.T) {
	df, _ := Parse(synthizCoreDockerfile)
	// Find the COPY --from in the final stage.
	var copyFrom *CopySpec
	for _, in := range df.Target().Instructions {
		if in.Op == OpCopy {
			c, err := ParseCopy(in.Args)
			if err != nil {
				t.Fatalf("parseCopy: %v", err)
			}
			if c.From != "" {
				copyFrom = &c
			}
		}
	}
	if copyFrom == nil {
		t.Fatal("no COPY --from found in final stage")
	}
	if copyFrom.From != "builder" {
		t.Errorf("COPY --from = %q, want builder", copyFrom.From)
	}
	st, ok := df.stageByName(copyFrom.From)
	if !ok || st.Name != "builder" {
		t.Errorf("stageByName(builder) failed: ok=%v", ok)
	}
	if copyFrom.Srcs[0] != "/out/core" || copyFrom.Dst != "/app/core" {
		t.Errorf("COPY --from srcs/dst = %v -> %q, want /out/core -> /app/core", copyFrom.Srcs, copyFrom.Dst)
	}
}

// TestParse_RunSecret peels the --mount=type=secret flag and keeps the command.
func TestParse_RunSecret(t *testing.T) {
	df, _ := Parse(synthizCoreDockerfile)
	var found bool
	for _, in := range df.Stages[0].Instructions {
		if in.Op == OpRun {
			r, err := ParseRun(in.Args)
			if err != nil {
				t.Fatalf("parseRun: %v", err)
			}
			if len(r.Secrets) > 0 {
				found = true
				if r.Secrets[0].ID != "github_token" {
					t.Errorf("secret id = %q, want github_token", r.Secrets[0].ID)
				}
				if r.Secrets[0].Target != "/run/secrets/github_token" {
					t.Errorf("secret target = %q", r.Secrets[0].Target)
				}
				if !contains(r.Cmd, "go mod download") {
					t.Errorf("RUN cmd lost the command body: %q", r.Cmd)
				}
			}
		}
	}
	if !found {
		t.Fatal("no RUN --mount=type=secret found")
	}
}

// TestParse_RejectsUnsupported: ADD / HEALTHCHECK must fail loudly.
func TestParse_RejectsUnsupported(t *testing.T) {
	for _, df := range []string{
		"FROM alpine\nADD http://x/y /y\n",
		"FROM alpine\nHEALTHCHECK CMD true\n",
		"FROM alpine\nVOLUME /data\n",
	} {
		if _, err := Parse(df); err == nil {
			t.Errorf("expected rejection for:\n%s", df)
		}
	}
}

// TestParse_RequiresFrom.
func TestParse_RequiresFrom(t *testing.T) {
	if _, err := Parse("RUN echo hi\n"); err == nil {
		t.Fatal("expected error when no FROM")
	}
}

// TestParseExec covers both ENTRYPOINT forms.
func TestParseExec(t *testing.T) {
	if got := ParseExec(`["/app/core"]`); len(got) != 1 || got[0] != "/app/core" {
		t.Errorf("exec form = %v", got)
	}
	if got := ParseExec(`node server.js`); len(got) != 3 || got[0] != "/bin/sh" {
		t.Errorf("shell form = %v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
