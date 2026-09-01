package gsync

import (
	"context"
	"errors"
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
	localPath := filepath.Join(basePath, testDefaultBranch)

	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localPath, "README.txt"), []byte("not a git repo"), 0644); err != nil {
		t.Fatal(err)
	}

	repoCfg := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: localPath},
		Name:         DefaultTestName,
		URL:          "https://example.com/repo.git",
	}

	r, err := getRepoWithSharedCache(localPath, filepath.Join(basePath, ".git", "cache.git"), "https://example.com/repo.git", nil)
	if err != nil {
		t.Fatalf("ensureClonedOpenVox failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(localPath, ".git")); err != nil {
		t.Fatalf("expected .git to exist after recovery: %v", err)
	}

	remote, err := r.Remote(RemoteOrigin)
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
		Name: DefaultTestName,
	}

	ensureProductionAlias(context.Background(), repo, testDefaultBranch, map[string]struct{}{testDefaultBranch: {}})

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
		Name: DefaultTestName,
	}

	ensureProductionAlias(context.Background(), repo, testDefaultBranch, map[string]struct{}{testDefaultBranch: {}, productionAliasName: {}})

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
		plumbing.NewHashReference(plumbing.NewTagReferenceName(DefaultTag), plumbing.ZeroHash),
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
	if len(matchedTags) != 1 || matchedTags[0].Name().Short() != DefaultTag {
		t.Fatalf("matched tags = %v, want [v1.0.0]", matchedTags)
	}
}

func TestCleanupOrphanOpenVoxLockFiles(t *testing.T) {
	basePath := t.TempDir()

	orphanLock := filepath.Join(basePath, "missing.gfetch.lock")
	if err := os.WriteFile(orphanLock, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(basePath, testDefaultBranch), 0755); err != nil {
		t.Fatal(err)
	}
	activeLock := filepath.Join(basePath, "main.gfetch.lock")
	if err := os.WriteFile(activeLock, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	cleanupOrphanOpenVoxLockFiles(DefaultTestName, basePath, false)

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

	sig := &object.Signature{Name: DefaultTestName, Email: DefaultTestEmail, When: time.Now()}
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
		{DefaultTag, "v1_0_0"},
		{FeatureAuth, "feature_auth"},
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
		names := []string{testDefaultBranch, DevelopBranch, FeatureAuth}
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
		names := []string{"feature/auth", FeatureAuth}
		msg := detectCollisions(names, m)
		if msg == "" {
			t.Error("expected collision between feature/auth and feature-auth")
		}
	})

	t.Run("same name no collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{testDefaultBranch, testDefaultBranch}
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
		Name: RemoteOrigin,
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
	sig := &object.Signature{Name: DefaultTestName, Email: DefaultTestEmail, When: commitTime}
	if _, err := wt.Commit("commit on "+branch, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
}

func TestPruneStaleOpenVoxDirs(t *testing.T) {
	basePath := t.TempDir()
	staleAge := 180 * 24 * time.Hour

	// Create branches: main (fresh), stale-feature (old), fresh-feature (recent).
	now := time.Now()
	past := now.Add(-365 * 24 * time.Hour) // 1 year ago

	initOpenVoxBranchRepo(t, basePath, testDefaultBranch, now)
	initOpenVoxBranchRepo(t, basePath, "stale-feature", past)
	initOpenVoxBranchRepo(t, basePath, "fresh-feature", now)

	activeNames := map[string]string{
		testDefaultBranch: testDefaultBranch,
		"stale_feature":   "stale-feature",
		"fresh_feature":   "fresh-feature",
	}

	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: basePath},
		Name:         DefaultTestName,
	}

	result := &Result{RepoName: DefaultTestName}

	// testDefaultBranch is the default branch — should be protected even if stale.
	pruneStaleOpenVoxDirs(context.Background(), repo, activeNames, staleAge, false, testDefaultBranch, result)

	// stale-feature should be pruned.
	if _, err := os.Stat(filepath.Join(basePath, "stale_feature")); !os.IsNotExist(err) {
		t.Error("stale-feature directory should have been pruned")
	}

	// fresh-feature should still exist.
	if _, err := os.Stat(filepath.Join(basePath, "fresh_feature")); err != nil {
		t.Error("fresh-feature directory should NOT have been pruned")
	}

	// main should still exist (protected as default branch).
	if _, err := os.Stat(filepath.Join(basePath, testDefaultBranch)); err != nil {
		t.Error("main directory should NOT have been pruned (default branch)")
	}

	// Check result.
	found := false
	for _, b := range result.BranchesPruned {
		if b == "stale-feature" {
			found = true
		}
		if b == testDefaultBranch {
			t.Error("main should not appear in pruned list")
		}
	}
	if !found {
		t.Errorf("expected stale-feature in pruned list, got %v", result.BranchesPruned)
	}
}

func TestPruneStaleOpenVoxDirs_DryRun(t *testing.T) {
	basePath := t.TempDir()
	staleAge := 180 * 24 * time.Hour
	past := time.Now().Add(-365 * 24 * time.Hour)

	initOpenVoxBranchRepo(t, basePath, OldBranch, past)

	activeNames := map[string]string{
		"old_branch": OldBranch,
	}

	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: basePath},
		Name:         DefaultTestName,
	}

	result := &Result{RepoName: DefaultTestName}

	// Dry run — directory should NOT be removed.
	pruneStaleOpenVoxDirs(context.Background(), repo, activeNames, staleAge, true, "", result)

	if _, err := os.Stat(filepath.Join(basePath, "old_branch")); err != nil {
		t.Error("directory should still exist in dry-run mode")
	}

	// But it should still appear in stale/pruned lists.
	if len(result.BranchesStale) != 1 || result.BranchesStale[0] != OldBranch {
		t.Errorf("expected old-branch in stale list, got %v", result.BranchesStale)
	}
}

func TestPruneStaleOpenVoxDirs_LeavesLockFileForOrphanCleanup(t *testing.T) {
	basePath := t.TempDir()
	staleAge := 180 * 24 * time.Hour
	past := time.Now().Add(-365 * 24 * time.Hour)

	initOpenVoxBranchRepo(t, basePath, OldBranch, past)
	lockPath := openVoxLockPath(filepath.Join(basePath, "old_branch"))
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	activeNames := map[string]string{
		"old_branch": OldBranch,
	}

	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: basePath},
		Name:         DefaultTestName,
	}

	result := &Result{RepoName: DefaultTestName}
	pruneStaleOpenVoxDirs(context.Background(), repo, activeNames, staleAge, false, "", result)

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock file to remain after stale prune, stat err=%v", err)
	}

	cleanupOrphanOpenVoxLockFiles(DefaultTestName, basePath, false)
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphan cleanup to remove lock file, stat err=%v", err)
	}
}

func TestPruneStaleOpenVoxDirs_MissingDir(t *testing.T) {
	basePath := t.TempDir()
	staleAge := 180 * 24 * time.Hour

	activeNames := map[string]string{
		"missing_branch": "missing-branch",
	}

	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: basePath},
		Name:         DefaultTestName,
	}

	result := &Result{RepoName: DefaultTestName}

	// Should NOT log a warning or fail if the directory is missing.
	// We can't easily check logs here without a custom handler, but we can ensure it doesn't crash or add to results.
	pruneStaleOpenVoxDirs(context.Background(), repo, activeNames, staleAge, false, "", result)

	if len(result.BranchesPruned) != 0 {
		t.Errorf("expected no branches pruned, got %v", result.BranchesPruned)
	}
}

func TestIsBranchUpToDateLocal_RefMatchesButObjectMissing(t *testing.T) {
	repo := initTestRepoWithCommit(t)

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	// Real branch whose object is present: up to date.
	upToDate, err := isBranchUpToDateLocal(repo, head.Name().Short(), head.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !upToDate {
		t.Fatal("expected up-to-date for a branch whose objects are present")
	}

	// Ref present but points at a missing object: must NOT take the fast path,
	// otherwise the subsequent checkout fails with "object not found".
	missing := plumbing.NewHash("1234567890123456789012345678901234567890")
	ghost := plumbing.NewHashReference(plumbing.NewBranchReferenceName("ghost"), missing)
	if err := repo.Storer.SetReference(ghost); err != nil {
		t.Fatal(err)
	}
	upToDate, err = isBranchUpToDateLocal(repo, "ghost", missing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upToDate {
		t.Fatal("expected NOT up-to-date when the ref's objects are missing locally")
	}
}

func TestIsTagUpToDateLocal_RefMatchesButObjectMissing(t *testing.T) {
	repo := initTestRepoWithCommit(t)

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	// Lightweight tag at a present commit: up to date.
	realTag := plumbing.NewHashReference(plumbing.NewTagReferenceName("v-real"), head.Hash())
	if err := repo.Storer.SetReference(realTag); err != nil {
		t.Fatal(err)
	}
	upToDate, err := isTagUpToDateLocal(repo, "v-real", head.Hash())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !upToDate {
		t.Fatal("expected up-to-date for a tag whose objects are present")
	}

	missing := plumbing.NewHash("1234567890123456789012345678901234567890")
	ghost := plumbing.NewHashReference(plumbing.NewTagReferenceName("v-ghost"), missing)
	if err := repo.Storer.SetReference(ghost); err != nil {
		t.Fatal(err)
	}
	upToDate, err = isTagUpToDateLocal(repo, "v-ghost", missing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upToDate {
		t.Fatal("expected NOT up-to-date when the tag's objects are missing locally")
	}
}

func TestIsRecoverableOpenVoxRepoError_ObjectNotFound(t *testing.T) {
	// The exact shape checkoutRefContext returns when a ref exists but its
	// objects are missing. This must be recoverable so the caller recreates
	// the repo and retries instead of failing the branch/tag.
	if !isRecoverableOpenVoxRepoError(errors.New("checkout main: object not found")) {
		t.Fatal("expected 'object not found' checkout error to be recoverable")
	}
	if !isRecoverableOpenVoxRepoError(errors.New("object not found")) {
		t.Fatal("expected 'object not found' to be recoverable")
	}
	if isRecoverableOpenVoxRepoError(errors.New("permission denied")) {
		t.Fatal("did not expect an unrelated error to be recoverable")
	}
	if isRecoverableOpenVoxRepoError(nil) {
		t.Fatal("nil must not be recoverable")
	}
}

// initBareRemoteWithDefaultBranch creates a bare remote whose default branch is
// defaultBranch (e.g. "main", not go-git's "master"), plus extra branches, each
// pointing at a single seeded commit. Returns the commit hash.
func initBareRemoteWithDefaultBranch(t *testing.T, bareDir, defaultBranch string, extraBranches []string) plumbing.Hash {
	t.Helper()
	bare, err := git.PlainInit(bareDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(defaultBranch))); err != nil {
		t.Fatal(err)
	}

	tmp := filepath.Join(t.TempDir(), "seed")
	work, err := git.PlainInit(tmp, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := work.CreateRemote(&gitconfig.RemoteConfig{Name: RemoteOrigin, URLs: []string{bareDir}}); err != nil {
		t.Fatal(err)
	}
	wt, err := work.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: DefaultTestName, Email: DefaultTestEmail, When: time.Now()}
	commit, err := wt.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(defaultBranch), commit)); err != nil {
		t.Fatal(err)
	}
	if err := work.Push(&git.PushOptions{RefSpecs: []gitconfig.RefSpec{gitconfig.RefSpec("+refs/heads/" + defaultBranch + ":refs/heads/" + defaultBranch)}}); err != nil {
		t.Fatal(err)
	}
	for _, b := range extraBranches {
		if err := bare.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), commit)); err != nil {
			t.Fatal(err)
		}
	}
	return commit
}

// TestSyncRepo_OpenVoxFirstCloneNonMasterDefault guards the first-clone case
// where the upstream default branch isn't "master". The shared cache is created
// with a bare "master" HEAD; after refs are fetched into it the cache HEAD still
// dangles, so cloning per-branch dirs from it fails with "reference not found"
// unless getRepoWithSharedCache falls back to init+alternates.
func TestSyncRepo_OpenVoxFirstCloneNonMasterDefault(t *testing.T) {
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	initBareRemoteWithDefaultBranch(t, bareDir, MainBranch, []string{DevelopBranch, FeatureAuth})

	localDir := filepath.Join(t.TempDir(), "openvox-local") // does not exist yet -> first clone

	patterns := []config.Pattern{{Raw: "*"}}
	if err := patterns[0].Compile(); err != nil {
		t.Fatal(err)
	}
	openvox := true
	repo := &config.RepoConfig{
		RepoDefaults: config.RepoDefaults{LocalPath: localDir, Branches: patterns, OpenVox: &openvox},
		Name:         DefaultTestName,
		URL:          bareDir,
	}

	res := New().SyncRepo(context.Background(), repo, SyncOptions{})
	if res.Err != nil {
		t.Fatalf("first-clone openvox sync errored: %v", res.Err)
	}
	if len(res.BranchesFailed) != 0 {
		t.Fatalf("branches failed on first clone: %v", res.BranchesFailed)
	}

	// Every branch dir must exist with the checked-out working tree.
	for _, b := range []string{MainBranch, DevelopBranch, FeatureAuth} {
		f := filepath.Join(localDir, SanitizeName(b), "README.md")
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing checked-out file for branch %q at %s: %v", b, f, err)
		}
	}
}

func TestIsUnclonableCacheErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"empty-remote", errors.New("remote repository is empty"), true},
		{"headless-cache", errors.New("reference not found"), true},
		{"wrapped-headless", errors.New("some context: reference not found"), true},
		{"unrelated", errors.New("permission denied"), false},
	}
	for _, tc := range cases {
		if got := isUnclonableCacheErr(tc.err); got != tc.want {
			t.Errorf("%s: isUnclonableCacheErr=%v want %v", tc.name, got, tc.want)
		}
	}
}
