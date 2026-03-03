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
	"github.com/obmondo/gfetch/pkg/telemetry"
)

// syncTagsWithResolved syncs tags using a pre-resolved remote tag list to avoid
// an extra remote ref-list call.
func syncTagsWithResolved(ctx context.Context, repo *git.Repository, repoConfig *config.RepoConfig, auth transport.AuthMethod, resolvedTags []string, pruneTags bool, dryRun bool, log *slog.Logger) (fetched, upToDate, failed, obsolete, pruned []string, err error) {
	start := time.Now()

	fetched, upToDate = resolveAndFilterTagsFromResolved(repo, resolvedTags)

	if err = fetchTags(ctx, repo, fetched, auth, log); err != nil {
		return nil, upToDate, fetched, nil, nil, err
	}

	obsolete, pruned, err = handleObsoleteTags(repo, repoConfig, pruneTags, dryRun, log)
	if err != nil {
		return fetched, upToDate, nil, nil, nil, err
	}

	duration := time.Since(start)
	telemetry.SyncDurationSeconds.WithLabelValues(repoConfig.Name, "tag").Observe(duration.Seconds())
	log.Debug("tags synced", "fetched", len(fetched), "duration", duration)

	return fetched, upToDate, nil, obsolete, pruned, nil
}

func resolveAndFilterTagsFromResolved(repo *git.Repository, resolvedTags []string) (fetched, upToDate []string) {
	for _, tagName := range resolvedTags {
		if _, err := repo.Reference(plumbing.NewTagReferenceName(tagName), true); err == nil {
			upToDate = append(upToDate, tagName)
		} else {
			fetched = append(fetched, tagName)
		}
	}

	return fetched, upToDate
}

func fetchTags(ctx context.Context, repo *git.Repository, fetched []string, auth transport.AuthMethod, log *slog.Logger) error {
	if len(fetched) == 0 {
		log.Debug("no new tags to fetch")
		return nil
	}

	refSpecs := make([]gitconfig.RefSpec, len(fetched))
	for i, tag := range fetched {
		refSpecs[i] = gitconfig.RefSpec(fmt.Sprintf("+refs/tags/%s:refs/tags/%s", tag, tag))
	}

	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   refSpecs,
		Auth:       auth,
		Tags:       git.NoTags,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("fetching tags: %w", err)
	}

	for _, tag := range fetched {
		log.Info("tag fetched", "tag", tag)
	}
	return nil
}

func handleObsoleteTags(repo *git.Repository, repoConfig *config.RepoConfig, pruneTags bool, dryRun bool, log *slog.Logger) (obsolete, pruned []string, err error) {
	tagRefs, err := repo.Tags()
	if err != nil {
		return nil, nil, fmt.Errorf("listing local tags: %w", err)
	}
	err = tagRefs.ForEach(func(ref *plumbing.Reference) error {
		tagName := ref.Name().Short()
		if !config.MatchesAny(tagName, repoConfig.Tags) {
			obsolete = append(obsolete, tagName)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("iterating local tags: %w", err)
	}

	if pruneTags && len(obsolete) > 0 {
		for _, tag := range obsolete {
			if dryRun {
				log.Info("tag would be pruned (dry-run)", "tag", tag)
				pruned = append(pruned, tag)
				continue
			}
			if err := repo.DeleteTag(tag); err != nil {
				log.Error("failed to delete obsolete tag", "tag", tag, "error", err)
				continue
			}
			log.Info("tag pruned", "tag", tag)
			pruned = append(pruned, tag)
		}
	}
	return obsolete, pruned, nil
}
