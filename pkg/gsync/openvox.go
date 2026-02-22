package gsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/telemetry"
)

// metaDir is the hidden directory used to store the resolver repo for listing remote refs.
const (
	metaDir        = ".gfetch-meta"
	defaultDirMode = 0755
)

// SanitizeName converts a Git ref name into a valid Puppet environment name.
// Puppet environments only allow [a-zA-Z0-9_]. Any character outside this set
// is replaced with an underscore.
func SanitizeName(name string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, name)
}

// syncRepoOpenVox syncs a repository in OpenVox mode: each matching branch/tag gets
// its own directory under local_path with a sanitized name, checked out as a working tree.
func (s *Syncer) syncRepoOpenVox(ctx context.Context, repo *config.RepoConfig, opts SyncOptions) Result {
	start := time.Now()
	result := Result{RepoName: repo.Name}
	log := s.logger.With("repo", repo.Name, "mode", "openvox")

	auth, err := resolveAuth(repo)
	if err != nil {
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "clone").Inc()
		result.Err = err
		return result
	}

	// Ensure the base local_path exists.
	if err := os.MkdirAll(repo.LocalPath, defaultDirMode); err != nil {
		result.Err = fmt.Errorf("creating local_path %s: %w", repo.LocalPath, err)
		return result
	}

	// Use a hidden resolver repo to list remote refs without polluting the workspace.
	resolverPath := filepath.Join(repo.LocalPath, metaDir)
	resolverRepo, err := ensureResolverRepo(ctx, resolverPath, repo.URL, auth)
	if err != nil {
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "clone").Inc()
		result.Err = fmt.Errorf("resolver repo: %w", err)
		return result
	}

	// Track sanitized name -> original name for collision detection.
	sanitizedToOriginal := make(map[string]string)

	if err := s.syncOpenVoxBranches(ctx, resolverRepo, repo, opts, auth, sanitizedToOriginal, log, &result); err != nil {
		return result
	}

	s.syncOpenVoxTags(ctx, resolverRepo, repo, auth, sanitizedToOriginal, log, &result)

	// Prune stale directories that no longer correspond to any matched ref.
	if opts.Prune {
		pruneOpenVoxDirs(repo.LocalPath, sanitizedToOriginal, opts.DryRun, log, &result)
	}

	// Prune directories whose latest commit is older than staleAge.
	if opts.PruneStale {
		defaultBranch := resolveDefaultBranch(ctx, resolverRepo, auth)
		pruneStaleOpenVoxDirs(repo, sanitizedToOriginal, opts.StaleAge, opts.DryRun, defaultBranch, log, &result)
	}

	s.recordOpenVoxMetrics(repo, start, &result, log)
	return result
}

func (s *Syncer) syncOpenVoxBranches(ctx context.Context, resolverRepo *git.Repository, repo *config.RepoConfig, opts SyncOptions, auth transport.AuthMethod, sanitizedToOriginal map[string]string, log *slog.Logger, result *Result) error {
	if len(repo.Branches) == 0 {
		return nil
	}

	branches, err := resolveBranches(ctx, resolverRepo, repo.Branches, auth)
	if err != nil {
		log.Error("failed to resolve branches", "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "branch_sync").Inc()
		result.Err = fmt.Errorf("resolving branches: %w", err)
		return result.Err
	}

	log.Debug("syncing branches", "count", len(branches))
	var branchNames []string
	for _, b := range branches {
		branchNames = append(branchNames, b.Name().Short())
	}

	if collision := detectCollisions(branchNames, sanitizedToOriginal); collision != "" {

		result.Err = fmt.Errorf("name collision after sanitization: %s", collision)
		return result.Err
	}

	for _, ref := range branches {
		branch := ref.Name().Short()

		if opts.PruneStale && opts.Prune && IsStale(ctx, resolverRepo, ref, opts.StaleAge, auth, log) {
			continue
		}

		s.syncOneOpenVoxBranch(ctx, repo, branch, auth, log, result)
	}
	return nil
}

func (*Syncer) syncOneOpenVoxBranch(ctx context.Context, repo *config.RepoConfig, branch string, auth transport.AuthMethod, log *slog.Logger, result *Result) {
	dirName := SanitizeName(branch)
	dirPath := filepath.Join(repo.LocalPath, dirName)

	// Build a sub-config pointing at the per-branch directory.
	subCfg := *repo
	subCfg.LocalPath = dirPath

	r, err := ensureCloned(ctx, &subCfg, auth)
	if err != nil {
		log.Error("openvox branch clone failed", "branch", branch, "dir", dirName, "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "branch_sync").Inc()
		result.BranchesFailed = append(result.BranchesFailed, branch)
		return
	}

	updated, err := syncBranch(ctx, r, branch, repo.URL, auth, repo.Name, log)
	if err != nil {
		log.Error("openvox branch sync failed", "branch", branch, "dir", dirName, "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "branch_sync").Inc()
		result.BranchesFailed = append(result.BranchesFailed, branch)
		return
	}

	if err := checkoutRef(r, branch, log); err != nil {
		log.Error("openvox branch checkout failed", "branch", branch, "dir", dirName, "error", err)
		result.BranchesFailed = append(result.BranchesFailed, branch)
		return
	}

	if updated {
		result.BranchesSynced = append(result.BranchesSynced, branch)
	} else {
		result.BranchesUpToDate = append(result.BranchesUpToDate, branch)
	}
}

func (s *Syncer) syncOpenVoxTags(ctx context.Context, resolverRepo *git.Repository, repo *config.RepoConfig, auth transport.AuthMethod, sanitizedToOriginal map[string]string, log *slog.Logger, result *Result) {
	if len(repo.Tags) == 0 {
		return
	}

	tags, err := resolveTags(ctx, resolverRepo, repo.Tags, auth)
	if err != nil {
		log.Error("failed to resolve tags", "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "tag_sync").Inc()
		if result.Err == nil {
			result.Err = fmt.Errorf("resolving tags: %w", err)
		}
		return
	}

	log.Debug("syncing tags", "count", len(tags))
	if collision := detectCollisions(tags, sanitizedToOriginal); collision != "" {
		result.Err = fmt.Errorf("name collision after sanitization: %s", collision)
		return
	}

	for _, tag := range tags {
		s.syncOneOpenVoxTag(ctx, repo, tag, auth, log, result)
	}
}

func (*Syncer) syncOneOpenVoxTag(ctx context.Context, repo *config.RepoConfig, tag string, auth transport.AuthMethod, log *slog.Logger, result *Result) {
	dirName := SanitizeName(tag)
	dirPath := filepath.Join(repo.LocalPath, dirName)

	// Build a sub-config pointing at the per-tag directory.
	subCfg := *repo
	subCfg.LocalPath = dirPath

	r, err := ensureCloned(ctx, &subCfg, auth)
	if err != nil {
		log.Error("openvox tag clone failed", "tag", tag, "dir", dirName, "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "tag_sync").Inc()
		if result.Err == nil {
			result.Err = fmt.Errorf("tag sync %s: %w", tag, err)
		}
		return
	}

	// Single-tag fetch and checkout.
	updated, err := syncOpenVoxTag(ctx, r, tag, auth, log)
	if err != nil {
		log.Error("openvox tag sync failed", "tag", tag, "dir", dirName, "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "tag_sync").Inc()
		if result.Err == nil {
			result.Err = fmt.Errorf("tag sync %s: %w", tag, err)
		}
		result.TagsFailed = append(result.TagsFailed, tag)
		return
	}

	if updated {
		result.TagsFetched = append(result.TagsFetched, tag)
	} else {
		result.TagsUpToDate = append(result.TagsUpToDate, tag)
	}
}

func (*Syncer) recordOpenVoxMetrics(repo *config.RepoConfig, start time.Time, result *Result, log *slog.Logger) {
	duration := time.Since(start)
	telemetry.SyncDurationSeconds.WithLabelValues(repo.Name, "total").Observe(duration.Seconds())

	if result.Err != nil {
		telemetry.LastFailureTimestamp.WithLabelValues(repo.Name).Set(float64(time.Now().Unix()))
		log.Error("openvox sync failed", "error", result.Err, "duration", duration)
	} else {
		logSyncSuccess(context.Background(), log, *result, duration)
		telemetry.SyncSuccessTotal.WithLabelValues(repo.Name).Inc()
		telemetry.LastSuccessTimestamp.WithLabelValues(repo.Name).Set(float64(time.Now().Unix()))
	}

	log.Debug("openvox sync details",
		"branches_synced", len(result.BranchesSynced),
		"branches_up_to_date", len(result.BranchesUpToDate),
		"branches_failed", len(result.BranchesFailed),
		"tags_fetched", len(result.TagsFetched),
		"tags_up_to_date", len(result.TagsUpToDate),
		"tags_failed", len(result.TagsFailed),
		"branches_pruned", len(result.BranchesPruned),
		"tags_pruned", len(result.TagsPruned),
		"duration", duration,
	)
}

// ensureResolverRepo opens or creates a bare repo used only for listing remote refs.
func ensureResolverRepo(_ context.Context, path, remoteURL string, _ transport.AuthMethod) (*git.Repository, error) {
	if _, err := os.Stat(path); err == nil {
		return git.PlainOpen(path)
	}

	r, err := git.PlainInit(path, true)
	if err != nil {
		return nil, fmt.Errorf("init resolver repo at %s: %w", path, err)
	}

	_, err = r.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteURL},
	})
	if err != nil {
		return nil, fmt.Errorf("creating remote in resolver repo: %w", err)
	}

	return r, nil
}

// syncOpenVoxTag fetches a single tag into a per-directory repo and checks it out.
// Returns true if the tag was updated, false if already up-to-date.
func syncOpenVoxTag(ctx context.Context, r *git.Repository, tag string, auth transport.AuthMethod, log *slog.Logger) (bool, error) {
	refSpec := gitconfig.RefSpec(fmt.Sprintf("+refs/tags/%s:refs/tags/%s", tag, tag))
	err := r.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{refSpec},
		Auth:       auth,
		Tags:       git.NoTags,
		Force:      true,
	})

	updated := true
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		updated = false
	} else if err != nil {
		return false, fmt.Errorf("fetching tag %s: %w", tag, err)
	}

	if err := checkoutRef(r, tag, log); err != nil {
		return false, fmt.Errorf("checkout tag %s: %w", tag, err)
	}
	return updated, nil
}

// detectCollisions checks if any names collide after sanitization and adds them to the map.
// Returns a descriptive error string if a collision is found, empty string otherwise.
func detectCollisions(names []string, sanitizedToOriginal map[string]string) string {
	for _, name := range names {
		sanitized := SanitizeName(name)
		if existing, ok := sanitizedToOriginal[sanitized]; ok && existing != name {
			return fmt.Sprintf("%q and %q both sanitize to %q", existing, name, sanitized)
		}
		sanitizedToOriginal[sanitized] = name
	}
	return ""
}

// pruneStaleOpenVoxDirs removes per-branch directories whose tip commit is older than staleAge.
// The remote's default branch is protected from stale pruning.
func pruneStaleOpenVoxDirs(repo *config.RepoConfig, activeNames map[string]string, staleAge time.Duration, dryRun bool, defaultBranch string, log *slog.Logger, result *Result) {
	if staleAge == 0 {
		return
	}
	cutoff := time.Now().Add(-staleAge)
	for sanitized, original := range activeNames {
		if original == defaultBranch {
			log.Info("skipping stale prune of default branch", "branch", original)
			continue
		}
		dirPath := filepath.Join(repo.LocalPath, sanitized)
		r, err := git.PlainOpen(dirPath)
		if err != nil {
			if errors.Is(err, git.ErrRepositoryNotExists) {
				if _, statErr := os.Stat(dirPath); os.IsNotExist(statErr) {
					continue
				}
			}
			log.Warn("skipping stale check: directory exists but is not a git repo", "dir", sanitized, "error", err)
			continue
		}
		head, err := r.Head()
		if err != nil {
			continue
		}
		commit, err := r.CommitObject(head.Hash())
		if err != nil {
			continue
		}
		if commit.Committer.When.Before(cutoff) {
			if dryRun {
				log.Info("stale directory would be pruned (dry-run)", "dir", sanitized, "branch", original)
			} else {
				if err := os.RemoveAll(dirPath); err != nil {
					log.Error("failed to prune stale directory", "dir", sanitized, "error", err)
					continue
				}
				log.Info("stale directory pruned", "dir", sanitized, "branch", original)
			}
			result.BranchesStale = append(result.BranchesStale, original)
			result.BranchesPruned = append(result.BranchesPruned, original)
		}
	}
}
