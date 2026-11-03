package secrets

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/obmondo/gfetch/pkg/telemetry"
)

const (
	testFieldName      = "private_key"
	testPEMValue       = "pem"
	testReasonUnavail  = "unavailable"
	testReasonRejected = "rejected"
)

// newTestServer starts an httptest.Server that simulates the OpenBao API.
// handler is called for each request. The server URL is returned for use as
// the OpenBao address.
func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// writeBody is a test helper that writes b to w, discarding the return values
// to satisfy errcheck.
func writeBody(w http.ResponseWriter, b []byte) {
	_, _ = w.Write(b)
}

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// kvResponse builds the JSON body for a successful KV v2 GET response.
func kvResponse(data map[string]any) []byte {
	body := map[string]any{
		"data": map[string]any{
			"data":     data,
			"metadata": map[string]any{"version": 1},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func loginResponseWithLease(token string, leaseDuration int) []byte {
	body := map[string]any{
		"auth": map[string]any{
			"client_token":   token,
			"lease_duration": leaseDuration,
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func loginResponse(token string) []byte {
	return loginResponseWithLease(token, 0)
}

func TestStore_LoginSendsRoleAndTrimmedJWT(t *testing.T) {
	tokenFile := writeTokenFile(t, "  my-jwt-token  \n")
	var gotRole, gotJWT string
	var gotHeaders http.Header

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			gotHeaders = r.Header.Clone()
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			gotRole = req["role"]
			gotJWT = req["jwt"]
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("test-token"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: "PEM"}))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// Set BAO_* env vars that should be ignored because api.NewConfig uses
	// DisableEnvironment:true internally.
	t.Setenv("BAO_TOKEN", "env-should-be-ignored")
	t.Setenv("BAO_NAMESPACE", "env-ns-should-be-ignored")
	t.Setenv("BAO_ADDR", "http://env-addr-should-be-ignored:9999")

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "my-role",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	data, err := store.Read(context.Background(), "test-repo", cfg, "ssh-keys/acme")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "PEM" {
		t.Errorf("got %q, want %q", data, "PEM")
	}
	if gotRole != "my-role" {
		t.Errorf("role = %q, want %q", gotRole, "my-role")
	}
	if gotJWT != "my-jwt-token" {
		t.Errorf("jwt = %q, want %q (trimmed)", gotJWT, "my-jwt-token")
	}
	// BAO_TOKEN / BAO_NAMESPACE env must not leak into login request headers.
	if v := gotHeaders.Get("X-Vault-Token"); v != "" {
		t.Errorf("X-Vault-Token header = %q, want empty (env must not leak)", v)
	}
	if v := gotHeaders.Get("X-Vault-Namespace"); v != "" {
		t.Errorf("X-Vault-Namespace header = %q, want empty (env must not leak)", v)
	}
}

func TestStore_TokenFileReReadPerLogin(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt-first")
	var lastJWT atomic.Value
	var logins atomic.Int32

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			logins.Add(1)
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			lastJWT.Store(req["jwt"])
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponseWithLease("tok", 90))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: "ok"}))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	fakeNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewStore()
	store.now = func() time.Time { return fakeNow }

	// First read: login reads jwt-first from disk.
	data, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("got %q, want %q", data, "ok")
	}
	if v := lastJWT.Load(); v != "jwt-first" {
		t.Errorf("first JWT = %q, want %q", v, "jwt-first")
	}

	// Rotate the token file, move past the 90s lease so the read itself logs
	// in again, and evict the path so the read reaches OpenBao.
	if err := os.WriteFile(tokenFile, []byte("jwt-second"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeNow = fakeNow.Add(91 * time.Second)
	store.Invalidate("path")

	data, err = store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("got %q, want %q", data, "ok")
	}
	if v := lastJWT.Load(); v != "jwt-second" {
		t.Errorf("last JWT = %q, want %q (re-read from disk)", v, "jwt-second")
	}
	if n := logins.Load(); n != 2 {
		t.Errorf("logins = %d, want 2", n)
	}
}

func TestStore_ReadReturnsField(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	var kvHits atomic.Int32
	var loginHits atomic.Int32
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			loginHits.Add(1)
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			kvHits.Add(1)
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: "-----BEGIN OPENSSH PRIVATE KEY-----\ndata\n-----END OPENSSH PRIVATE KEY-----\n"}))
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	data, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.HasPrefix(string(data), "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("expected PEM prefix, got %q", data)
	}
	want := string(data)
	// A caller writing into its copy must not reach the cache.
	data[0] = 'X'

	// Second read must return cached value without hitting OpenBao.
	data2, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if string(data2) != want {
		t.Errorf("second read returned different value: %q vs %q", data2, want)
	}
	data2[0] = 'Y'
	data3, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("third Read: %v", err)
	}
	if string(data3) != want {
		t.Errorf("third read returned different value: %q vs %q", data3, want)
	}
	if got := kvHits.Load(); got != 1 {
		t.Errorf("kv hits = %d, want 1 (second read should be cached)", got)
	}
	if got := loginHits.Load(); got != 1 {
		t.Errorf("login hits = %d, want 1", got)
	}
}

func TestStore_MissingField_ErrInvalidSecret(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{"other_field": "value"}))
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	_, err := store.Read(context.Background(), "repo", cfg, "path")
	if err == nil {
		t.Fatal("expected error for missing field")
	}
	if !strings.Contains(err.Error(), "not present") {
		t.Errorf("expected 'not present' in error, got %q", err)
	}
}

func TestStore_403FreshToken_ErrForbidden(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	var loginCalls atomic.Int32

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			loginCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusForbidden)
			writeBody(w, []byte(`{"errors":["permission denied"]}`))
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	_, err := store.Read(context.Background(), "repo", cfg, "path")
	if err == nil {
		t.Fatal("expected ErrForbidden")
	}
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("expected ErrForbidden, got %v", err)
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("expected 'denied' in error, got %q", err)
	}
	// Fresh token: should only login once (no 403 re-login attempt for fresh tokens).
	if n := loginCalls.Load(); n != 1 {
		t.Errorf("login called %d times, want exactly 1 (fresh token should not re-login)", n)
	}
}

type reloginRetryRow struct {
	name         string
	retryStatus  int // HTTP status for 3rd KV call (after 403 + re-login)
	wantErr      error
	leaseSeconds int           // token lease; 0 means none
	tokenAge     time.Duration // token age when the read starts; 0 means past reloginCooldown
	slowBy       time.Duration // how far the clock moves while OpenBao answers the 403
}

func runReloginRetryRow(t *testing.T, tt reloginRetryRow) {
	t.Helper()
	tokenFile := writeTokenFile(t, "jwt")
	var kvCalls atomic.Int32
	var loginCalls atomic.Int32

	// The handler moves the clock while the client reads it, so it is shared
	// under a mutex.
	var clockMu sync.Mutex
	fakeNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	advance := func(d time.Duration) {
		clockMu.Lock()
		fakeNow = fakeNow.Add(d)
		clockMu.Unlock()
	}

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			loginCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponseWithLease("fresh-token", tt.leaseSeconds))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			n := kvCalls.Add(1)
			switch n {
			case 1:
				w.WriteHeader(http.StatusOK)
				writeBody(w, kvResponse(map[string]any{testFieldName: "initial"}))
			case 2:
				advance(tt.slowBy)
				w.WriteHeader(http.StatusForbidden)
				writeBody(w, []byte(`{"errors":["permission denied"]}`))
			default:
				w.WriteHeader(tt.retryStatus)
				if tt.retryStatus == http.StatusOK {
					writeBody(w, kvResponse(map[string]any{testFieldName: "ok"}))
				} else {
					writeBody(w, []byte(`{"errors":["permission denied"]}`))
				}
			}
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	store.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return fakeNow
	}

	// First read populates cache.
	if _, err := store.Read(context.Background(), "repo", cfg, "path"); err != nil {
		t.Fatalf("initial read: %v", err)
	}

	// Age the token so that, by the 403, it is past its policy denial window
	// and the 403 triggers re-login.
	tokenAge := tt.tokenAge
	if tokenAge == 0 {
		tokenAge = 2 * time.Minute
	}
	advance(tokenAge)

	// Invalidate so next read hits OpenBao (which will 403 then retry).
	store.Invalidate("path")

	data, err := store.Read(context.Background(), "repo", cfg, "path")
	if tt.wantErr != nil {
		if !errors.Is(err, tt.wantErr) {
			t.Errorf("expected %v, got %v", tt.wantErr, err)
		}
	} else {
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if string(data) != "ok" {
			t.Errorf("got %q, want %q", data, "ok")
		}
	}
	if n := loginCalls.Load(); n != 2 {
		t.Errorf("login called %d times, want 2", n)
	}
}

func TestStore_403ThenReloginThen200(t *testing.T) {
	tests := []reloginRetryRow{
		{name: "retry succeeds", retryStatus: http.StatusOK},
		{name: "retry also 403", retryStatus: http.StatusForbidden, wantErr: ErrForbidden},
		// A 30s lease is renewed at 20s, so its token never reaches the one
		// minute cooldown; past a third of the lease the 403 still re-logs in.
		{name: "short lease retry succeeds", retryStatus: http.StatusOK, leaseSeconds: 30, tokenAge: 15 * time.Second},
		// The token is 5s old, inside the 10s window, when the read starts,
		// but the read takes 10s: at the 403 it is 15s old, so it re-logs in.
		{name: "slow read past the window retry succeeds", retryStatus: http.StatusOK, leaseSeconds: 30, tokenAge: 5 * time.Second, slowBy: 10 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runReloginRetryRow(t, tt)
		})
	}
}

func TestStore_NotFound(t *testing.T) {
	tests := []struct {
		name     string
		response func(w http.ResponseWriter)
	}{
		{
			name: "404 status",
			response: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNotFound)
				writeBody(w, []byte(`{"errors":[]}`))
			},
		},
		{
			name: "soft-deleted secret",
			response: func(w http.ResponseWriter) {
				// KV v2 returns 200 with data.data=null for soft-deleted secrets.
				w.WriteHeader(http.StatusOK)
				writeBody(w, []byte(`{"data":{"data":null,"metadata":{"created_time":"2026-01-01T00:00:00Z","deletion_time":"2026-06-01T00:00:00Z","destroyed":false,"version":2}}}`))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokenFile := writeTokenFile(t, "jwt")
			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
					w.WriteHeader(http.StatusOK)
					writeBody(w, loginResponse("tok"))
				case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
					tt.response(w)
				}
			})

			cfg := &OpenBaoConfig{
				Addr:                    ts.URL,
				Role:                    "r",
				ServiceAccountTokenPath: tokenFile,
				KVMount:                 "kv",
				Field:                   testFieldName,
			}

			store := NewStore()
			_, err := store.Read(context.Background(), "repo", cfg, "path")
			if err == nil {
				t.Fatal("expected ErrNotFound")
			}
		})
	}
}

func TestStore_5xx_ErrUnavailable(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	tests := []struct {
		code       int
		wantErr    error
		wantReason string
	}{
		{500, ErrUnavailable, testReasonUnavail},
		{503, ErrUnavailable, testReasonUnavail},
		{429, ErrUnavailable, testReasonUnavail},
		{400, ErrRejected, testReasonRejected},
		{405, ErrRejected, testReasonRejected},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.code), func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
					w.WriteHeader(http.StatusOK)
					writeBody(w, loginResponse("tok"))
				case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
					w.WriteHeader(tt.code)
					writeBody(w, []byte(`{"errors":["error"]}`))
				}
			})

			cfg := &OpenBaoConfig{
				Addr:                    ts.URL,
				Role:                    "r",
				ServiceAccountTokenPath: tokenFile,
				KVMount:                 "kv",
				Field:                   testFieldName,
			}

			before := testutil.ToFloat64(telemetry.SecretFetchFailuresTotal.WithLabelValues("status-repo-"+fmt.Sprint(tt.code), tt.wantReason))

			store := NewStore()
			_, err := store.Read(context.Background(), "status-repo-"+fmt.Sprint(tt.code), cfg, "path")
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected %v, got %v", tt.wantErr, err)
			}

			after := testutil.ToFloat64(telemetry.SecretFetchFailuresTotal.WithLabelValues("status-repo-"+fmt.Sprint(tt.code), tt.wantReason))
			if after != before+1 {
				t.Errorf("metric delta: got %v, want 1", after-before)
			}
		})
	}

}

func TestStore_503ThenSuccess_FailureNotCached(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	var kvCalls atomic.Int32
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			n := kvCalls.Add(1)
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				writeBody(w, []byte(`{"errors":["unavailable"]}`))
			} else {
				w.WriteHeader(http.StatusOK)
				writeBody(w, kvResponse(map[string]any{testFieldName: "ok"}))
			}
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	_, err := store.Read(context.Background(), "retry-repo", cfg, "path")
	if err == nil {
		t.Fatal("first read: expected error")
	}

	// Second read should succeed (failure was not cached).
	data, err := store.Read(context.Background(), "retry-repo", cfg, "path")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("got %q, want %q", data, "ok")
	}
}

func TestStore_Login404_ErrRejected(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusNotFound)
			writeBody(w, []byte(`{"errors":["not found"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	_, err := store.Read(context.Background(), "login-404-repo", cfg, "path")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrRejected) {
		t.Errorf("expected ErrRejected, got %v", err)
	}
}

func TestStore_ConnectionRefused_ErrUnavailable(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	cfg := &OpenBaoConfig{
		Addr:                    "http://127.0.0.1:1", // nothing listening
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	_, err := store.Read(context.Background(), "repo", cfg, "path")
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestStore_Timeout_ErrUnavailable(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		default:
			// Stall long enough for the 10s client timeout (we override it below).
			time.Sleep(5 * time.Second)
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	store.mu.Lock()
	c, err := store.clientForLocked(cfg)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Override the HTTP client timeout to something short.
	c.api.SetClientTimeout(100 * time.Millisecond)

	_, err = store.Read(context.Background(), "repo", cfg, "path")
	if err == nil {
		t.Fatal("expected error for timeout")
	}
}

// Actions run by TestStore_Invalidate between its first and last read.
const (
	invalidateActionPath             = "invalidate_path"
	invalidateActionOther            = "invalidate_other"
	invalidateActionOtherPath        = "other_path"
	invalidateActionConfigChanged    = "config_changed"
	invalidateActionConfigAlternated = "config_alternated"
)

type invalidateRow struct {
	name       string
	action     string // one of the invalidateAction constants
	wantKVHits int32
	wantLogins int32
}

func TestStore_Invalidate(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")

	tests := []invalidateRow{
		{
			name:       "invalidate path refetches",
			action:     invalidateActionPath,
			wantKVHits: 2,
			wantLogins: 1,
		},
		{
			name:       "invalidate other path does not evict",
			action:     invalidateActionOther,
			wantKVHits: 1,
			wantLogins: 1,
		},
		{
			name:       "other path reuses the client",
			action:     invalidateActionOtherPath,
			wantKVHits: 2,
			wantLogins: 1,
		},
		{
			name:       "changed config gets its own client and cache",
			action:     invalidateActionConfigChanged,
			wantKVHits: 2,
			wantLogins: 2,
		},
		{
			// Syncs on the old and the new config interleave after a reload:
			// each config logs in once and keeps its cached key.
			name:       "alternating configs keep their clients and caches",
			action:     invalidateActionConfigAlternated,
			wantKVHits: 2,
			wantLogins: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runInvalidateRow(t, tokenFile, tt)
		})
	}
}

func runInvalidateRow(t *testing.T, tokenFile string, tt invalidateRow) {
	t.Helper()
	var kvHits, loginHits atomic.Int32
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			loginHits.Add(1)
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse(fmt.Sprintf("tok-%d", loginHits.Load())))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			kvHits.Add(1)
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: testPEMValue}))
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()

	// First read to populate cache.
	if _, err := store.Read(context.Background(), "repo", cfg, "path/a"); err != nil {
		t.Fatalf("initial read: %v", err)
	}

	secondCfg, secondPath := cfg, "path/a"
	switch tt.action {
	case invalidateActionPath:
		store.Invalidate("path/a")
	case invalidateActionOther:
		store.Invalidate("path/b")
	case invalidateActionOtherPath:
		secondPath = "path/b"
	case invalidateActionConfigChanged, invalidateActionConfigAlternated:
		changed := *cfg
		changed.Role = "r2"
		secondCfg = &changed
	}
	if tt.action == invalidateActionConfigAlternated {
		for _, c := range []*OpenBaoConfig{secondCfg, cfg} {
			if _, err := store.Read(context.Background(), "repo", c, secondPath); err != nil {
				t.Fatalf("alternating read: %v", err)
			}
		}
	}

	// Second read.
	if _, err := store.Read(context.Background(), "repo", secondCfg, secondPath); err != nil {
		t.Fatalf("second read: %v", err)
	}

	if got := kvHits.Load(); got != tt.wantKVHits {
		t.Errorf("kv hits = %d, want %d", got, tt.wantKVHits)
	}
	if got := loginHits.Load(); got != tt.wantLogins {
		t.Errorf("login hits = %d, want %d", got, tt.wantLogins)
	}
}

type refetchRow struct {
	name          string
	refetchStatus int    // status of the second KV read, the refetch
	refetchValue  string // value the refetch finds
	wantErr       error
	wantCached    string // what a Read after the refetch returns
}

func runRefetchRow(t *testing.T, tt refetchRow) {
	t.Helper()
	tokenFile := writeTokenFile(t, "jwt")
	var kvHits atomic.Int32
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			if kvHits.Add(1) == 1 {
				w.WriteHeader(http.StatusOK)
				writeBody(w, kvResponse(map[string]any{testFieldName: testPEMValue}))
				return
			}
			w.WriteHeader(tt.refetchStatus)
			if tt.refetchStatus == http.StatusOK {
				writeBody(w, kvResponse(map[string]any{testFieldName: tt.refetchValue}))
			} else {
				writeBody(w, []byte(`{"errors":["unavailable"]}`))
			}
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	if _, err := store.Read(context.Background(), "repo", cfg, "path"); err != nil {
		t.Fatalf("initial read: %v", err)
	}

	data, err := store.Refetch(context.Background(), "repo", cfg, "path")
	switch {
	case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
		t.Fatalf("Refetch error = %v, want %v", err, tt.wantErr)
	case tt.wantErr == nil && err != nil:
		t.Fatalf("Refetch: %v", err)
	case tt.wantErr == nil && string(data) != tt.refetchValue:
		t.Errorf("Refetch = %q, want %q", data, tt.refetchValue)
	}

	cached, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("read after refetch: %v", err)
	}
	if string(cached) != tt.wantCached {
		t.Errorf("cached = %q, want %q", cached, tt.wantCached)
	}
	// The refetch always reads OpenBao; the read after it is served from the cache.
	if got := kvHits.Load(); got != 2 {
		t.Errorf("kv hits = %d, want 2", got)
	}
}

// TestStore_Refetch verifies that Refetch always reads OpenBao, replaces the
// cached value only when the fetched one differs, and leaves the cache in
// place when the fetch fails.
func TestStore_Refetch(t *testing.T) {
	const rotatedPEMValue = "rotated-pem"

	tests := []refetchRow{
		{name: "same key keeps the cache", refetchStatus: http.StatusOK, refetchValue: testPEMValue, wantCached: testPEMValue},
		{name: "rotated key replaces the cache", refetchStatus: http.StatusOK, refetchValue: rotatedPEMValue, wantCached: rotatedPEMValue},
		{name: "failed refetch keeps the cache", refetchStatus: http.StatusServiceUnavailable, wantErr: ErrUnavailable, wantCached: testPEMValue},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runRefetchRow(t, tt)
		})
	}
}

// TestStore_Invalidate_InFlightRace verifies that evicting or replacing a path
// while a read of it is in flight stops that read from caching what it
// fetched, and that doing so to another path does not.
// inFlightRaceSecondRead is what TestStore_Invalidate_InFlightRace's second
// OpenBao read of path/x returns.
const inFlightRaceSecondRead = "path/x-2"

func TestStore_Invalidate_InFlightRace(t *testing.T) {
	tests := []struct {
		name      string
		act       func(store *Store, cfg *OpenBaoConfig) error
		wantHits  int32  // OpenBao reads of path/x, in-flight read included
		wantFinal string // what a read of path/x returns afterwards
	}{
		{
			name:      "invalidate same path stops caching",
			act:       func(store *Store, _ *OpenBaoConfig) error { store.Invalidate("path/x"); return nil },
			wantHits:  2,
			wantFinal: inFlightRaceSecondRead,
		},
		{
			name:      "invalidate other path still caches",
			act:       func(store *Store, _ *OpenBaoConfig) error { store.Invalidate("path/y"); return nil },
			wantHits:  1,
			wantFinal: "path/x-1",
		},
		{
			// path/x is not cached yet, so the refetch fills an empty entry.
			// It must still stop the older in-flight read from caching.
			name: "changed key on same path is not overwritten",
			act: func(store *Store, cfg *OpenBaoConfig) error {
				_, err := store.Refetch(context.Background(), "repo", cfg, "path/x")
				return err
			},
			wantHits:  2,
			wantFinal: inFlightRaceSecondRead,
		},
		{
			// Another read of path/x finishes first and caches the newer
			// key; the older in-flight read must not overwrite it.
			name: "older read finishing last does not overwrite a newer cached key",
			act: func(store *Store, cfg *OpenBaoConfig) error {
				_, err := store.Read(context.Background(), "repo", cfg, "path/x")
				return err
			},
			wantHits:  2,
			wantFinal: inFlightRaceSecondRead,
		},
		{
			name: "changed key on other path still caches",
			act: func(store *Store, cfg *OpenBaoConfig) error {
				_, err := store.Refetch(context.Background(), "repo", cfg, "path/y")
				return err
			},
			wantHits:  1,
			wantFinal: "path/x-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runInFlightRaceRow(t, tt.act, tt.wantHits, tt.wantFinal)
		})
	}
}

// runInFlightRaceRow blocks the first OpenBao read of path/x, runs act while
// it is in flight, then releases it. Each OpenBao read of a path returns
// "<path>-<n>" for its n-th read, so a changed key is always found.
func runInFlightRaceRow(t *testing.T, act func(*Store, *OpenBaoConfig) error, wantHits int32, wantFinal string) {
	t.Helper()
	tokenFile := writeTokenFile(t, "jwt")
	gate := make(chan struct{})
	var hitsMu sync.Mutex
	hits := map[string]int32{}

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			path := strings.TrimPrefix(r.URL.Path, "/v1/kv/data/")
			hitsMu.Lock()
			hits[path]++
			n := hits[path]
			hitsMu.Unlock()
			if path == "path/x" && n == 1 {
				<-gate // block the first read of path/x
			}
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: fmt.Sprintf("%s-%d", path, n)}))
		}
	})
	xHits := func() int32 {
		hitsMu.Lock()
		defer hitsMu.Unlock()
		return hits["path/x"]
	}

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	// path/y is cached up front, so a refetch of it finds a changed key.
	if _, err := store.Read(context.Background(), "repo", cfg, "path/y"); err != nil {
		t.Fatalf("path/y read: %v", err)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = store.Read(context.Background(), "repo", cfg, "path/x")
	})

	// Wait for the blocked request to be in-flight.
	deadline := time.After(5 * time.Second)
	for xHits() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for in-flight request")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	if err := act(store, cfg); err != nil {
		t.Fatalf("act: %v", err)
	}

	// Release the blocked request.
	close(gate)
	wg.Wait()

	data, err := store.Read(context.Background(), "repo", cfg, "path/x")
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if string(data) != wantFinal {
		t.Errorf("final read = %q, want %q", data, wantFinal)
	}
	if got := xHits(); got != wantHits {
		t.Errorf("path/x kv hits = %d, want %d", got, wantHits)
	}
}

// TestStore_ConfigChangeDuringInFlightRead verifies that a read in flight on
// the old config when a read on a changed config starts caches only under the
// old config: neither read's key replaces the other's, and each config keeps
// its own client.
func TestStore_ConfigChangeDuringInFlightRead(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	var oldKVHits, newKVHits atomic.Int32
	gate := make(chan struct{})

	kvServer := func(value string, hits *atomic.Int32, block bool) *httptest.Server {
		return newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
				w.WriteHeader(http.StatusOK)
				writeBody(w, loginResponse("tok"))
			case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
				hits.Add(1)
				if block {
					<-gate
				}
				w.WriteHeader(http.StatusOK)
				writeBody(w, kvResponse(map[string]any{testFieldName: value}))
			}
		})
	}
	oldTS := kvServer("old-key", &oldKVHits, true)
	newTS := kvServer("new-key", &newKVHits, false)

	oldCfg := &OpenBaoConfig{Addr: oldTS.URL, Role: "r", ServiceAccountTokenPath: tokenFile, KVMount: "kv", Field: testFieldName}
	newCfg := *oldCfg
	newCfg.Addr = newTS.URL

	store := NewStore()

	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = store.Read(context.Background(), "repo", oldCfg, "path/x")
	})

	deadline := time.After(5 * time.Second)
	for oldKVHits.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for in-flight request")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Read on the changed config while the old read is in flight.
	data, err := store.Read(context.Background(), "repo", &newCfg, "path/x")
	if err != nil {
		t.Fatalf("new-config read: %v", err)
	}
	if string(data) != "new-key" {
		t.Fatalf("new-config read = %q, want %q", data, "new-key")
	}

	close(gate)
	wg.Wait()

	// The in-flight old read must not have overwritten the cache.
	data, err = store.Read(context.Background(), "repo", &newCfg, "path/x")
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if string(data) != "new-key" {
		t.Errorf("final read = %q, want %q (old backend key cached)", data, "new-key")
	}
	if got := newKVHits.Load(); got != 1 {
		t.Errorf("new backend kv hits = %d, want 1 (final read served from cache)", got)
	}

	// The old config's read cached its own key.
	data, err = store.Read(context.Background(), "repo", oldCfg, "path/x")
	if err != nil {
		t.Fatalf("old-config read: %v", err)
	}
	if string(data) != "old-key" {
		t.Errorf("old-config read = %q, want %q", data, "old-key")
	}
	if got := oldKVHits.Load(); got != 1 {
		t.Errorf("old backend kv hits = %d, want 1 (served from its cache)", got)
	}
}

func TestStore_CancelledContext(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: testPEMValue}))
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := store.Read(ctx, "repo", cfg, "path")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestStore_ConcurrentFirstReads_OneLogin(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	var loginCalls atomic.Int32

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			loginCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: testPEMValue}))
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_, _ = store.Read(context.Background(), "repo", cfg, "path")
		}()
	}
	wg.Wait()

	// The server never returns 403, so exactly one login should happen.
	// Subsequent goroutines find the token already set under the mutex.
	if got := loginCalls.Load(); got != 1 {
		t.Errorf("login called %d times, want exactly 1", got)
	}
}

// A missing or unreadable token file is transient (the kubelet rotates it), not
// a denial.
func TestStore_TokenFileMissing_ErrUnavailable(t *testing.T) {
	cfg := &OpenBaoConfig{
		Addr:                    "http://localhost:8200",
		Role:                    "r",
		ServiceAccountTokenPath: "/nonexistent/token/path",
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	_, err := store.Read(context.Background(), "repo", cfg, "path")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable for missing token file, got %v", err)
	}
}

func TestStore_KeyBytesNeverInSlogOutput(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	secretPEM := "-----BEGIN OPENSSH PRIVATE KEY-----\nSUPERSECRET\n-----END OPENSSH PRIVATE KEY-----\n"

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: secretPEM}))
		}
	})

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	// Capture slog output.
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(old) })

	store := NewStore()
	data, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != secretPEM {
		t.Errorf("data mismatch")
	}

	logged := buf.String()
	if strings.Contains(logged, "SUPERSECRET") {
		t.Errorf("key bytes leaked into slog output: %s", logged)
	}
	if strings.Contains(logged, "BEGIN OPENSSH") {
		t.Errorf("PEM header leaked into slog output: %s", logged)
	}
}

// TestStore_FetchFailuresMetric verifies that a failed read returns the
// matching sentinel and is counted under its reason label, including a client
// that cannot be built from the config.
func TestStore_FetchFailuresMetric(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusNotFound)
			writeBody(w, []byte(`{"errors":[]}`))
		}
	})

	tests := []struct {
		name    string
		addr    string
		wantErr error
		reason  string
	}{
		{name: "secret not found", addr: ts.URL, wantErr: ErrNotFound, reason: "not_found"},
		{name: "address that does not parse", addr: "http://[::1", wantErr: ErrClientConfig, reason: "config_invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &OpenBaoConfig{
				Addr:                    tt.addr,
				Role:                    "r",
				ServiceAccountTokenPath: tokenFile,
				KVMount:                 "kv",
				Field:                   testFieldName,
			}

			before := testutil.ToFloat64(telemetry.SecretFetchFailuresTotal.WithLabelValues("metric-repo", tt.reason))

			_, err := NewStore().Read(context.Background(), "metric-repo", cfg, "path")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Read error = %v, want %v", err, tt.wantErr)
			}

			after := testutil.ToFloat64(telemetry.SecretFetchFailuresTotal.WithLabelValues("metric-repo", tt.reason))
			if after != before+1 {
				t.Errorf("SecretFetchFailuresTotal{reason=%q}: got %v, want %v", tt.reason, after, before+1)
			}
		})
	}
}

// tokenRenewalHarness serves logins and KV reads for TestStore_TokenRenewal.
// Login n answers loginStatus[n] (200 by default) with token "tok-<n>", and
// blocks until loginGate[n] is closed when one is set. The clock is shared
// with the background renewal, so it is guarded.
type tokenRenewalHarness struct {
	t     *testing.T
	store *Store
	cfg   *OpenBaoConfig

	mu          sync.Mutex
	clock       time.Time
	logins      int
	loginStatus map[int]int
	loginGate   map[int]chan struct{}
	kvToken     string
}

func newTokenRenewalHarness(t *testing.T, leaseSeconds int) *tokenRenewalHarness {
	t.Helper()
	h := &tokenRenewalHarness{
		t:           t,
		clock:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		loginStatus: map[int]int{},
		loginGate:   map[int]chan struct{}{},
	}
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			h.mu.Lock()
			h.logins++
			n := h.logins
			status := cmp.Or(h.loginStatus[n], http.StatusOK)
			gate := h.loginGate[n]
			h.mu.Unlock()
			if gate != nil {
				<-gate
			}
			w.WriteHeader(status)
			if status != http.StatusOK {
				writeBody(w, []byte(`{"errors":["unavailable"]}`))
				return
			}
			writeBody(w, loginResponseWithLease(fmt.Sprintf("tok-%d", n), leaseSeconds))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			h.mu.Lock()
			h.kvToken = r.Header.Get("X-Vault-Token")
			h.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: testPEMValue}))
		}
	})
	h.cfg = &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: writeTokenFile(t, "jwt"),
		KVMount:                 "kv",
		Field:                   testFieldName,
	}
	h.store = NewStore()
	h.store.now = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.clock
	}
	return h
}

func (h *tokenRenewalHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
}

// blockLogin makes login n wait until the returned func is called; the
// test's cleanup releases it too.
func (h *tokenRenewalHarness) blockLogin(n int) func() {
	gate := make(chan struct{})
	h.mu.Lock()
	h.loginGate[n] = gate
	h.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	h.t.Cleanup(release)
	return release
}

func (h *tokenRenewalHarness) failLogin(n, status int) {
	h.mu.Lock()
	h.loginStatus[n] = status
	h.mu.Unlock()
}

func (h *tokenRenewalHarness) loginCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logins
}

// read evicts the path and reads it, so it reaches OpenBao, and returns the
// token the KV request carried. It fails the test if the read waits longer
// than a second, the bound for a read that must not wait on a login.
func (h *tokenRenewalHarness) read() (string, error) {
	h.t.Helper()
	h.store.Invalidate("path/renew")
	done := make(chan error, 1)
	go func() {
		_, err := h.store.Read(context.Background(), "repo", h.cfg, "path/renew")
		done <- err
	}()
	select {
	case err := <-done:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.kvToken, err
	case <-time.After(time.Second):
		h.t.Fatal("read waited on a login")
		return "", nil
	}
}

// client returns the store's client for the harness config.
func (h *tokenRenewalHarness) client() *client {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return h.store.clients[*h.cfg]
}

func (h *tokenRenewalHarness) renewing() bool {
	c := h.client()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.renewing
}

// waitFor polls cond until it holds, failing the test after five seconds.
func (h *tokenRenewalHarness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *tokenRenewalHarness) mustRead(wantToken string) {
	h.t.Helper()
	token, err := h.read()
	if err != nil {
		h.t.Fatalf("read: %v", err)
	}
	if token != wantToken {
		h.t.Fatalf("read used token %q, want %q", token, wantToken)
	}
}

// TestStore_TokenRenewal verifies that a token past 2/3 of its lease is
// renewed in the background while reads keep using it, that a failed renewal
// keeps it and is retried once the retry window passes, and that only a
// missing or expired token makes a read wait for a login.
func TestStore_TokenRenewal(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{name: "no renewal before two thirds of the lease", run: func(t *testing.T) {
			h := newTokenRenewalHarness(t, 90)
			h.mustRead("tok-1")
			h.advance(59 * time.Second)
			h.mustRead("tok-1")
			if h.renewing() || h.loginCount() != 1 {
				t.Errorf("renewing = %v, logins = %d, want no renewal", h.renewing(), h.loginCount())
			}
		}},
		{name: "non-expiring token is never renewed", run: func(t *testing.T) {
			h := newTokenRenewalHarness(t, 0)
			h.mustRead("tok-1")
			h.advance(24 * time.Hour)
			h.mustRead("tok-1")
			if h.renewing() || h.loginCount() != 1 {
				t.Errorf("renewing = %v, logins = %d, want no renewal", h.renewing(), h.loginCount())
			}
		}},
		{name: "reads keep the current token while the renewal runs", run: func(t *testing.T) {
			h := newTokenRenewalHarness(t, 90)
			h.mustRead("tok-1")
			release := h.blockLogin(2)
			h.advance(61 * time.Second)
			h.mustRead("tok-1")
			h.waitFor("the renewal login", func() bool { return h.loginCount() == 2 })
			// A second read while the renewal is blocked neither waits nor
			// starts another login.
			h.mustRead("tok-1")
			release()
			h.waitFor("the renewal to finish", func() bool { return !h.renewing() })
			h.mustRead("tok-2")
			if n := h.loginCount(); n != 2 {
				t.Errorf("logins = %d, want 2", n)
			}
		}},
		{name: "failed renewal keeps the token and is retried after the window", run: func(t *testing.T) {
			// A 600s lease renews from 400s and retries a failure once a
			// minute, well before it expires.
			h := newTokenRenewalHarness(t, 600)
			h.failLogin(2, http.StatusServiceUnavailable)
			h.mustRead("tok-1")
			h.advance(401 * time.Second)
			h.mustRead("tok-1")
			h.waitFor("the failed renewal", func() bool { return h.loginCount() == 2 && !h.renewing() })
			h.mustRead("tok-1")
			if h.renewing() || h.loginCount() != 2 {
				t.Fatalf("renewing = %v, logins = %d, want no retry inside the window", h.renewing(), h.loginCount())
			}
			h.advance(time.Minute)
			h.mustRead("tok-1")
			h.waitFor("the retried renewal", func() bool { return h.loginCount() == 3 && !h.renewing() })
			h.mustRead("tok-3")
		}},
		{name: "expired token logs in before the read", run: func(t *testing.T) {
			h := newTokenRenewalHarness(t, 90)
			h.mustRead("tok-1")
			h.advance(91 * time.Second)
			h.mustRead("tok-2")
		}},
		{name: "failed login on an expired token fails the read", run: func(t *testing.T) {
			h := newTokenRenewalHarness(t, 90)
			h.failLogin(2, http.StatusServiceUnavailable)
			h.mustRead("tok-1")
			h.advance(91 * time.Second)
			if _, err := h.read(); !errors.Is(err, ErrUnavailable) {
				t.Errorf("read error = %v, want ErrUnavailable", err)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// readSharedForSharedLogin evicts and reads the path TestStore_SharedLogin uses,
// so the read reaches the client.
func readSharedForSharedLogin(ctx context.Context, h *tokenRenewalHarness) error {
	h.store.Invalidate("path/shared")
	_, err := h.store.Read(ctx, "repo", h.cfg, "path/shared")
	return err
}

// readConcurrentlyForSharedLogin starts n reads of the shared path, waits
// until the first login has started, runs release, and returns every read's
// error.
func readConcurrentlyForSharedLogin(h *tokenRenewalHarness, n int, release func()) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			errs[i] = readSharedForSharedLogin(context.Background(), h)
		}()
	}
	h.waitFor("the first login", func() bool { return h.loginCount() == 1 })
	release()
	wg.Wait()
	return errs
}

// TestStore_SharedLogin verifies that reads without a token share one login,
// that a failed login is answered from memory for loginFailureBackoff instead
// of being retried by every read, and that a read waiting for a login gives up
// at its own deadline.
func TestStore_SharedLogin(t *testing.T) {
	const readers = 5
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{name: "concurrent reads share one successful login", run: func(t *testing.T) {
			h := newTokenRenewalHarness(t, 90)
			release := h.blockLogin(1)
			for i, err := range readConcurrentlyForSharedLogin(h, readers, release) {
				if err != nil {
					t.Errorf("read %d: %v", i, err)
				}
			}
			if n := h.loginCount(); n != 1 {
				t.Errorf("logins = %d, want 1", n)
			}
		}},
		{name: "concurrent reads share one failed login, which is not retried until the backoff ends", run: func(t *testing.T) {
			h := newTokenRenewalHarness(t, 90)
			for n := 1; n <= readers; n++ {
				h.failLogin(n, http.StatusServiceUnavailable)
			}
			release := h.blockLogin(1)
			for i, err := range readConcurrentlyForSharedLogin(h, readers, release) {
				if !errors.Is(err, ErrUnavailable) {
					t.Errorf("read %d error = %v, want ErrUnavailable", i, err)
				}
			}
			if n := h.loginCount(); n != 1 {
				t.Fatalf("logins = %d, want 1", n)
			}
			// Inside the backoff the failure is answered without a login.
			if err := readSharedForSharedLogin(context.Background(), h); !errors.Is(err, ErrUnavailable) {
				t.Errorf("read inside the backoff error = %v, want ErrUnavailable", err)
			}
			if n := h.loginCount(); n != 1 {
				t.Fatalf("logins inside the backoff = %d, want 1", n)
			}
			// Past it the next read logs in again.
			h.advance(loginFailureBackoff)
			h.failLogin(2, http.StatusOK)
			h.mustRead("tok-2")
		}},
		{name: "a read waiting for a slow login returns at its deadline", run: func(t *testing.T) {
			h := newTokenRenewalHarness(t, 90)
			release := h.blockLogin(1)
			firstDone := make(chan error, 1)
			go func() { firstDone <- readSharedForSharedLogin(context.Background(), h) }()
			h.waitFor("the first login", func() bool { return h.loginCount() == 1 })

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := readSharedForSharedLogin(ctx, h)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("read error = %v, want context.DeadlineExceeded", err)
			}
			if waited := time.Since(start); waited > time.Second {
				t.Errorf("read waited %v for a login past its deadline", waited)
			}

			release()
			if err := <-firstDone; err != nil {
				t.Fatalf("first read: %v", err)
			}
			if n := h.loginCount(); n != 1 {
				t.Errorf("logins = %d, want 1", n)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
