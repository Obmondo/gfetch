package secrets

import (
	"bytes"
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
	// ErrClientConfig is a client that cannot be built from the OpenBao
	// config, such as an address that does not parse. A retry does not fix it.
	ErrClientConfig = errors.New("openbao client config invalid")
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
// OpenBao config and path after first fetch. A path is evicted when a config
// reload adds, removes or re-keys its repo (Invalidate), and replaced when a
// refetch after an SSH auth failure finds a different key (Refetch). Callers
// get their own copy of a value, so the cache is never written through.
//
// Each distinct OpenBao config gets its own client, built on first use and
// kept for the process, and its own cache entries. After a reload that changes
// the openbao block, syncs still running on the old config and syncs on the
// new one therefore use their own client and token side by side, rather than
// replacing a shared client and clearing the cache on every alternate read.
// Configs change rarely, so the clients kept for old ones cost little.
//
// Failures are never cached: an OpenBao outage on cache miss fails the sync;
// the next sync retries. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	clients map[OpenBaoConfig]*client
	cache   map[cacheKey][]byte
	// gens counts, per path, the evictions and key replacements. A fetch
	// caches its value only if its path's generation is unchanged since it
	// started, so an eviction or replacement of one path is not undone by a
	// read already in flight, and does not stop reads of other paths from
	// caching.
	gens   map[string]uint64
	now    func() time.Time
	logger *slog.Logger
}

// cacheKey identifies a cached value: the path read under one OpenBao config.
type cacheKey struct {
	cfg  OpenBaoConfig
	path string
}

// NewStore creates a new Store.
func NewStore() *Store {
	return &Store{
		clients: make(map[OpenBaoConfig]*client),
		cache:   make(map[cacheKey][]byte),
		gens:    make(map[string]uint64),
		now:     time.Now,
		logger:  slog.Default(),
	}
}

// Read fetches the SSH private key for repoName from the OpenBao secret at
// secretPath, using the provided OpenBao configuration. On success the value
// is cached under that config and path, unless a read of them that finished
// first already cached one; subsequent reads return a copy of the cached value
// without contacting OpenBao. Failures are never cached. The raw key bytes are
// never logged.
func (s *Store) Read(ctx context.Context, repoName string, obCfg *OpenBaoConfig, secretPath string) ([]byte, error) {
	// The cache lookup and the gen capture share one critical section, so a
	// read that started before an eviction or replacement of its path fails
	// the gen check below and does not cache what it fetched.
	key := cacheKey{cfg: *obCfg, path: secretPath}
	s.mu.Lock()
	c, err := s.clientForLocked(obCfg)
	if err != nil {
		s.mu.Unlock()
		s.countFailure(repoName, secretPath, err)
		return nil, err
	}
	if cached, ok := s.cache[key]; ok {
		s.mu.Unlock()
		return bytes.Clone(cached), nil
	}
	gen := s.gens[secretPath]
	s.mu.Unlock()

	data, err := s.fetch(ctx, c, repoName, obCfg, secretPath)
	if err != nil {
		return nil, err
	}

	// A read of the same path that finished first has already cached its
	// value; this one, slower, may hold the key from before a rotation, so it
	// does not overwrite it.
	s.mu.Lock()
	if _, cached := s.cache[key]; !cached && s.gens[secretPath] == gen {
		s.cache[key] = bytes.Clone(data)
	}
	s.mu.Unlock()
	return data, nil
}

// Refetch reads the secret at secretPath from OpenBao, bypassing the cache.
// The value cached under obCfg is stored only when the fetched one differs,
// so a refetch that finds the same key leaves the cache as it was. Storing it
// bumps the path's generation, also when it fills an empty entry: a read of
// the path already in flight may have fetched the key from before a rotation,
// and must not cache it over this one. That read still returns its value to
// its caller, and the next read hits the value stored here, so the bump costs
// no extra OpenBao read. Failures are never cached and leave the cached value
// in place.
func (s *Store) Refetch(ctx context.Context, repoName string, obCfg *OpenBaoConfig, secretPath string) ([]byte, error) {
	key := cacheKey{cfg: *obCfg, path: secretPath}
	s.mu.Lock()
	c, err := s.clientForLocked(obCfg)
	if err != nil {
		s.mu.Unlock()
		s.countFailure(repoName, secretPath, err)
		return nil, err
	}
	gen := s.gens[secretPath]
	s.mu.Unlock()

	data, err := s.fetch(ctx, c, repoName, obCfg, secretPath)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.gens[secretPath] == gen && !bytes.Equal(s.cache[key], data) {
		s.cache[key] = bytes.Clone(data)
		s.gens[secretPath]++
	}
	s.mu.Unlock()
	return data, nil
}

// fetch reads the secret at secretPath through c, counting and logging a
// failure. A failure caused by ctx returns ctx's error, uncounted.
func (s *Store) fetch(ctx context.Context, c *client, repoName string, obCfg *OpenBaoConfig, secretPath string) ([]byte, error) {
	data, err := c.readField(ctx, secretPath, obCfg.Field)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.countFailure(repoName, secretPath, err)
		return nil, err
	}
	return data, nil
}

// countFailure counts and logs a failed secret read under its reason label.
func (s *Store) countFailure(repoName, secretPath string, err error) {
	label := reason(err)
	telemetry.SecretFetchFailuresTotal.WithLabelValues(repoName, label).Inc()
	s.logger.Warn("secret fetch failed", "repo", repoName, "path", secretPath, "reason", label, "error", err)
}

// Invalidate evicts the cached value for secretPath under every OpenBao
// config and bumps the path's generation, so reads of the path in flight since
// before the invalidation do not re-populate it. Other paths are untouched.
func (s *Store) Invalidate(secretPath string) {
	s.mu.Lock()
	for key := range s.cache {
		if key.path == secretPath {
			delete(s.cache, key)
		}
	}
	s.gens[secretPath]++
	s.mu.Unlock()
}

// clientForLocked returns the client for cfg, building it on first use. Must
// be called with s.mu held.
func (s *Store) clientForLocked(cfg *OpenBaoConfig) (*client, error) {
	if c, ok := s.clients[*cfg]; ok {
		return c, nil
	}

	c, err := newClient(cfg, s.now, s.logger)
	if err != nil {
		return nil, err
	}
	s.clients[*cfg] = c
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
	case errors.Is(err, ErrClientConfig):
		return "config_invalid"
	default:
		return "unavailable"
	}
}
