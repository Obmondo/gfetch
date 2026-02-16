package daemon

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ashish1099/gfetch/pkg/config"
	"github.com/ashish1099/gfetch/pkg/sync"
)

func newServer(syncer *sync.Syncer, logger *slog.Logger, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	mux.Handle("GET /metrics", promhttp.Handler())

	// Build a map for quick repo lookup by name.
	repoMap := make(map[string]*config.RepoConfig, len(cfg.Repos))
	for i := range cfg.Repos {
		repoMap[cfg.Repos[i].Name] = &cfg.Repos[i]
	}

	mux.HandleFunc("POST /sync/{repo}", func(w http.ResponseWriter, r *http.Request) {
		repoName := r.PathValue("repo")
		repo, ok := repoMap[repoName]
		if !ok {
			http.Error(w, `{"error":"repo not found"}`, http.StatusNotFound)
			return
		}

		logger.Info("manual sync triggered", "repo", repoName)
		result := syncer.SyncRepo(r.Context(), repo, sync.SyncOptions{})
		writeResult(w, []sync.Result{result})
	})

	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("manual sync triggered for all repos")
		results := syncer.SyncAll(r.Context(), cfg, sync.SyncOptions{})
		writeResult(w, results)
	})

	return mux
}

func writeResult(w http.ResponseWriter, results []sync.Result) {
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
	json.NewEncoder(w).Encode(out)
}