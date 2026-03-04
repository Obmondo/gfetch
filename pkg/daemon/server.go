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

func newServer(s *gsync.Syncer, logger *slog.Logger, cfg *config.Config, state *syncRuntimeState) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.Handle("GET /metrics", promhttp.Handler())

	mux.HandleFunc("POST /sync/{repo}", func(w http.ResponseWriter, r *http.Request) {
		repoName := r.PathValue("repo")
		repo, ok := cfg.Repos[repoName]
		if !ok {
			http.Error(w, `{"error":"repo not found"}`, http.StatusNotFound)
			return
		}

		logger.Info("manual sync triggered", "repo", repoName)
		result := runGuardedSync(r.Context(), s, state, &repo, logger, "manual")
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
		logger.Info("manual sync triggered for all repos")
		results := make([]gsync.Result, 0, len(cfg.Repos))
		hasShutdownErr := false
		hasInProgressErr := false
		for name := range cfg.Repos {
			repo := cfg.Repos[name]
			res := runGuardedSync(r.Context(), s, state, &repo, logger, "manual")
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

func runGuardedSync(ctx context.Context, s *gsync.Syncer, state *syncRuntimeState, repo *config.RepoConfig, logger *slog.Logger, source string) gsync.Result {
	if state.shuttingDown.Load() {
		logger.Warn("skipping sync: daemon is shutting down", "repo", repo.Name, "source", source)
		return gsync.Result{RepoName: repo.Name, Err: fmt.Errorf("%w: %s", errDaemonShuttingDown, repo.Name)}
	}

	if !state.guard.TryStart(repo.Name) {
		if repo.OpenVox != nil && *repo.OpenVox {
			telemetry.OpenVoxSyncOverlapSkippedTotal.WithLabelValues(repo.Name, source).Inc()
		}
		logger.Warn("skipping sync: repo already in progress", "repo", repo.Name, "source", source)
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
	} else if hasErr {
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(out)
}
