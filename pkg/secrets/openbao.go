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
// lazy kubernetes-auth login, renewal in the background from 2/3 of the
// lease, and one automatic re-login on 403 (rate-limited to reloginCooldown).
//
// api is shared by every read and by the logins: api.Client guards its token
// with its own lock (SetToken writes it, each request reads it), so swapping
// the token while reads are in flight is safe.
type client struct {
	api       *api.Client
	kv        *api.KVv2
	role      string
	tokenPath string

	mu         sync.Mutex
	token      string
	loggedInAt time.Time
	lease      time.Duration
	// renewing is set while a background renewal runs. lastRenewAttempt is
	// when the last one started, so a failing renewal is retried at most once
	// per policyDenialWindow instead of on every read.
	renewing         bool
	lastRenewAttempt time.Time
	now              func() time.Time
	logger           *slog.Logger
}

func newClient(cfg *OpenBaoConfig, now func() time.Time, logger *slog.Logger) (*client, error) {
	apiCfg := api.NewConfig()
	if apiCfg.Error != nil {
		return nil, fmt.Errorf("%w: %w", ErrClientConfig, apiCfg.Error)
	}
	apiCfg.Address = cfg.Addr
	apiCfg.MaxRetries = 0
	apiCfg.Timeout = requestTimeout

	ac, err := api.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("%w: creating openbao client: %w", ErrClientConfig, err)
	}

	return &client{
		api:       ac,
		kv:        ac.KVv2(cfg.KVMount),
		role:      cfg.Role,
		tokenPath: cfg.ServiceAccountTokenPath,
		now:       now,
		logger:    logger,
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

// policyDenialWindow is how young a token must be for a 403 on it to read as
// a policy denial rather than an expired or revoked token: reloginCooldown,
// shortened to the first third of the lease when token_ttl is short. A token
// is renewed at 2/3 of its lease, so with a fixed minute and a ttl under 90s
// every token would read as fresh and a 403 would never get its re-login. Must
// be called with c.mu held.
func (c *client) policyDenialWindow() time.Duration {
	if c.lease > 0 && c.lease/renewFractionDenom < reloginCooldown {
		return c.lease / renewFractionDenom
	}
	return reloginCooldown
}

// expired reports whether the token's lease has fully elapsed. Must be called
// with c.mu held.
func (c *client) expired() bool {
	return c.lease > 0 && !c.now().Before(c.loggedInAt.Add(c.lease))
}

// ensureToken makes sure there is a token to read with. Without one, or with
// an expired one, it logs in itself, and readers queued on c.mu wait for that
// one login. A token past 2/3 of its lease is renewed in the background while
// this read goes ahead with it, so a slow login never stalls reads that still
// hold a valid token. Must be called with c.mu held.
func (c *client) ensureToken(ctx context.Context) error {
	if c.token == "" || c.expired() {
		return c.login(ctx)
	}
	if c.renewDue() {
		c.startRenewal()
	}
	return nil
}

// startRenewal starts a background renewal unless one is running or the last
// one started less than policyDenialWindow ago. Must be called with c.mu held.
func (c *client) startRenewal() {
	now := c.now()
	if c.renewing || (!c.lastRenewAttempt.IsZero() && now.Sub(c.lastRenewAttempt) < c.policyDenialWindow()) {
		return
	}
	c.renewing = true
	c.lastRenewAttempt = now
	go c.renew()
}

// renew logs in without holding c.mu, then swaps the new token in. A failure
// keeps the current token, which stays usable until its lease ends; the next
// read past the retry window starts another renewal.
func (c *client) renew() {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	token, lease, err := c.requestToken(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.renewing = false
	if err != nil {
		c.logger.Warn("openbao token renewal failed, using current token", "error", err)
		return
	}
	c.setToken(token, lease)
}

// do ensures a valid token exists, runs fn, and retries once after a re-login
// on 403. Renewal from 2/3 of the lease prevents most 403s. A 403 on a token
// still younger than policyDenialWindow when the 403 arrives is treated as a
// policy denial and returns ErrForbidden without attempting re-login. Logins
// made for a read hold c.mu, so concurrent readers do not log in twice.
func (c *client) do(ctx context.Context, path string, fn func() error) error {
	c.mu.Lock()
	if err := c.ensureToken(ctx); err != nil {
		c.mu.Unlock()
		return err
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

	// 403: re-login once, skip if another goroutine or a background renewal
	// already swapped the token. The token's age is taken here, at the 403
	// rather than before fn, so a slow request does not make an expired token
	// look fresh. loggedInAt still belongs to usedToken, since only a login
	// moves it.
	c.mu.Lock()
	if c.token == usedToken {
		if c.now().Sub(c.loggedInAt) < c.policyDenialWindow() {
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

// login logs in and swaps the new token in. Must be called with c.mu held.
func (c *client) login(ctx context.Context) error {
	token, lease, err := c.requestToken(ctx)
	if err != nil {
		return err
	}
	c.setToken(token, lease)
	return nil
}

// requestToken reads the service-account JWT from disk and authenticates
// against the kubernetes auth backend, returning the new token and its lease.
// It touches no client state, so it runs without c.mu.
func (c *client) requestToken(ctx context.Context) (string, time.Duration, error) {
	// The kubelet rotates the projected token file, so a failed read is
	// treated as transient rather than as a denial.
	jwt, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", 0, fmt.Errorf("%w: reading service account token: %w", ErrUnavailable, err)
	}

	secret, err := c.api.Logical().WriteWithContext(ctx,
		"auth/"+kubernetesAuthMount+"/login",
		map[string]any{
			"role": c.role,
			"jwt":  strings.TrimSpace(string(jwt)),
		})
	if err != nil {
		return "", 0, mapLoginError(err)
	}

	if secret == nil || secret.Auth == nil {
		return "", 0, fmt.Errorf("%w: login returned no auth data", ErrForbidden)
	}
	return secret.Auth.ClientToken, time.Duration(secret.Auth.LeaseDuration) * time.Second, nil
}

// setToken makes token the one reads use. Must be called with c.mu held.
func (c *client) setToken(token string, lease time.Duration) {
	c.token = token
	c.api.SetToken(token)
	c.loggedInAt = c.now()
	c.lease = lease
}

// transient reports whether an HTTP status code indicates a transient error
// that should be retried.
func transient(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests
}

// statusError maps an HTTP status code to the appropriate package sentinel,
// wrapping both the sentinel and the original error. prefix distinguishes
// read-path errors ("status") from login-path errors ("login status").
func statusError(code int, prefix string, err error) error {
	switch {
	case code == http.StatusForbidden:
		return fmt.Errorf("%w (%s %d): %w", ErrForbidden, prefix, code, err)
	case transient(code):
		return fmt.Errorf("%w (%s %d): %w", ErrUnavailable, prefix, code, err)
	default:
		return fmt.Errorf("%w (%s %d): %w", ErrRejected, prefix, code, err)
	}
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
		if respErr.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		return statusError(respErr.StatusCode, "status", err)
	}

	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// mapLoginError translates errors from the kubernetes auth login request.
func mapLoginError(err error) error {
	if respErr, ok := errors.AsType[*api.ResponseError](err); ok {
		if respErr.StatusCode == http.StatusBadRequest {
			return fmt.Errorf("%w (login status %d): %w", ErrForbidden, respErr.StatusCode, err)
		}
		return statusError(respErr.StatusCode, "login status", err)
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func is403(err error) bool {
	respErr, ok := errors.AsType[*api.ResponseError](err)
	return ok && respErr.StatusCode == http.StatusForbidden
}
