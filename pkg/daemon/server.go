package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/gsync"
	"github.com/obmondo/gfetch/pkg/telemetry"
)

var errSyncInProgress = errors.New("sync already in progress")
var errDaemonShuttingDown = errors.New("daemon shutting down")

func newServer(sched *Scheduler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.Handle("GET /metrics", promhttp.Handler())

	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		handleReload(w, r, sched)
	})

	mux.HandleFunc("POST /sync/{repo}", func(w http.ResponseWriter, r *http.Request) {
		cfg := sched.Config()
		repoName := r.PathValue("repo")
		repo, ok := cfg.Repos[repoName]
		if !ok {
			http.Error(w, `{"error":"repo not found"}`, http.StatusNotFound)
			return
		}

		slog.Info("manual sync triggered", "repo", repoName)
		result := RunGuardedSync(r.Context(), sched.syncer, sched.state, &repo, "manual")
		if errors.Is(result.Err, errDaemonShuttingDown) {
			writeResultWithStatus(w, []gsync.Result{result}, http.StatusServiceUnavailable)
			return
		}
		if errors.Is(result.Err, errSyncInProgress) {
			writeResultWithStatus(w, []gsync.Result{result}, http.StatusConflict)
			return
		}
		writeResult(w, []gsync.Result{result})
	})

	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		cfg := sched.Config()
		slog.Info("manual sync triggered for all repos")
		results := make([]gsync.Result, 0, len(cfg.Repos))
		hasShutdownErr := false
		hasInProgressErr := false
		for name := range cfg.Repos {
			repo := cfg.Repos[name]
			res := RunGuardedSync(r.Context(), sched.syncer, sched.state, &repo, "manual")
			if errors.Is(res.Err, errDaemonShuttingDown) {
				hasShutdownErr = true
			}
			if errors.Is(res.Err, errSyncInProgress) {
				hasInProgressErr = true
			}
			results = append(results, res)
		}

		if hasShutdownErr {
			writeResultWithStatus(w, results, http.StatusServiceUnavailable)
			return
		}
		if hasInProgressErr {
			writeResultWithStatus(w, results, http.StatusConflict)
			return
		}
		writeResult(w, results)
	})

	return mux
}

// handleReload services POST /reload directly.
func handleReload(w http.ResponseWriter, _ *http.Request, sched *Scheduler) {
	res, status, err := loadValidateAndReload(sched)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// loadValidateAndReload reads the config from disk, validates it, and applies
// it via Scheduler.Reload. On fatal failure the old config keeps running and
// the returned status is the HTTP status the caller should write. Per-repo
// validate failures are surfaced as a *config.PartialValidateError and the
// reload proceeds with the remaining repos; an empty result after partial
// errors is treated as a 422.
func loadValidateAndReload(sched *Scheduler) (ReloadResult, int, error) {
	cfg, err := config.Load(sched.ConfigPath())
	if err != nil {
		telemetry.ConfigReloadFailuresTotal.WithLabelValues("load").Inc()
		slog.Warn("config reload failed (load)", "error", err)
		return ReloadResult{}, http.StatusInternalServerError, fmt.Errorf("load config: %w", err)
	}

	var partialValidate *config.PartialValidateError
	switch err := cfg.Validate(); {
	case errors.As(err, &partialValidate):
		slog.Warn("config reload: some repos failed validation, dropping",
			"failed_repos", len(partialValidate.Failures))
	case err != nil:
		telemetry.ConfigReloadFailuresTotal.WithLabelValues("validate").Inc()
		slog.Warn("config reload failed (validate)", "error", err)
		return ReloadResult{}, http.StatusUnprocessableEntity, fmt.Errorf("validate config: %w", err)
	}

	if len(cfg.Repos) == 0 {
		telemetry.ConfigReloadFailuresTotal.WithLabelValues("validate").Inc()
		slog.Warn("config reload aborted: no usable repos after load+validate, keeping previous config")
		return ReloadResult{}, http.StatusUnprocessableEntity, fmt.Errorf("validate config: no usable repos after load+validate")
	}

	res, err := sched.Reload(cfg)
	if err != nil {
		telemetry.ConfigReloadFailuresTotal.WithLabelValues("apply").Inc()
		slog.Warn("config reload failed (apply)", "error", err)
		return ReloadResult{}, http.StatusInternalServerError, fmt.Errorf("apply config: %w", err)
	}
	slog.Info("config reloaded", "repos", len(res.Repos))
	return res, http.StatusOK, nil
}

func RunGuardedSync(ctx context.Context, s *gsync.Syncer, state *SyncRuntimeState, repo *config.RepoConfig, source string) gsync.Result {
	if state.shuttingDown.Load() {
		slog.Warn("skipping sync: daemon is shutting down", "repo", repo.Name, "source", source)
		return gsync.Result{RepoName: repo.Name, Err: fmt.Errorf("%w: %s", errDaemonShuttingDown, repo.Name)}
	}

	if !state.guard.TryStart(repo.Name) {
		if repo.IsOpenVox() {
			telemetry.OpenVoxSyncOverlapSkippedTotal.WithLabelValues(repo.Name, source).Inc()
		}
		slog.Warn("skipping sync: repo already in progress", "repo", repo.Name, "source", source)
		return gsync.Result{RepoName: repo.Name, Err: fmt.Errorf("%w: %s", errSyncInProgress, repo.Name)}
	}
	state.syncWG.Add(1)
	defer func() {
		state.guard.Finish(repo.Name)
		state.syncWG.Done()
	}()

	return s.SyncRepo(ctx, repo, gsync.SyncOptions{})
}

func writeResult(w http.ResponseWriter, results []gsync.Result) {
	writeResultWithStatus(w, results, 0)
}

func writeResultWithStatus(w http.ResponseWriter, results []gsync.Result, status int) {
	w.Header().Set("Content-Type", "application/json")

	hasErr := false
	for _, r := range results {
		if r.Err != nil {
			hasErr = true
			break
		}
	}

	type jsonResult struct {
		RepoName         string   `json:"repo"`
		BranchesSynced   []string `json:"branches_synced,omitempty"`
		BranchesUpToDate []string `json:"branches_up_to_date,omitempty"`
		BranchesFailed   []string `json:"branches_failed,omitempty"`
		TagsFetched      []string `json:"tags_fetched,omitempty"`
		TagsUpToDate     []string `json:"tags_up_to_date,omitempty"`
		Error            string   `json:"error,omitempty"`
	}

	out := make([]jsonResult, len(results))
	for i, r := range results {
		out[i] = jsonResult{
			RepoName:         r.RepoName,
			BranchesSynced:   r.BranchesSynced,
			BranchesUpToDate: r.BranchesUpToDate,
			BranchesFailed:   r.BranchesFailed,
			TagsFetched:      r.TagsFetched,
			TagsUpToDate:     r.TagsUpToDate,
		}
		if r.Err != nil {
			out[i].Error = r.Err.Error()
		}
	}

	if status != 0 {
		w.WriteHeader(status)
	}

	if status == 0 && hasErr {
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(out)
}
