package sync

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ashish1099/gitsync/pkg/config"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestMatchesAnyPattern(t *testing.T) {
	patterns := []config.Pattern{
		{Raw: "v1.0.0"},
		{Raw: "/^v[0-9]+\\./"},
	}
	// Compile regex patterns.
	for i := range patterns {
		if err := patterns[i].Compile(); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name   string
		expect bool
	}{
		{"v1.0.0", true},
		{"v2.3.4", true},
		{"release-1.0", false},
		{"v0.1-beta", true},
	}
	for _, tt := range tests {
		if got := matchesAnyPattern(tt.name, patterns); got != tt.expect {
			t.Errorf("matchesAnyPattern(%q) = %v, want %v", tt.name, got, tt.expect)
		}
	}
}

func TestMatchesAnyPattern_Branches(t *testing.T) {
	patterns := []config.Pattern{
		{Raw: "main"},
		{Raw: "/^release-.*/"},
	}
	for i := range patterns {
		if err := patterns[i].Compile(); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name   string
		expect bool
	}{
		{"main", true},
		{"release-1.0", true},
		{"release-2.0-beta", true},
		{"develop", false},
		{"main2", false},
	}
	for _, tt := range tests {
		if got := matchesAnyPattern(tt.name, patterns); got != tt.expect {
			t.Errorf("matchesAnyPattern(%q) = %v, want %v", tt.name, got, tt.expect)
		}
	}
}

func TestNew(t *testing.T) {
	logger := slog.Default()
	s := New(logger)
	if s == nil {
		t.Fatal("expected non-nil syncer")
	}
	if s.logger != logger {
		t.Error("logger not set correctly")
	}
}

// initBareAndClone creates a bare "remote" repo with a single commit, clones it to localPath,
// and creates the given extra branches in the clone. Returns the clone.
func initBareAndClone(t *testing.T, bareDir, localDir string, extraBranches []string) *git.Repository {
	t.Helper()

	// Init bare remote with an initial commit.
	bare, err := git.PlainInit(bareDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// We need a commit in the bare repo. Create a temp working clone to make a commit.
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	clone, err := git.PlainClone(tmpClone, false, &git.CloneOptions{URL: bareDir})
	if err != nil {
		// bare repo is empty, init and push instead
		clone, err = git.PlainInit(tmpClone, false)
		if err != nil {
			t.Fatal(err)
		}
		_, err = clone.CreateRemote(&gitconfig.RemoteConfig{
			Name: "origin",
			URLs: []string{bareDir},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	wt, err := clone.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	// Create a file and commit.
	fpath := filepath.Join(tmpClone, "README.md")
	if err := os.WriteFile(fpath, []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	commitHash, err := wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Push main to bare.
	if err := clone.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}

	// Create extra branches in bare repo pointing to the same commit.
	for _, branch := range extraBranches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), commitHash)
		if err := bare.Storer.SetReference(ref); err != nil {
			t.Fatal(err)
		}
	}

	// Now clone the bare repo to the actual local path.
	local, err := git.PlainClone(localDir, false, &git.CloneOptions{URL: bareDir})
	if err != nil {
		t.Fatal(err)
	}

	// Fetch and create local branches for the extras.
	for _, branch := range extraBranches {
		refSpec := gitconfig.RefSpec("+refs/heads/" + branch + ":refs/remotes/origin/" + branch)
		if err := local.Fetch(&git.FetchOptions{RefSpecs: []gitconfig.RefSpec{refSpec}}); err != nil && err != git.NoErrAlreadyUpToDate {
			t.Fatal(err)
		}
		remoteRef, err := local.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
		if err != nil {
			t.Fatal(err)
		}
		localRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), remoteRef.Hash())
		if err := local.Storer.SetReference(localRef); err != nil {
			t.Fatal(err)
		}
	}

	return local
}

func TestCheckoutBranchNotPruned(t *testing.T) {
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	localDir := filepath.Join(t.TempDir(), "local")

	// Create a repo with main + obsolete-branch + checkout-branch.
	// Configure patterns to only match "main", making the others obsolete.
	// Set checkout to "checkout-branch" — it should survive pruning.
	repo := initBareAndClone(t, bareDir, localDir, []string{"obsolete-branch", "checkout-branch"})

	patterns := []config.Pattern{{Raw: "main"}}
	for i := range patterns {
		if err := patterns[i].Compile(); err != nil {
			t.Fatal(err)
		}
	}

	obsolete, err := findObsoleteBranches(repo, patterns)
	if err != nil {
		t.Fatal(err)
	}

	// Both "obsolete-branch" and "checkout-branch" should appear as obsolete.
	found := map[string]bool{}
	for _, b := range obsolete {
		found[b] = true
	}
	if !found["obsolete-branch"] {
		t.Error("expected obsolete-branch in obsolete list")
	}
	if !found["checkout-branch"] {
		t.Error("expected checkout-branch in obsolete list")
	}

	// Simulate the pruning loop from SyncRepo with checkout protection.
	checkoutName := "checkout-branch"
	var pruned []string
	for _, branch := range obsolete {
		if branch == checkoutName {
			continue // protected
		}
		if err := deleteBranch(repo, branch); err != nil {
			t.Fatal(err)
		}
		pruned = append(pruned, branch)
	}

	// Verify checkout-branch was NOT pruned.
	for _, b := range pruned {
		if b == checkoutName {
			t.Errorf("checkout branch %q should not have been pruned", checkoutName)
		}
	}

	// Verify checkout-branch ref still exists.
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(checkoutName), true); err != nil {
		t.Errorf("checkout branch ref should still exist, got: %v", err)
	}

	// Verify obsolete-branch ref was deleted.
	if _, err := repo.Reference(plumbing.NewBranchReferenceName("obsolete-branch"), true); err == nil {
		t.Error("obsolete-branch should have been pruned")
	}
}

func TestCheckoutRef(t *testing.T) {
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	localDir := filepath.Join(t.TempDir(), "local")

	repo := initBareAndClone(t, bareDir, localDir, []string{"develop"})

	logger := slog.Default()

	// Checkout develop branch.
	if err := checkoutRef(repo, "develop", logger); err != nil {
		t.Fatalf("checkoutRef(develop) failed: %v", err)
	}

	// Verify HEAD points to develop.
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Name() != plumbing.NewBranchReferenceName("develop") {
		t.Errorf("HEAD = %s, want refs/heads/develop", head.Name())
	}
}

func TestCheckoutRef_NotFound(t *testing.T) {
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	localDir := filepath.Join(t.TempDir(), "local")

	repo := initBareAndClone(t, bareDir, localDir, nil)

	logger := slog.Default()

	err := checkoutRef(repo, "nonexistent", logger)
	if err == nil {
		t.Error("expected error for nonexistent ref")
	}
}
