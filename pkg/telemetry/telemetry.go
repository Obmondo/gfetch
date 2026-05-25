package telemetry

import "github.com/prometheus/client_golang/prometheus"

const labelRepo = "repo"

var (
	SyncFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_sync_failures_total",
		Help: "Total number of sync failures per repo and operation.",
	}, []string{labelRepo, "operation"})

	SyncSuccessTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_sync_success_total",
		Help: "Total number of successful syncs per repo.",
	}, []string{labelRepo})

	LastFailureTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gfetch_last_failure_timestamp",
		Help: "Unix timestamp of the last sync failure per repo.",
	}, []string{labelRepo})

	LastSuccessTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gfetch_last_success_timestamp",
		Help: "Unix timestamp of the last successful sync per repo.",
	}, []string{labelRepo})

	SyncsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_syncs_total",
		Help: "Total number of syncs attempted per repo.",
	}, []string{labelRepo})

	SyncDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gfetch_sync_duration_seconds",
		Help:    "Duration of sync operations in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{labelRepo, "operation"})

	OpenVoxStaleBranchesSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_stale_branches_skipped_total",
		Help: "Total number of OpenVox branches skipped due to staleness.",
	}, []string{labelRepo})

	OpenVoxLockAcquireTimeoutsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_lock_acquire_timeouts_total",
		Help: "Total number of OpenVox lock acquisition timeouts.",
	}, []string{labelRepo, "kind"})

	OpenVoxSyncOverlapSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_sync_overlap_skipped_total",
		Help: "Total number of OpenVox sync requests skipped due to in-progress sync.",
	}, []string{labelRepo, "source"})

	OpenVoxOrphanLockfilesRemovedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_orphan_lockfiles_removed_total",
		Help: "Total number of orphan OpenVox lockfiles removed.",
	}, []string{labelRepo})

	OpenVoxOrphanLockfilesSkippedInUseTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_orphan_lockfiles_skipped_in_use_total",
		Help: "Total number of orphan OpenVox lockfiles skipped because lock is in use.",
	}, []string{labelRepo})

	RemoteRefListTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_remote_ref_list_calls_total",
		Help: "Total number of remote ref-list calls per repo and sync mode.",
	}, []string{labelRepo, "mode"})

	ConfigReloadsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gfetch_config_reloads_total",
		Help: "Total number of successful config reloads.",
	})

	ConfigReloadFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_config_reload_failures_total",
		Help: "Total number of config reload failures, by reason (load, validate, apply).",
	}, []string{"reason"})

	ConfigRepoValidateFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_config_repo_validate_failures_total",
		Help: "Total number of per-repo config validation failures, labelled by repo name. Counted when Validate drops an invalid repo and continues.",
	}, []string{labelRepo})

	ConfigLastReloadTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gfetch_config_last_reload_timestamp",
		Help: "Unix timestamp of the last successful config reload.",
	})

	ConfigManagedRepos = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gfetch_config_managed_repos",
		Help: "Number of repos currently managed by the daemon.",
	})

	RemoteRefsCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gfetch_remote_refs_count",
		Help: "Total number of branches and tags advertised by the remote server.",
	}, []string{labelRepo})

	LocalActiveRefsCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gfetch_local_active_refs_count",
		Help: "Total number of branches and tags actively tracked and synced locally (after staleness filtering).",
	}, []string{labelRepo})

	CacheSyncRetriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_cache_sync_retries_total",
		Help: "Total number of times the central cache sync was retried due to missing remote refs.",
	}, []string{labelRepo})
)

func init() {
	prometheus.MustRegister(
		SyncFailuresTotal,
		SyncSuccessTotal,
		LastFailureTimestamp,
		LastSuccessTimestamp,
		SyncsTotal,
		SyncDurationSeconds,
		OpenVoxStaleBranchesSkippedTotal,
		OpenVoxLockAcquireTimeoutsTotal,
		OpenVoxSyncOverlapSkippedTotal,
		OpenVoxOrphanLockfilesRemovedTotal,
		OpenVoxOrphanLockfilesSkippedInUseTotal,
		RemoteRefListTotal,
		ConfigReloadsTotal,
		ConfigReloadFailuresTotal,
		ConfigRepoValidateFailuresTotal,
		ConfigLastReloadTimestamp,
		ConfigManagedRepos,
		RemoteRefsCount,
		LocalActiveRefsCount,
		CacheSyncRetriesTotal,
	)
}
