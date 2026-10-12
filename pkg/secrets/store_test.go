package secrets

import (
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
	testFieldName = "private_key"
	testCachedPEM = "cached-pem"
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

func loginResponse(token string) []byte {
	body := map[string]any{
		"auth": map[string]any{
			"client_token": token,
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func TestStore_LoginSendsRoleAndTrimmedJWT(t *testing.T) {
	tokenFile := writeTokenFile(t, "  my-jwt-token  \n")
	var gotRole, gotJWT string

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
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
}

func TestStore_TokenFileReReadPerLogin(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt-first")
	var loginCount atomic.Int32
	var lastJWT atomic.Value

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			lastJWT.Store(req["jwt"])
			n := loginCount.Add(1)
			if n == 1 {
				// First login succeeds.
				w.WriteHeader(http.StatusOK)
				writeBody(w, loginResponse("tok1"))
			} else {
				// Second login also succeeds.
				w.WriteHeader(http.StatusOK)
				writeBody(w, loginResponse("tok2"))
			}
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			// First call returns 403 to trigger re-login; second succeeds.
			if loginCount.Load() < 2 {
				w.WriteHeader(http.StatusForbidden)
				writeBody(w, []byte(`{"errors":["permission denied"]}`))
			} else {
				w.WriteHeader(http.StatusOK)
				writeBody(w, kvResponse(map[string]any{testFieldName: "ok"}))
			}
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

	// Rewrite the token file before the read that will trigger re-login.
	if err := os.WriteFile(tokenFile, []byte("jwt-second"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewStore()
	data, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("got %q, want %q", data, "ok")
	}
	if v := lastJWT.Load(); v != "jwt-second" {
		t.Errorf("last JWT = %q, want %q (re-read from disk)", v, "jwt-second")
	}
}

func TestStore_ReadReturnsField(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
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

func TestStore_403ThenReloginThen200(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	var kvCalls atomic.Int32

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("fresh-token"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			n := kvCalls.Add(1)
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				writeBody(w, []byte(`{"errors":["permission denied"]}`))
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
	data, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("got %q, want %q", data, "ok")
	}
}

func TestStore_403Twice_ErrForbidden(t *testing.T) {
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
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("expected 'denied' in error, got %q", err)
	}
	// Login should be called at most 2 times (initial + re-login).
	if n := loginCalls.Load(); n > 2 {
		t.Errorf("login called %d times, want at most 2", n)
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

			// Seed the cache so we can verify eviction.
			store.mu.Lock()
			store.cache[cacheKey{cfg: *cfg, path: "path"}] = []byte("stale-key")
			store.mu.Unlock()

			_, err := store.Read(context.Background(), "repo", cfg, "path")
			if err == nil {
				t.Fatal("expected ErrNotFound")
			}

			// Cache must be evicted on NotFound.
			store.mu.Lock()
			_, cached := store.cache[cacheKey{cfg: *cfg, path: "path"}]
			store.mu.Unlock()
			if cached {
				t.Error("expected cache to be evicted after NotFound")
			}
		})
	}
}

func TestStore_5xx_ErrUnavailable(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	for _, code := range []int{500, 503} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
					w.WriteHeader(http.StatusOK)
					writeBody(w, loginResponse("tok"))
				case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
					w.WriteHeader(code)
					writeBody(w, []byte(`{"errors":["internal"]}`))
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
				t.Fatal("expected error")
			}
		})
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
	c, err := store.clientFor(cfg)
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

func newFallbackServer(t *testing.T, responses []int) *httptest.Server {
	t.Helper()
	var callIdx atomic.Int32
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			idx := int(callIdx.Add(1)) - 1
			if idx >= len(responses) {
				idx = len(responses) - 1
			}
			code := responses[idx]
			if code == 200 {
				w.WriteHeader(http.StatusOK)
				writeBody(w, kvResponse(map[string]any{testFieldName: testCachedPEM}))
			} else {
				w.WriteHeader(code)
				writeBody(w, []byte(`{"errors":["error"]}`))
			}
		}
	})
	return ts
}

func TestStore_FallbackCache_SuccessThenUnavailableReturnsCached(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newFallbackServer(t, []int{200, 503})
	cfg := &OpenBaoConfig{
		Addr: ts.URL, Role: "r", ServiceAccountTokenPath: tokenFile,
		KVMount: "kv", Field: testFieldName,
	}

	store := NewStore()
	before := testutil.ToFloat64(telemetry.SecretCachedKeyUsedTotal.WithLabelValues("fb-repo"))

	data, err := store.Read(context.Background(), "fb-repo", cfg, "path")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(data) != testCachedPEM {
		t.Errorf("first read: got %q, want %q", data, testCachedPEM)
	}

	data, err = store.Read(context.Background(), "fb-repo", cfg, "path")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if string(data) != testCachedPEM {
		t.Errorf("second read: got %q, want %q", data, testCachedPEM)
	}

	after := testutil.ToFloat64(telemetry.SecretCachedKeyUsedTotal.WithLabelValues("fb-repo"))
	if after <= before {
		t.Error("expected SecretCachedKeyUsedTotal to increment")
	}
}

func TestStore_FallbackCache_UnavailableNoCacheReturnsError(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newFallbackServer(t, []int{503})
	cfg := &OpenBaoConfig{
		Addr: ts.URL, Role: "r", ServiceAccountTokenPath: tokenFile,
		KVMount: "kv", Field: testFieldName,
	}

	store := NewStore()
	_, err := store.Read(context.Background(), "repo", cfg, "path")
	if err == nil {
		t.Fatal("expected error when unavailable with no cache")
	}
}

func TestStore_FallbackCache_SuccessThen404EvictsCache(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newFallbackServer(t, []int{200, 404})
	cfg := &OpenBaoConfig{
		Addr: ts.URL, Role: "r", ServiceAccountTokenPath: tokenFile,
		KVMount: "kv", Field: testFieldName,
	}

	store := NewStore()

	data, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(data) != testCachedPEM {
		t.Errorf("first read: got %q, want %q", data, testCachedPEM)
	}

	_, err = store.Read(context.Background(), "repo", cfg, "path")
	if err == nil {
		t.Fatal("expected error after 404 eviction")
	}
}

func TestStore_CancelledContext_NoFallback(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, kvResponse(map[string]any{testFieldName: "pem"}))
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
	// Populate cache.
	_, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("initial read: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err = store.Read(ctx, "repo", cfg, "path")
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
			writeBody(w, kvResponse(map[string]any{testFieldName: "pem"}))
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

func TestStore_TokenFileMissing_ErrForbidden(t *testing.T) {
	cfg := &OpenBaoConfig{
		Addr:                    "http://localhost:8200",
		Role:                    "r",
		ServiceAccountTokenPath: "/nonexistent/token/path",
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	store := NewStore()
	_, err := store.Read(context.Background(), "repo", cfg, "path")
	if err == nil {
		t.Fatal("expected error for missing token file")
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

	cfg := &OpenBaoConfig{
		Addr:                    ts.URL,
		Role:                    "r",
		ServiceAccountTokenPath: tokenFile,
		KVMount:                 "kv",
		Field:                   testFieldName,
	}

	before := testutil.ToFloat64(telemetry.SecretFetchFailuresTotal.WithLabelValues("metric-repo", "not_found"))

	store := NewStore()
	_, _ = store.Read(context.Background(), "metric-repo", cfg, "path")

	after := testutil.ToFloat64(telemetry.SecretFetchFailuresTotal.WithLabelValues("metric-repo", "not_found"))
	if after != before+1 {
		t.Errorf("SecretFetchFailuresTotal: got %v, want %v", after, before+1)
	}
}
