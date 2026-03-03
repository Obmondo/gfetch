package telemetry

import "github.com/prometheus/client_golang/prometheus"

var (
	SyncFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_sync_failures_total",
		Help: "Total number of sync failures per repo and operation.",
	}, []string{"repo", "operation"})

	SyncSuccessTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_sync_success_total",
		Help: "Total number of successful syncs per repo.",
	}, []string{"repo"})

	LastFailureTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gfetch_last_failure_timestamp",
		Help: "Unix timestamp of the last sync failure per repo.",
	}, []string{"repo"})

	LastSuccessTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gfetch_last_success_timestamp",
		Help: "Unix timestamp of the last successful sync per repo.",
	}, []string{"repo"})

	SyncsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_syncs_total",
		Help: "Total number of syncs attempted per repo.",
	}, []string{"repo"})

	SyncDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gfetch_sync_duration_seconds",
		Help:    "Duration of sync operations in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"repo", "operation"})

	OpenVoxStaleBranchesSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_stale_branches_skipped_total",
		Help: "Total number of OpenVox branches skipped due to staleness.",
	}, []string{"repo"})

	OpenVoxLockAcquireTimeoutsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_lock_acquire_timeouts_total",
		Help: "Total number of OpenVox lock acquisition timeouts.",
	}, []string{"repo", "kind"})

	OpenVoxSyncOverlapSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_sync_overlap_skipped_total",
		Help: "Total number of OpenVox sync requests skipped due to in-progress sync.",
	}, []string{"repo", "source"})

	OpenVoxOrphanLockfilesRemovedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_orphan_lockfiles_removed_total",
		Help: "Total number of orphan OpenVox lockfiles removed.",
	}, []string{"repo"})

	OpenVoxOrphanLockfilesSkippedInUseTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gfetch_openvox_orphan_lockfiles_skipped_in_use_total",
		Help: "Total number of orphan OpenVox lockfiles skipped because lock is in use.",
	}, []string{"repo"})
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
	)
}
