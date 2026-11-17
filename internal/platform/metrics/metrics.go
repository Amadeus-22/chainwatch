// Package metrics defines chainwatch's Prometheus metrics in one place, so the
// README table and the code cannot drift apart.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Metrics groups every chainwatch collector.
type Metrics struct {
	IndexedBlock  *prometheus.GaugeVec   // label: chain_id
	HeadLag       *prometheus.GaugeVec   // label: chain_id
	Events        *prometheus.CounterVec // labels: chain_id, kind
	Findings      *prometheus.CounterVec // labels: chain_id, rule
	Reorgs        *prometheus.CounterVec // label: chain_id
	RPCErrors     *prometheus.CounterVec // labels: chain_id, method
	AlertsPending prometheus.Gauge
}

// New creates the collectors and registers them with reg. Tests pass a fresh
// prometheus.NewRegistry() so runs do not share state.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		IndexedBlock: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "chainwatch_indexed_block",
			Help: "Last block fully indexed and committed, per chain.",
		}, []string{"chain_id"}),
		HeadLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "chainwatch_head_lag_blocks",
			Help: "Chain head minus last indexed block (includes the confirmation depth).",
		}, []string{"chain_id"}),
		Events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chainwatch_events_total",
			Help: "ERC-20 events indexed for watched wallets.",
		}, []string{"chain_id", "kind"}),
		Findings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chainwatch_findings_total",
			Help: "Risk rule findings, before alert de-duplication.",
		}, []string{"chain_id", "rule"}),
		Reorgs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chainwatch_reorgs_total",
			Help: "Chain reorganisations detected at the checkpoint.",
		}, []string{"chain_id"}),
		RPCErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chainwatch_rpc_errors_total",
			Help: "Failed JSON-RPC calls by method.",
		}, []string{"chain_id", "method"}),
		AlertsPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chainwatch_alerts_pending",
			Help: "Alerts persisted but not yet delivered.",
		}),
	}
	reg.MustRegister(m.IndexedBlock, m.HeadLag, m.Events, m.Findings, m.Reorgs, m.RPCErrors, m.AlertsPending)
	return m
}
