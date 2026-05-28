package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/gsync"
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

func TestReloadEndpoint_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	keyFile := writeFakeSSHKey(t, dir)
	writeSSHRepoConfig(t, dir, "repo1", keyFile)

	sched := newTestScheduler(t)
	sched.configPath = dir
	if err := sched.applyInitial(&config.Config{Repos: map[string]config.RepoConfig{}}); err != nil {
		t.Fatalf("applyInitial: %v", err)
	}

	h := newServer(sched)
	req := httptest.NewRequest(http.MethodPost, "/reload", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var res ReloadResult
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Repos) != 1 || res.Repos[0] != "repo1" {
		t.Errorf("expected [repo1], got %+v", res)
	}
	if _, ok := sched.Config().Repos["repo1"]; !ok {
		t.Errorf("expected repo1 to be in scheduler config")
	}
}

func TestReloadEndpoint_InvalidConfigKeepsOldConfig(t *testing.T) {
	dir := t.TempDir()
	brokenDir := filepath.Join(dir, "broken")
	if err := os.MkdirAll(brokenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "config.yaml"), []byte(": :: bad yaml ::"), 0o644); err != nil {
		t.Fatal(err)
	}

	sched := newTestScheduler(t)
	sched.configPath = dir
	old := &config.Config{Repos: map[string]config.RepoConfig{"existing": testRepo("existing", "main")}}
	if err := sched.applyInitial(old); err != nil {
		t.Fatalf("applyInitial: %v", err)
	}

	h := newServer(sched)
	req := httptest.NewRequest(http.MethodPost, "/reload", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError && rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 500 or 422, body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := sched.Config().Repos["existing"]; !ok {
		t.Errorf("old config was lost after failed reload")
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

	// Dropped from config but a sync still holds the guard: 409.
	if !sched.state.guard.TryStart(dropped) {
		t.Fatalf("TryStart(%q) = false, want true", dropped)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, syncURL, nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("in-flight: status = %d, want %d, body=%s", rr.Code, http.StatusConflict, rr.Body.String())
	}

	// Sync finished, guard cleared: back to terminal 404.
	sched.state.guard.Finish(dropped)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, syncURL, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("after finish: status = %d, want %d, body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
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
