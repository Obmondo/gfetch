package sync

import (
	"context"
	"fmt"
	"log/slog"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/ashish1099/gitsync/pkg/config"
)

// syncTags lists remote tags, filters by patterns, and fetches new matching tags.
// It returns newly fetched, already up-to-date, obsolete (local tags not matching
// any pattern), and pruned tag lists.
func syncTags(ctx context.Context, repo *git.Repository, repoConfig *config.RepoConfig, auth transport.AuthMethod, pruneTags bool, dryRun bool, log *slog.Logger) (fetched, upToDate, obsolete, pruned []string, err error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("getting remote: %w", err)
	}

	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("listing remote refs: %w", err)
	}

	// Collect remote tags that match our patterns.
	matchedRemote := make(map[string]bool)
	for _, ref := range refs {
		name := ref.Name()
		if !name.IsTag() {
			continue
		}
		tagName := name.Short()

		if !matchesAnyPattern(tagName, repoConfig.Tags) {
			continue
		}

		matchedRemote[tagName] = true

		// Check if we already have this tag locally.
		if _, err := repo.Reference(plumbing.NewTagReferenceName(tagName), true); err == nil {
			upToDate = append(upToDate, tagName)
			continue
		}

		fetched = append(fetched, tagName)
	}

	// Fetch new tags.
	if len(fetched) > 0 {
		refSpecs := make([]gitconfig.RefSpec, len(fetched))
		for i, tag := range fetched {
			refSpecs[i] = gitconfig.RefSpec(fmt.Sprintf("+refs/tags/%s:refs/tags/%s", tag, tag))
		}

		err = repo.FetchContext(ctx, &git.FetchOptions{
			RemoteName: "origin",
			RefSpecs:   refSpecs,
			Auth:       auth,
			Tags:       git.NoTags,
		})
		if err != nil && err != git.NoErrAlreadyUpToDate {
			return nil, nil, nil, nil, fmt.Errorf("fetching tags: %w", err)
		}

		for _, tag := range fetched {
			log.Info("tag fetched", "tag", tag)
		}
	} else {
		log.Debug("no new tags to fetch")
	}

	// Find obsolete local tags: tags present locally that don't match any configured pattern.
	tagRefs, err := repo.Tags()
	if err != nil {
		return fetched, upToDate, nil, nil, fmt.Errorf("listing local tags: %w", err)
	}
	err = tagRefs.ForEach(func(ref *plumbing.Reference) error {
		tagName := ref.Name().Short()
		if !matchesAnyPattern(tagName, repoConfig.Tags) {
			obsolete = append(obsolete, tagName)
		}
		return nil
	})
	if err != nil {
		return fetched, upToDate, nil, nil, fmt.Errorf("iterating local tags: %w", err)
	}

	// Prune obsolete tags if requested.
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

	return fetched, upToDate, obsolete, pruned, nil
}
