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
	listenAddr string
	state      *SyncRuntimeState
}

type SyncRuntimeState struct {
	guard        *repoSyncGuard
	syncWG       sync.WaitGroup
	shuttingDown atomic.Bool
}

// NewScheduler creates a new Scheduler.
func NewScheduler(s *gsync.Syncer, listenAddr string) *Scheduler {
	return &Scheduler{
		syncer:     s,
		listenAddr: listenAddr,
		state:      &SyncRuntimeState{guard: newRepoSyncGuard()},
	}
}

// Run starts the gocron scheduler and HTTP server, blocking until SIGINT/SIGTERM.
func (s *Scheduler) Run(ctx context.Context, cfg *config.Config) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	scheduler, err := gocron.NewScheduler()
	if err != nil {
		slog.Default().Error("failed to create scheduler", "error", err)
		return
	}

	for name := range cfg.Repos {
		repo := cfg.Repos[name]
		interval := time.Duration(repo.PollInterval)

		_, err := scheduler.NewJob(
			gocron.DurationJob(interval),
			gocron.NewTask(func() {
				RunGuardedSync(ctx, s.syncer, s.state, &repo, "scheduler")
			}),
			gocron.WithSingletonMode(gocron.LimitModeReschedule),
			gocron.WithStartAt(gocron.WithStartImmediately()),
		)
		if err != nil {
			slog.Default().Error("failed to schedule job", "repo", repo.Name, "error", err)
			return
		}
		slog.Default().Info("scheduled repo sync", "repo", repo.Name, "interval", interval)
	}

	scheduler.Start()

	// Start HTTP server.
	srv := newServer(s.syncer, cfg, s.state)
	httpServer := &http.Server{
		Addr:    s.listenAddr,
		Handler: srv,
	}

	go func() {
		slog.Default().Info("http server starting", "addr", s.listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Default().Error("http server error", "error", err)
		}
	}()

	slog.Default().Info("daemon started", "repos", len(cfg.Repos))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	slog.Default().Info("received signal, shutting down", "signal", sig)
	s.state.shuttingDown.Store(true)

	if err := scheduler.Shutdown(); err != nil {
		slog.Default().Error("scheduler shutdown error", "error", err)
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
		slog.Default().Info("all in-flight syncs stopped")
	case <-shutdownCtx.Done():
		slog.Default().Warn("timed out waiting for in-flight syncs to stop")
	}

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Default().Error("http server shutdown error", "error", err)
	}

	slog.Default().Info("daemon stopped")
}
