package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
}

// NewScheduler creates a new Scheduler.
func NewScheduler(s *gsync.Syncer, logger *slog.Logger, listenAddr string) *Scheduler {
	return &Scheduler{syncer: s, logger: logger, listenAddr: listenAddr}
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
				s.syncer.SyncRepo(ctx, &repo, gsync.SyncOptions{})
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
	srv := newServer(s.syncer, s.logger, cfg)
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
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("http server shutdown error", "error", err)
	}

	if err := scheduler.Shutdown(); err != nil {
		s.logger.Error("scheduler shutdown error", "error", err)
	}

	s.logger.Info("daemon stopped")
}
