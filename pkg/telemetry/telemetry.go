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
)

func init() {
	prometheus.MustRegister(
		SyncFailuresTotal,
		SyncSuccessTotal,
		LastFailureTimestamp,
		LastSuccessTimestamp,
		SyncsTotal,
		SyncDurationSeconds,
	)
}
