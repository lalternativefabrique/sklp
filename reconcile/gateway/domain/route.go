package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrRouteNotFound is returned when a route id does not exist on the node.
var ErrRouteNotFound = errors.New("gateway route not found")

// ErrRouteConflict is returned when a host+path pair is already routed on the
// node. Two apps claiming one host is the conflict the master document exists
// to prevent, so it is refused rather than resolved by last-write-wins.
var ErrRouteConflict = errors.New("gateway route already exists for this host and path")

// ErrNodeNotFound is returned when a node id does not exist on the project.
var ErrNodeNotFound = errors.New("gateway node not found")

// ErrNodeExists is returned when a node name is already taken on the project.
var ErrNodeExists = errors.New("gateway node already exists")

// Route is one entry of a node's master route document. It mirrors sklp's
// ResolvedRoute, which is the driver-neutral model both the native mux and the
// traefik driver consume — so the API never names a front-door driver.
//
// It carries no port on purpose: the port is joined from the service in the
// node's manifest, which keeps a route from pointing at a port no declared
// service owns.
type Route struct {
	ID        string
	NodeID    string
	Space     string
	Service   string
	Host      string
	Path      string
	Rate      float64
	Burst     int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewRoute validates and builds a route. An empty path means the host root.
func NewRoute(nodeID, space, service, host, path string, rate float64, burst int) (*Route, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, errors.New("node_id is required")
	}
	space = strings.TrimSpace(space)
	if space == "" {
		return nil, errors.New("space is required")
	}
	service = strings.TrimSpace(service)
	if service == "" {
		return nil, errors.New("service is required")
	}
	host, err := normalizeHost(host)
	if err != nil {
		return nil, err
	}
	path, err = normalizePath(path)
	if err != nil {
		return nil, err
	}
	if rate < 0 {
		return nil, errors.New("rate cannot be negative")
	}
	if burst < 0 {
		return nil, errors.New("burst cannot be negative")
	}
	now := time.Now().UTC()
	return &Route{
		ID:        uuid.New().String(),
		NodeID:    nodeID,
		Space:     space,
		Service:   service,
		Host:      host,
		Path:      path,
		Rate:      rate,
		Burst:     burst,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Update rewrites the routable fields and bumps UpdatedAt.
func (r *Route) Update(space, service, host, path string, rate float64, burst int) error {
	next, err := NewRoute(r.NodeID, space, service, host, path, rate, burst)
	if err != nil {
		return err
	}
	r.Space, r.Service, r.Host, r.Path = next.Space, next.Service, next.Host, next.Path
	r.Rate, r.Burst = next.Rate, next.Burst
	r.UpdatedAt = time.Now().UTC()
	return nil
}

// normalizeHost lower-cases the host so two routes differing only in case
// cannot both exist — the mux looks up lower-cased hosts, so they would collide
// there anyway, silently.
func normalizeHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", errors.New("host is required")
	}
	if strings.ContainsAny(host, "/ ") {
		return "", errors.New("host must not contain a path or spaces")
	}
	if strings.Contains(host, ":") {
		return "", errors.New("host must not carry a port")
	}
	return host, nil
}

func normalizePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if !strings.HasPrefix(path, "/") {
		return "", errors.New("path must start with /")
	}
	if strings.Contains(path, " ") {
		return "", errors.New("path must not contain spaces")
	}
	return path, nil
}
