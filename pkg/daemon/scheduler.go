package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	gosync "sync"
	"syscall"
	"time"

	"github.com/ashish1099/gitsync/pkg/config"
	"github.com/ashish1099/gitsync/pkg/sync"
)

// Scheduler manages periodic syncing of repositories.
type Scheduler struct {
	syncer *sync.Syncer
	logger *slog.Logger
}

// NewScheduler creates a new Scheduler.
func NewScheduler(syncer *sync.Syncer, logger *slog.Logger) *Scheduler {
	return &Scheduler{syncer: syncer, logger: logger}
}

// Run starts a goroutine per repo with independent poll intervals.
// It blocks until SIGINT or SIGTERM is received, then cancels all goroutines and waits.
func (s *Scheduler) Run(ctx context.Context, cfg *config.Config) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var wg gosync.WaitGroup

	for i := range cfg.Repos {
		repo := &cfg.Repos[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.pollRepo(ctx, repo)
		}()
	}

	s.logger.Info("daemon started", "repos", len(cfg.Repos))

	sig := <-sigCh
	s.logger.Info("received signal, shutting down", "signal", sig)
	cancel()

	wg.Wait()
	s.logger.Info("daemon stopped")
}

func (s *Scheduler) pollRepo(ctx context.Context, repo *config.RepoConfig) {
	log := s.logger.With("repo", repo.Name)

	// Immediate first sync.
	log.Info("initial sync")
	result := s.syncer.SyncRepo(ctx, repo, sync.SyncOptions{})
	if result.Err != nil {
		log.Error("sync failed", "error", result.Err)
	}

	ticker := time.NewTicker(repo.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("stopping poll")
			return
		case <-ticker.C:
			log.Debug("polling")
			result := s.syncer.SyncRepo(ctx, repo, sync.SyncOptions{})
			if result.Err != nil {
				log.Error("sync failed", "error", result.Err)
			}
		}
	}
}
