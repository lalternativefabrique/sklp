package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPipelineRefusesAnUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	for v, wantErr := range map[string]bool{"1": false, "2": true} {
		path := filepath.Join(dir, "v"+v+".yaml")
		_ = os.WriteFile(path, []byte("version: "+v+"\nkind: pipeline\nname: ci\nsteps: []\n"), 0o644)
		_, err := LoadPipeline(path)
		if (err != nil) != wantErr || (wantErr && !strings.Contains(err.Error(), "only pipeline version")) {
			t.Fatalf("version %s: err = %v", v, err)
		}
	}
}
