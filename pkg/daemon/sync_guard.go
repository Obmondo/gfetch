package daemon

import "sync"

type repoSyncGuard struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func newRepoSyncGuard() *repoSyncGuard {
	return &repoSyncGuard{active: make(map[string]struct{})}
}

func (g *repoSyncGuard) TryStart(repoName string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.active[repoName]; exists {
		return false
	}
	g.active[repoName] = struct{}{}
	return true
}

func (g *repoSyncGuard) Finish(repoName string) {
	g.mu.Lock()
	delete(g.active, repoName)
	g.mu.Unlock()
}

// IsActive reports whether a sync is currently running for repoName. The guard
// is keyed by name and outlives config membership, so this stays true for a
// repo dropped by a reload until its in-flight sync drains.
func (g *repoSyncGuard) IsActive(repoName string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, exists := g.active[repoName]
	return exists
}
