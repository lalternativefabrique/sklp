package domain

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const WatchFilePath = ".sklp/image-watches.yaml"

// WatchFile is .sklp/image-watches.yaml: the images a push to the repo keeps
// rolled on its nodes.
type WatchFile struct {
	Watches []WatchFileEntry `yaml:"watches"`
}

// WatchFileEntry is one watch: when the registry gains a tag of ImageRepo
// matching TagPattern, Service in Space on Node rolls to it.
type WatchFileEntry struct {
	// Name identifies the watch within the stack; unique across the repos feeding it.
	Name string `yaml:"name"`
	// Service is the deploy.yaml service (or job) whose image rolls.
	Service string `yaml:"service"`
	// ImageRepo is the image repository without a tag, e.g. rg.fr-par.scw.cloud/ns/app.
	ImageRepo string `yaml:"image_repo"`
	// TagPattern is the regexp a tag must match to roll; empty means the publish
	// tag format YYYYMMDD-HHMM-<sha>.
	TagPattern string `yaml:"tag_pattern,omitempty"`
	// Node names the registered node, the same form deploy-root.yaml uses.
	Node string `yaml:"node,omitempty"`
	// NodeID is the superseded way to name the node, still read for files already committed.
	NodeID string `yaml:"node_id,omitempty"`
	// Space is the space on the node that runs Service.
	Space string `yaml:"space"`
	// RegistryUser is the user of a private registry; its password is set on the
	// stack, never in this file.
	RegistryUser string `yaml:"registry_user,omitempty"`
}

func ParseWatchFile(raw []byte) (*WatchFile, error) {
	var f WatchFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse image-watches: %w", err)
	}
	return &f, nil
}

func (e WatchFileEntry) Target() string {
	if e.Node != "" {
		return e.Node
	}
	return e.NodeID
}

// Problems lists what would make a reconcile skip this entry, checked without
// the node: the registry and the node's credentials are only known server-side.
func (e WatchFileEntry) Problems() []string {
	var out []string
	if strings.TrimSpace(e.Name) == "" {
		out = append(out, "name is required")
	}
	switch repo := strings.TrimSpace(e.ImageRepo); {
	case repo == "":
		out = append(out, "image_repo is required")
	default:
		if tagged, _ := splitTag(repo); tagged {
			out = append(out, fmt.Sprintf("image_repo %q must not carry a tag: the tag is what the watch discovers", repo))
		}
	}
	switch svc := strings.TrimSpace(e.Service); {
	case svc == "":
		out = append(out, "service is required")
	case !serviceNameRE.MatchString(svc):
		out = append(out, fmt.Sprintf("service %q must match %s", svc, serviceNameRE))
	}
	if e.Node == "" && e.NodeID == "" {
		out = append(out, "node is required")
	}
	if strings.TrimSpace(e.Space) == "" {
		out = append(out, "space is required: a node hosts several spaces")
	}
	if p := strings.TrimSpace(e.TagPattern); p != "" {
		if _, err := regexp.Compile(p); err != nil {
			out = append(out, fmt.Sprintf("tag_pattern: %v", err))
		}
	}
	return out
}
