package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

// flakyNotifier fails the first `failures` calls for each alert, then succeeds.
type flakyNotifier struct {
	mu        sync.Mutex
	failures  int
	calls     map[int64]int
	delivered map[int64]int
}

func newFlakyNotifier(failures int) *flakyNotifier {
	return &flakyNotifier{failures: failures, calls: map[int64]int{}, delivered: map[int64]int{}}
}

func (n *flakyNotifier) Notify(_ context.Context, a domain.Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls[a.ID]++
	if n.calls[a.ID] <= n.failures {
		return errors.New("webhook returned 503")
	}
	n.delivered[a.ID]++
	return nil
}

func seedAlerts(t *testing.T, s *fakeStore, n int) {
	t.Helper()
	var alerts []domain.Alert
	for i := range n {
		alerts = append(alerts, domain.NewAlert(victim, domain.Event{TxHash: domain.Hash{byte(i + 1)}},
			domain.Finding{Rule: "unlimited_approval", Severity: domain.SeverityHigh}))
	}
	if err := s.CommitRange(context.Background(), app.RangeResult{Alerts: alerts}); err != nil {
		t.Fatal(err)
	}
}

func newTestAlerter(s *fakeStore, n app.Notifier, clock *time.Time) (*app.Alerter, *metrics.Metrics) {
	m := metrics.New(prometheus.NewRegistry())
	a := app.NewAlerter(s, n, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	app.SetAlerterClock(a, func() time.Time { return *clock })
	return a, m
}

func TestAlerterRetriesUntilDeliveredExactlyOnce(t *testing.T) {
	store := newFakeStore()
	seedAlerts(t, store, 1)
	notifier := newFlakyNotifier(2)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	alerter, m := newTestAlerter(store, notifier, &now)
	ctx := context.Background()

	for step := range 6 {
		if err := alerter.DeliverDue(ctx); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		now = now.Add(time.Hour) // past any backoff
	}

	if notifier.calls[1] != 3 {
		t.Errorf("notifier called %d times, want 3 (two failures, one success)", notifier.calls[1])
	}
	if notifier.delivered[1] != 1 {
		t.Errorf("alert delivered %d times, want exactly 1", notifier.delivered[1])
	}
	_, alerts, _ := store.snapshot()
	if alerts[0].Status != domain.AlertDelivered || alerts[0].Attempts != 3 || alerts[0].lastError == "" {
		t.Errorf("stored alert = %+v, want delivered after 3 attempts with the last error kept", alerts[0])
	}
	if got := testutil.ToFloat64(m.AlertsPending); got != 0 {
		t.Errorf("alerts_pending = %v, want 0", got)
	}
}

func TestAlerterWaitsForBackoff(t *testing.T) {
	store := newFakeStore()
	seedAlerts(t, store, 1)
	notifier := newFlakyNotifier(1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	alerter, m := newTestAlerter(store, notifier, &now)
	ctx := context.Background()

	_ = alerter.DeliverDue(ctx) // fails, rescheduled at least 2.5s later
	_ = alerter.DeliverDue(ctx) // same instant: not due yet
	if notifier.calls[1] != 1 {
		t.Fatalf("notifier called %d times before the backoff elapsed, want 1", notifier.calls[1])
	}
	if got := testutil.ToFloat64(m.AlertsPending); got != 1 {
		t.Fatalf("alerts_pending = %v, want 1 while the notifier is down", got)
	}
}

func TestAlerterFailureDoesNotBlockOtherAlerts(t *testing.T) {
	store := newFakeStore()
	seedAlerts(t, store, 3)
	notifier := &selectiveNotifier{fail: 2}
	now := time.Now()
	alerter, _ := newTestAlerter(store, notifier, &now)

	if err := alerter.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, alerts, _ := store.snapshot()
	for _, a := range alerts {
		want := domain.AlertDelivered
		if a.ID == 2 {
			want = domain.AlertPending
		}
		if a.Status != want {
			t.Errorf("alert %d status %s, want %s", a.ID, a.Status, want)
		}
	}
}

type selectiveNotifier struct{ fail int64 }

func (n *selectiveNotifier) Notify(_ context.Context, a domain.Alert) error {
	if a.ID == n.fail {
		return errors.New("boom")
	}
	return nil
}
