package secrets

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/obmondo/gfetch/pkg/telemetry"
)

var (
	ErrNotFound      = errors.New("secret not found")
	ErrForbidden     = errors.New("secret access denied")
	ErrUnavailable   = errors.New("secret backend unavailable")
	ErrInvalidSecret = errors.New("secret field missing or not a string")
)

// OpenBaoConfig holds the connection parameters for an OpenBao KV v2 backend.
// It is comparable so it can be used as a map key.
type OpenBaoConfig struct {
	Addr                    string `yaml:"addr"`
	Role                    string `yaml:"role"`
	ServiceAccountTokenPath string `yaml:"service_account_token_path"`
	KVMount                 string `yaml:"kv_mount"`
	Field                   string `yaml:"field"`
}

// Store reads secrets from OpenBao KV v2 backends, caching the last known
// good value per path as a fallback when the backend is temporarily
// unavailable. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	clients map[OpenBaoConfig]*client
	cache   map[cacheKey][]byte
}

type cacheKey struct {
	cfg  OpenBaoConfig
	path string
}

// NewStore creates a new Store.
func NewStore() *Store {
	return &Store{
		clients: make(map[OpenBaoConfig]*client),
		cache:   make(map[cacheKey][]byte),
	}
}

// Read fetches the SSH private key for repoName from the OpenBao secret at
// secretPath, using the provided OpenBao configuration. On success the value
// is cached. On ErrUnavailable with a cached value the cached copy is
// returned. The raw key bytes are never logged.
func (s *Store) Read(ctx context.Context, repoName string, obCfg *OpenBaoConfig, secretPath string) ([]byte, error) {
	c, err := s.clientFor(obCfg)
	if err != nil {
		return nil, err
	}

	key := cacheKey{cfg: *obCfg, path: secretPath}

	data, err := c.readField(ctx, secretPath, obCfg.Field)
	if err == nil {
		s.mu.Lock()
		s.cache[key] = data
		s.mu.Unlock()
		return data, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	reason := errorReason(err)
	telemetry.SecretFetchFailuresTotal.WithLabelValues(repoName, reason).Inc()
	slog.Warn("secret fetch failed", "repo", repoName, "path", secretPath, "reason", reason)

	if errors.Is(err, ErrUnavailable) {
		s.mu.Lock()
		cached, ok := s.cache[key]
		s.mu.Unlock()
		if ok {
			telemetry.SecretCachedKeyUsedTotal.WithLabelValues(repoName).Inc()
			slog.Warn("using cached secret key", "repo", repoName, "path", secretPath)
			return cached, nil
		}
		return nil, err
	}

	// NotFound, Forbidden, InvalidSecret: evict any stale cache entry.
	s.mu.Lock()
	delete(s.cache, key)
	s.mu.Unlock()
	return nil, err
}

// clientFor returns (or lazily creates) a client for the given config.
func (s *Store) clientFor(cfg *OpenBaoConfig) (*client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.clients[*cfg]; ok {
		return c, nil
	}

	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	s.clients[*cfg] = c
	return c, nil
}

func errorReason(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrInvalidSecret):
		return "invalid"
	default:
		return "unavailable"
	}
}
