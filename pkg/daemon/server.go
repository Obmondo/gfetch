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

func newServer(s *gsync.Syncer, logger *slog.Logger, cfg *config.Config, guard *repoSyncGuard) http.Handler {
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
		result := runGuardedSync(r.Context(), s, guard, &repo, logger, "manual")
		if errors.Is(result.Err, errSyncInProgress) {
			w.WriteHeader(http.StatusConflict)
		}
		writeResult(w, []gsync.Result{result})
	})

	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("manual sync triggered for all repos")
		results := make([]gsync.Result, 0, len(cfg.Repos))
		for name := range cfg.Repos {
			repo := cfg.Repos[name]
			results = append(results, runGuardedSync(r.Context(), s, guard, &repo, logger, "manual"))
		}
		writeResult(w, results)
	})

	return mux
}

func runGuardedSync(ctx context.Context, s *gsync.Syncer, guard *repoSyncGuard, repo *config.RepoConfig, logger *slog.Logger, source string) gsync.Result {
	if !guard.TryStart(repo.Name) {
		if repo.OpenVox != nil && *repo.OpenVox {
			telemetry.OpenVoxSyncOverlapSkippedTotal.WithLabelValues(repo.Name, source).Inc()
		}
		logger.Warn("skipping sync: repo already in progress", "repo", repo.Name, "source", source)
		return gsync.Result{RepoName: repo.Name, Err: fmt.Errorf("%w: %s", errSyncInProgress, repo.Name)}
	}
	defer guard.Finish(repo.Name)

	return s.SyncRepo(ctx, repo, gsync.SyncOptions{})
}

func writeResult(w http.ResponseWriter, results []gsync.Result) {
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

	if hasErr {
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(out)
}
