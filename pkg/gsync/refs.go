package gsync

import (
	"context"
	"errors"
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
	defer func() { _ = repo.Storer.RemoveReference(plumbing.ReferenceName(tmpRef)) }()

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
		log.Debug("skipping stale branch: no commits within age threshold", "branch", ref.Name().Short(), "max_age", age)
		return true
	}
	return false
}

// batchFetchForStaleness fetches all branch tip commits into the resolver repo
// with a single depth-1 fetch. This avoids N individual SSH connections when
// checking staleness for N branches. Temporary refs are cleaned up after use.
func batchFetchForStaleness(ctx context.Context, repo *git.Repository, refs []*plumbing.Reference, auth transport.AuthMethod) (cleanup func(), err error) {
	if len(refs) == 0 {
		return func() {}, nil
	}

	refSpecs := make([]gitconfig.RefSpec, len(refs))
	tmpRefs := make([]plumbing.ReferenceName, len(refs))
	for i, ref := range refs {
		name := ref.Name().Short()
		tmp := plumbing.ReferenceName(fmt.Sprintf("refs/gfetch-tmp/%s", name))
		refSpecs[i] = gitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:%s", name, tmp))
		tmpRefs[i] = tmp
	}

	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   refSpecs,
		Depth:      1,
		Auth:       auth,
		Tags:       git.NoTags,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return func() {}, fmt.Errorf("batch fetch for staleness: %w", err)
	}

	cleanup = func() {
		for _, tmp := range tmpRefs {
			_ = repo.Storer.RemoveReference(tmp)
		}
	}
	return cleanup, nil
}

// isStaleLocal checks if a reference is stale by looking up the commit in the
// local object store only (no network). Returns false if the commit is not found
// locally (fail-safe: sync the branch if we can't determine staleness).
func isStaleLocal(repo *git.Repository, ref *plumbing.Reference, age time.Duration, log *slog.Logger) bool {
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		log.Warn("failed to check staleness locally, syncing anyway", "branch", ref.Name().Short(), "error", err)
		return false
	}
	if time.Since(commit.Committer.When) > age {
		log.Debug(
			"skipping stale branch: no commits within age threshold",
			"branch", ref.Name().Short(),
			"max_age", age,
			"commit_age", time.Since(commit.Committer.When),
			"commit_time", commit.Committer.When,
		)
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
