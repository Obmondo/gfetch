package gsync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/obmondo/gfetch/pkg/config"
)

// resolveBranches lists remote branches and returns references matching any of the configured patterns.
func resolveBranches(ctx context.Context, repo *git.Repository, patterns []config.Pattern, auth transport.AuthMethod) ([]*plumbing.Reference, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return nil, fmt.Errorf("getting remote: %w", err)
	}

	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("listing remote refs: %w", err)
	}

	var matched []*plumbing.Reference
	seen := make(map[string]bool)
	for _, ref := range refs {
		name := ref.Name()
		if name.IsBranch() {
			branchName := name.Short()
			if seen[branchName] {
				continue
			}
			if config.MatchesAny(branchName, patterns) {
				matched = append(matched, ref)
				seen[branchName] = true
			}
		}
	}
	return matched, nil
}

// resolveDefaultBranch returns the short name of the remote's default branch (HEAD target).
func resolveDefaultBranch(ctx context.Context, repo *git.Repository, auth transport.AuthMethod) string {
	remote, err := repo.Remote("origin")
	if err != nil {
		return ""
	}
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return ""
	}
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD {
			return ref.Target().Short()
		}
	}
	return ""
}

// resolveTags lists remote tags and returns names matching any of the configured patterns.
func resolveTags(ctx context.Context, repo *git.Repository, patterns []config.Pattern, auth transport.AuthMethod) ([]string, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return nil, fmt.Errorf("getting remote: %w", err)
	}

	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("listing remote refs: %w", err)
	}

	var matched []string
	seen := make(map[string]bool)
	for _, ref := range refs {
		name := ref.Name()
		if name.IsTag() {
			tagName := name.Short()
			if seen[tagName] {
				continue
			}
			if config.MatchesAny(tagName, patterns) {
				matched = append(matched, tagName)
				seen[tagName] = true
			}
		}
	}
	return matched, nil
}

// checkStaleness checks if a remote reference is stale (older than age) by inspecting its commit date.
// It tries to find the commit locally first. If not found, it fetches the commit metadata (depth 1).
func checkStaleness(ctx context.Context, repo *git.Repository, ref *plumbing.Reference, age time.Duration, auth transport.AuthMethod) (bool, error) {
	// 1. Check if we already have the commit locally.
	commit, err := repo.CommitObject(ref.Hash())
	if err == nil {
		return time.Since(commit.Committer.When) > age, nil
	}

	// 2. If not found locally, fetch minimal info (Depth 1) to check the date.
	// We fetch into a temporary ref to avoid polluting refs/heads or refs/remotes/origin
	// if we decide not to keep it.
	refName := ref.Name().String()
	tmpRef := fmt.Sprintf("refs/gfetch-tmp/%s", ref.Name().Short())
	refSpec := fmt.Sprintf("+%s:%s", refName, tmpRef)

	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec(refSpec)},
		Depth:      1,
		Auth:       auth,
		Tags:       git.NoTags,
	})
	if err != nil {
		return false, fmt.Errorf("fetching depth 1 for stale check: %w", err)
	}

	// Clean up the temporary ref defer-style or immediately.
	defer repo.Storer.RemoveReference(plumbing.ReferenceName(tmpRef))

	// After fetch, the object should be in the store.
	commit, err = repo.CommitObject(ref.Hash())
	if err != nil {
		return false, fmt.Errorf("getting commit after fetch: %w", err)
	}

	return time.Since(commit.Committer.When) > age, nil
}

// IsStale is a helper that wraps checkStaleness and handles logging.
// Returns true if the branch is definitely stale and should be skipped.
// Returns false if it's fresh OR if we couldn't determine (fail-safe).
func IsStale(ctx context.Context, repo *git.Repository, ref *plumbing.Reference, age time.Duration, auth transport.AuthMethod, log *slog.Logger) bool {
	stale, err := checkStaleness(ctx, repo, ref, age, auth)
	if err != nil {
		log.Warn("failed to check staleness, syncing anyway", "branch", ref.Name().Short(), "error", err)
		return false
	}
	if stale {
		log.Info("skipping stale branch sync", "branch", ref.Name().Short())
		return true
	}
	return false
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
		if !config.MatchesAny(name, patterns) {
			obsolete = append(obsolete, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterating local branches: %w", err)
	}
	return obsolete, nil
}

// findStaleBranches returns local branches that match configured patterns but have no commits in the last age duration.
func findStaleBranches(repo *git.Repository, patterns []config.Pattern, age time.Duration) ([]string, error) {
	if age == 0 {
		return nil, nil
	}

	branches, err := repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("listing local branches: %w", err)
	}

	var stale []string
	cutoff := time.Now().Add(-age)

	err = branches.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		if !config.MatchesAny(name, patterns) {
			return nil
		}

		commit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return fmt.Errorf("getting commit for %s: %w", name, err)
		}

		if commit.Committer.When.Before(cutoff) {
			stale = append(stale, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterating local branches: %w", err)
	}
	return stale, nil
}
