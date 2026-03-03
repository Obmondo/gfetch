package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/gsync"
)

const defaultShutdownTimeout = 10 * time.Second

// Scheduler manages periodic syncing of repositories using gocron.
type Scheduler struct {
	syncer     *gsync.Syncer
	logger     *slog.Logger
	listenAddr string
	state      *syncRuntimeState
}

type syncRuntimeState struct {
	guard        *repoSyncGuard
	syncWG       sync.WaitGroup
	shuttingDown atomic.Bool
}

// NewScheduler creates a new Scheduler.
func NewScheduler(s *gsync.Syncer, logger *slog.Logger, listenAddr string) *Scheduler {
	return &Scheduler{
		syncer:     s,
		logger:     logger,
		listenAddr: listenAddr,
		state:      &syncRuntimeState{guard: newRepoSyncGuard()},
	}
}

// Run starts the gocron scheduler and HTTP server, blocking until SIGINT/SIGTERM.
func (s *Scheduler) Run(ctx context.Context, cfg *config.Config) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	scheduler, err := gocron.NewScheduler()
	if err != nil {
		s.logger.Error("failed to create scheduler", "error", err)
		return
	}

	for name := range cfg.Repos {
		repo := cfg.Repos[name]
		interval := time.Duration(repo.PollInterval)

		_, err := scheduler.NewJob(
			gocron.DurationJob(interval),
			gocron.NewTask(func() {
				runGuardedSync(ctx, s.syncer, s.state, &repo, s.logger, "scheduler")
			}),
			gocron.WithSingletonMode(gocron.LimitModeReschedule),
			gocron.WithStartAt(gocron.WithStartImmediately()),
		)
		if err != nil {
			s.logger.Error("failed to schedule job", "repo", repo.Name, "error", err)
			return
		}
		s.logger.Info("scheduled repo sync", "repo", repo.Name, "interval", interval)
	}

	scheduler.Start()

	// Start HTTP server.
	srv := newServer(s.syncer, s.logger, cfg, s.state)
	httpServer := &http.Server{
		Addr:    s.listenAddr,
		Handler: srv,
	}

	go func() {
		s.logger.Info("http server starting", "addr", s.listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("http server error", "error", err)
		}
	}()

	s.logger.Info("daemon started", "repos", len(cfg.Repos))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	s.logger.Info("received signal, shutting down", "signal", sig)
	s.state.shuttingDown.Store(true)

	if err := scheduler.Shutdown(); err != nil {
		s.logger.Error("scheduler shutdown error", "error", err)
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer shutdownCancel()

	waitCh := make(chan struct{})
	go func() {
		s.state.syncWG.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		s.logger.Info("all in-flight syncs stopped")
	case <-shutdownCtx.Done():
		s.logger.Warn("timed out waiting for in-flight syncs to stop")
	}

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("http server shutdown error", "error", err)
	}

	s.logger.Info("daemon stopped")
}
