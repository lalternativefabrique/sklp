package devstack

import (
	"slices"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline"
)

func TestExpandMuxURLs(t *testing.T) {
	services := []NamedService{
		{Name: "rustfs", Def: pipeline.Service{Port: 9000}},
		{Name: "web", Def: pipeline.Service{Ready: &pipeline.Ready{HTTP: "http://web:5273/"}}},
		{Name: "postgres", Def: pipeline.Service{}},
	}
	got, err := expandMuxURLs([]string{
		"S3_PUBLIC_ENDPOINT=${mux:rustfs}",
		"PUBLIC_WEB_URL=${mux:web}/app",
		"DATABASE_URL=postgres://postgres:5432/app",
	}, "abc123", 8328, services)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"S3_PUBLIC_ENDPOINT=http://rustfs.abc123.localhost:8328",
		"PUBLIC_WEB_URL=http://web.abc123.localhost:8328/app",
		"DATABASE_URL=postgres://postgres:5432/app",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	for _, c := range []struct {
		name, env string
		mux       int
		wantErr   string
	}{
		{"unrouted service", "X=${mux:postgres}", 8328, "no `port:`"},
		{"unknown service", "X=${mux:redis}", 8328, "no `port:`"},
		{"mux unavailable", "X=${mux:web}", 0, "mux is unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := expandMuxURLs([]string{c.env}, "abc123", c.mux, services)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}
