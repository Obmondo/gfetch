package sync

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/ashish1099/gitsync/pkg/config"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// SyncOptions controls optional sync behaviour.
type SyncOptions struct {
	Prune  bool
	DryRun bool
}

// Result holds the outcome of syncing a single repository.
type Result struct {
	RepoName         string
	BranchesSynced   []string
	BranchesUpToDate []string
	BranchesFailed   []string
	TagsFetched      []string
	TagsUpToDate     []string
	TagsObsolete     []string
	TagsPruned       []string
	BranchesObsolete []string
	BranchesPruned   []string
	Checkout         string
	Err              error
}

// Syncer performs git sync operations.
type Syncer struct {
	logger *slog.Logger
}

// New creates a new Syncer with the given logger.
func New(logger *slog.Logger) *Syncer {
	return &Syncer{logger: logger}
}

// SyncAll syncs all repositories in the config.
func (s *Syncer) SyncAll(ctx context.Context, cfg *config.Config, opts SyncOptions) []Result {
	results := make([]Result, 0, len(cfg.Repos))
	for i := range cfg.Repos {
		results = append(results, s.SyncRepo(ctx, &cfg.Repos[i], opts))
	}
	return results
}

// SyncRepo syncs a single repository.
func (s *Syncer) SyncRepo(ctx context.Context, repo *config.RepoConfig, opts SyncOptions) Result {
	result := Result{RepoName: repo.Name}
	log := s.logger.With("repo", repo.Name)

	log.Info("starting sync")

	auth, err := resolveAuth(repo)
	if err != nil {
		result.Err = err
		return result
	}

	r, err := ensureCloned(ctx, repo, auth)
	if err != nil {
		result.Err = err
		return result
	}

	// Resolve branches: list remote branches and filter by configured patterns.
	if len(repo.Branches) > 0 {
		branches, err := resolveBranches(ctx, r, repo.Branches, auth)
		if err != nil {
			log.Error("failed to resolve branches", "error", err)
			result.Err = fmt.Errorf("resolving branches: %w", err)
			return result
		}

		for _, branch := range branches {
			synced, err := syncBranch(ctx, r, branch, repo.URL, auth, log)
			if err != nil {
				log.Error("branch sync failed", "branch", branch, "error", err)
				result.BranchesFailed = append(result.BranchesFailed, branch)
			} else if synced {
				result.BranchesSynced = append(result.BranchesSynced, branch)
			} else {
				result.BranchesUpToDate = append(result.BranchesUpToDate, branch)
			}
		}
	}

	if len(repo.Branches) > 0 {
		obsolete, err := findObsoleteBranches(r, repo.Branches)
		if err != nil {
			log.Error("failed to find obsolete branches", "error", err)
		} else {
			result.BranchesObsolete = obsolete
			for _, branch := range obsolete {
				if repo.Checkout != "" && branch == repo.Checkout {
					log.Info("skipping prune of checkout branch", "branch", branch)
					continue
				}
				switch {
				case !opts.Prune:
					// just reported as obsolete, no action
				case opts.DryRun:
					log.Info("branch would be pruned (dry-run)", "branch", branch)
					result.BranchesPruned = append(result.BranchesPruned, branch)
				default:
					if err := deleteBranch(r, branch); err != nil {
						log.Error("failed to prune branch", "branch", branch, "error", err)
						continue
					}
					log.Info("branch pruned", "branch", branch)
					result.BranchesPruned = append(result.BranchesPruned, branch)
				}
			}
		}
	}

	if len(repo.Tags) > 0 {
		fetched, upToDate, obsolete, pruned, err := syncTags(ctx, r, repo, auth, opts.Prune, opts.DryRun, log)
		if err != nil {
			log.Error("tag sync failed", "error", err)
			if result.Err == nil {
				result.Err = fmt.Errorf("tag sync: %w", err)
			}
		}
		result.TagsFetched = fetched
		result.TagsUpToDate = upToDate
		result.TagsObsolete = obsolete
		result.TagsPruned = pruned
	}

	if repo.Checkout != "" {
		if err := checkoutRef(r, repo.Checkout, log); err != nil {
			log.Error("failed to checkout", "ref", repo.Checkout, "error", err)
			if result.Err == nil {
				result.Err = fmt.Errorf("checkout %s: %w", repo.Checkout, err)
			}
		} else {
			result.Checkout = repo.Checkout
		}
	}

	log.Info("sync complete",
		"branches_synced", len(result.BranchesSynced),
		"branches_up_to_date", len(result.BranchesUpToDate),
		"branches_failed", len(result.BranchesFailed),
		"branches_obsolete", len(result.BranchesObsolete),
		"branches_pruned", len(result.BranchesPruned),
		"tags_fetched", len(result.TagsFetched),
		"tags_up_to_date", len(result.TagsUpToDate),
		"tags_obsolete", len(result.TagsObsolete),
		"tags_pruned", len(result.TagsPruned),
	)
	return result
}

// ensureCloned opens an existing repo or inits an empty one with the remote configured.
// Actual fetching is deferred to syncBranch/syncTags which use narrow refspecs.
func ensureCloned(ctx context.Context, repo *config.RepoConfig, auth transport.AuthMethod) (*git.Repository, error) {
	if _, err := os.Stat(repo.LocalPath); err == nil {
		return git.PlainOpen(repo.LocalPath)
	}

	r, err := git.PlainInit(repo.LocalPath, false)
	if err != nil {
		return nil, fmt.Errorf("init %s: %w", repo.LocalPath, err)
	}

	_, err = r.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{repo.URL},
	})
	if err != nil {
		return nil, fmt.Errorf("creating remote: %w", err)
	}

	return r, nil
}

// resolveBranches lists remote branches and returns names matching any of the configured patterns.
func resolveBranches(ctx context.Context, repo *git.Repository, patterns []config.Pattern, auth transport.AuthMethod) ([]string, error) {
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
		if name.IsBranch() {
			branchName := name.Short()
			if seen[branchName] {
				continue
			}
			if matchesAnyPattern(branchName, patterns) {
				matched = append(matched, branchName)
				seen[branchName] = true
			}
		}
	}

	// Also match against symbolic refs like HEAD that resolve to branches.
	// For refs/heads/* references, Short() already gives us the branch name.
	// If a pattern is a literal (non-regex) and didn't match any remote branch, skip silently.

	return matched, nil
}

// matchesAnyPattern returns true if the given name matches any of the patterns.
func matchesAnyPattern(name string, patterns []config.Pattern) bool {
	for i := range patterns {
		if patterns[i].Matches(name) {
			return true
		}
	}
	return false
}
