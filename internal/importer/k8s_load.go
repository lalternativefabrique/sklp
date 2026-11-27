package importer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type k8sObject struct {
	apiVersion, kind, name, namespace string
	raw                               map[string]any
}

func (o k8sObject) ref() string { return o.kind + "/" + o.name }

func (o k8sObject) spec() map[string]any { return asMap(o.raw["spec"]) }

func (o k8sObject) group() string {
	g, _, ok := strings.Cut(o.apiVersion, "/")
	if !ok {
		return ""
	}
	return g
}

var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

func kustomizationFile(dir string) string {
	for _, n := range kustomizationNames {
		if isFile(filepath.Join(dir, n)) {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

var k8sDirNames = map[string]bool{"k8s": true, "kube": true, "kubernetes": true, "manifests": true, "deploy": true, "deployment": true}

// FindK8sSources picks what an operator would `kubectl apply -k` in
// production: the kustomizations nothing else includes, narrowed to the ones
// whose path names production when there are any. Without kustomize it falls
// back to the first conventional manifests directory.
func FindK8sSources(root string) []string {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "vendor" || strings.Count(rel, string(filepath.Separator)) > 5) {
			return filepath.SkipDir
		}
		if kustomizationFile(p) != "" {
			dirs = append(dirs, p)
		}
		return nil
	})
	if len(dirs) > 0 {
		included := map[string]bool{}
		for _, dir := range dirs {
			for _, ref := range kustomizationRefs(dir) {
				included[filepath.Clean(filepath.Join(dir, ref))] = true
			}
		}
		var roots, prod []string
		for _, dir := range dirs {
			if included[filepath.Clean(dir)] {
				continue
			}
			rel, _ := filepath.Rel(root, dir)
			roots = append(roots, rel)
			if namesProduction(rel) {
				prod = append(prod, rel)
			}
		}
		if len(prod) > 0 {
			return prod
		}
		return roots
	}
	var found []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || len(found) > 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if k8sDirNames[d.Name()] && isFile(filepath.Join(p, "Chart.yaml")) == false && hasManifests(p) {
			found = append(found, rel)
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

func namesProduction(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		seg = strings.ToLower(seg)
		if strings.HasPrefix(seg, "prod") || strings.Contains(seg, "-prod") || strings.Contains(seg, "_prod") {
			return true
		}
	}
	return false
}

func kustomizationRefs(dir string) []string {
	data, err := os.ReadFile(kustomizationFile(dir))
	if err != nil {
		return nil
	}
	var k struct {
		Resources  []string `yaml:"resources"`
		Bases      []string `yaml:"bases"`
		Components []string `yaml:"components"`
	}
	_ = yaml.Unmarshal(data, &k)
	var out []string
	for _, r := range append(append(k.Resources, k.Bases...), k.Components...) {
		if !strings.Contains(r, "://") && !strings.HasSuffix(r, ".yaml") && !strings.HasSuffix(r, ".yml") {
			out = append(out, r)
		}
	}
	return out
}

func hasManifests(dir string) bool {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && isYAML(e.Name()) {
			if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && bytes.Contains(data, []byte("apiVersion:")) && bytes.Contains(data, []byte("kind:")) {
				return true
			}
		}
	}
	return false
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// loadK8s renders each source (a kustomization directory, a manifests
// directory or a single file) into its Kubernetes objects.
func loadK8s(root string, sources []string) ([]k8sObject, error) {
	var objs []k8sObject
	for _, src := range sources {
		abs := src
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, src)
		}
		var docs []byte
		info, err := os.Stat(abs)
		switch {
		case err != nil:
			return nil, err
		case info.IsDir() && kustomizationFile(abs) != "":
			docs, err = kustomizeBuild(abs)
		case info.IsDir():
			docs, err = readManifestDir(abs)
		default:
			docs, err = os.ReadFile(abs)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		parsed, err := parseK8sDocs(docs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		objs = append(objs, parsed...)
	}
	return objs, nil
}

// ErrNoKustomize is returned when a kustomization must be rendered and
// neither kustomize nor kubectl is on PATH.
var ErrNoKustomize = errors.New("rendering a kustomization needs `kustomize` or `kubectl` on PATH; or render it yourself (`kubectl kustomize <dir> > all.yaml`) and pass -path all.yaml")

// KustomizeCommand returns the command that renders a kustomization dir,
// preferring a standalone kustomize over kubectl's embedded one.
func KustomizeCommand(dir string) (*exec.Cmd, error) {
	if p, err := exec.LookPath("kustomize"); err == nil {
		return exec.Command(p, "build", dir), nil
	}
	if p, err := exec.LookPath("kubectl"); err == nil {
		return exec.Command(p, "kustomize", dir), nil
	}
	return nil, ErrNoKustomize
}

func kustomizeBuild(dir string) ([]byte, error) {
	cmd, err := KustomizeCommand(dir)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func readManifestDir(dir string) ([]byte, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(d.Name(), ".") || isFile(filepath.Join(p, "Chart.yaml"))) {
				return filepath.SkipDir
			}
			return nil
		}
		if isYAML(d.Name()) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var buf bytes.Buffer
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		buf.WriteString("\n---\n")
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

func parseK8sDocs(data []byte) ([]k8sObject, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []k8sObject
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, k8sObjects(doc)...)
	}
}

func k8sObjects(doc map[string]any) []k8sObject {
	kind, apiVersion := str(doc["kind"]), str(doc["apiVersion"])
	if kind == "" || apiVersion == "" {
		return nil
	}
	if strings.HasSuffix(kind, "List") {
		var out []k8sObject
		for _, item := range anyList(doc["items"]) {
			out = append(out, k8sObjects(asMap(item))...)
		}
		return out
	}
	meta := asMap(doc["metadata"])
	return []k8sObject{{apiVersion: apiVersion, kind: kind, name: str(meta["name"]), namespace: str(meta["namespace"]), raw: doc}}
}
