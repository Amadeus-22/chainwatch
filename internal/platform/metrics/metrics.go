// Package metrics defines chainwatch's Prometheus metrics in one place, so the
// README table and the code cannot drift apart.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups every chainwatch collector.
type Metrics struct {
	IndexedBlock  prometheus.Gauge
	HeadLag       prometheus.Gauge
	Events        *prometheus.CounterVec // label: kind
	Findings      *prometheus.CounterVec // label: rule
	Reorgs        prometheus.Counter
	RPCErrors     *prometheus.CounterVec // label: method
	AlertsPending prometheus.Gauge
}

// New creates the collectors and registers them with reg. Tests pass a fresh
// prometheus.NewRegistry() so runs do not share state.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		IndexedBlock: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chainwatch_indexed_block",
			Help: "Last block fully indexed and committed.",
		}),
		HeadLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chainwatch_head_lag_blocks",
			Help: "Chain head minus last indexed block (includes the confirmation depth).",
		}),
		Events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chainwatch_events_total",
			Help: "ERC-20 events indexed for watched wallets.",
		}, []string{"kind"}),
		Findings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chainwatch_findings_total",
			Help: "Risk rule findings, before alert de-duplication.",
		}, []string{"rule"}),
		Reorgs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chainwatch_reorgs_total",
			Help: "Chain reorganisations detected at the checkpoint.",
		}),
		RPCErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chainwatch_rpc_errors_total",
			Help: "Failed JSON-RPC calls by method.",
		}, []string{"method"}),
		AlertsPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chainwatch_alerts_pending",
			Help: "Alerts persisted but not yet delivered.",
		}),
	}
	reg.MustRegister(m.IndexedBlock, m.HeadLag, m.Events, m.Findings, m.Reorgs, m.RPCErrors, m.AlertsPending)
	return m
}
