package gsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/telemetry"
)

// metaDir is the hidden directory used to store the resolver repo for listing remote refs.
const (
	metaDir             = ".gfetch-meta"
	locksDirName        = "locks"
	productionAliasName = "production"
	defaultDirMode      = 0755
	defaultLockFileMode = os.FileMode(0o600)
	maxWorkers          = 5
	lockPollDelay       = 100 * time.Millisecond
	lockAcquireTimeout  = 30 * time.Second
)

var (
	resolverLocks = newKeyedLockManager()
	openVoxLocks  = newKeyedLockManager()
)

type keyedLockManager struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func newKeyedLockManager() *keyedLockManager {
	return &keyedLockManager{entries: make(map[string]*lockEntry)}
}

func (m *keyedLockManager) Acquire(key string) func() {
	m.mu.Lock()
	entry, ok := m.entries[key]
	if !ok {
		entry = &lockEntry{}
		m.entries[key] = entry
	}
	entry.refs++
	m.mu.Unlock()

	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()

		m.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(m.entries, key)
		}
		m.mu.Unlock()
	}
}

func (m *keyedLockManager) ForgetIfIdle(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok {
		return
	}
	if entry.refs == 0 {
		delete(m.entries, key)
	}
}

func acquireResolverLock(path string) func() {
	return resolverLocks.Acquire(path)
}

func acquireOpenVoxDirLock(path string) func() {
	return openVoxLocks.Acquire(path)
}

func forgetOpenVoxDirLock(path string) {
	openVoxLocks.ForgetIfIdle(path)
}

type fileLock struct {
	file *os.File
}

func openVoxLocksDir(basePath string) string {
	return filepath.Join(basePath, metaDir, locksDirName)
}

func openVoxLockPath(dirPath string) string {
	basePath := filepath.Dir(dirPath)
	dirName := filepath.Base(dirPath)
	return filepath.Join(openVoxLocksDir(basePath), dirName+".lock")
}

func withOpenVoxLockTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, lockAcquireTimeout)
}

func acquireOpenVoxFileLock(ctx context.Context, lockPath string) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), defaultDirMode); err != nil {
		return nil, fmt.Errorf("creating lock directory for %s: %w", lockPath, err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, defaultLockFileMode)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", lockPath, err)
	}

	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("acquiring file lock %s: %w", lockPath, err)
		}

		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for file lock %s: %w", lockPath, ctx.Err())
		case <-time.After(lockPollDelay):
		}
	}
}

func tryAcquireOpenVoxFileLock(lockPath string) (*fileLock, error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR, defaultLockFileMode)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening lock file %s: %w", lockPath, err)
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return &fileLock{file: f}, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		_ = f.Close()
		return nil, nil
	}

	_ = f.Close()
	return nil, fmt.Errorf("acquiring file lock %s: %w", lockPath, err)
}

func (l *fileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
		_ = l.file.Close()
		return fmt.Errorf("releasing file lock: %w", err)
	}
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("closing lock file: %w", err)
	}
	return nil
}

func isLockAcquireTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

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
	resolverRepo, refs, err := loadResolverRepoAndRefs(ctx, repo.Name, resolverPath, repo.URL, auth)
	if err != nil {
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "clone").Inc()
		result.Err = fmt.Errorf("resolver repo: %w", err)
		return result
	}

	defaultBranch, remoteBranches, matchedBranches, matchedTags := extractRemoteRefState(refs, repo.Branches, repo.Tags)

	// Track active names separately for stale-branch pruning, and combined for
	// collision detection and obsolete-dir pruning.
	activeBranchNames := make(map[string]string)
	activeTagNames := make(map[string]string)
	sanitizedToOriginal := make(map[string]string)

	if err := s.syncOpenVoxBranches(ctx, resolverRepo, resolverPath, repo, opts, auth, matchedBranches, activeBranchNames, sanitizedToOriginal, log, &result); err != nil {
		return result
	}

	ensureProductionAlias(ctx, repo, defaultBranch, remoteBranches, log)

	s.syncOpenVoxTags(ctx, repo, auth, matchedTags, activeTagNames, sanitizedToOriginal, log, &result)

	// Prune stale directories that no longer correspond to any matched ref.
	if opts.Prune {
		pruneOpenVoxDirs(ctx, repo.Name, repo.LocalPath, sanitizedToOriginal, opts.DryRun, log, &result)
		cleanupOrphanOpenVoxLockFiles(repo.Name, repo.LocalPath, opts.DryRun, log)
	}

	// Prune directories whose latest commit is older than staleAge.
	if opts.PruneStale {
		pruneStaleOpenVoxDirs(ctx, repo, activeBranchNames, opts.StaleAge, opts.DryRun, defaultBranch, log, &result)
		cleanupOrphanOpenVoxLockFiles(repo.Name, repo.LocalPath, opts.DryRun, log)
	}

	s.recordOpenVoxMetrics(repo, start, &result, log)
	return result
}

func loadResolverRepoAndRefs(ctx context.Context, repoName, resolverPath, remoteURL string, auth transport.AuthMethod) (*git.Repository, []*plumbing.Reference, error) {
	releaseResolverLock := acquireResolverLock(resolverPath)
	defer releaseResolverLock()

	resolverRepo, err := ensureResolverRepo(ctx, resolverPath, remoteURL, auth)
	if err != nil {
		return nil, nil, err
	}

	refs, err := listRemoteRefs(ctx, resolverRepo, auth, repoName, "openvox")
	if err != nil {
		return nil, nil, err
	}

	return resolverRepo, refs, nil
}

func listRemoteRefs(ctx context.Context, repo *git.Repository, auth transport.AuthMethod, repoName, mode string) ([]*plumbing.Reference, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return nil, fmt.Errorf("getting remote: %w", err)
	}

	telemetry.RemoteRefListTotal.WithLabelValues(repoName, mode).Inc()
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("listing remote refs: %w", err)
	}
	return refs, nil
}

func extractRemoteRefState(refs []*plumbing.Reference, branchPatterns, tagPatterns []config.Pattern) (string, map[string]struct{}, []*plumbing.Reference, []string) {
	branches := make(map[string]struct{})
	defaultBranch := ""
	matchedBranches := make([]*plumbing.Reference, 0)
	matchedTags := make([]string, 0)
	seenBranches := make(map[string]bool)
	seenTags := make(map[string]bool)

	for _, ref := range refs {
		name := ref.Name()
		if name == plumbing.HEAD {
			defaultBranch = ref.Target().Short()
			continue
		}
		if name.IsBranch() {
			branch := name.Short()
			branches[branch] = struct{}{}
			if !seenBranches[branch] && config.MatchesAny(branch, branchPatterns) {
				matchedBranches = append(matchedBranches, ref)
				seenBranches[branch] = true
			}
			continue
		}
		if name.IsTag() {
			tag := name.Short()
			if !seenTags[tag] && config.MatchesAny(tag, tagPatterns) {
				matchedTags = append(matchedTags, tag)
				seenTags[tag] = true
			}
		}
	}

	return defaultBranch, branches, matchedBranches, matchedTags
}

func ensureProductionAlias(ctx context.Context, repo *config.RepoConfig, defaultBranch string, remoteBranches map[string]struct{}, log *slog.Logger) {
	if repo.ProductionAlias == nil || !*repo.ProductionAlias {
		return
	}

	if _, hasProduction := remoteBranches[productionAliasName]; hasProduction {
		log.Debug("skipping production alias: upstream production branch exists")
		return
	}

	if defaultBranch == "" {
		log.Warn("skipping production alias: remote default branch not found")
		return
	}

	sourceDirName := SanitizeName(defaultBranch)
	aliasDirName := SanitizeName(productionAliasName)
	if sourceDirName == aliasDirName {
		return
	}

	sourcePath := filepath.Join(repo.LocalPath, sourceDirName)
	if _, err := os.Stat(sourcePath); err != nil {
		log.Warn("skipping production alias: source directory not available", "source_branch", defaultBranch, "source_dir", sourceDirName, "error", err)
		return
	}

	aliasPath := filepath.Join(repo.LocalPath, aliasDirName)
	releaseDirLock := acquireOpenVoxDirLock(aliasPath)
	defer releaseDirLock()

	lockCtx, cancel := withOpenVoxLockTimeout(ctx)
	defer cancel()

	processLock, err := acquireOpenVoxFileLock(lockCtx, openVoxLockPath(aliasPath))
	if err != nil {
		if isLockAcquireTimeout(err) {
			telemetry.OpenVoxLockAcquireTimeoutsTotal.WithLabelValues(repo.Name, "alias").Inc()
		}
		log.Warn("failed to acquire production alias lock", "error", err)
		return
	}
	defer func() {
		if relErr := processLock.Release(); relErr != nil {
			log.Warn("failed to release production alias lock", "error", relErr)
		}
	}()

	if err := ensureSymlink(aliasPath, sourceDirName); err != nil {
		log.Warn("failed to ensure production alias", "source_branch", defaultBranch, "source_dir", sourceDirName, "alias", aliasDirName, "error", err)
		return
	}

	log.Debug("production alias ensured", "alias", aliasDirName, "target", sourceDirName)
}

func ensureSymlink(linkPath, target string) error {
	info, err := os.Lstat(linkPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.Symlink(target, linkPath); err != nil {
				return fmt.Errorf("creating symlink %s -> %s: %w", linkPath, target, err)
			}
			return nil
		}
		return fmt.Errorf("lstat %s: %w", linkPath, err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("path %s exists and is not a symlink", linkPath)
	}

	currentTarget, err := os.Readlink(linkPath)
	if err != nil {
		return fmt.Errorf("reading symlink %s: %w", linkPath, err)
	}
	if currentTarget == target {
		return nil
	}

	if err := os.Remove(linkPath); err != nil {
		return fmt.Errorf("removing symlink %s: %w", linkPath, err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		return fmt.Errorf("creating symlink %s -> %s: %w", linkPath, target, err)
	}

	return nil
}

func cleanupOpenVoxArtifactsForDir(dirPath string) {
	forgetOpenVoxDirLock(dirPath)
}

func cleanupOrphanOpenVoxLockFiles(repoName, basePath string, dryRun bool, log *slog.Logger) {
	cleanupOrphanOpenVoxLockFilesInDir(repoName, basePath, basePath, ".gfetch.lock", dryRun, log)
	cleanupOrphanOpenVoxLockFilesInDir(repoName, basePath, openVoxLocksDir(basePath), ".lock", dryRun, log)
}

func cleanupOrphanOpenVoxLockFilesInDir(repoName, basePath, scanPath, suffix string, dryRun bool, log *slog.Logger) {
	entries, err := os.ReadDir(scanPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Warn("failed to scan local_path for orphan lock files", "path", scanPath, "error", err)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}

		refDirName := strings.TrimSuffix(name, suffix)
		if refDirName == "" {
			continue
		}

		refDirPath := filepath.Join(basePath, refDirName)
		if _, statErr := os.Stat(refDirPath); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			log.Warn("failed to inspect lock file target directory", "lock_file", name, "error", statErr)
			continue
		}

		lockPath := filepath.Join(scanPath, name)
		if dryRun {
			log.Info("orphan lock file would be removed (dry-run)", "lock_file", lockPath)
			continue
		}

		lock, lockErr := tryAcquireOpenVoxFileLock(lockPath)
		if lockErr != nil {
			log.Warn("failed to lock orphan lock file for cleanup", "lock_file", lockPath, "error", lockErr)
			continue
		}
		if lock == nil {
			telemetry.OpenVoxOrphanLockfilesSkippedInUseTotal.WithLabelValues(repoName).Inc()
			log.Debug("skipping orphan lock file cleanup: lock is in use", "lock_file", lockPath)
			continue
		}

		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			if relErr := lock.Release(); relErr != nil {
				log.Warn("failed to release orphan lock file lock", "lock_file", lockPath, "error", relErr)
			}
			log.Warn("failed to remove orphan lock file", "lock_file", lockPath, "error", err)
			continue
		}

		if relErr := lock.Release(); relErr != nil {
			log.Warn("failed to release orphan lock file lock", "lock_file", lockPath, "error", relErr)
		}

		telemetry.OpenVoxOrphanLockfilesRemovedTotal.WithLabelValues(repoName).Inc()
		forgetOpenVoxDirLock(refDirPath)
		log.Debug("removed orphan lock file", "lock_file", lockPath)
	}
}

func (s *Syncer) syncOpenVoxBranches(ctx context.Context, resolverRepo *git.Repository, resolverPath string, repo *config.RepoConfig, opts SyncOptions, auth transport.AuthMethod, branches []*plumbing.Reference, activeBranchNames map[string]string, sanitizedToOriginal map[string]string, log *slog.Logger, result *Result) error {
	if len(branches) == 0 {
		return nil
	}

	log.Debug("syncing branches", "count", len(branches))
	var branchNames []string
	for _, b := range branches {
		branchNames = append(branchNames, b.Name().Short())
	}

	if collision := detectCollisions(branchNames, sanitizedToOriginal); collision != "" {
		s.setErr(result, fmt.Errorf("name collision after sanitization: %s", collision))
		return result.Err
	}
	for _, branch := range branchNames {
		activeBranchNames[SanitizeName(branch)] = branch
	}

	toSync := filterOpenVoxBranchesForSync(ctx, resolverRepo, resolverPath, repo, opts, auth, branches, log)

	var wg sync.WaitGroup
	jobs := make(chan *plumbing.Reference, len(toSync))

	for range maxWorkers {
		wg.Go(func() {
			for ref := range jobs {
				branch := ref.Name().Short()
				s.syncOneOpenVoxBranch(ctx, repo, branch, auth, log, result)
			}
		})
	}

	for _, ref := range toSync {
		jobs <- ref
	}
	close(jobs)
	wg.Wait()

	return nil
}

func filterOpenVoxBranchesForSync(ctx context.Context, resolverRepo *git.Repository, resolverPath string, repo *config.RepoConfig, opts SyncOptions, auth transport.AuthMethod, branches []*plumbing.Reference, log *slog.Logger) []*plumbing.Reference {
	if !opts.PruneStale || !opts.Prune {
		return branches
	}

	var nonStale []*plumbing.Reference
	var unresolved []*plumbing.Reference
	for _, ref := range branches {
		doSync, unresolvedLocal := shouldSyncBranchLocalFirst(repo.LocalPath, ref, opts.StaleAge, log)
		if unresolvedLocal {
			unresolved = append(unresolved, ref)
			continue
		}
		if doSync {
			nonStale = append(nonStale, ref)
			continue
		}

		telemetry.OpenVoxStaleBranchesSkippedTotal.WithLabelValues(repo.Name).Inc()
	}

	nonStale = resolveUnresolvedBranchStaleness(ctx, resolverRepo, resolverPath, repo, opts, auth, unresolved, nonStale, log)
	log.Debug("staleness filter applied", "total", len(branches), "non_stale", len(nonStale), "unresolved", len(unresolved))
	return nonStale
}

func resolveUnresolvedBranchStaleness(ctx context.Context, resolverRepo *git.Repository, resolverPath string, repo *config.RepoConfig, opts SyncOptions, auth transport.AuthMethod, unresolved, nonStale []*plumbing.Reference, log *slog.Logger) []*plumbing.Reference {
	if len(unresolved) == 0 {
		return nonStale
	}

	releaseResolverLock := acquireResolverLock(resolverPath)
	cleanup, err := batchFetchForStaleness(ctx, resolverRepo, unresolved, auth)
	if err != nil {
		releaseResolverLock()
		log.Warn("batch staleness fetch failed for unresolved branches, syncing unresolved set", "count", len(unresolved), "error", err)
		return append(nonStale, unresolved...)
	}
	defer func() {
		cleanup()
		releaseResolverLock()
	}()

	for _, ref := range unresolved {
		if isStaleLocal(resolverRepo, ref, opts.StaleAge, log) {
			telemetry.OpenVoxStaleBranchesSkippedTotal.WithLabelValues(repo.Name).Inc()
			continue
		}
		nonStale = append(nonStale, ref)
	}

	return nonStale
}

// shouldSyncBranchLocalFirst returns whether a branch should be synced based on
// local state first. If unresolved is true, caller should fallback to resolver
// based stale checks.
func shouldSyncBranchLocalFirst(basePath string, ref *plumbing.Reference, staleAge time.Duration, log *slog.Logger) (shouldSync bool, unresolved bool) {
	if staleAge <= 0 {
		return true, false
	}

	branch := ref.Name().Short()
	dirPath := filepath.Join(basePath, SanitizeName(branch))

	r, err := git.PlainOpen(dirPath)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return true, true
		}
		log.Debug("local-first stale check unresolved: cannot open branch repo", "branch", branch, "dir", dirPath, "error", err)
		return true, true
	}

	localRef, err := r.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		log.Debug("local-first stale check unresolved: missing local branch ref", "branch", branch, "error", err)
		return true, true
	}

	remoteHash := ref.Hash()
	localHash := localRef.Hash()
	if localHash != remoteHash {
		log.Debug("local-first stale check requires sync: local and remote hashes differ", "branch", branch, "local_hash", localHash, "remote_hash", remoteHash)
		return true, false
	}

	commit, err := r.CommitObject(localHash)
	if err != nil {
		log.Debug("local-first stale check unresolved: commit lookup failed", "branch", branch, "hash", localHash, "error", err)
		return true, true
	}

	if time.Since(commit.Committer.When) > staleAge {
		log.Debug(
			"local-first stale check: skipping stale branch",
			"branch", branch,
			"max_age", staleAge,
			"commit_age", time.Since(commit.Committer.When),
			"commit_time", commit.Committer.When,
		)
		return false, false
	}

	return true, false
}

func (s *Syncer) syncOneOpenVoxBranch(ctx context.Context, repo *config.RepoConfig, branch string, auth transport.AuthMethod, log *slog.Logger, result *Result) {
	dirName := SanitizeName(branch)
	dirPath := filepath.Join(repo.LocalPath, dirName)
	releaseDirLock := acquireOpenVoxDirLock(dirPath)
	defer releaseDirLock()

	lockCtx, cancel := withOpenVoxLockTimeout(ctx)
	defer cancel()

	processLock, err := acquireOpenVoxFileLock(lockCtx, openVoxLockPath(dirPath))
	if err != nil {
		if isLockAcquireTimeout(err) {
			telemetry.OpenVoxLockAcquireTimeoutsTotal.WithLabelValues(repo.Name, "branch").Inc()
		}
		log.Error("openvox branch lock failed", "branch", branch, "dir", dirName, "error", err)
		s.addBranchFailed(result, branch)
		return
	}
	defer func() {
		if relErr := processLock.Release(); relErr != nil {
			log.Warn("failed to release openvox branch lock", "branch", branch, "dir", dirName, "error", relErr)
		}
	}()

	// Build a sub-config pointing at the per-branch directory.
	subCfg := *repo
	subCfg.LocalPath = dirPath

	r, err := ensureClonedOpenVox(ctx, &subCfg, auth, log)
	if err != nil {
		log.Error("openvox branch clone failed", "branch", branch, "dir", dirName, "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "branch_sync").Inc()
		s.addBranchFailed(result, branch)
		return
	}

	updated, err := syncBranch(ctx, r, branch, repo.URL, auth, repo.Name, log)
	if err != nil {
		log.Error("openvox branch sync failed", "branch", branch, "dir", dirName, "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "branch_sync").Inc()
		s.addBranchFailed(result, branch)
		return
	}

	needsCheckout, dirtyBranch, stateErr := shouldCheckoutBranch(r, branch, updated)
	if stateErr != nil {
		log.Warn("openvox branch state check failed; forcing checkout", "branch", branch, "dir", dirName, "error", stateErr)
		needsCheckout = true
	}

	if dirtyBranch {
		log.Warn("dirty branch detected, likely manual local changes", "branch", branch, "dir", dirName)
	}

	if needsCheckout {
		if err := checkoutRef(r, branch, log); err != nil {
			log.Error("openvox branch checkout failed", "branch", branch, "dir", dirName, "error", err)
			s.addBranchFailed(result, branch)
			return
		}
	}

	if updated {
		s.addBranchSynced(result, branch)
	} else {
		s.addBranchUpToDate(result, branch)
	}
}

func (s *Syncer) syncOpenVoxTags(ctx context.Context, repo *config.RepoConfig, auth transport.AuthMethod, tags []string, activeTagNames map[string]string, sanitizedToOriginal map[string]string, log *slog.Logger, result *Result) {
	if len(tags) == 0 {
		return
	}

	log.Debug("syncing tags", "count", len(tags))
	if collision := detectCollisions(tags, sanitizedToOriginal); collision != "" {
		s.setErr(result, fmt.Errorf("name collision after sanitization: %s", collision))
		return
	}
	for _, tag := range tags {
		activeTagNames[SanitizeName(tag)] = tag
	}

	var wg sync.WaitGroup
	jobs := make(chan string, len(tags))

	for range maxWorkers {
		wg.Go(func() {
			for tag := range jobs {
				s.syncOneOpenVoxTag(ctx, repo, tag, auth, log, result)
			}
		})
	}

	for _, tag := range tags {
		jobs <- tag
	}
	close(jobs)
	wg.Wait()
}

func (s *Syncer) syncOneOpenVoxTag(ctx context.Context, repo *config.RepoConfig, tag string, auth transport.AuthMethod, log *slog.Logger, result *Result) {
	dirName := SanitizeName(tag)
	dirPath := filepath.Join(repo.LocalPath, dirName)
	releaseDirLock := acquireOpenVoxDirLock(dirPath)
	defer releaseDirLock()

	lockCtx, cancel := withOpenVoxLockTimeout(ctx)
	defer cancel()

	processLock, err := acquireOpenVoxFileLock(lockCtx, openVoxLockPath(dirPath))
	if err != nil {
		if isLockAcquireTimeout(err) {
			telemetry.OpenVoxLockAcquireTimeoutsTotal.WithLabelValues(repo.Name, "tag").Inc()
		}
		log.Error("openvox tag lock failed", "tag", tag, "dir", dirName, "error", err)
		s.addTagFailed(result, tag)
		return
	}
	defer func() {
		if relErr := processLock.Release(); relErr != nil {
			log.Warn("failed to release openvox tag lock", "tag", tag, "dir", dirName, "error", relErr)
		}
	}()

	// Build a sub-config pointing at the per-tag directory.
	subCfg := *repo
	subCfg.LocalPath = dirPath

	r, err := ensureClonedOpenVox(ctx, &subCfg, auth, log)
	if err != nil {
		log.Error("openvox tag clone failed", "tag", tag, "dir", dirName, "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "tag_sync").Inc()
		s.setErr(result, fmt.Errorf("tag sync %s: %w", tag, err))
		return
	}

	// Single-tag fetch and checkout.
	updated, err := syncOpenVoxTag(ctx, r, tag, auth, log)
	if err != nil {
		log.Error("openvox tag sync failed", "tag", tag, "dir", dirName, "error", err)
		telemetry.SyncFailuresTotal.WithLabelValues(repo.Name, "tag_sync").Inc()
		s.setErr(result, fmt.Errorf("tag sync %s: %w", tag, err))
		s.addTagFailed(result, tag)
		return
	}

	if updated {
		s.addTagFetched(result, tag)
	} else {
		s.addTagUpToDate(result, tag)
	}
}

// ensureClonedOpenVox opens/initializes a per-ref OpenVox repo and self-heals
// directories that exist but are not valid git repositories.
func ensureClonedOpenVox(ctx context.Context, repo *config.RepoConfig, auth transport.AuthMethod, log *slog.Logger) (*git.Repository, error) {
	r, err := ensureCloned(ctx, repo, auth)
	if err == nil {
		return r, nil
	}

	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, err
	}

	log.Warn("openvox directory is not a git repo, recreating", "path", repo.LocalPath)
	if rmErr := os.RemoveAll(repo.LocalPath); rmErr != nil {
		return nil, fmt.Errorf("removing non-repo path %s: %w", repo.LocalPath, rmErr)
	}

	r, err = ensureCloned(ctx, repo, auth)
	if err != nil {
		return nil, fmt.Errorf("recreating repo at %s: %w", repo.LocalPath, err)
	}

	return r, nil
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
func pruneStaleOpenVoxDirs(ctx context.Context, repo *config.RepoConfig, activeNames map[string]string, staleAge time.Duration, dryRun bool, defaultBranch string, log *slog.Logger, result *Result) {
	if staleAge == 0 {
		return
	}
	cutoff := time.Now().Add(-staleAge)
	for sanitized, original := range activeNames {
		if original == defaultBranch {
			log.Debug("skipping stale prune of default branch", "branch", original)
			continue
		}
		dirPath := filepath.Join(repo.LocalPath, sanitized)
		isStale := isOpenVoxDirStale(dirPath, sanitized, cutoff, log)
		if !isStale {
			continue
		}

		if dryRun {
			log.Info("stale directory would be pruned (dry-run)", "dir", sanitized, "branch", original)
			result.BranchesStale = append(result.BranchesStale, original)
			result.BranchesPruned = append(result.BranchesPruned, original)
			continue
		}

		if err := pruneOpenVoxDirWithLocks(ctx, repo.Name, dirPath, "prune_stale"); err != nil {
			log.Warn("skipping stale prune: failed to prune with locks", "dir", sanitized, "error", err)
			continue
		}
		cleanupOpenVoxArtifactsForDir(dirPath)
		log.Info("stale directory pruned", "dir", sanitized, "branch", original)
		result.BranchesStale = append(result.BranchesStale, original)
		result.BranchesPruned = append(result.BranchesPruned, original)
	}
}

func isOpenVoxDirStale(dirPath, dirName string, cutoff time.Time, log *slog.Logger) bool {
	r, err := git.PlainOpen(dirPath)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			if _, statErr := os.Stat(dirPath); os.IsNotExist(statErr) {
				return false
			}
		}
		log.Warn("skipping stale check: directory exists but is not a git repo", "dir", dirName, "error", err)
		return false
	}

	head, err := r.Head()
	if err != nil {
		return false
	}
	commit, err := r.CommitObject(head.Hash())
	if err != nil {
		return false
	}

	return commit.Committer.When.Before(cutoff)
}

func pruneOpenVoxDirWithLocks(ctx context.Context, repoName, dirPath, kind string) error {
	releaseDirLock := acquireOpenVoxDirLock(dirPath)
	defer releaseDirLock()

	lockCtx, cancel := withOpenVoxLockTimeout(ctx)
	defer cancel()

	processLock, err := acquireOpenVoxFileLock(lockCtx, openVoxLockPath(dirPath))
	if err != nil {
		if isLockAcquireTimeout(err) {
			telemetry.OpenVoxLockAcquireTimeoutsTotal.WithLabelValues(repoName, kind).Inc()
		}
		return fmt.Errorf("acquiring prune lock: %w", err)
	}
	defer func() {
		_ = processLock.Release()
	}()

	if err := os.RemoveAll(dirPath); err != nil {
		return fmt.Errorf("removing directory: %w", err)
	}

	return nil
}
