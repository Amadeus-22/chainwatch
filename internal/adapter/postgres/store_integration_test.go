//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Amadeus-22/chainwatch/internal/adapter/postgres"
	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// newStore returns a migrated store in a fresh schema, dropped after the test.
// Needs CHAINWATCH_TEST_DATABASE_URL (see `make test-integration`).
func newStore(t *testing.T) *postgres.Store {
	t.Helper()
	base := os.Getenv("CHAINWATCH_TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("CHAINWATCH_TEST_DATABASE_URL is not set; run `docker compose -f deploy/docker-compose.yml up -d postgres` and `make test-integration`")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := postgres.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

var (
	owner   = domain.Address{19: 1}
	spender = domain.Address{19: 2}
	token   = domain.Address{19: 3}
	maxU256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func approval(amount *big.Int, block uint64, tx byte) domain.Event {
	return domain.Event{Kind: domain.KindApproval, Token: token, From: owner, To: spender, Amount: amount,
		BlockNumber: block, BlockHash: domain.Hash{31: byte(block)}, TxHash: domain.Hash{0: tx}, LogIndex: 0}
}

func alertFor(ev domain.Event, rule string) domain.Alert {
	return domain.NewAlert(owner, ev, domain.Finding{Rule: rule, Severity: domain.SeverityHigh, Reason: "r"})
}

func cp(block uint64) domain.Checkpoint {
	return domain.Checkpoint{Block: block, Hash: domain.Hash{31: byte(block)}}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestWallets(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	w, created, err := s.AddWallet(ctx, owner, "treasury")
	if err != nil || !created || w.Label != "treasury" || w.CreatedAt.IsZero() {
		t.Fatalf("AddWallet = %+v, %v, %v", w, created, err)
	}
	w, created, err = s.AddWallet(ctx, owner, "other label")
	if err != nil || created || w.Label != "treasury" {
		t.Fatalf("second AddWallet = %+v, %v, %v; want existing wallet unchanged", w, created, err)
	}
	ws, err := s.Wallets(ctx)
	if err != nil || len(ws) != 1 || ws[0].Address != owner {
		t.Fatalf("Wallets = %+v, %v", ws, err)
	}
	if err := s.RemoveWallet(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveWallet(ctx, owner); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second RemoveWallet: %v, want ErrNotFound", err)
	}
	if _, err := s.Wallet(ctx, owner); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Wallet after removal: %v", err)
	}
}

func TestCommitRangeIsIdempotentAndKeepsLatestAllowance(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	unlimited := approval(maxU256, 10, 0xa)
	lowered := approval(big.NewInt(5), 12, 0xb)
	r := app.RangeResult{
		Checkpoint: cp(12),
		Events:     []domain.Event{unlimited, lowered},
		Alerts:     []domain.Alert{alertFor(unlimited, "unlimited_approval")},
	}
	for range 2 {
		if err := s.CommitRange(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// An older approval arriving late must not overwrite the newer allowance.
	late := app.RangeResult{Checkpoint: cp(12), Events: []domain.Event{approval(big.NewInt(99), 11, 0xc)}}
	if err := s.CommitRange(ctx, late); err != nil {
		t.Fatal(err)
	}

	events, err := s.ApprovalsSince(ctx, owner, token, 0)
	if err != nil || len(events) != 3 {
		t.Fatalf("ApprovalsSince = %d events, %v; want 3", len(events), err)
	}
	if events[0].Amount.Cmp(maxU256) != 0 || events[0].TxHash != unlimited.TxHash || events[0].BlockHash != unlimited.BlockHash {
		t.Errorf("uint256 max or hashes did not round-trip: %+v", events[0])
	}
	if got, _ := s.ApprovalsSince(ctx, owner, token, 11); len(got) != 2 {
		t.Errorf("ApprovalsSince(11) = %d events, want 2", len(got))
	}
	allowances, err := s.Allowances(ctx, owner)
	if err != nil || len(allowances) != 1 || allowances[0].Amount.Int64() != 5 || allowances[0].UpdatedBlock != 12 {
		t.Fatalf("Allowances = %+v, %v; want 5 set at block 12", allowances, err)
	}
	if n, _ := s.CountPendingAlerts(ctx); n != 1 {
		t.Fatalf("pending alerts = %d, want 1 (deduplicated)", n)
	}
	got, ok, err := s.Checkpoint(ctx)
	if err != nil || !ok || got != cp(12) {
		t.Fatalf("Checkpoint = %+v, %v, %v", got, ok, err)
	}
}

func TestRewindRebuildsAllowancesAndDropsPendingAlerts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first := approval(big.NewInt(7), 5, 0x1)
	reorged := approval(maxU256, 9, 0x2)
	deliveredEv := approval(big.NewInt(8), 8, 0x3)
	err := s.CommitRange(ctx, app.RangeResult{
		Checkpoint: cp(9),
		Events:     []domain.Event{first, deliveredEv, reorged},
		Alerts:     []domain.Alert{alertFor(deliveredEv, "approval_to_eoa"), alertFor(reorged, "unlimited_approval")},
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := s.PendingAlerts(ctx, time.Now().Add(time.Minute), 10)
	if err := s.MarkAlertDelivered(ctx, pending[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := s.Rewind(ctx, cp(6)); err != nil {
		t.Fatal(err)
	}

	allowances, _ := s.Allowances(ctx, owner)
	if len(allowances) != 1 || allowances[0].Amount.Int64() != 7 || allowances[0].UpdatedBlock != 5 {
		t.Fatalf("allowances after rewind = %+v, want 7 from block 5", allowances)
	}
	if events, _ := s.ApprovalsSince(ctx, owner, token, 0); len(events) != 1 {
		t.Fatalf("%d events after rewind, want 1", len(events))
	}
	alerts, _ := s.Alerts(ctx, app.AlertQuery{Limit: 10})
	if len(alerts) != 1 || alerts[0].Status != domain.AlertDelivered {
		t.Fatalf("alerts after rewind = %+v, want only the delivered one", alerts)
	}
	if got, _, _ := s.Checkpoint(ctx); got != cp(6) {
		t.Fatalf("checkpoint = %+v", got)
	}
}

func TestAlertDeliveryLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ev := approval(maxU256, 1, 0x1)
	if err := s.CommitRange(ctx, app.RangeResult{Checkpoint: cp(1), Alerts: []domain.Alert{alertFor(ev, "unlimited_approval")}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	due, err := s.PendingAlerts(ctx, now.Add(time.Second), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("PendingAlerts = %v, %v", due, err)
	}
	id := due[0].ID

	if err := s.MarkAlertFailed(ctx, id, now.Add(time.Hour), "503"); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.PendingAlerts(ctx, now.Add(time.Minute), 10); len(due) != 0 {
		t.Fatal("alert due again before its backoff elapsed")
	}
	due, _ = s.PendingAlerts(ctx, now.Add(2*time.Hour), 10)
	if len(due) != 1 || due[0].Attempts != 1 {
		t.Fatalf("after backoff: %+v", due)
	}
	if err := s.MarkAlertDelivered(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAlertDelivered(ctx, id, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delivery mark: %v, want ErrNotFound (no double delivery)", err)
	}
	if n, _ := s.CountPendingAlerts(ctx); n != 0 {
		t.Fatalf("pending = %d", n)
	}

	w := owner
	got, err := s.Alerts(ctx, app.AlertQuery{Wallet: &w, Since: now.Add(-time.Hour), Limit: 10})
	if err != nil || len(got) != 1 || got[0].Status != domain.AlertDelivered || got[0].DeliveredAt == nil || got[0].Attempts != 2 {
		t.Fatalf("Alerts = %+v, %v", got, err)
	}
	other := spender
	if got, _ := s.Alerts(ctx, app.AlertQuery{Wallet: &other, Limit: 10}); len(got) != 0 {
		t.Fatalf("wallet filter ignored: %+v", got)
	}
	if got, _ := s.Alerts(ctx, app.AlertQuery{Since: now.Add(time.Hour), Limit: 10}); len(got) != 0 {
		t.Fatalf("since filter ignored: %+v", got)
	}
}
