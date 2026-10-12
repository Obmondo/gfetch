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
)

// client wraps an openbao API client for a single OpenBaoConfig. It handles
// lazy kubernetes-auth login and one automatic re-login on 403.
type client struct {
	api       *api.Client
	kv        *api.KVv2
	role      string
	tokenPath string

	mu    sync.Mutex
	token string
}

func newClient(cfg *OpenBaoConfig) (*client, error) {
	apiCfg := api.DefaultConfig()
	if apiCfg.Error != nil {
		return nil, fmt.Errorf("secrets: openbao default config: %w", apiCfg.Error)
	}
	apiCfg.Address = cfg.Addr
	apiCfg.MaxRetries = 0
	apiCfg.Timeout = requestTimeout

	ac, err := api.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("secrets: creating openbao client: %w", err)
	}
	ac.ClearToken()

	return &client{
		api:       ac,
		kv:        ac.KVv2(cfg.KVMount),
		role:      cfg.Role,
		tokenPath: cfg.ServiceAccountTokenPath,
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

// do ensures a valid token exists, runs fn, and retries once after a re-login
// on 403. The mutex prevents multiple goroutines from logging in concurrently.
func (c *client) do(ctx context.Context, path string, fn func() error) error {
	c.mu.Lock()
	if c.token == "" {
		if err := c.login(ctx); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	usedToken := c.token
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
		if loginErr := c.login(ctx); loginErr != nil {
			c.mu.Unlock()
			return loginErr
		}
	}
	c.mu.Unlock()

	err = fn()
	if err != nil {
		if is403(err) {
			slog.Error("openbao access denied after re-login", "path", path)
			return ErrForbidden
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
		slog.Error("failed to read service account token file", "path", c.tokenPath, "error", err)
		return ErrForbidden
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
		slog.Error("openbao login returned no auth data")
		return ErrForbidden
	}

	c.token = secret.Auth.ClientToken
	c.api.SetToken(c.token)
	return nil
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
		switch respErr.StatusCode {
		case http.StatusNotFound:
			return ErrNotFound
		case http.StatusForbidden:
			slog.Error("openbao access denied", "error", err)
			return ErrForbidden
		default:
			slog.Error("openbao request error", "error", err, "status", respErr.StatusCode)
			return ErrUnavailable
		}
	}

	slog.Error("openbao connection error", "error", err)
	return ErrUnavailable
}

// mapLoginError translates errors from the kubernetes auth login request.
func mapLoginError(err error) error {
	if respErr, ok := errors.AsType[*api.ResponseError](err); ok {
		if respErr.StatusCode == http.StatusBadRequest || respErr.StatusCode == http.StatusForbidden {
			slog.Error("openbao login rejected", "error", err)
			return ErrForbidden
		}
		slog.Error("openbao login server error", "error", err)
		return ErrUnavailable
	}
	slog.Error("openbao login connection error", "error", err)
	return ErrUnavailable
}

func is403(err error) bool {
	var respErr *api.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusForbidden
}
