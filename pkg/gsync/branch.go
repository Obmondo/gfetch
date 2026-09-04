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
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/obmondo/gfetch/pkg/telemetry"
)

func isContextCancellationError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// syncBranch fetches a single branch and hard-resets the local branch to match remote.
// Returns true if the branch was updated, false if already up-to-date.
func syncBranch(ctx context.Context, repo *git.Repository, branch, _ string, auth transport.AuthMethod, repoName string) (bool, error) {
	start := time.Now()
	remoteName := RemoteOrigin
	refSpec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remoteName, branch)

	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: remoteName,
		RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec(refSpec)},
		Auth:       auth,
		Tags:       git.NoTags,
		Force:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return false, fmt.Errorf("fetching branch %s: %w", branch, err)
	}

	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName(remoteName, branch), true)
	if err != nil {
		return false, fmt.Errorf("resolving remote ref for %s: %w", branch, err)
	}

	localRefName := plumbing.NewBranchReferenceName(branch)
	localRef, err := repo.Reference(localRefName, true)

	if err == nil && localRef.Hash() == remoteRef.Hash() {
		slog.Debug("branch already up-to-date", "branch", branch)
		return false, nil
	}

	// Update or create the local branch reference to point to the remote hash.
	newRef := plumbing.NewHashReference(localRefName, remoteRef.Hash())
	if err := repo.Storer.SetReference(newRef); err != nil {
		return false, fmt.Errorf("setting local ref for %s: %w", branch, err)
	}

	duration := time.Since(start)
	telemetry.SyncDurationSeconds.WithLabelValues(repoName, "branch").Observe(duration.Seconds())
	slog.Info("branch synced", "branch", branch, "hash", remoteRef.Hash().String()[:12], "duration", duration)
	return true, nil
}

// checkoutRef checks out the named branch or tag and hard-resets the working
// tree, using a background context.
func checkoutRef(repo *git.Repository, name string) error {
	return checkoutRefContext(context.Background(), repo, name)
}

// checkoutRefContext checks out the named branch or tag and hard-resets the
// working tree.
//
// A corrupt .git/index is repaired rather than reported. go-git reads the index
// before touching the worktree, so a truncated one - a container killed
// mid-write, which the gfetch pod is prone to since it has no memory limit -
// makes every later checkout fail with "malformed index signature file" and the
// repo never recovers. The index is pure cache, fully rebuildable from HEAD, so
// deleting it and retrying is safe and far cheaper than re-cloning.
//
// The repair lives here rather than in a wrapper so every caller gets it. The
// openvox path calls this directly, and with N worktrees and concurrent workers
// per repo it is the mode most exposed to a container killed mid-index-write.
func checkoutRefContext(ctx context.Context, repo *git.Repository, name string) error {
	err := checkoutRefOnce(ctx, repo, name)
	if err == nil || !isMalformedIndexErr(err) {
		return err
	}

	idxPath, pathErr := gitIndexPath(repo)
	if pathErr != nil {
		return err
	}
	slog.Warn("removing corrupt git index and retrying checkout", "ref", name, "index", idxPath, "error", err)
	if rmErr := os.Remove(idxPath); rmErr != nil && !os.IsNotExist(rmErr) {
		return fmt.Errorf("removing corrupt index after %w: %w", err, rmErr)
	}
	return checkoutRefOnce(ctx, repo, name)
}

// isMalformedIndexErr reports whether the error is go-git refusing to decode
// .git/index (plumbing/format/index.ErrMalformedSignature).
func isMalformedIndexErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, index.ErrMalformedSignature) ||
		strings.Contains(err.Error(), index.ErrMalformedSignature.Error())
}

// gitIndexPath locates .git/index for a repo backed by the filesystem.
func gitIndexPath(repo *git.Repository) (string, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	root := wt.Filesystem.Root()
	if root == "" {
		return "", fmt.Errorf("worktree has no filesystem root")
	}
	return filepath.Join(root, ".git", "index"), nil
}

func checkoutRefOnce(ctx context.Context, repo *git.Repository, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("checkout cancelled for %s: %w", name, err)
	}

	// Try branch first, then tag.
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), true)
	if err != nil {
		ref, err = repo.Reference(plumbing.NewTagReferenceName(name), true)
		if err != nil {
			return fmt.Errorf("ref %q not found as branch or tag: %w", name, err)
		}
	}

	hash := ref.Hash()
	// Annotated tags point to a tag object, not a commit directly. Peel to the commit.
	if tagObj, err := repo.TagObject(hash); err == nil {
		commit, err := tagObj.Commit()
		if err != nil {
			return fmt.Errorf("peeling tag %s to commit: %w", name, err)
		}
		hash = commit.Hash
	}

	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("checkout cancelled for %s: %w", name, err)
	}

	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: ref.Name(),
		Force:  true,
	}); err != nil {
		return fmt.Errorf("checkout %s: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reset cancelled for %s: %w", name, err)
	}

	if err := wt.Reset(&git.ResetOptions{
		Commit: hash,
		Mode:   git.HardReset,
	}); err != nil {
		return fmt.Errorf("reset %s: %w", name, err)
	}

	slog.Debug("checked out ref", "ref", name, "hash", hash.String()[:12])
	return nil
}

// isObjectNotFoundErr reports whether err is a missing-object failure. This
// happens when a ref resolves but its object graph is incomplete locally — e.g.
// an interrupted prior fetch on a persistent volume left the commit but not its
// tree/blobs. go-git then advertises the commit as a "have", so an ordinary
// fetch never repairs it and the checkout fails reading the missing tree.
func isObjectNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, plumbing.ErrObjectNotFound) ||
		strings.Contains(err.Error(), plumbing.ErrObjectNotFound.Error())
}

// shouldCheckoutBranch reports whether checkoutRef should run for a branch sync.
// We always checkout on updates. For non-updates, we checkout only if local state
// is out-of-sync or dirty (e.g. manual local changes).
func shouldCheckoutBranch(repo *git.Repository, branch string, updated bool) (needsCheckout bool, dirty bool, err error) {
	if updated {
		return true, false, nil
	}

	branchRef, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return true, false, fmt.Errorf("resolving branch ref %s: %w", branch, err)
	}

	headRef, err := repo.Head()
	if err != nil {
		return true, false, fmt.Errorf("resolving HEAD: %w", err)
	}

	if headRef.Hash() != branchRef.Hash() || headRef.Name() != branchRef.Name() {
		return true, false, nil
	}

	wt, err := repo.Worktree()
	if err != nil {
		return true, false, fmt.Errorf("getting worktree: %w", err)
	}

	status, err := wt.Status()
	if err != nil {
		return true, false, fmt.Errorf("getting worktree status: %w", err)
	}

	if !status.IsClean() {
		slog.Debug("branch state is not unmodified", slog.String("branch", branch), slog.String("git_status", status.String()))
		return true, true, nil
	}

	return false, false, nil
}
