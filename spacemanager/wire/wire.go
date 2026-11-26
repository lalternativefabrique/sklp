// Package wire holds the types a node's control API sends and receives that a
// client needs to decode. It imports nothing Linux-only, so the CLI talking to
// a node builds on every OS.
package wire

import "time"

// TrustedRegistriesEnv lists the registry hosts a node pulls from, comma
// separated (e.g. "rg.fr-par.scw.cloud,docker.io"). Unset means any registry.
const TrustedRegistriesEnv = "SKLP_TRUSTED_REGISTRIES"

// RequireImageDigestEnv, when "1", refuses an image that is not pinned by
// digest: a tag is a mutable pointer the registry owner can repoint.
const RequireImageDigestEnv = "SKLP_REQUIRE_IMAGE_DIGEST"

type ResolvedRoute struct {
	Service string
	Host    string
	Path    string
	Rate    float64       // per-client-IP requests/sec; 0 = unlimited
	Burst   int           // token-bucket size; 0 = unlimited
	Timeout time.Duration // per-request cap (dial+response+body); 0 = unbounded
	MaxBody int64         // per-request body cap in bytes; 0 = unbounded

	RedirectApex    bool // 301 www.<host> -> <host>
	SecurityHeaders bool // HSTS, frameDeny, nosniff, referrer-policy
	Compress        bool // gzip text responses
	Passthrough     int  // TLS passthrough port; 0 = an HTTP route
}

type DataEngine string

const (
	EnginePostgres DataEngine = "postgres"
	EngineRedis    DataEngine = "redis"
	EngineMySQL    DataEngine = "mysql"
	EngineMariaDB  DataEngine = "mariadb"
	EngineMongo    DataEngine = "mongo"
	EngineNATS     DataEngine = "nats"
	EngineUnknown  DataEngine = "unknown"
)

// Protection grades what the platform can do for a store's data if the node
// is lost: nothing, run a backup it cannot read back, or read it back itself.
type Protection string

const (
	ProtectionNone       Protection = "none"
	ProtectionOpaque     Protection = "opaque"
	ProtectionBacked     Protection = "backed"
	ProtectionDisposable Protection = "disposable"
)

type BackupOutcome struct {
	Service string    `json:"service"`
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Key     string    `json:"key,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	Error   string    `json:"error,omitempty"`
	Warning string    `json:"warning,omitempty"`
}
type DataStore struct {
	Service    string         `json:"service"`
	Engine     DataEngine     `json:"engine"`
	Image      string         `json:"image"`
	Volumes    []string       `json:"volumes"`
	Protection Protection     `json:"protection"`
	Detail     string         `json:"detail"`
	LastBackup *BackupOutcome `json:"last_backup,omitempty"`
}
