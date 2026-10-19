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
// It is comparable, so a config change can be detected by value.
type OpenBaoConfig struct {
	Addr                    string `yaml:"addr"`
	Role                    string `yaml:"role"`
	ServiceAccountTokenPath string `yaml:"service_account_token_path"`
	KVMount                 string `yaml:"kv_mount"`
	Field                   string `yaml:"field"`
}

// Store reads secrets from an OpenBao KV v2 backend, caching the value per
// path after first fetch. A path is evicted on an SSH auth failure and when a
// config reload adds, removes or re-keys its repo (Invalidate). The client and
// its token live for the process unless the OpenBao config changes, which
// replaces the client and clears the cache. Failures are never cached: an
// OpenBao outage on cache miss fails the sync; the next sync retries. It is
// safe for concurrent use.
type Store struct {
	mu        sync.Mutex
	client    *client
	clientCfg OpenBaoConfig
	cache     map[string][]byte
	gen       uint64
	now       func() time.Time
	logger    *slog.Logger
}

// NewStore creates a new Store.
func NewStore() *Store {
	return &Store{
		cache:  make(map[string][]byte),
		now:    time.Now,
		logger: slog.Default(),
	}
}

// Read fetches the SSH private key for repoName from the OpenBao secret at
// secretPath, using the provided OpenBao configuration. On success the value
// is cached; subsequent reads for the same path return the cached copy without
// contacting OpenBao. Failures are never cached. The raw key bytes are never
// logged.
func (s *Store) Read(ctx context.Context, repoName string, obCfg *OpenBaoConfig, secretPath string) ([]byte, error) {
	// Picking the client, the cache lookup and the gen capture share one
	// critical section, so a read that started before a client replacement
	// fails the gen check below and does not cache the old backend's key.
	s.mu.Lock()
	c, err := s.clientForLocked(obCfg)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if cached, ok := s.cache[secretPath]; ok {
		s.mu.Unlock()
		return cached, nil
	}
	gen := s.gen
	s.mu.Unlock()

	data, err := c.readField(ctx, secretPath, obCfg.Field)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		label := reason(err)
		telemetry.SecretFetchFailuresTotal.WithLabelValues(repoName, label).Inc()
		s.logger.Warn("secret fetch failed", "repo", repoName, "path", secretPath, "reason", label, "error", err)
		return nil, err
	}

	s.mu.Lock()
	if s.gen == gen {
		s.cache[secretPath] = data
	}
	s.mu.Unlock()
	return data, nil
}

// Invalidate evicts the cached value for a single secret path and increments
// the generation counter so any in-flight reads that started before the
// invalidation will not re-populate the evicted entry.
func (s *Store) Invalidate(secretPath string) {
	s.mu.Lock()
	delete(s.cache, secretPath)
	s.gen++
	s.mu.Unlock()
}

// clientForLocked returns the client for cfg, building it lazily. A cfg
// different from the current client's replaces the client and clears the
// cache: a different backend means different secrets. Must be called with
// s.mu held.
func (s *Store) clientForLocked(cfg *OpenBaoConfig) (*client, error) {
	if s.client != nil && s.clientCfg == *cfg {
		return s.client, nil
	}

	c, err := newClient(cfg, s.now, s.logger)
	if err != nil {
		return nil, err
	}
	s.client = c
	s.clientCfg = *cfg
	s.cache = make(map[string][]byte)
	s.gen++
	return c, nil
}

// reason returns the reason label for a secret fetch error, used by the
// secret fetch failure metric.
func reason(err error) string {
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
