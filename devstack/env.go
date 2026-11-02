package devstack

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// loadEnvFiles parses dotenv files in order and returns a merged map.
// Later files override earlier ones; per-service Env still wins on top.
// Missing files are silently skipped — `.env.local` not existing is
// not an error, by design.
//
// Format accepted: `KEY=value` per line, blank lines ignored, `#`
// starts a comment, single/double quotes around the value are
// stripped. No interpolation, no `export` keyword — keep it boring.
func loadEnvFiles(paths []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := strings.TrimSpace(s.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				continue
			}
			k := strings.TrimSpace(line[:eq])
			v := strings.TrimSpace(line[eq+1:])
			v = strings.Trim(v, `"'`)
			out[k] = v
		}
		_ = f.Close()
		if err := s.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// mergeEnv returns the union of base and overlay; overlay wins on
// key conflicts. Used to fold the pipeline-level Env into a service's
// env without mutating either map. nil inputs are treated as empty.
func mergeEnv(base, overlay map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// composeEnv builds the final []string env passed to exec.Cmd. Layer
// order: process env → env files → pipeline-level Env → service Env.
// Each later layer overrides keys from earlier ones.
func composeEnv(envFiles map[string]string, pipelineEnv, serviceEnv map[string]string) []string {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		merged[kv[:eq]] = kv[eq+1:]
	}
	return mergeLayers(merged, envFiles, pipelineEnv, serviceEnv)
}

// composeContainerEnv is the embedded-runtime (runc) counterpart of composeEnv.
// It builds the env from ONLY the declared sources — env files, pipeline Env,
// service Env — and deliberately OMITS os.Environ(). Leaking the host shell's
// PATH/HOME/GOPATH into a container would override the image's own PATH (OCI
// last-wins), so e.g. postgres could no longer find initdb. The container's PATH
// comes from the image, exactly like docker.
func composeContainerEnv(envFiles map[string]string, pipelineEnv, serviceEnv map[string]string) []string {
	return mergeLayers(map[string]string{}, envFiles, pipelineEnv, serviceEnv)
}

// mergeLayers folds env files < pipeline < service onto base (each later layer
// wins on key conflict) and renders KEY=value. base is the seed layer:
// os.Environ() for the host-process path, empty for the container path. It
// mutates base.
func mergeLayers(base, envFiles, pipelineEnv, serviceEnv map[string]string) []string {
	for k, v := range envFiles {
		base[k] = v
	}
	for k, v := range pipelineEnv {
		base[k] = v
	}
	for k, v := range serviceEnv {
		base[k] = v
	}
	out := make([]string, 0, len(base))
	for k, v := range base {
		out = append(out, k+"="+v)
	}
	return out
}

// rewriteLoopbackPorts points env values at the stack's services: a value
// naming localhost, 127.0.0.1 or [::1] on a port some other service declares
// is rewritten to that service's name, which the space resolves. A .env
// written for services sharing one host keeps working inside a space, where
// each service's loopback is its own. The service's own port is left alone.
func rewriteLoopbackPorts(env []string, self string, portOwner map[int]string) []string {
	if len(portOwner) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		for port, owner := range portOwner {
			if owner == self {
				continue
			}
			target := owner + ":" + strconv.Itoa(port)
			for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
				v = strings.ReplaceAll(v, host+":"+strconv.Itoa(port), target)
			}
		}
		out = append(out, k+"="+v)
	}
	return out
}

// declaredPortOwners maps each declared `port:` to the service that binds it.
func declaredPortOwners(services []NamedService) map[int]string {
	owners := map[int]string{}
	for _, ns := range services {
		if ns.Def.Port > 0 {
			owners[ns.Def.Port] = ns.Name
		}
	}
	return owners
}
