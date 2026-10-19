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

// reloadError is a fatal reload failure carrying its failure code.
type reloadError struct {
	code failureCode
	err  error
}

func (e *reloadError) Error() string { return e.err.Error() }
func (e *reloadError) Unwrap() error { return e.err }

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
			// A repo dropped by a reload can still have an in-flight sync (jobs
			// are cancelled but the running goroutine finishes against its
			// captured config) writing to the clone dir. The guard outlives
			// config membership, so report 409 until that sync drains; only a
			// quiescent dropped repo gets the terminal 404. This lets a caller
			// tearing the clone down wait instead of racing the live sync.
			if sched.state.guard.IsActive(repoName) {
				writeJSONError(w, codeSyncInProgress, errSyncInProgress.Error())
				return
			}
			writeJSONError(w, codeRepoNotFound, codeRepoNotFound.message())
			return
		}

		slog.Info("manual sync triggered", "repo", repoName)
		result := RunGuardedSync(r.Context(), sched.syncer, sched.state, &repo, "manual")
		writeResult(r.Context(), w, []gsync.Result{result})
	})

	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		cfg := sched.Config()
		slog.Info("manual sync triggered for all repos")
		results := make([]gsync.Result, 0, len(cfg.Repos))
		for name := range cfg.Repos {
			repo := cfg.Repos[name]
			results = append(results, RunGuardedSync(r.Context(), sched.syncer, sched.state, &repo, "manual"))
		}
		writeResult(r.Context(), w, results)
	})

	return mux
}

// handleReload services POST /reload directly.
func handleReload(w http.ResponseWriter, _ *http.Request, sched *Scheduler) {
	res, err := loadValidateAndReload(sched)
	if err != nil {
		code := codeConfigApplyFailed
		if re, ok := errors.AsType[*reloadError](err); ok {
			code = re.code
		}
		writeJSONError(w, code, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// writeJSONError writes a JSON error body carrying the raw error text plus
// the stable code, with the status and message taken from the code's table
// row.
func writeJSONError(w http.ResponseWriter, code failureCode, errText string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code.status())
	_ = json.NewEncoder(w).Encode(map[string]string{"error": errText, "code": string(code), "message": code.message()})
}

// loadValidateAndReload reads the config from disk, validates it, and applies
// it via Scheduler.Reload. On fatal failure the old config keeps running and
// the returned error is a *reloadError carrying the failure code. Per-repo validate failures are surfaced as a
// *config.PartialValidateError and the reload proceeds with the remaining
// repos; an empty result after partial errors is treated as a 422.
func loadValidateAndReload(sched *Scheduler) (ReloadResult, error) {
	cfg, err := config.Load(sched.ConfigPath())
	if err != nil {
		telemetry.ConfigReloadFailuresTotal.WithLabelValues("load").Inc()
		slog.Warn("config reload failed (load)", "error", err)
		return ReloadResult{}, &reloadError{code: codeConfigLoadFailed, err: fmt.Errorf("load config: %w", err)}
	}

	var partialValidate *config.PartialValidateError
	switch err := cfg.Validate(); {
	case errors.As(err, &partialValidate):
		slog.Warn("config reload: some repos failed validation, dropping",
			"failed_repos", len(partialValidate.Failures))
	case err != nil:
		telemetry.ConfigReloadFailuresTotal.WithLabelValues("validate").Inc()
		slog.Warn("config reload failed (validate)", "error", err)
		return ReloadResult{}, &reloadError{code: codeConfigInvalid, err: fmt.Errorf("validate config: %w", err)}
	}

	if len(cfg.Repos) == 0 {
		telemetry.ConfigReloadFailuresTotal.WithLabelValues("validate").Inc()
		slog.Warn("config reload aborted: no usable repos after load+validate, keeping previous config")
		return ReloadResult{}, &reloadError{code: codeConfigInvalid, err: fmt.Errorf("validate config: no usable repos after load+validate")}
	}

	res, err := sched.Reload(cfg)
	if err != nil {
		telemetry.ConfigReloadFailuresTotal.WithLabelValues("apply").Inc()
		slog.Warn("config reload failed (apply)", "error", err)
		code := codeConfigApplyFailed
		if errors.Is(err, errDaemonShuttingDown) {
			code = codeShuttingDown
		}
		return ReloadResult{}, &reloadError{code: code, err: fmt.Errorf("apply config: %w", err)}
	}
	slog.Info("config reloaded", "repos", len(res.Repos))
	return res, nil
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

// resultCode returns the failure code for a sync result error.
func resultCode(ctx context.Context, err error) failureCode {
	switch {
	case errors.Is(err, errDaemonShuttingDown):
		return codeShuttingDown
	case errors.Is(err, errSyncInProgress):
		return codeSyncInProgress
	case ctx.Err() != nil:
		// The caller went away; nobody reads this reply.
		return failureCode(gsync.CodeInternal)
	}
	return failureCode(gsync.Classify(err))
}

// writeResult writes the per-repo results. The status is the aggregated
// failure status, or 200 when no repo failed.
func writeResult(ctx context.Context, w http.ResponseWriter, results []gsync.Result) {
	w.Header().Set("Content-Type", "application/json")

	type jsonResult struct {
		RepoName         string   `json:"repo"`
		BranchesSynced   []string `json:"branches_synced,omitempty"`
		BranchesUpToDate []string `json:"branches_up_to_date,omitempty"`
		BranchesFailed   []string `json:"branches_failed,omitempty"`
		TagsFetched      []string `json:"tags_fetched,omitempty"`
		TagsUpToDate     []string `json:"tags_up_to_date,omitempty"`
		Error            string   `json:"error,omitempty"`
		Code             string   `json:"code,omitempty"`
		Message          string   `json:"message,omitempty"`
	}

	out := make([]jsonResult, len(results))
	failStatus := 0
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
			code := resultCode(ctx, r.Err)
			out[i].Code = string(code)
			out[i].Message = code.message()
			// All failures sharing one status report it; mixed statuses
			// fall back to 500. Successful repos don't count.
			switch st := code.status(); failStatus {
			case 0:
				failStatus = st
			case st:
			default:
				failStatus = http.StatusInternalServerError
			}
		}
	}

	if failStatus == 0 {
		failStatus = http.StatusOK
	}
	w.WriteHeader(failStatus)
	_ = json.NewEncoder(w).Encode(out)
}
