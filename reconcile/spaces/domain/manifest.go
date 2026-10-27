package domain

import "context"

// SpaceManifest is what a space was DECLARED to run, as the node that runs it
// reports it — the counterpart of SpaceState (what it is actually running).
//
// The node is the source of truth here, not the repo: it persists the manifest
// it applied, so this reports what is really deployed rather than what the
// current tip of a deploy.yaml says. Reading it from Git would answer a
// different question (see the deployconfig context, which reconciles that way).
type SpaceManifest struct {
	Space    string            `json:"space"`
	Stack    string            `json:"stack,omitempty"`
	URL      string            `json:"url,omitempty"`
	Routes   []ManifestRoute   `json:"routes,omitempty"`
	Services []ManifestService `json:"services"`
	Jobs     []ManifestService `json:"jobs,omitempty"`
	Err      string            `json:"error,omitempty"`
}

// ManifestRoute is one public gateway route of the space.
type ManifestRoute struct {
	Service string `json:"service"`
	Host    string `json:"host,omitempty"`
	Path    string `json:"path,omitempty"`
}

// ManifestService is one declared service or job.
type ManifestService struct {
	Name      string             `json:"name"`
	Image     string             `json:"image,omitempty"`
	Command   []string           `json:"command,omitempty"`
	Replicas  int                `json:"replicas,omitempty"`
	Port      int                `json:"port,omitempty"`
	Ports     []string           `json:"ports,omitempty"`
	Volumes   []string           `json:"volumes,omitempty"`
	DependsOn []string           `json:"depends_on,omitempty"`
	Schedule  string             `json:"schedule,omitempty"`
	Env       []ManifestEnv      `json:"env,omitempty"`
	Resources *ManifestResources `json:"resources,omitempty"`
	Ready     *ManifestReady     `json:"ready,omitempty"`
	Backup    *ManifestBackup    `json:"backup,omitempty"`
}

// ManifestEnv is one environment entry. Source tells a reader why a value is
// absent: "reference" means it is held in the node's 0600 env file — Ref names
// the variables and Value is the template with its placeholders when the value
// was composed around them —, "sensitive" marks an inlined value whose key
// looks secret, "literal" is an ordinary declared value.
type ManifestEnv struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Source string `json:"source"`
}

// ManifestResources are the declared cgroup caps.
type ManifestResources struct {
	CPUCores    float64 `json:"cpu_cores,omitempty"`
	MemoryBytes int64   `json:"memory_bytes,omitempty"`
	PidsMax     int64   `json:"pids_max,omitempty"`
	Require     bool    `json:"require,omitempty"`
}

// ManifestReady is the declared readiness probe.
type ManifestReady struct {
	HTTP    string `json:"http,omitempty"`
	TCP     string `json:"tcp,omitempty"`
	Log     string `json:"log,omitempty"`
	File    string `json:"file,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// ManifestBackup is the declared backup command of a service.
type ManifestBackup struct {
	Schedule string   `json:"schedule,omitempty"`
	Command  []string `json:"command,omitempty"`
}

// ManifestReader reads one space's declared manifest from the node running it.
// It takes the same (spaceName, nodeID) pair as StateReader, and for the same
// reason: on a multi-space node the space cannot be resolved from the node
// registration alone.
type ManifestReader interface {
	ReadManifest(ctx context.Context, stackID, spaceName, nodeID string) SpaceManifest
}
