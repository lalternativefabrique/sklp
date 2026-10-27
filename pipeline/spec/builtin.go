package spec

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

const builtinPrefix = "sklp:"

//go:embed builtin/*.yaml
var builtinFS embed.FS

func builtinNames() []string {
	entries, _ := fs.ReadDir(builtinFS, "builtin")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(names)
	return names
}

func readBuiltin(ref string) ([]byte, error) {
	name := strings.TrimPrefix(ref, builtinPrefix)
	raw, err := builtinFS.ReadFile("builtin/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("unknown built-in pipeline %q (available: %v)", ref, builtinNames())
	}
	return raw, nil
}

// Builtin returns the embedded pipeline a repo runs when it declares no
// .sklp/pipelines/<name>.yaml of its own.
func Builtin(name string) ([]byte, bool) {
	raw, err := builtinFS.ReadFile("builtin/" + name + ".yaml")
	return raw, err == nil
}
