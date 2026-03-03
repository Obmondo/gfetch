package gsync

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/obmondo/gfetch/pkg/config"
)

const testDefaultBranch = "main"

func TestEnsureClonedOpenVox_RecreatesNonRepoDir(t *testing.T) {
	basePath := t.TempDir()
	localPath := filepath.Join(basePath, "main")

	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localPath, "README.txt"), []byte("not a git repo"), 0644); err != nil {
		t.Fatal(err)
	}

	repoCfg := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: localPath},
		Name:         "test",
		URL:          "https://example.com/repo.git",
	}

	r, err := ensureClonedOpenVox(context.Background(), repoCfg, nil, slog.Default())
	if err != nil {
		t.Fatalf("ensureClonedOpenVox failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(localPath, ".git")); err != nil {
		t.Fatalf("expected .git to exist after recovery: %v", err)
	}

	remote, err := r.Remote("origin")
	if err != nil {
		t.Fatalf("expected origin remote: %v", err)
	}
	if len(remote.Config().URLs) != 1 || remote.Config().URLs[0] != repoCfg.URL {
		t.Fatalf("origin URL = %v, want [%s]", remote.Config().URLs, repoCfg.URL)
	}
}

func TestAcquireOpenVoxFileLock_Exclusive(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "main.gfetch.lock")

	first, err := acquireOpenVoxFileLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("acquire first lock failed: %v", err)
	}
	defer func() {
		if err := first.Release(); err != nil {
			t.Fatalf("release first lock failed: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = acquireOpenVoxFileLock(ctx, lockPath)
	if err == nil {
		t.Fatal("expected second lock acquisition to time out while first lock is held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release first lock failed: %v", err)
	}
	first = nil

	second, err := acquireOpenVoxFileLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("acquire second lock after release failed: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("release second lock failed: %v", err)
	}
}

func TestEnsureProductionAlias(t *testing.T) {
	basePath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(basePath, testDefaultBranch), 0755); err != nil {
		t.Fatal(err)
	}

	openVox := true
	productionAlias := true
	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{
			LocalPath:       basePath,
			OpenVox:         &openVox,
			ProductionAlias: &productionAlias,
		},
		Name: "test",
	}

	log := slog.Default()

	ensureProductionAlias(context.Background(), repo, testDefaultBranch, map[string]struct{}{testDefaultBranch: {}}, log)

	aliasPath := filepath.Join(basePath, "production")
	aliasInfo, err := os.Lstat(aliasPath)
	if err != nil {
		t.Fatalf("expected production alias symlink to exist: %v", err)
	}
	if aliasInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected production to be a symlink")
	}
	target, err := os.Readlink(aliasPath)
	if err != nil {
		t.Fatalf("readlink failed: %v", err)
	}
	if target != testDefaultBranch {
		t.Fatalf("production target = %q, want %q", target, testDefaultBranch)
	}
}

func TestEnsureProductionAlias_SkipsWhenProductionBranchExists(t *testing.T) {
	basePath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(basePath, testDefaultBranch), 0755); err != nil {
		t.Fatal(err)
	}

	openVox := true
	productionAlias := true
	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{
			LocalPath:       basePath,
			OpenVox:         &openVox,
			ProductionAlias: &productionAlias,
		},
		Name: "test",
	}

	log := slog.Default()
	ensureProductionAlias(context.Background(), repo, testDefaultBranch, map[string]struct{}{testDefaultBranch: {}, productionAliasName: {}}, log)

	if _, err := os.Lstat(filepath.Join(basePath, "production")); !os.IsNotExist(err) {
		t.Fatalf("expected no production alias when production branch exists upstream, got err=%v", err)
	}
}

func TestEnsureSymlink_UpdatesExistingTarget(t *testing.T) {
	basePath := t.TempDir()
	linkPath := filepath.Join(basePath, "production")

	if err := os.Symlink("master", linkPath); err != nil {
		t.Fatal(err)
	}
	if err := ensureSymlink(linkPath, testDefaultBranch); err != nil {
		t.Fatalf("ensureSymlink failed: %v", err)
	}

	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink failed: %v", err)
	}
	if target != testDefaultBranch {
		t.Fatalf("symlink target = %q, want %q", target, testDefaultBranch)
	}
}

func TestExtractRemoteRefState(t *testing.T) {
	refs := []*plumbing.Reference{
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(testDefaultBranch)),
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(testDefaultBranch), plumbing.ZeroHash),
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature-a"), plumbing.ZeroHash),
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(productionAliasName), plumbing.ZeroHash),
		plumbing.NewHashReference(plumbing.NewTagReferenceName("v1.0.0"), plumbing.ZeroHash),
	}

	defaultBranch, branches, matchedBranches, matchedTags := extractRemoteRefState(
		refs,
		[]config.Pattern{{Raw: "*"}},
		[]config.Pattern{{Raw: "*"}},
	)

	if defaultBranch != testDefaultBranch {
		t.Fatalf("default branch = %q, want %q", defaultBranch, testDefaultBranch)
	}
	if _, ok := branches[productionAliasName]; !ok {
		t.Fatalf("expected %q to be present in remote branch set", productionAliasName)
	}
	if len(matchedBranches) != 3 {
		t.Fatalf("matched branches = %d, want 3", len(matchedBranches))
	}
	if len(matchedTags) != 1 || matchedTags[0] != "v1.0.0" {
		t.Fatalf("matched tags = %v, want [v1.0.0]", matchedTags)
	}
}

func TestCleanupOrphanOpenVoxLockFiles(t *testing.T) {
	basePath := t.TempDir()
	log := slog.Default()

	orphanLock := filepath.Join(basePath, "missing.gfetch.lock")
	if err := os.WriteFile(orphanLock, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(basePath, "main"), 0755); err != nil {
		t.Fatal(err)
	}
	activeLock := filepath.Join(basePath, "main.gfetch.lock")
	if err := os.WriteFile(activeLock, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	cleanupOrphanOpenVoxLockFiles("test", basePath, false, log)

	if _, err := os.Stat(orphanLock); !os.IsNotExist(err) {
		t.Fatalf("expected orphan lock to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(activeLock); err != nil {
		t.Fatalf("expected active lock to remain, got err=%v", err)
	}
}

func TestCleanupOpenVoxArtifactsForDir(t *testing.T) {
	dirPath := filepath.Join(t.TempDir(), "feature")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		t.Fatal(err)
	}
	lockPath := openVoxLockPath(dirPath)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	cleanupOpenVoxArtifactsForDir(dirPath)

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock file to remain for orphan cleanup, got err=%v", err)
	}
}

func TestKeyedLockManager_RemovesEntryAfterRelease(t *testing.T) {
	manager := newKeyedLockManager()
	release := manager.Acquire("/tmp/example")
	if len(manager.entries) != 1 {
		t.Fatalf("expected 1 lock entry after acquire, got %d", len(manager.entries))
	}

	release()
	if len(manager.entries) != 0 {
		t.Fatalf("expected lock entry removed after release, got %d", len(manager.entries))
	}
}

func TestShouldCheckoutBranch_WhenUpdated(t *testing.T) {
	needsCheckout, dirty, err := shouldCheckoutBranch(nil, "ignored", true)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !needsCheckout {
		t.Fatal("expected checkout when branch was updated")
	}
	if dirty {
		t.Fatal("did not expect dirty flag when branch was updated")
	}
}

func TestShouldCheckoutBranch_WhenUpToDateAndClean(t *testing.T) {
	repo := initTestRepoWithCommit(t)

	needsCheckout, dirty, err := shouldCheckoutBranch(repo, "master", false)
	if err != nil {
		t.Fatalf("shouldCheckoutBranch failed: %v", err)
	}
	if needsCheckout {
		t.Fatal("expected checkout to be skipped for clean up-to-date branch")
	}
	if dirty {
		t.Fatal("did not expect dirty flag for clean up-to-date branch")
	}
}

func TestShouldCheckoutBranch_WhenUpToDateButDirty(t *testing.T) {
	basePath := t.TempDir()
	repo := initTestRepoWithCommitAtPath(t, basePath)

	if err := os.WriteFile(filepath.Join(basePath, "README.md"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}

	needsCheckout, dirty, err := shouldCheckoutBranch(repo, "master", false)
	if err != nil {
		t.Fatalf("shouldCheckoutBranch failed: %v", err)
	}
	if !needsCheckout {
		t.Fatal("expected checkout when branch is dirty")
	}
	if !dirty {
		t.Fatal("expected dirty flag for manual local changes")
	}
}

func initTestRepoWithCommit(t *testing.T) *git.Repository {
	t.Helper()
	return initTestRepoWithCommitAtPath(t, t.TempDir())
}

func initTestRepoWithCommitAtPath(t *testing.T, dir string) *git.Repository {
	t.Helper()
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}

	sig := &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}

	return r
}

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
	pruneStaleOpenVoxDirs(context.Background(), repo, activeNames, staleAge, false, "main", log, result)

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
	pruneStaleOpenVoxDirs(context.Background(), repo, activeNames, staleAge, true, "", log, result)

	if _, err := os.Stat(filepath.Join(basePath, "old_branch")); err != nil {
		t.Error("directory should still exist in dry-run mode")
	}

	// But it should still appear in stale/pruned lists.
	if len(result.BranchesStale) != 1 || result.BranchesStale[0] != "old-branch" {
		t.Errorf("expected old-branch in stale list, got %v", result.BranchesStale)
	}
}

func TestPruneStaleOpenVoxDirs_LeavesLockFileForOrphanCleanup(t *testing.T) {
	basePath := t.TempDir()
	log := slog.Default()
	staleAge := 180 * 24 * time.Hour
	past := time.Now().Add(-365 * 24 * time.Hour)

	initOpenVoxBranchRepo(t, basePath, "old-branch", past)
	lockPath := openVoxLockPath(filepath.Join(basePath, "old_branch"))
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	activeNames := map[string]string{
		"old_branch": "old-branch",
	}

	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: basePath},
		Name:         "test",
	}

	result := &Result{RepoName: "test"}
	pruneStaleOpenVoxDirs(context.Background(), repo, activeNames, staleAge, false, "", log, result)

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock file to remain after stale prune, stat err=%v", err)
	}

	cleanupOrphanOpenVoxLockFiles("test", basePath, false, log)
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphan cleanup to remove lock file, stat err=%v", err)
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
	pruneStaleOpenVoxDirs(context.Background(), repo, activeNames, staleAge, false, "", log, result)

	if len(result.BranchesPruned) != 0 {
		t.Errorf("expected no branches pruned, got %v", result.BranchesPruned)
	}
}
