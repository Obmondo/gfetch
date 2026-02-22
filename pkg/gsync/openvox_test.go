package gsync

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/obmondo/gfetch/pkg/config"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"production", "production"},
		{"feature-branch", "feature_branch"},
		{"v1.0.0", "v1_0_0"},
		{"feature-auth", "feature_auth"},
		{"v2.0.0", "v2_0_0"},
		{"a-b.c", "a_b_c"},
		{"no_change", "no_change"},
		{"", ""},
		{"---", "___"},
		{"...", "___"},
		{"a-b-c.d.e", "a_b_c_d_e"},
		{"feature/my-branch", "feature_my_branch"},
		{"bugfix/auth/login", "bugfix_auth_login"},
		{"user@domain", "user_domain"},
		{"release/v1.0.0-rc1", "release_v1_0_0_rc1"},
		{"branch~1", "branch_1"},
		{"branch^2", "branch_2"},
		{"my branch", "my_branch"},
		{"a//b", "a__b"},
	}

	for _, tt := range tests {
		got := SanitizeName(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDetectCollisions(t *testing.T) {
	t.Run("no collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{"main", "develop", "feature-auth"}
		if msg := detectCollisions(names, m); msg != "" {
			t.Errorf("expected no collision, got: %s", msg)
		}
	})

	t.Run("hyphen vs dot collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{"a-b", "a.b"}
		msg := detectCollisions(names, m)
		if msg == "" {
			t.Error("expected collision between a-b and a.b")
		}
	})

	t.Run("collision across calls", func(t *testing.T) {
		m := make(map[string]string)
		// First call with branches.
		if msg := detectCollisions([]string{"feature-1"}, m); msg != "" {
			t.Errorf("unexpected collision: %s", msg)
		}
		// Second call with tags that collides.
		msg := detectCollisions([]string{"feature.1"}, m)
		if msg == "" {
			t.Error("expected collision between feature-1 (branch) and feature.1 (tag)")
		}
	})

	t.Run("slash vs hyphen collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{"feature/auth", "feature-auth"}
		msg := detectCollisions(names, m)
		if msg == "" {
			t.Error("expected collision between feature/auth and feature-auth")
		}
	})

	t.Run("same name no collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{"main", "main"}
		if msg := detectCollisions(names, m); msg != "" {
			t.Errorf("same name should not collide, got: %s", msg)
		}
	})
}

// initOpenVoxBranchRepo creates a per-branch directory with a git repo containing a single
// commit with the given committer timestamp.
func initOpenVoxBranchRepo(t *testing.T, basePath, branch string, commitTime time.Time) {
	t.Helper()
	dirName := SanitizeName(branch)
	dirPath := filepath.Join(basePath, dirName)

	r, err := git.PlainInit(dirPath, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://example.com/repo.git"},
	})
	if err != nil {
		t.Fatal(err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	fpath := filepath.Join(dirPath, "README.md")
	if err := os.WriteFile(fpath, []byte("content for "+branch), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "test", Email: "test@test.com", When: commitTime}
	if _, err := wt.Commit("commit on "+branch, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
}

func TestPruneStaleOpenVoxDirs(t *testing.T) {
	basePath := t.TempDir()
	log := slog.Default()
	staleAge := 180 * 24 * time.Hour

	// Create branches: main (fresh), stale-feature (old), fresh-feature (recent).
	now := time.Now()
	past := now.Add(-365 * 24 * time.Hour) // 1 year ago

	initOpenVoxBranchRepo(t, basePath, "main", now)
	initOpenVoxBranchRepo(t, basePath, "stale-feature", past)
	initOpenVoxBranchRepo(t, basePath, "fresh-feature", now)

	activeNames := map[string]string{
		"main":          "main",
		"stale_feature": "stale-feature",
		"fresh_feature": "fresh-feature",
	}

	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: basePath},
		Name:         "test",
	}

	result := &Result{RepoName: "test"}

	// "main" is the default branch — should be protected even if stale.
	pruneStaleOpenVoxDirs(repo, activeNames, staleAge, false, "main", log, result)

	// stale-feature should be pruned.
	if _, err := os.Stat(filepath.Join(basePath, "stale_feature")); !os.IsNotExist(err) {
		t.Error("stale-feature directory should have been pruned")
	}

	// fresh-feature should still exist.
	if _, err := os.Stat(filepath.Join(basePath, "fresh_feature")); err != nil {
		t.Error("fresh-feature directory should NOT have been pruned")
	}

	// main should still exist (protected as default branch).
	if _, err := os.Stat(filepath.Join(basePath, "main")); err != nil {
		t.Error("main directory should NOT have been pruned (default branch)")
	}

	// Check result.
	found := false
	for _, b := range result.BranchesPruned {
		if b == "stale-feature" {
			found = true
		}
		if b == "main" {
			t.Error("main should not appear in pruned list")
		}
	}
	if !found {
		t.Errorf("expected stale-feature in pruned list, got %v", result.BranchesPruned)
	}
}

func TestPruneStaleOpenVoxDirs_DryRun(t *testing.T) {
	basePath := t.TempDir()
	log := slog.Default()
	staleAge := 180 * 24 * time.Hour
	past := time.Now().Add(-365 * 24 * time.Hour)

	initOpenVoxBranchRepo(t, basePath, "old-branch", past)

	activeNames := map[string]string{
		"old_branch": "old-branch",
	}

	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: basePath},
		Name:         "test",
	}

	result := &Result{RepoName: "test"}

	// Dry run — directory should NOT be removed.
	pruneStaleOpenVoxDirs(repo, activeNames, staleAge, true, "", log, result)

	if _, err := os.Stat(filepath.Join(basePath, "old_branch")); err != nil {
		t.Error("directory should still exist in dry-run mode")
	}

	// But it should still appear in stale/pruned lists.
	if len(result.BranchesStale) != 1 || result.BranchesStale[0] != "old-branch" {
		t.Errorf("expected old-branch in stale list, got %v", result.BranchesStale)
	}
}

func TestPruneStaleOpenVoxDirs_MissingDir(t *testing.T) {
	basePath := t.TempDir()
	log := slog.Default()
	staleAge := 180 * 24 * time.Hour

	activeNames := map[string]string{
		"missing_branch": "missing-branch",
	}

	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: basePath},
		Name:         "test",
	}

	result := &Result{RepoName: "test"}

	// Should NOT log a warning or fail if the directory is missing.
	// We can't easily check logs here without a custom handler, but we can ensure it doesn't crash or add to results.
	pruneStaleOpenVoxDirs(repo, activeNames, staleAge, false, "", log, result)

	if len(result.BranchesPruned) != 0 {
		t.Errorf("expected no branches pruned, got %v", result.BranchesPruned)
	}
}
