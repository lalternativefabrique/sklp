package importer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

var composeBaseNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

var composeOverrideNames = map[string][]string{
	"compose.yaml":        {"compose.override.yaml", "compose.override.yml"},
	"compose.yml":         {"compose.override.yml", "compose.override.yaml"},
	"docker-compose.yaml": {"docker-compose.override.yaml", "docker-compose.override.yml"},
	"docker-compose.yml":  {"docker-compose.override.yml", "docker-compose.override.yaml"},
}

// FindComposeFiles returns the files `docker compose up` reads by default at
// root: the first base file in compose's own lookup order, then its override.
func FindComposeFiles(root string) []string {
	for _, base := range composeBaseNames {
		if !isFile(filepath.Join(root, base)) {
			continue
		}
		files := []string{base}
		for _, o := range composeOverrideNames[base] {
			if isFile(filepath.Join(root, o)) {
				files = append(files, o)
				break
			}
		}
		return files
	}
	return nil
}

type composeService struct {
	name string
	raw  map[string]any
}

type composeProject struct {
	services []composeService
	volumes  map[string]any
	topLevel map[string]any
}

func (p *composeProject) service(name string) *composeService {
	for i := range p.services {
		if p.services[i].name == name {
			return &p.services[i]
		}
	}
	return nil
}

func loadComposeProject(root string, files []string) (*composeProject, error) {
	p := &composeProject{volumes: map[string]any{}, topLevel: map[string]any{}}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			return nil, err
		}
		if err := p.merge(data); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	return p, nil
}

func (p *composeProject) merge(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		return nil
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return errors.New("top level is not a mapping")
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, val := top.Content[i].Value, top.Content[i+1]
		switch key {
		case "services":
			if err := p.mergeServices(val); err != nil {
				return err
			}
		case "volumes":
			var m map[string]any
			if err := val.Decode(&m); err != nil {
				return fmt.Errorf("volumes: %w", err)
			}
			for k, v := range m {
				p.volumes[k] = v
			}
		default:
			var v any
			if err := val.Decode(&v); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			p.topLevel[key] = v
		}
	}
	return nil
}

func (p *composeProject) mergeServices(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("services is not a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		var raw map[string]any
		if err := node.Content[i+1].Decode(&raw); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if raw == nil {
			raw = map[string]any{}
		}
		normalizeService(raw)
		if existing := p.service(name); existing != nil {
			existing.raw = mergeServiceMaps(existing.raw, raw)
			continue
		}
		p.services = append(p.services, composeService{name: name, raw: raw})
	}
	return nil
}

// resolveExtends folds same-file `extends: {service: x}` into the extending
// service. A cross-file extends is left in place for the translator to report.
func (p *composeProject) resolveExtends() {
	for depth := 0; depth < 10; depth++ {
		changed := false
		for i := range p.services {
			svc := &p.services[i]
			base, ok := sameFileExtends(svc.raw)
			if !ok {
				continue
			}
			parent := p.service(base)
			if parent == nil || parent.name == svc.name {
				continue
			}
			if _, parentExtends := sameFileExtends(parent.raw); parentExtends {
				continue
			}
			child := svc.raw
			delete(child, "extends")
			merged := mergeServiceMaps(cloneMap(parent.raw), child)
			svc.raw = merged
			changed = true
		}
		if !changed {
			return
		}
	}
}

func sameFileExtends(raw map[string]any) (string, bool) {
	switch e := raw["extends"].(type) {
	case string:
		return e, true
	case map[string]any:
		if _, hasFile := e["file"]; hasFile {
			return "", false
		}
		s, ok := e["service"].(string)
		return s, ok
	}
	return "", false
}

func normalizeService(raw map[string]any) {
	for _, k := range []string{"environment", "labels"} {
		if list, ok := raw[k].([]any); ok {
			m := map[string]any{}
			for _, item := range list {
				s, _ := item.(string)
				key, val, hasVal := cutEq(s)
				if hasVal {
					m[key] = val
				} else {
					m[key] = nil
				}
			}
			raw[k] = m
		}
	}
	if list, ok := raw["depends_on"].([]any); ok {
		m := map[string]any{}
		for _, item := range list {
			if s, ok := item.(string); ok {
				m[s] = map[string]any{"condition": "service_started"}
			}
		}
		raw["depends_on"] = m
	}
	if s, ok := raw["env_file"].(string); ok {
		raw["env_file"] = []any{s}
	}
}

var replacedOnMerge = map[string]bool{"command": true, "entrypoint": true, "healthcheck": true, "image": true, "build": true}

func mergeServiceMaps(base, over map[string]any) map[string]any {
	for k, ov := range over {
		bv, exists := base[k]
		if !exists || replacedOnMerge[k] {
			base[k] = ov
			continue
		}
		switch o := ov.(type) {
		case map[string]any:
			if b, ok := bv.(map[string]any); ok {
				for mk, mv := range o {
					b[mk] = mv
				}
				continue
			}
		case []any:
			if b, ok := bv.([]any); ok {
				base[k] = appendUnique(b, o)
				continue
			}
		}
		base[k] = ov
	}
	return base
}

func appendUnique(base, extra []any) []any {
	seen := map[string]bool{}
	for _, v := range base {
		seen[fmt.Sprint(v)] = true
	}
	for _, v := range extra {
		if !seen[fmt.Sprint(v)] {
			base = append(base, v)
			seen[fmt.Sprint(v)] = true
		}
	}
	return base
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			out[k] = cloneMap(t)
		case []any:
			out[k] = append([]any(nil), t...)
		default:
			out[k] = v
		}
	}
	return out
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func cutEq(s string) (key, val string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
