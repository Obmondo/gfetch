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
