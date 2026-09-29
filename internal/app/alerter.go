package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/platform/backoff"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

// Alerter delivers pending alerts through a Notifier. Alerts are persisted before
// delivery and marked delivered only after the notifier succeeds, so a notifier
// outage delays alerts but never loses them; a failed alert is rescheduled with
// jittered exponential backoff. The single Alerter loop is the only sender, so an
// alert is not sent twice unless the process dies between a successful send and
// the acknowledgement; the dedup key lets receivers drop that rare redelivery.
type Alerter struct {
	store    Store
	notifier Notifier
	log      *slog.Logger
	m        *metrics.Metrics
	interval time.Duration
	retry    backoff.Policy
	batch    int
	now      func() time.Time
}

// NewAlerter wires an Alerter that polls for due alerts every interval.
func NewAlerter(store Store, notifier Notifier, interval time.Duration, log *slog.Logger, m *metrics.Metrics) *Alerter {
	return &Alerter{
		store:    store,
		notifier: notifier,
		log:      log,
		m:        m,
		interval: interval,
		retry:    backoff.Policy{Base: 5 * time.Second, Max: 10 * time.Minute},
		batch:    100,
		now:      time.Now,
	}
}

// Run delivers due alerts every interval until ctx is cancelled.
func (a *Alerter) Run(ctx context.Context) error {
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		if err := a.DeliverDue(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("deliver alerts failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// DeliverDue sends every pending alert whose next attempt is due. A failure on one
// alert does not block the others.
func (a *Alerter) DeliverDue(ctx context.Context) error {
	now := a.now()
	alerts, err := a.store.PendingAlerts(ctx, now, a.batch)
	if err != nil {
		return fmt.Errorf("load pending alerts: %w", err)
	}
	for _, al := range alerts {
		if err := a.notifier.Notify(ctx, al); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			next := now.Add(a.retry.Delay(al.Attempts + 1))
			if mErr := a.store.MarkAlertFailed(ctx, al.ID, next, err.Error()); mErr != nil {
				return fmt.Errorf("record failed delivery of alert %d: %w", al.ID, mErr)
			}
			a.log.Warn("alert delivery failed", "alert_id", al.ID, "rule", al.Rule,
				"attempt", al.Attempts+1, "next_attempt", next, "err", err)
			continue
		}
		if err := a.store.MarkAlertDelivered(ctx, al.ID, a.now()); err != nil {
			return fmt.Errorf("mark alert %d delivered: %w", al.ID, err)
		}
		a.log.Info("alert delivered", "alert_id", al.ID, "rule", al.Rule, "wallet", al.Wallet.String())
	}
	pending, err := a.store.CountPendingAlerts(ctx)
	if err != nil {
		return fmt.Errorf("count pending alerts: %w", err)
	}
	a.m.AlertsPending.Set(float64(pending))
	return nil
}
