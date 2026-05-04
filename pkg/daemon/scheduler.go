package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/gsync"
	"github.com/obmondo/gfetch/pkg/telemetry"
)

const defaultShutdownTimeout = 10 * time.Second

// Scheduler manages periodic syncing of repositories using gocron.
// It supports live reload: scheduled jobs and HTTP handlers read the current
// config from an atomic pointer, so a Reload() call swaps the visible config
// in a single atomic write before adjusting gocron jobs.
type Scheduler struct {
	syncer     *gsync.Syncer
	listenAddr string
	configPath string
	state      *SyncRuntimeState

	cfg     atomic.Pointer[config.Config] // current live config; readers use Load()
	cron    gocron.Scheduler              // promoted to field so Reload() can mutate
	jobs    map[string]uuid.UUID          // repoName -> gocron job id
	applyMu sync.Mutex                    // serializes Reload()
	runCtx  context.Context               // lifecycle ctx for scheduled tasks; set in Run
}

type SyncRuntimeState struct {
	guard        *repoSyncGuard
	syncWG       sync.WaitGroup
	shuttingDown atomic.Bool
}

// ReloadResult describes the outcome of a Scheduler.Reload call. It carries
// the sorted list of repos managed after the reload so the caller can confirm
// what got loaded.
type ReloadResult struct {
	Repos []string `json:"repos"`
}

// NewScheduler creates a new Scheduler. configPath is the file or directory
// from which the daemon re-reads its config on SIGHUP or POST /reload.
func NewScheduler(s *gsync.Syncer, listenAddr, configPath string) *Scheduler {
	return &Scheduler{
		syncer:     s,
		listenAddr: listenAddr,
		configPath: configPath,
		state:      &SyncRuntimeState{guard: newRepoSyncGuard()},
		jobs:       make(map[string]uuid.UUID),
	}
}

// Config returns the current live config. Safe for concurrent use.
func (s *Scheduler) Config() *config.Config {
	return s.cfg.Load()
}

// ConfigPath returns the file or directory the daemon was started with.
func (s *Scheduler) ConfigPath() string {
	return s.configPath
}

// Run starts the gocron scheduler and HTTP server, blocking until SIGINT/SIGTERM.
func (s *Scheduler) Run(ctx context.Context, cfg *config.Config) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	scheduler, err := gocron.NewScheduler()
	if err != nil {
		slog.Error("failed to create scheduler", "error", err)
		return
	}
	s.cron = scheduler
	s.runCtx = ctx

	if err := s.applyInitial(cfg); err != nil {
		slog.Error("failed to apply initial config", "error", err)
		return
	}

	scheduler.Start()

	// Start HTTP server.
	srv := newServer(s)
	httpServer := &http.Server{
		Addr:    s.listenAddr,
		Handler: srv,
	}

	go func() {
		slog.Info("http server starting", "addr", s.listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
		}
	}()

	slog.Info("daemon started", "repos", len(cfg.Repos))

	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	go s.runReloadOnSignal(ctx, hupCh)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig)
	case <-ctx.Done():
		slog.Info("context cancelled, shutting down")
	}
	s.state.shuttingDown.Store(true)

	if err := scheduler.Shutdown(); err != nil {
		slog.Error("scheduler shutdown error", "error", err)
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
		slog.Info("all in-flight syncs stopped")
	case <-shutdownCtx.Done():
		slog.Warn("timed out waiting for in-flight syncs to stop")
	}

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}

	slog.Info("daemon stopped")
}

// runReloadOnSignal runs the load+validate+reload pipeline whenever SIGHUP
// fires, following Prometheus's model (`SIGHUP` and `POST /reload` are the
// two explicit triggers; there is no filesystem watcher). Failures keep the
// previous config running — they're logged + counted by loadValidateAndReload
// itself.
func (s *Scheduler) runReloadOnSignal(ctx context.Context, hupCh <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hupCh:
			slog.Info("received SIGHUP, reloading config")
			if _, _, err := loadValidateAndReload(s); err != nil {
				slog.Warn("SIGHUP reload failed", "error", err)
			}
		}
	}
}

// applyInitial schedules every repo from cfg and stores cfg in the atomic
// pointer. Called once at startup. Jobs fire immediately on creation so the
// daemon syncs every repo as soon as it starts; concurrent reloads happen
// via Reload(), which uses delayed first-fires to avoid stampeding upstream.
func (s *Scheduler) applyInitial(cfg *config.Config) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	s.cfg.Store(cfg)

	for name, repo := range cfg.Repos {
		jobID, err := s.scheduleJob(name, time.Duration(repo.PollInterval), true)
		if err != nil {
			return fmt.Errorf("scheduling repo %q: %w", name, err)
		}
		s.jobs[name] = jobID
		slog.Info("scheduled repo sync", "repo", name, "interval", time.Duration(repo.PollInterval))
	}

	telemetry.ConfigManagedRepos.Set(float64(len(cfg.Repos)))
	return nil
}

// Reload cancels every running job, swaps in newCfg, and re-schedules every
// repo with a delayed first-fire (one full poll_interval out). The delay is
// deliberate: SIGHUP / POST /reload can flip many repos' configs at once
// (e.g. a defaults edit cascading to every tenant), and firing them all
// immediately would stampede the upstream git host.
//
// In-flight syncs are NOT interrupted. Cancelling a gocron job does not kill
// the goroutine it already spawned; in-flight tasks finish against their
// captured *RepoConfig via the scheduler's lifecycle context. The per-repo
// guard (RunGuardedSync → repoSyncGuard.TryStart) prevents the post-reload
// fire from racing with an in-flight sync of the same repo.
func (s *Scheduler) Reload(newCfg *config.Config) (ReloadResult, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	if s.state.shuttingDown.Load() {
		return ReloadResult{}, fmt.Errorf("daemon is shutting down")
	}

	for name, jobID := range s.jobs {
		if err := s.cron.RemoveJob(jobID); err != nil {
			slog.Warn("failed to remove gocron job", "repo", name, "error", err)
		}
	}
	s.jobs = make(map[string]uuid.UUID, len(newCfg.Repos))

	// Swap the live config so any concurrent fire/handler sees the new view.
	// In-flight syncs already hold their *RepoConfig and finish against it.
	s.cfg.Store(newCfg)

	names := make([]string, 0, len(newCfg.Repos))
	for name, repo := range newCfg.Repos {
		jobID, err := s.scheduleJob(name, time.Duration(repo.PollInterval), false)
		if err != nil {
			slog.Error("failed to schedule repo", "repo", name, "error", err)
			continue
		}
		s.jobs[name] = jobID
		names = append(names, name)
		slog.Info("repo scheduled", "repo", name, "interval", time.Duration(repo.PollInterval))
	}
	sort.Strings(names)

	telemetry.ConfigReloadsTotal.Inc()
	telemetry.ConfigLastReloadTimestamp.Set(float64(time.Now().Unix()))
	telemetry.ConfigManagedRepos.Set(float64(len(newCfg.Repos)))

	return ReloadResult{Repos: names}, nil
}

// scheduleJob registers a gocron job whose task body looks up the current repo
// from the atomic config pointer on every fire. This is what makes config
// changes visible to scheduled fires without recreating the job. The task
// uses the Scheduler's lifecycle context (s.runCtx) — never the context of
// the call that triggered scheduling — so jobs scheduled by Reload survive
// after the triggering HTTP request ends.
//
// startNow controls whether the job fires immediately on creation. We set it
// for boot (applyInitial) and for newly-added repos — both cases where the
// operator expects a sync as soon as the daemon knows about a repo. We do
// NOT set it for interval-change reschedules: a single config edit can flip
// many repos' intervals at once (e.g. a default-poll_interval change cascades
// to every tenant) and we don't want that to stampede the upstream git host.
func (s *Scheduler) scheduleJob(name string, interval time.Duration, startNow bool) (uuid.UUID, error) {
	opts := []gocron.JobOption{
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
	}
	if startNow {
		opts = append(opts, gocron.WithStartAt(gocron.WithStartImmediately()))
	}
	job, err := s.cron.NewJob(
		gocron.DurationJob(interval),
		gocron.NewTask(func() {
			cur := s.cfg.Load()
			if cur == nil {
				return
			}
			repo, ok := cur.Repos[name]
			if !ok {
				// Removed mid-flight (between fire and lookup); next fire is gone.
				return
			}
			ctx := s.runCtx
			if ctx == nil {
				ctx = context.Background()
			}
			RunGuardedSync(ctx, s.syncer, s.state, &repo, "scheduler")
		}),
		opts...,
	)
	if err != nil {
		return uuid.Nil, err
	}
	return job.ID(), nil
}
