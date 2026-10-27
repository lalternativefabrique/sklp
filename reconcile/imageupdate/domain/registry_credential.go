package domain

import (
	"context"
	"strings"
	"time"
)

// RegistryCredential authenticates tag listings for EVERY watch of the stack
// whose ImageRepo starts with Prefix — one credential per registry namespace
// (e.g. "registry.example.com/acme") instead of a copy on each watch. A
// watch's own RegistrySecret, when set, still wins: it is the explicit
// override, the shared credential the default.
type RegistryCredential struct {
	ID        string
	StackID   string
	Prefix    string
	User      string
	Secret    string // encrypted at rest; clear across the repository boundary
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RegistryCredentialRepository persists shared registry credentials.
// Secret crosses in CLEAR; the implementation seals it (same contract as
// WatchRepository's credential fields).
type RegistryCredentialRepository interface {
	Upsert(ctx context.Context, c *RegistryCredential) error
	ListByStack(ctx context.Context, stackID string) ([]*RegistryCredential, error)
	Delete(ctx context.Context, stackID, id string) error
}

// MatchCredential returns the credential whose Prefix is the LONGEST match
// for imageRepo, or nil. Longest wins so "rg.host/team/app" can override a
// broader "rg.host/team" entry.
func MatchCredential(creds []*RegistryCredential, imageRepo string) *RegistryCredential {
	var best *RegistryCredential
	for _, c := range creds {
		if !strings.HasPrefix(imageRepo, c.Prefix) {
			continue
		}
		if best == nil || len(c.Prefix) > len(best.Prefix) {
			best = c
		}
	}
	return best
}
