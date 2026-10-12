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

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			lastJWT.Store(req["jwt"])
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
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

	store := NewStore()

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

	// Rewrite the token file, then InvalidateAll to clear the client so the
	// next Read creates a fresh client that re-reads the file.
	if err := os.WriteFile(tokenFile, []byte("jwt-second"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.InvalidateAll()

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

	// Second read must return cached value without hitting OpenBao.
	data2, err := store.Read(context.Background(), "repo", cfg, "path")
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if string(data2) != string(data) {
		t.Errorf("second read returned different value: %q vs %q", data2, data)
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
	name        string
	retryStatus int // HTTP status for 3rd KV call (after 403 + re-login)
	wantErr     error
}

func runReloginRetryRow(t *testing.T, tt reloginRetryRow) {
	t.Helper()
	tokenFile := writeTokenFile(t, "jwt")
	var kvCalls atomic.Int32
	var loginCalls atomic.Int32

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			loginCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("fresh-token"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			n := kvCalls.Add(1)
			switch n {
			case 1:
				w.WriteHeader(http.StatusOK)
				writeBody(w, kvResponse(map[string]any{testFieldName: "initial"}))
			case 2:
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

	fakeNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewStore()
	store.now = func() time.Time { return fakeNow }

	// First read populates cache.
	if _, err := store.Read(context.Background(), "repo", cfg, "path"); err != nil {
		t.Fatalf("initial read: %v", err)
	}

	// Advance time past reloginCooldown so the 403 triggers re-login.
	fakeNow = fakeNow.Add(2 * time.Minute)

	// Invalidate so next read hits OpenBao (which will 403 then retry).
	store.Invalidate(cfg, "path")

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
		{"retry succeeds", http.StatusOK, nil},
		{"retry also 403", http.StatusForbidden, ErrForbidden},
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

func TestStore_Invalidate(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")

	tests := []struct {
		name          string
		action        string // "invalidate_path", "invalidate_other", "invalidate_all"
		wantKVHits    int32
		wantLogins    int32
		wantClientLen int // only checked for invalidate_all
	}{
		{
			name:       "invalidate path refetches",
			action:     "invalidate_path",
			wantKVHits: 2,
			wantLogins: 1,
		},
		{
			name:       "invalidate other path does not evict",
			action:     "invalidate_other",
			wantKVHits: 1,
			wantLogins: 1,
		},
		{
			name:          "invalidate all refetches and re-logins",
			action:        "invalidate_all",
			wantKVHits:    2,
			wantLogins:    2,
			wantClientLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

			switch tt.action {
			case "invalidate_path":
				store.Invalidate(cfg, "path/a")
			case "invalidate_other":
				store.Invalidate(cfg, "path/b")
			case "invalidate_all":
				store.InvalidateAll()
				// Verify clients map is empty right after call.
				store.mu.Lock()
				clientLen := len(store.clients)
				store.mu.Unlock()
				if clientLen != tt.wantClientLen {
					t.Errorf("clients len = %d, want %d", clientLen, tt.wantClientLen)
				}
			}

			// Second read.
			if _, err := store.Read(context.Background(), "repo", cfg, "path/a"); err != nil {
				t.Fatalf("second read: %v", err)
			}

			if got := kvHits.Load(); got != tt.wantKVHits {
				t.Errorf("kv hits = %d, want %d", got, tt.wantKVHits)
			}
			if got := loginHits.Load(); got != tt.wantLogins {
				t.Errorf("login hits = %d, want %d", got, tt.wantLogins)
			}
		})
	}

}

func TestStore_InvalidateAll_InFlightRace(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")
	var kvHits atomic.Int32
	gate := make(chan struct{})

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponse("tok"))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			n := kvHits.Add(1)
			if n == 1 {
				<-gate // block first request
			}
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

	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = store.Read(context.Background(), "repo", cfg, "path/x")
	})

	// Wait for the blocked request to be in-flight.
	deadline := time.After(5 * time.Second)
	for kvHits.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for in-flight request")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Invalidate while in-flight.
	store.InvalidateAll()

	// Release the blocked request.
	close(gate)
	wg.Wait()

	// The in-flight read should have succeeded but not populated the cache
	// (gen mismatch), so next read refetches.
	if _, err := store.Read(context.Background(), "repo", cfg, "path/x"); err != nil {
		t.Fatalf("post-invalidate read: %v", err)
	}

	if got := kvHits.Load(); got != 2 {
		t.Errorf("kv hits = %d, want 2 (in-flight + refetch)", got)
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

type proactiveReloginRow struct {
	name              string
	leaseSeconds      int
	advanceDuration   time.Duration
	secondLoginStatus int
	wantLogins        int32
	wantErr           bool
}

func runProactiveReloginRow(t *testing.T, tokenFile string, tt proactiveReloginRow) {
	t.Helper()
	var loginHits atomic.Int32
	var lastTokenSeen atomic.Value

	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/auth/kubernetes/login"):
			n := loginHits.Add(1)
			if n > 1 && tt.secondLoginStatus != 0 {
				w.WriteHeader(tt.secondLoginStatus)
				writeBody(w, []byte(`{"errors":["unavailable"]}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			writeBody(w, loginResponseWithLease(fmt.Sprintf("tok-%d", n), tt.leaseSeconds))
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			lastTokenSeen.Store(r.Header.Get("X-Vault-Token"))
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

	fakeNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewStore()
	store.now = func() time.Time { return fakeNow }

	// First read.
	if _, err := store.Read(context.Background(), "repo", cfg, "path/pr"); err != nil {
		t.Fatalf("first read: %v", err)
	}

	// Advance time and invalidate to force a new OpenBao call.
	fakeNow = fakeNow.Add(tt.advanceDuration)
	store.Invalidate(cfg, "path/pr")

	// Second read.
	_, err := store.Read(context.Background(), "repo", cfg, "path/pr")
	if tt.wantErr {
		if err == nil {
			t.Fatal("expected error")
		}
	} else if err != nil {
		t.Fatalf("second read: %v", err)
	}

	if got := loginHits.Load(); got != tt.wantLogins {
		t.Errorf("logins = %d, want %d", got, tt.wantLogins)
	}

	// For the 90/61s success case, verify the new token was used.
	if tt.name == "90s lease, 61s advance, proactive renewal" {
		if v := lastTokenSeen.Load(); v != "tok-2" {
			t.Errorf("second kv request carried token %q, want %q", v, "tok-2")
		}
	}
}

func TestStore_ProactiveRelogin(t *testing.T) {
	tokenFile := writeTokenFile(t, "jwt")

	tests := []proactiveReloginRow{
		{
			name:            "90s lease, 59s advance, no renewal",
			leaseSeconds:    90,
			advanceDuration: 59 * time.Second,
			wantLogins:      1,
		},
		{
			name:            "90s lease, 61s advance, proactive renewal",
			leaseSeconds:    90,
			advanceDuration: 61 * time.Second,
			wantLogins:      2,
		},
		{
			name:            "0 lease, 24h advance, no renewal (non-expiring)",
			leaseSeconds:    0,
			advanceDuration: 24 * time.Hour,
			wantLogins:      1,
		},
		{
			name:              "90s lease, 61s advance, renewal fails 503, old token still works",
			leaseSeconds:      90,
			advanceDuration:   61 * time.Second,
			secondLoginStatus: 503,
			wantLogins:        2,
			wantErr:           false,
		},
		{
			name:              "90s lease, 91s advance, renewal fails 503, expired token",
			leaseSeconds:      90,
			advanceDuration:   91 * time.Second,
			secondLoginStatus: 503,
			wantLogins:        2,
			wantErr:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runProactiveReloginRow(t, tokenFile, tt)
		})
	}
}
