package gsync

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// PruneItems is a generic helper for pruning items (branches, tags, directories).
// It iterates over a list of obsolete items, checks for dry-run, logs actions, and executes the deletion logic.
func PruneItems[T any](
	items []T,
	dryRun bool,
	log *slog.Logger,
	dryRunMsg string,
	prunedMsg string,
	errorMsg string,
	getName func(T) string,
	deleteFunc func(T) error,
) []string {
	var pruned []string
	for _, item := range items {
		name := getName(item)
		if dryRun {
			log.Info(dryRunMsg, "item", name)
			pruned = append(pruned, name)
			continue
		}
		if err := deleteFunc(item); err != nil {
			log.Error(errorMsg, "item", name, "error", err)
			continue
		}
		log.Info(prunedMsg, "item", name)
		pruned = append(pruned, name)
	}
	return pruned
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

// pruneOpenVoxDirs removes directories under basePath that don't correspond to any active ref.
func pruneOpenVoxDirs(basePath string, activeNames map[string]string, dryRun bool, log *slog.Logger, result *Result) {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		log.Error("failed to read local_path for pruning", "path", basePath, "error", err)
		return
	}

	var obsoleteDirs []string
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
		obsoleteDirs = append(obsoleteDirs, name)
	}

	pruned := PruneItems(
		obsoleteDirs,
		dryRun,
		log,
		"directory would be pruned (dry-run)",
		"directory pruned",
		"failed to prune directory",
		func(name string) string { return name },
		func(name string) error {
			return os.RemoveAll(filepath.Join(basePath, name))
		},
	)
	result.BranchesPruned = append(result.BranchesPruned, pruned...)
}
