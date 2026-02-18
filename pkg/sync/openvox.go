package sync

import (
	"context"
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
	"github.com/obmondo/gfetch/pkg/metrics"
)

// metaDir is the hidden directory used to store the resolver repo for listing remote refs.
const metaDir = ".gfetch-meta"

// SanitizeName replaces hyphens and dots with underscores for OpenVox environment names.
func SanitizeName(name string) string {
	r := strings.NewReplacer("-", "_", ".", "_")
	return r.Replace(name)
}

// syncRepoOpenVox syncs a repository in OpenVox mode: each matching branch/tag gets
// its own directory under local_path with a sanitized name, checked out as a working tree.
func (s *Syncer) syncRepoOpenVox(ctx context.Context, repo *config.RepoConfig, opts SyncOptions) Result {
	start := time.Now()
	result := Result{RepoName: repo.Name}
	log := s.logger.With("repo", repo.Name, "mode", "openvox")

	auth, err := resolveAuth(repo)
	if err != nil {
		metrics.SyncFailuresTotal.WithLabelValues(repo.Name, "clone").Inc()
		result.Err = err
		return result
	}

	// Ensure the base local_path exists.
	if err := os.MkdirAll(repo.LocalPath, 0755); err != nil {
		result.Err = fmt.Errorf("creating local_path %s: %w", repo.LocalPath, err)
		return result
	}

	// Use a hidden resolver repo to list remote refs without polluting the workspace.
	resolverPath := filepath.Join(repo.LocalPath, metaDir)
	resolverRepo, err := ensureResolverRepo(ctx, resolverPath, repo.URL, auth)
	if err != nil {
		metrics.SyncFailuresTotal.WithLabelValues(repo.Name, "clone").Inc()
		result.Err = fmt.Errorf("resolver repo: %w", err)
		return result
	}

	// Track sanitized name -> original name for collision detection.
	sanitizedToOriginal := make(map[string]string)

	// Resolve and sync branches.
	if len(repo.Branches) > 0 {
		branches, err := resolveBranches(ctx, resolverRepo, repo.Branches, auth)
		if err != nil {
			log.Error("failed to resolve branches", "error", err)
			metrics.SyncFailuresTotal.WithLabelValues(repo.Name, "branch_sync").Inc()
			result.Err = fmt.Errorf("resolving branches: %w", err)
			return result
		}

		if collision := detectCollisions(branches, sanitizedToOriginal); collision != "" {
			result.Err = fmt.Errorf("name collision after sanitization: %s", collision)
			return result
		}

		for _, branch := range branches {
			dirName := SanitizeName(branch)
			dirPath := filepath.Join(repo.LocalPath, dirName)

			// Build a sub-config pointing at the per-branch directory.
			subCfg := *repo
			subCfg.LocalPath = dirPath

			r, err := ensureCloned(ctx, &subCfg, auth)
			if err != nil {
				log.Error("openvox branch clone failed", "branch", branch, "dir", dirName, "error", err)
				metrics.SyncFailuresTotal.WithLabelValues(repo.Name, "branch_sync").Inc()
				result.BranchesFailed = append(result.BranchesFailed, branch)
				continue
			}

			if _, err := syncBranch(ctx, r, branch, repo.URL, auth, repo.Name, log); err != nil {
				log.Error("openvox branch sync failed", "branch", branch, "dir", dirName, "error", err)
				metrics.SyncFailuresTotal.WithLabelValues(repo.Name, "branch_sync").Inc()
				result.BranchesFailed = append(result.BranchesFailed, branch)
				continue
			}

			if err := checkoutRef(r, branch, log); err != nil {
				log.Error("openvox branch checkout failed", "branch", branch, "dir", dirName, "error", err)
				result.BranchesFailed = append(result.BranchesFailed, branch)
				continue
			}

			result.BranchesSynced = append(result.BranchesSynced, branch)
		}
	}

	// Resolve and sync tags.
	if len(repo.Tags) > 0 {
		tags, err := resolveTags(ctx, resolverRepo, repo.Tags, auth)
		if err != nil {
			log.Error("failed to resolve tags", "error", err)
			metrics.SyncFailuresTotal.WithLabelValues(repo.Name, "tag_sync").Inc()
			if result.Err == nil {
				result.Err = fmt.Errorf("resolving tags: %w", err)
			}
		} else {
			if collision := detectCollisions(tags, sanitizedToOriginal); collision != "" {
				result.Err = fmt.Errorf("name collision after sanitization: %s", collision)
				return result
			}

			for _, tag := range tags {
				dirName := SanitizeName(tag)
				dirPath := filepath.Join(repo.LocalPath, dirName)

				// Build a sub-config pointing at the per-tag directory.
				subCfg := *repo
				subCfg.LocalPath = dirPath

				r, err := ensureCloned(ctx, &subCfg, auth)
				if err != nil {
					log.Error("openvox tag clone failed", "tag", tag, "dir", dirName, "error", err)
					metrics.SyncFailuresTotal.WithLabelValues(repo.Name, "tag_sync").Inc()
					if result.Err == nil {
						result.Err = fmt.Errorf("tag sync %s: %w", tag, err)
					}
					continue
				}

				// Single-tag fetch and checkout.
				if err := syncOpenVoxTag(ctx, r, tag, auth, log); err != nil {
					log.Error("openvox tag sync failed", "tag", tag, "dir", dirName, "error", err)
					metrics.SyncFailuresTotal.WithLabelValues(repo.Name, "tag_sync").Inc()
					if result.Err == nil {
						result.Err = fmt.Errorf("tag sync %s: %w", tag, err)
					}
					continue
				}

				result.TagsFetched = append(result.TagsFetched, tag)
			}
		}
	}

	// Prune stale directories that no longer correspond to any matched ref.
	if opts.Prune {
		pruneOpenVoxDirs(repo.LocalPath, sanitizedToOriginal, opts.DryRun, log, &result)
	}

	duration := time.Since(start)
	metrics.SyncDurationSeconds.WithLabelValues(repo.Name, "total").Observe(duration.Seconds())

	if result.Err != nil {
		metrics.LastFailureTimestamp.WithLabelValues(repo.Name).Set(float64(time.Now().Unix()))
	} else {
		metrics.SyncSuccessTotal.WithLabelValues(repo.Name).Inc()
		metrics.LastSuccessTimestamp.WithLabelValues(repo.Name).Set(float64(time.Now().Unix()))
	}

	log.Info("openvox sync complete",
		"branches_synced", len(result.BranchesSynced),
		"branches_failed", len(result.BranchesFailed),
		"tags_fetched", len(result.TagsFetched),
		"branches_pruned", len(result.BranchesPruned),
		"tags_pruned", len(result.TagsPruned),
		"duration", duration,
	)
	return result
}

// ensureResolverRepo opens or creates a bare repo used only for listing remote refs.
func ensureResolverRepo(_ context.Context, path, remoteURL string, auth transport.AuthMethod) (*git.Repository, error) {
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
			if matchesAnyPattern(tagName, patterns) {
				matched = append(matched, tagName)
				seen[tagName] = true
			}
		}
	}
	return matched, nil
}

// syncOpenVoxTag fetches a single tag into a per-directory repo and checks it out.
func syncOpenVoxTag(ctx context.Context, r *git.Repository, tag string, auth transport.AuthMethod, log *slog.Logger) error {
	refSpec := gitconfig.RefSpec(fmt.Sprintf("+refs/tags/%s:refs/tags/%s", tag, tag))
	err := r.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{refSpec},
		Auth:       auth,
		Tags:       git.NoTags,
		Force:      true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("fetching tag %s: %w", tag, err)
	}

	if err := checkoutRef(r, tag, log); err != nil {
		return fmt.Errorf("checkout tag %s: %w", tag, err)
	}
	return nil
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

// pruneOpenVoxDirs removes directories under basePath that don't correspond to any active ref.
func pruneOpenVoxDirs(basePath string, activeNames map[string]string, dryRun bool, log *slog.Logger, result *Result) {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		log.Error("failed to read local_path for pruning", "path", basePath, "error", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip hidden directories (includes .gfetch-meta).
		if strings.HasPrefix(name, ".") {
			continue
		}
		if _, active := activeNames[name]; active {
			continue
		}

		dirPath := filepath.Join(basePath, name)
		if dryRun {
			log.Info("directory would be pruned (dry-run)", "dir", name)
		} else {
			if err := os.RemoveAll(dirPath); err != nil {
				log.Error("failed to prune directory", "dir", name, "error", err)
				continue
			}
			log.Info("directory pruned", "dir", name)
		}

		result.BranchesPruned = append(result.BranchesPruned, name)
	}
}
