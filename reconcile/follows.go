package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// FollowsBucket keeps the repos this node was told to follow, the node-side
// counterpart of the core's project-to-repo link.
const FollowsBucket = "reconcile-follows"

var (
	ErrFollowNotFound = errors.New("no followed repo by that name")
	ErrFollowTaken    = errors.New("a followed repo already has that name")
	followNameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

type Follow struct {
	Name    string    `json:"name"`
	Repo    string    `json:"repo"`
	Branch  string    `json:"branch"`
	AddedAt time.Time `json:"added_at"`
}

type FollowStore struct {
	kv jetstream.KeyValue
}

func NewFollowStore(kv jetstream.KeyValue) *FollowStore { return &FollowStore{kv: kv} }

// NormalizeFollow validates a follow request and fills its defaults. A repo is
// an https URL or an absolute path on the node; a URL carrying a credential is
// refused, since this list is served back over the control API.
func NormalizeFollow(f Follow) (Follow, error) {
	f.Repo = strings.TrimSpace(f.Repo)
	switch {
	case filepath.IsAbs(f.Repo):
	case strings.HasPrefix(f.Repo, "https://"):
		u, err := url.Parse(f.Repo)
		if err != nil || u.Host == "" || strings.Trim(u.Path, "/") == "" {
			return Follow{}, fmt.Errorf("repo %q is not a clone URL", f.Repo)
		}
		if u.User != nil {
			return Follow{}, fmt.Errorf("repo %q carries a credential; only public repos can be followed this way", u.Redacted())
		}
	default:
		return Follow{}, fmt.Errorf("repo %q must be an https:// clone URL", f.Repo)
	}
	if f.Branch == "" {
		f.Branch = "main"
	}
	if f.Name == "" {
		f.Name = strings.ToLower(RepoName(f.Repo))
	}
	if !followNameRE.MatchString(f.Name) {
		return Follow{}, fmt.Errorf("name %q must be lowercase letters, digits and dashes", f.Name)
	}
	return f, nil
}

func (s *FollowStore) List(ctx context.Context) ([]Follow, error) {
	keys, err := s.kv.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list follows: %w", err)
	}
	var out []Follow
	for key := range keys.Keys() {
		entry, err := s.kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("read follow %s: %w", key, err)
		}
		var f Follow
		if err := json.Unmarshal(entry.Value(), &f); err != nil {
			return nil, fmt.Errorf("decode follow %s: %w", key, err)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *FollowStore) Add(ctx context.Context, f Follow) (Follow, error) {
	f, err := NormalizeFollow(f)
	if err != nil {
		return Follow{}, err
	}
	f.AddedAt = time.Now().UTC()
	raw, err := json.Marshal(f)
	if err != nil {
		return Follow{}, err
	}
	if _, err := s.kv.Create(ctx, f.Name, raw); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return Follow{}, ErrFollowTaken
		}
		return Follow{}, fmt.Errorf("store follow: %w", err)
	}
	return f, nil
}

func (s *FollowStore) Remove(ctx context.Context, name string) error {
	if _, err := s.kv.Get(ctx, name); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrFollowNotFound
		}
		return fmt.Errorf("read follow %s: %w", name, err)
	}
	if err := s.kv.Purge(ctx, name); err != nil {
		return fmt.Errorf("remove follow %s: %w", name, err)
	}
	return nil
}
