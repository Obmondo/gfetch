package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	api "github.com/openbao/openbao/api/v2"
)

const (
	kubernetesAuthMount = "kubernetes"
	requestTimeout      = 10 * time.Second
	reloginCooldown     = time.Minute
	renewFractionDenom  = 3
)

// client wraps an openbao API client for a single OpenBaoConfig. It handles
// lazy kubernetes-auth login, proactive renewal at 2/3 lease, and one
// automatic re-login on 403 (rate-limited to reloginCooldown).
type client struct {
	api       *api.Client
	kv        *api.KVv2
	role      string
	tokenPath string

	mu         sync.Mutex
	token      string
	loggedInAt time.Time
	lease      time.Duration
	now        func() time.Time
}

func newClient(cfg *OpenBaoConfig, now func() time.Time) (*client, error) {
	apiCfg := api.NewConfig()
	if apiCfg.Error != nil {
		return nil, fmt.Errorf("secrets: openbao config: %w", apiCfg.Error)
	}
	apiCfg.Address = cfg.Addr
	apiCfg.MaxRetries = 0
	apiCfg.Timeout = requestTimeout

	ac, err := api.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("secrets: creating openbao client: %w", err)
	}

	return &client{
		api:       ac,
		kv:        ac.KVv2(cfg.KVMount),
		role:      cfg.Role,
		tokenPath: cfg.ServiceAccountTokenPath,
		now:       now,
	}, nil
}

// readField fetches the secret at path and returns the value of the configured
// field as raw bytes.
func (c *client) readField(ctx context.Context, path, field string) ([]byte, error) {
	var secret *api.KVSecret
	err := c.do(ctx, path, func() error {
		var getErr error
		secret, getErr = c.kv.Get(ctx, path)
		return getErr
	})
	if err != nil {
		return nil, err
	}

	// Soft-deleted secret: Data is nil.
	if secret == nil || secret.Data == nil {
		return nil, ErrNotFound
	}

	raw, ok := secret.Data[field]
	if !ok {
		return nil, fmt.Errorf("%w: field %q not present in secret at %q", ErrInvalidSecret, field, path)
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%w: field %q is not a string in secret at %q", ErrInvalidSecret, field, path)
	}
	return []byte(s), nil
}

// renewDue reports whether the token should be renewed. Must be called with
// c.mu held.
func (c *client) renewDue() bool {
	return c.lease > 0 && !c.now().Before(c.loggedInAt.Add(c.lease*2/renewFractionDenom))
}

// expired reports whether the token's lease has fully elapsed. Must be called
// with c.mu held.
func (c *client) expired() bool {
	return c.lease > 0 && !c.now().Before(c.loggedInAt.Add(c.lease))
}

// ensureToken acquires or renews the token. Must be called with c.mu held.
func (c *client) ensureToken(ctx context.Context) error {
	if c.token == "" {
		return c.login(ctx)
	}
	if !c.renewDue() {
		return nil
	}
	if err := c.login(ctx); err != nil {
		if c.expired() {
			return err
		}
		slog.Warn("openbao token renewal failed, using current token", "error", err)
	}
	return nil
}

// do ensures a valid token exists, runs fn, and retries once after a re-login
// on 403. Proactive renewal at 2/3 lease prevents most 403s. A 403 on a
// token younger than reloginCooldown is treated as a policy denial and
// returns ErrForbidden without attempting re-login. The mutex prevents
// multiple goroutines from logging in concurrently.
func (c *client) do(ctx context.Context, path string, fn func() error) error {
	c.mu.Lock()
	if err := c.ensureToken(ctx); err != nil {
		c.mu.Unlock()
		return err
	}
	usedToken := c.token
	tokenAge := c.now().Sub(c.loggedInAt)
	c.mu.Unlock()

	err := fn()
	if err == nil {
		return nil
	}

	if !is403(err) {
		return mapError(ctx, err)
	}

	// 403: re-login once, skip if another goroutine already refreshed.
	c.mu.Lock()
	if c.token == usedToken {
		if tokenAge < reloginCooldown {
			// Fresh token denied: this is a policy denial, not an expired token.
			c.mu.Unlock()
			return fmt.Errorf("%w: denied with fresh token for path %q", ErrForbidden, path)
		}
		if loginErr := c.login(ctx); loginErr != nil {
			c.mu.Unlock()
			return loginErr
		}
	}
	c.mu.Unlock()

	err = fn()
	if err != nil {
		if is403(err) {
			return fmt.Errorf("%w: denied after re-login for path %q", ErrForbidden, path)
		}
		return mapError(ctx, err)
	}
	return nil
}

// login reads the service-account JWT from disk and authenticates against the
// kubernetes auth backend. Must be called with c.mu held.
func (c *client) login(ctx context.Context) error {
	jwt, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return fmt.Errorf("%w: reading service account token: %w", ErrForbidden, err)
	}

	secret, err := c.api.Logical().WriteWithContext(ctx,
		"auth/"+kubernetesAuthMount+"/login",
		map[string]any{
			"role": c.role,
			"jwt":  strings.TrimSpace(string(jwt)),
		})
	if err != nil {
		return mapLoginError(err)
	}

	if secret == nil || secret.Auth == nil {
		return fmt.Errorf("%w: login returned no auth data", ErrForbidden)
	}

	c.token = secret.Auth.ClientToken
	c.api.SetToken(c.token)
	c.loggedInAt = c.now()
	c.lease = time.Duration(secret.Auth.LeaseDuration) * time.Second
	return nil
}

// transient reports whether an HTTP status code indicates a transient error
// that should be retried.
func transient(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests
}

// mapError translates openbao errors into package sentinels.
func mapError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if errors.Is(err, api.ErrSecretNotFound) {
		return ErrNotFound
	}

	if respErr, ok := errors.AsType[*api.ResponseError](err); ok {
		switch {
		case respErr.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case respErr.StatusCode == http.StatusForbidden:
			return fmt.Errorf("%w (status %d): %w", ErrForbidden, respErr.StatusCode, err)
		case transient(respErr.StatusCode):
			return fmt.Errorf("%w (status %d): %w", ErrUnavailable, respErr.StatusCode, err)
		default:
			return fmt.Errorf("%w (status %d): %w", ErrRejected, respErr.StatusCode, err)
		}
	}

	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// mapLoginError translates errors from the kubernetes auth login request.
func mapLoginError(err error) error {
	if respErr, ok := errors.AsType[*api.ResponseError](err); ok {
		if respErr.StatusCode == http.StatusBadRequest || respErr.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w (login status %d): %w", ErrForbidden, respErr.StatusCode, err)
		}
		if transient(respErr.StatusCode) {
			return fmt.Errorf("%w (login status %d): %w", ErrUnavailable, respErr.StatusCode, err)
		}
		return fmt.Errorf("%w (login status %d): %w", ErrRejected, respErr.StatusCode, err)
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func is403(err error) bool {
	respErr, ok := errors.AsType[*api.ResponseError](err)
	return ok && respErr.StatusCode == http.StatusForbidden
}
