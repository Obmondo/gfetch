package secrets

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/obmondo/gfetch/pkg/telemetry"
)

var (
	ErrNotFound      = errors.New("secret not found")
	ErrForbidden     = errors.New("secret access denied")
	ErrUnavailable   = errors.New("secret backend unavailable")
	ErrRejected      = errors.New("secret backend rejected request")
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

// Store reads secrets from OpenBao KV v2 backends, caching the value per path
// after first fetch. The cache is cleared on every successful config reload
// (InvalidateAll) and per-path on SSH auth failure (Invalidate). Failures are
// never cached: an OpenBao outage on cache miss fails the sync; the next sync
// retries. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	clients map[OpenBaoConfig]*client
	cache   map[cacheKey][]byte
	gen     uint64
	now     func() time.Time
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
		now:     time.Now,
	}
}

// Read fetches the SSH private key for repoName from the OpenBao secret at
// secretPath, using the provided OpenBao configuration. On success the value
// is cached; subsequent reads for the same path return the cached copy without
// contacting OpenBao. Failures are never cached. The raw key bytes are never
// logged.
func (s *Store) Read(ctx context.Context, repoName string, obCfg *OpenBaoConfig, secretPath string) ([]byte, error) {
	key := cacheKey{cfg: *obCfg, path: secretPath}

	s.mu.Lock()
	if cached, ok := s.cache[key]; ok {
		s.mu.Unlock()
		return cached, nil
	}
	gen := s.gen
	s.mu.Unlock()

	c, err := s.clientFor(obCfg)
	if err != nil {
		return nil, err
	}

	data, err := c.readField(ctx, secretPath, obCfg.Field)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reason := errorReason(err)
		telemetry.SecretFetchFailuresTotal.WithLabelValues(repoName, reason).Inc()
		slog.Warn("secret fetch failed", "repo", repoName, "path", secretPath, "reason", reason, "error", err)
		return nil, err
	}

	s.mu.Lock()
	if s.gen == gen {
		s.cache[key] = data
	}
	s.mu.Unlock()
	return data, nil
}

// Invalidate evicts the cached value for a single secret path and increments
// the generation counter so any in-flight reads that started before the
// invalidation will not re-populate the evicted entry.
func (s *Store) Invalidate(obCfg *OpenBaoConfig, secretPath string) {
	s.mu.Lock()
	delete(s.cache, cacheKey{cfg: *obCfg, path: secretPath})
	s.gen++
	s.mu.Unlock()
}

// InvalidateAll clears the entire cache and all cached clients, and increments
// the generation counter. Called by the daemon after a successful config reload.
func (s *Store) InvalidateAll() {
	s.mu.Lock()
	s.cache = make(map[cacheKey][]byte)
	s.clients = make(map[OpenBaoConfig]*client)
	s.gen++
	s.mu.Unlock()
}

// clientFor returns (or lazily creates) a client for the given config.
func (s *Store) clientFor(cfg *OpenBaoConfig) (*client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.clients[*cfg]; ok {
		return c, nil
	}

	c, err := newClient(cfg, s.now)
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
	case errors.Is(err, ErrRejected):
		return "rejected"
	default:
		return "unavailable"
	}
}
