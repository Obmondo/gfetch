package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ashish1099/gfetch/pkg/config"
	"github.com/ashish1099/gfetch/pkg/metrics"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// syncBranch fetches a single branch and hard-resets the local branch to match remote.
// Returns true if the branch was updated, false if already up-to-date.
func syncBranch(ctx context.Context, repo *git.Repository, branch, remoteURL string, auth transport.AuthMethod, repoName string, log *slog.Logger) (bool, error) {
	start := time.Now()
	remoteName := "origin"
	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remoteName, branch)

	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: remoteName,
		RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec(refSpec)},
		Auth:       auth,
		Tags:       git.NoTags,
		Force:      true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return false, fmt.Errorf("fetching branch %s: %w", branch, err)
	}

	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName(remoteName, branch), true)
	if err != nil {
		return false, fmt.Errorf("resolving remote ref for %s: %w", branch, err)
	}

	localRefName := plumbing.NewBranchReferenceName(branch)
	localRef, err := repo.Reference(localRefName, true)

	if err == nil && localRef.Hash() == remoteRef.Hash() {
		log.Debug("branch already up-to-date", "branch", branch)
		return false, nil
	}

	// Update or create the local branch reference to point to the remote hash.
	newRef := plumbing.NewHashReference(localRefName, remoteRef.Hash())
	if err := repo.Storer.SetReference(newRef); err != nil {
		return false, fmt.Errorf("setting local ref for %s: %w", branch, err)
	}

	duration := time.Since(start)
	metrics.SyncDurationSeconds.WithLabelValues(repoName, "branch").Observe(duration.Seconds())
	log.Info("branch synced", "branch", branch, "hash", remoteRef.Hash().String()[:12], "duration", duration)
	return true, nil
}

// findObsoleteBranches returns local branches that don't match any configured pattern.
func findObsoleteBranches(repo *git.Repository, patterns []config.Pattern) ([]string, error) {
	branches, err := repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("listing local branches: %w", err)
	}

	var obsolete []string
	err = branches.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		if !matchesAnyPattern(name, patterns) {
			obsolete = append(obsolete, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterating local branches: %w", err)
	}
	return obsolete, nil
}

// checkoutRef checks out the named branch or tag and hard-resets the working tree.
func checkoutRef(repo *git.Repository, name string, log *slog.Logger) error {
	// Try branch first, then tag.
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), true)
	if err != nil {
		ref, err = repo.Reference(plumbing.NewTagReferenceName(name), true)
		if err != nil {
			return fmt.Errorf("ref %q not found as branch or tag: %w", name, err)
		}
	}

	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}

	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: ref.Name(),
		Force:  true,
	}); err != nil {
		return fmt.Errorf("checkout %s: %w", name, err)
	}

	if err := wt.Reset(&git.ResetOptions{
		Commit: ref.Hash(),
		Mode:   git.HardReset,
	}); err != nil {
		return fmt.Errorf("reset %s: %w", name, err)
	}

	log.Info("checked out ref", "ref", name, "hash", ref.Hash().String()[:12])
	return nil
}

// deleteBranch removes a local branch and its remote tracking ref.
func deleteBranch(repo *git.Repository, branch string) error {
	localRef := plumbing.NewBranchReferenceName(branch)
	if err := repo.Storer.RemoveReference(localRef); err != nil {
		return fmt.Errorf("deleting local branch ref %s: %w", branch, err)
	}

	remoteRef := plumbing.NewRemoteReferenceName("origin", branch)
	if _, err := repo.Reference(remoteRef, true); err == nil {
		if err := repo.Storer.RemoveReference(remoteRef); err != nil {
			return fmt.Errorf("deleting remote tracking ref %s: %w", branch, err)
		}
	}
	return nil
}
