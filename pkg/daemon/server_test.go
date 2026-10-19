package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/gsync"
	"github.com/obmondo/gfetch/pkg/secrets"
)

const (
	testExistingRepoName     = "existing"
	testMainBranch           = "main"
	testShuttingDownCaseName = "shutting down"
	testOpenBaoAddr          = "http://openbao:8200"
)

func TestHealthEndpoint(t *testing.T) {
	slog.Default()
	sched := NewScheduler(gsync.New(), ":0", "")
	sched.cfg.Store(&config.Config{Repos: map[string]config.RepoConfig{}})
	h := newServer(sched)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want %q", got, "application/json")
	}

	body, err := io.ReadAll(rr.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("body = %q, want %q", string(body), `{"status":"ok"}`)
	}
}

func TestReloadEndpoint_Status(t *testing.T) {
	tests := []struct {
		name         string
		initial      map[string]config.RepoConfig
		shuttingDown bool
		wantStatus   int
		wantCode     string
		wantMessage  string
		wantRepo     string
	}{
		{
			name:       "valid config",
			initial:    map[string]config.RepoConfig{},
			wantStatus: http.StatusOK,
			wantRepo:   "repo1",
		},
		{
			name:         testShuttingDownCaseName,
			initial:      map[string]config.RepoConfig{testExistingRepoName: testRepo(testExistingRepoName, testMainBranch)},
			shuttingDown: true,
			wantStatus:   http.StatusServiceUnavailable,
			wantCode:     string(codeShuttingDown),
			wantMessage:  "daemon shutting down",
			wantRepo:     testExistingRepoName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			keyFile := writeFakeSSHKey(t, dir)
			writeSSHRepoConfig(t, dir, "repo1", keyFile)

			sched := newTestScheduler(t)
			sched.configPath = dir
			if err := sched.applyInitial(&config.Config{Repos: tt.initial}); err != nil {
				t.Fatalf("applyInitial: %v", err)
			}
			if tt.shuttingDown {
				sched.state.shuttingDown.Store(true)
			}

			h := newServer(sched)
			req := httptest.NewRequest(http.MethodPost, "/reload", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if tt.wantStatus == http.StatusOK {
				assertReloadResultForTestReloadEndpointStatus(t, rr, tt.wantRepo)
			} else {
				body := decodeJSONErrorForDaemonEndpointTests(t, rr, tt.wantCode)
				if body["message"] != tt.wantMessage {
					t.Errorf("message = %q, want %q", body["message"], tt.wantMessage)
				}
			}
			if _, ok := sched.Config().Repos[tt.wantRepo]; !ok {
				t.Errorf("expected %s to be in scheduler config", tt.wantRepo)
			}
		})
	}
}

// assertReloadResultForTestReloadEndpointStatus asserts a successful
// reload response listing exactly wantRepo.
func assertReloadResultForTestReloadEndpointStatus(t *testing.T, rr *httptest.ResponseRecorder, wantRepo string) {
	t.Helper()
	var res ReloadResult
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Repos) != 1 || res.Repos[0] != wantRepo {
		t.Errorf("expected [%s], got %+v", wantRepo, res)
	}
}

func TestReloadEndpoint_InvalidConfigKeepsOldConfig(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "malformed yaml",
			body:       ": :: bad yaml ::",
			wantStatus: http.StatusInternalServerError,
			wantCode:   string(codeConfigLoadFailed),
		},
		{
			name:       "no valid repos",
			body:       "repos:\n  broken:\n    poll_interval: 1m\n",
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   string(codeConfigInvalid),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			brokenDir := filepath.Join(dir, "broken")
			if err := os.MkdirAll(brokenDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(brokenDir, "config.yaml"), []byte(tt.body), 0o644); err != nil {
				t.Fatal(err)
			}

			sched := newTestScheduler(t)
			sched.configPath = dir
			old := &config.Config{Repos: map[string]config.RepoConfig{testExistingRepoName: testRepo(testExistingRepoName, testMainBranch)}}
			if err := sched.applyInitial(old); err != nil {
				t.Fatalf("applyInitial: %v", err)
			}

			h := newServer(sched)
			req := httptest.NewRequest(http.MethodPost, "/reload", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			decodeJSONErrorForDaemonEndpointTests(t, rr, tt.wantCode)
			if _, ok := sched.Config().Repos[testExistingRepoName]; !ok {
				t.Errorf("old config was lost after failed reload")
			}
		})
	}
}

// TestSyncEndpoint_DroppedRepoGuardGates covers a repo that a reload removed
// from config while a sync of it is still in flight. /sync must report 409
// while the guard is held (so a caller deleting the clone waits) and only the
// terminal 404 once the sync has drained.
func TestSyncEndpoint_DroppedRepoGuardGates(t *testing.T) {
	sched := newTestScheduler(t)
	sched.cfg.Store(&config.Config{Repos: map[string]config.RepoConfig{}})
	h := newServer(sched)

	const dropped = "dropped-repo"
	syncURL := "/sync/" + dropped

	// Not in config, no in-flight sync: terminal 404.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, syncURL, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("quiescent: status = %d, want %d, body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
	if body := decodeJSONErrorForDaemonEndpointTests(t, rr, string(codeRepoNotFound)); body["error"] != "repo not found" {
		t.Errorf("error = %q, want %q", body["error"], "repo not found")
	}

	// Dropped from config but a sync still holds the guard: 409.
	if !sched.state.guard.TryStart(dropped) {
		t.Fatalf("TryStart(%q) = false, want true", dropped)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, syncURL, nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("in-flight: status = %d, want %d, body=%s", rr.Code, http.StatusConflict, rr.Body.String())
	}
	if body := decodeJSONErrorForDaemonEndpointTests(t, rr, string(codeSyncInProgress)); body["error"] != "sync already in progress" {
		t.Errorf("error = %q, want %q", body["error"], "sync already in progress")
	}

	// Sync finished, guard cleared: back to terminal 404.
	sched.state.guard.Finish(dropped)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, syncURL, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("after finish: status = %d, want %d, body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
	if body := decodeJSONErrorForDaemonEndpointTests(t, rr, string(codeRepoNotFound)); body["error"] != "repo not found" {
		t.Errorf("error = %q, want %q", body["error"], "repo not found")
	}
}

// decodeJSONErrorForDaemonEndpointTests asserts a JSON error response with
// wantCode and non-empty message and error, and returns the body for any
// further checks.
func decodeJSONErrorForDaemonEndpointTests(t *testing.T, rr *httptest.ResponseRecorder, wantCode string) map[string]string {
	t.Helper()
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want %q", got, "application/json")
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["code"] != wantCode {
		t.Errorf("code = %q, want %q", body["code"], wantCode)
	}
	if body["message"] == "" || body["error"] == "" {
		t.Errorf("expected non-empty message and error, got %+v", body)
	}
	return body
}

// TestFailureSpecs verifies every failure code has a non-empty, unique
// message and a status in the HTTP error contract.
func TestFailureSpecs(t *testing.T) {
	allowed := map[int]bool{
		http.StatusNotFound:            true,
		http.StatusConflict:            true,
		http.StatusUnprocessableEntity: true,
		http.StatusInternalServerError: true,
		http.StatusServiceUnavailable:  true,
	}
	seen := make(map[string]gsync.FailureCode, len(failureSpecs))
	for code, spec := range failureSpecs {
		if spec.message == "" {
			t.Errorf("%s: empty message", code)
		}
		if other, dup := seen[spec.message]; dup {
			t.Errorf("%s: message %q duplicates %s", code, spec.message, other)
		}
		seen[spec.message] = code
		if !allowed[spec.status] {
			t.Errorf("%s: status %d is not in the contract", code, spec.status)
		}
	}
}

// TestSyncEndpoint_FailureStatusAndCode verifies that a failed manual sync
// maps to the HTTP status of its failure class and carries the stable code
// and message while error keeps the detailed chain.
func TestSyncEndpoint_FailureStatusAndCode(t *testing.T) {
	const repoName = "failing-repo"

	tests := []struct {
		name         string
		readErr      error
		cancelled    bool
		guardHeld    bool
		shuttingDown bool
		repo         func(t *testing.T, name string) config.RepoConfig
		wantStatus   int
		wantCode     string
		wantErr      string
	}{
		{
			name:       "secret unavailable",
			readErr:    secrets.ErrUnavailable,
			repo:       openBaoRepoForSyncEndpointTests,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   string(gsync.CodeSecretUnavailable),
			wantErr:    secrets.ErrUnavailable.Error(),
		},
		{
			name:       "secret forbidden",
			readErr:    fmt.Errorf("%w: denied", secrets.ErrForbidden),
			repo:       openBaoRepoForSyncEndpointTests,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   string(gsync.CodeSecretForbidden),
			wantErr:    secrets.ErrForbidden.Error() + ": denied",
		},
		{
			name:       "secret not found",
			readErr:    secrets.ErrNotFound,
			repo:       openBaoRepoForSyncEndpointTests,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   string(gsync.CodeSecretNotFound),
			wantErr:    secrets.ErrNotFound.Error(),
		},
		{
			name:       "local repo not found",
			repo:       missingLocalRepoForSyncEndpointTests,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   string(gsync.CodeGitRepoNotFound),
			wantErr:    "repository not found",
		},
		{
			// Store.Read returns the caller's ctx error unwrapped: internal.
			name:       "caller cancelled",
			cancelled:  true,
			repo:       openBaoRepoForSyncEndpointTests,
			wantStatus: http.StatusInternalServerError,
			wantCode:   string(gsync.CodeInternal),
			wantErr:    "context canceled",
		},
		{
			// An OpenBao request timeout is wrapped in ErrUnavailable: 503.
			name:       "openbao request timeout",
			readErr:    fmt.Errorf("%w: %w", secrets.ErrUnavailable, &url.Error{Op: "Put", URL: testOpenBaoAddr, Err: context.DeadlineExceeded}),
			repo:       openBaoRepoForSyncEndpointTests,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   string(gsync.CodeSecretUnavailable),
			wantErr:    secrets.ErrUnavailable.Error(),
		},
		{
			// The API's gfetch cleanup job keys on this 409 (utils/gfetch/client.go ErrSyncInProgress).
			name:       "sync in progress",
			guardHeld:  true,
			repo:       openBaoRepoForSyncEndpointTests,
			wantStatus: http.StatusConflict,
			wantCode:   string(codeSyncInProgress),
			wantErr:    "sync already in progress",
		},
		{
			name:         testShuttingDownCaseName,
			shuttingDown: true,
			repo:         openBaoRepoForSyncEndpointTests,
			wantStatus:   http.StatusServiceUnavailable,
			wantCode:     string(codeShuttingDown),
			wantErr:      "daemon shutting down",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeSecretReaderForDaemonTests{err: tt.readErr}
			sched := NewScheduler(gsync.New(gsync.WithSecretReader(reader)), ":0", "")
			sched.cfg.Store(&config.Config{Repos: map[string]config.RepoConfig{repoName: tt.repo(t, repoName)}})
			if tt.guardHeld {
				if !sched.state.guard.TryStart(repoName) {
					t.Fatalf("TryStart(%q) = false, want true", repoName)
				}
				t.Cleanup(func() { sched.state.guard.Finish(repoName) })
			}
			if tt.shuttingDown {
				sched.state.shuttingDown.Store(true)
			}
			h := newServer(sched)

			req := httptest.NewRequest(http.MethodPost, "/sync/"+repoName, nil)
			if tt.cancelled {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			byRepo := decodeResultsByRepoForSyncEndpointTests(t, rr)
			if len(byRepo) != 1 {
				t.Fatalf("expected 1 result, got %d", len(byRepo))
			}
			got := byRepo[repoName]
			if got["code"] != tt.wantCode {
				t.Errorf("code = %q, want %q", got["code"], tt.wantCode)
			}
			if got["message"] == "" {
				t.Error("expected non-empty message")
			}
			if !strings.Contains(got["error"], tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", got["error"], tt.wantErr)
			}
		})
	}
}

// TestSyncAllEndpoint_AggregatesStatus verifies that POST /sync reports the
// status shared by all failed repos, falls back to 500 for mixed statuses,
// and ignores successful repos.
func TestSyncAllEndpoint_AggregatesStatus(t *testing.T) {
	tests := []struct {
		name         string
		r1           func(t *testing.T, name string) config.RepoConfig
		r2           func(t *testing.T, name string) config.RepoConfig
		shuttingDown bool
		wantStatus   int
		wantCodes    map[string]string
	}{
		{
			name:       "same",
			r1:         openBaoRepoForSyncEndpointTests,
			r2:         openBaoRepoForSyncEndpointTests,
			wantStatus: http.StatusServiceUnavailable,
			wantCodes:  map[string]string{"r1": string(gsync.CodeSecretUnavailable), "r2": string(gsync.CodeSecretUnavailable)},
		},
		{
			name:       "mixed",
			r1:         openBaoRepoForSyncEndpointTests,
			r2:         missingLocalRepoForSyncEndpointTests,
			wantStatus: http.StatusInternalServerError,
			wantCodes:  map[string]string{"r1": string(gsync.CodeSecretUnavailable), "r2": string(gsync.CodeGitRepoNotFound)},
		},
		{
			name:         testShuttingDownCaseName,
			r1:           openBaoRepoForSyncEndpointTests,
			r2:           openBaoRepoForSyncEndpointTests,
			shuttingDown: true,
			wantStatus:   http.StatusServiceUnavailable,
			wantCodes:    map[string]string{"r1": string(codeShuttingDown), "r2": string(codeShuttingDown)},
		},
		{
			name:       "success+422",
			r1:         emptyBareRepoForSyncEndpointTests,
			r2:         missingLocalRepoForSyncEndpointTests,
			wantStatus: http.StatusUnprocessableEntity,
			wantCodes:  map[string]string{"r1": "", "r2": string(gsync.CodeGitRepoNotFound)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeSecretReaderForDaemonTests{err: secrets.ErrUnavailable}
			sched := NewScheduler(gsync.New(gsync.WithSecretReader(reader)), ":0", "")
			sched.cfg.Store(&config.Config{Repos: map[string]config.RepoConfig{
				"r1": tt.r1(t, "r1"),
				"r2": tt.r2(t, "r2"),
			}})
			if tt.shuttingDown {
				sched.state.shuttingDown.Store(true)
			}
			h := newServer(sched)

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sync", nil))

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			byRepo := decodeResultsByRepoForSyncEndpointTests(t, rr)
			if len(byRepo) != len(tt.wantCodes) {
				t.Fatalf("expected %d results, got %d: %+v", len(tt.wantCodes), len(byRepo), byRepo)
			}
			for repo, wantCode := range tt.wantCodes {
				got, ok := byRepo[repo]
				if !ok {
					t.Errorf("missing result for %s", repo)
					continue
				}
				if got["code"] != wantCode {
					t.Errorf("%s: code = %q, want %q", repo, got["code"], wantCode)
				}
			}
		})
	}
}

// fakeSecretReaderForDaemonTests implements the secretReader interface. Read
// fails with the caller's context error when it is set (as Store.Read does),
// otherwise with err; Invalidate records the evicted paths.
type fakeSecretReaderForDaemonTests struct {
	err         error
	mu          sync.Mutex
	invalidated []string
}

func (f *fakeSecretReaderForDaemonTests) Read(ctx context.Context, _ string, _ *secrets.OpenBaoConfig, _ string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, f.err
}

func (f *fakeSecretReaderForDaemonTests) Invalidate(path string) {
	f.mu.Lock()
	f.invalidated = append(f.invalidated, path)
	f.mu.Unlock()
}

// decodeResultsByRepoForSyncEndpointTests decodes a /sync response body and
// returns the per-repo results indexed by repo name.
func decodeResultsByRepoForSyncEndpointTests(t *testing.T, rr *httptest.ResponseRecorder) map[string]map[string]string {
	t.Helper()
	var body []map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	byRepo := make(map[string]map[string]string, len(body))
	for _, r := range body {
		byRepo[r["repo"]] = r
	}
	return byRepo
}

// openBaoRepoForSyncEndpointTests returns an OpenBao-keyed repo that fails in
// resolveAuth before any dial.
func openBaoRepoForSyncEndpointTests(t *testing.T, name string) config.RepoConfig {
	t.Helper()
	return config.RepoConfig{
		Name:         name,
		URL:          "ssh://git@127.0.0.1:1/x.git",
		SSHKeyPath:   "ssh-keys/c/kubeaid",
		OpenBao:      &secrets.OpenBaoConfig{Addr: testOpenBaoAddr, Role: "gfetch", Field: "private_key"},
		LocalPath:    t.TempDir(),
		PollInterval: config.Duration(time.Minute),
		Branches:     []config.Pattern{{Raw: testMainBranch}},
	}
}

// missingLocalRepoForSyncEndpointTests returns a repo whose URL is a local
// path with no repo: the test bypass in resolveAuth gives nil auth and
// go-git's file transport reports transport.ErrRepositoryNotFound.
func missingLocalRepoForSyncEndpointTests(t *testing.T, name string) config.RepoConfig {
	t.Helper()
	return config.RepoConfig{
		Name:         name,
		URL:          filepath.Join(t.TempDir(), "missing"),
		LocalPath:    t.TempDir(),
		PollInterval: config.Duration(time.Minute),
		Branches:     []config.Pattern{{Raw: testMainBranch}},
	}
}

// emptyBareRepoForSyncEndpointTests returns a repo whose URL is an empty bare
// repo: listing its refs gives transport.ErrEmptyRemoteRepository, which the
// sync treats as a benign success.
func emptyBareRepoForSyncEndpointTests(t *testing.T, name string) config.RepoConfig {
	t.Helper()
	dir := t.TempDir()
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}
	return config.RepoConfig{
		Name:         name,
		URL:          dir,
		LocalPath:    t.TempDir(),
		PollInterval: config.Duration(time.Minute),
		Branches:     []config.Pattern{{Raw: testMainBranch}},
	}
}

// writeFakeSSHKey writes a placeholder SSH key file under dir and returns the path.
// This satisfies validateAuth's existence check without requiring a real SSH key.
func writeFakeSSHKey(t *testing.T, dir string) string {
	t.Helper()
	keyPath := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(keyPath, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

// writeSSHRepoConfig writes <dir>/<name>/config.yaml using an SSH URL so
// validation does not perform any network call.
func writeSSHRepoConfig(t *testing.T, dir, name, keyFile string) {
	t.Helper()
	repoDir := filepath.Join(dir, name)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "repos:\n  " + name + ":\n" +
		"    url: git@github.com:test/" + name + ".git\n" +
		"    ssh_key_path: " + keyFile + "\n" +
		"    local_path: " + filepath.Join(dir, ".clones", name) + "\n" +
		"    poll_interval: 1m\n" +
		"    branches: [\"main\"]\n"
	if err := os.WriteFile(filepath.Join(repoDir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
