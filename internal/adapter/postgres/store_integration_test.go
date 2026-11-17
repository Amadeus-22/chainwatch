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

const chain domain.ChainID = 31337

var (
	owner   = domain.Address{19: 1}
	spender = domain.Address{19: 2}
	token   = domain.Address{19: 3}
	alice   = domain.Address{19: 0xa1}
	bob     = domain.Address{19: 0xb0}
	maxU256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func approval(amount *big.Int, block uint64, tx byte) domain.Event {
	return domain.Event{Kind: domain.KindApproval, Token: token, From: owner, To: spender, Amount: amount,
		BlockNumber: block, BlockHash: domain.Hash{31: byte(block)}, TxHash: domain.Hash{0: tx}, LogIndex: 0}
}

func alertFor(org domain.OrgID, ev domain.Event, rule string) domain.Alert {
	return domain.NewAlert(org, chain, owner, ev, domain.Finding{Rule: rule, Severity: domain.SeverityHigh, Reason: "r"})
}

func cp(block uint64) domain.Checkpoint {
	return domain.Checkpoint{Block: block, Hash: domain.Hash{31: byte(block)}}
}

// orgWatching creates an organization owned by by that watches owner on chain.
func orgWatching(t *testing.T, s *postgres.Store, by domain.Address) domain.OrgID {
	t.Helper()
	ctx := context.Background()
	o, err := s.CreateOrg(ctx, "org of "+by.String(), by)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddWallet(ctx, domain.Wallet{OrgID: o.ID, ChainID: chain, Address: owner}); err != nil {
		t.Fatal(err)
	}
	return o.ID
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestWatchList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, _ := s.CreateOrg(ctx, "A", alice)
	b, _ := s.CreateOrg(ctx, "B", bob)

	w, created, err := s.AddWallet(ctx, domain.Wallet{OrgID: a.ID, ChainID: chain, Address: owner, Label: "treasury"})
	if err != nil || !created || w.Label != "treasury" || w.CreatedAt.IsZero() {
		t.Fatalf("AddWallet = %+v, %v, %v", w, created, err)
	}
	w, created, err = s.AddWallet(ctx, domain.Wallet{OrgID: a.ID, ChainID: chain, Address: owner, Label: "other label"})
	if err != nil || created || w.Label != "treasury" {
		t.Fatalf("second AddWallet = %+v, %v, %v; want existing wallet unchanged", w, created, err)
	}
	// Same address: on another chain for A, and on the same chain for B.
	if _, created, _ := s.AddWallet(ctx, domain.Wallet{OrgID: a.ID, ChainID: 1, Address: owner}); !created {
		t.Fatal("same address on another chain was not a new wallet")
	}
	if _, created, _ := s.AddWallet(ctx, domain.Wallet{OrgID: b.ID, ChainID: chain, Address: owner}); !created {
		t.Fatal("same wallet in another organization was not a new wallet")
	}

	if ws, _ := s.OrgWallets(ctx, a.ID); len(ws) != 2 {
		t.Fatalf("org A wallets = %+v", ws)
	}
	watched, err := s.WatchedWallets(ctx, chain)
	if err != nil || len(watched) != 2 || watched[0].OrgID == watched[1].OrgID {
		t.Fatalf("WatchedWallets(%d) = %+v, %v; want one row per organization", chain, watched, err)
	}

	if err := s.RemoveWallet(ctx, b.ID, chain, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveWallet(ctx, b.ID, chain, owner); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second RemoveWallet: %v, want ErrNotFound", err)
	}
	if _, err := s.OrgWallet(ctx, a.ID, chain, owner); err != nil {
		t.Fatalf("org B's removal affected org A: %v", err)
	}
	if _, err := s.OrgWallet(ctx, b.ID, chain, owner); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("OrgWallet after removal: %v", err)
	}
}

func TestOrganizationsAndMembers(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	o, err := s.CreateOrg(ctx, "Fund", alice)
	if err != nil || o.ID == 0 {
		t.Fatalf("CreateOrg = %+v, %v", o, err)
	}
	if r, err := s.Role(ctx, o.ID, alice); err != nil || r != domain.RoleOwner {
		t.Fatalf("creator role = %q, %v", r, err)
	}
	if _, err := s.Role(ctx, o.ID, bob); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("non-member role: %v", err)
	}
	if m, created, err := s.AddMember(ctx, o.ID, bob, domain.RoleMember); err != nil || !created || m.Role != domain.RoleMember {
		t.Fatalf("AddMember = %+v, %v, %v", m, created, err)
	}
	if m, created, _ := s.AddMember(ctx, o.ID, bob, domain.RoleOwner); created || m.Role != domain.RoleMember {
		t.Fatalf("re-adding changed the member: %+v", m)
	}
	ms, err := s.Memberships(ctx, bob)
	if err != nil || len(ms) != 1 || ms[0].Org.Name != "Fund" || ms[0].Role != domain.RoleMember {
		t.Fatalf("Memberships = %+v, %v", ms, err)
	}
	if members, _ := s.Members(ctx, o.ID); len(members) != 2 || members[0].Address != alice {
		t.Fatalf("Members = %+v, want owner first", members)
	}
	if err := s.RemoveMember(ctx, o.ID, alice); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("removing the last owner: %v, want ErrConflict", err)
	}
	if r, _ := s.Role(ctx, o.ID, alice); r != domain.RoleOwner {
		t.Fatal("last owner was removed despite the conflict")
	}
	if err := s.RemoveMember(ctx, o.ID, bob); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, o.ID, bob); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second removal: %v", err)
	}
}

func TestNoncesAreSingleUse(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateNonce(ctx, "abcdef0123456789", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNonce(ctx, "expired000000000", now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeNonce(ctx, "abcdef0123456789", now); err != nil {
		t.Fatalf("first use: %v", err)
	}
	for name, n := range map[string]string{"reused": "abcdef0123456789", "expired": "expired000000000", "unknown": "zzzzzzzzzzzzzzzz"} {
		if err := s.ConsumeNonce(ctx, n, now); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s nonce: %v, want ErrNotFound", name, err)
		}
	}
}

func TestSessions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now()
	sess := domain.Session{TokenHash: [32]byte{1}, Address: alice, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(ctx, sess.TokenHash, now)
	if err != nil || got.Address != alice {
		t.Fatalf("Session = %+v, %v", got, err)
	}
	if _, err := s.Session(ctx, sess.TokenHash, now.Add(2*time.Hour)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired session: %v", err)
	}
	if err := s.DeleteSession(ctx, sess.TokenHash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sess.TokenHash, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted session: %v", err)
	}
}

// TestTenantIsolation checks the SQL itself scopes reads to the organization:
// the app layer checks membership, and the queries are the second line.
func TestTenantIsolation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := orgWatching(t, s, alice)
	b, _ := s.CreateOrg(ctx, "B", bob) // watches nothing
	other := domain.Address{19: 0x99}
	if _, _, err := s.AddWallet(ctx, domain.Wallet{OrgID: b.ID, ChainID: chain, Address: other}); err != nil {
		t.Fatal(err)
	}
	ev := approval(maxU256, 3, 0x1)
	if err := s.CommitRange(ctx, app.RangeResult{ChainID: chain, Checkpoint: cp(3), Events: []domain.Event{ev},
		Alerts: []domain.Alert{alertFor(a, ev, "unlimited_approval")}}); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.Allowances(ctx, app.AllowanceQuery{Org: a}); len(got) != 1 {
		t.Fatalf("org A allowances = %+v", got)
	}
	o := owner
	if got, _ := s.Allowances(ctx, app.AllowanceQuery{Org: b.ID, Owner: &o}); len(got) != 0 {
		t.Fatalf("org B sees allowances of a wallet it does not watch: %+v", got)
	}
	if got, _ := s.Alerts(ctx, app.AlertQuery{Org: a, Limit: 10}); len(got) != 1 {
		t.Fatalf("org A alerts = %+v", got)
	}
	if got, _ := s.Alerts(ctx, app.AlertQuery{Org: b.ID, Wallet: &o, Limit: 10}); len(got) != 0 {
		t.Fatalf("org B sees org A's alerts: %+v", got)
	}

	// Once B watches the wallet too, it sees the (public) allowance, but not A's alerts.
	if _, _, err := s.AddWallet(ctx, domain.Wallet{OrgID: b.ID, ChainID: chain, Address: owner}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Allowances(ctx, app.AllowanceQuery{Org: b.ID, Owner: &o}); len(got) != 1 {
		t.Fatalf("org B allowances after watching = %+v", got)
	}
	if got, _ := s.Alerts(ctx, app.AlertQuery{Org: b.ID, Limit: 10}); len(got) != 0 {
		t.Fatalf("org B sees org A's alerts: %+v", got)
	}
}

func TestChainsAreIndependent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	org := orgWatching(t, s, alice)
	ev := approval(maxU256, 5, 0x1)
	for _, c := range []domain.ChainID{chain, 1} {
		// Same tx hash and log index on both chains.
		if err := s.CommitRange(ctx, app.RangeResult{ChainID: c, Checkpoint: cp(5), Events: []domain.Event{ev},
			Alerts: []domain.Alert{domain.NewAlert(org, c, owner, ev, domain.Finding{Rule: "unlimited_approval"})}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Rewind(ctx, 1, cp(2)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Checkpoint(ctx, chain); got != cp(5) {
		t.Fatalf("rewinding chain 1 moved chain %d's checkpoint to %+v", chain, got)
	}
	if got, _ := s.ApprovalsSince(ctx, chain, owner, token, 0); len(got) != 1 {
		t.Fatalf("rewinding chain 1 deleted chain %d's events", chain)
	}
	if got, _ := s.ApprovalsSince(ctx, 1, owner, token, 0); len(got) != 0 {
		t.Fatalf("chain 1 events after rewind = %+v", got)
	}
	c := chain
	if got, _ := s.Alerts(ctx, app.AlertQuery{Org: org, ChainID: &c, Limit: 10}); len(got) != 1 {
		t.Fatalf("chain %d alerts = %+v", chain, got)
	}
	if _, ok, _ := s.Checkpoint(ctx, 8453); ok {
		t.Fatal("checkpoint for a chain never indexed")
	}
}

func TestCommitRangeIsIdempotentAndKeepsLatestAllowance(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	org := orgWatching(t, s, alice)
	unlimited := approval(maxU256, 10, 0xa)
	lowered := approval(big.NewInt(5), 12, 0xb)
	r := app.RangeResult{
		ChainID:    chain,
		Checkpoint: cp(12),
		Events:     []domain.Event{unlimited, lowered},
		Alerts:     []domain.Alert{alertFor(org, unlimited, "unlimited_approval")},
	}
	for range 2 {
		if err := s.CommitRange(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// An older approval arriving late must not overwrite the newer allowance.
	late := app.RangeResult{ChainID: chain, Checkpoint: cp(12), Events: []domain.Event{approval(big.NewInt(99), 11, 0xc)}}
	if err := s.CommitRange(ctx, late); err != nil {
		t.Fatal(err)
	}

	events, err := s.ApprovalsSince(ctx, chain, owner, token, 0)
	if err != nil || len(events) != 3 {
		t.Fatalf("ApprovalsSince = %d events, %v; want 3", len(events), err)
	}
	if events[0].Amount.Cmp(maxU256) != 0 || events[0].TxHash != unlimited.TxHash || events[0].BlockHash != unlimited.BlockHash {
		t.Errorf("uint256 max or hashes did not round-trip: %+v", events[0])
	}
	if got, _ := s.ApprovalsSince(ctx, chain, owner, token, 11); len(got) != 2 {
		t.Errorf("ApprovalsSince(11) = %d events, want 2", len(got))
	}
	allowances, err := s.Allowances(ctx, app.AllowanceQuery{Org: org})
	if err != nil || len(allowances) != 1 || allowances[0].Amount.Int64() != 5 || allowances[0].UpdatedBlock != 12 ||
		allowances[0].ChainID != chain {
		t.Fatalf("Allowances = %+v, %v; want 5 set at block 12", allowances, err)
	}
	if n, _ := s.CountPendingAlerts(ctx); n != 1 {
		t.Fatalf("pending alerts = %d, want 1 (deduplicated)", n)
	}
	got, ok, err := s.Checkpoint(ctx, chain)
	if err != nil || !ok || got != cp(12) {
		t.Fatalf("Checkpoint = %+v, %v, %v", got, ok, err)
	}
}

func TestAlertsForDeletedOrgAreSkipped(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ev := approval(maxU256, 1, 0x1)
	err := s.CommitRange(ctx, app.RangeResult{ChainID: chain, Checkpoint: cp(1), Alerts: []domain.Alert{alertFor(424242, ev, "unlimited_approval")}})
	if err != nil {
		t.Fatalf("CommitRange with an alert for a missing organization: %v", err)
	}
	if n, _ := s.CountPendingAlerts(ctx); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}

func TestRewindRebuildsAllowancesAndDropsPendingAlerts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	org := orgWatching(t, s, alice)
	first := approval(big.NewInt(7), 5, 0x1)
	reorged := approval(maxU256, 9, 0x2)
	deliveredEv := approval(big.NewInt(8), 8, 0x3)
	err := s.CommitRange(ctx, app.RangeResult{
		ChainID:    chain,
		Checkpoint: cp(9),
		Events:     []domain.Event{first, deliveredEv, reorged},
		Alerts:     []domain.Alert{alertFor(org, deliveredEv, "approval_to_eoa"), alertFor(org, reorged, "unlimited_approval")},
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := s.PendingAlerts(ctx, time.Now().Add(time.Minute), 10)
	if err := s.MarkAlertDelivered(ctx, pending[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := s.Rewind(ctx, chain, cp(6)); err != nil {
		t.Fatal(err)
	}

	allowances, _ := s.Allowances(ctx, app.AllowanceQuery{Org: org})
	if len(allowances) != 1 || allowances[0].Amount.Int64() != 7 || allowances[0].UpdatedBlock != 5 {
		t.Fatalf("allowances after rewind = %+v, want 7 from block 5", allowances)
	}
	if events, _ := s.ApprovalsSince(ctx, chain, owner, token, 0); len(events) != 1 {
		t.Fatalf("%d events after rewind, want 1", len(events))
	}
	alerts, _ := s.Alerts(ctx, app.AlertQuery{Org: org, Limit: 10})
	if len(alerts) != 1 || alerts[0].Status != domain.AlertDelivered {
		t.Fatalf("alerts after rewind = %+v, want only the delivered one", alerts)
	}
	if got, _, _ := s.Checkpoint(ctx, chain); got != cp(6) {
		t.Fatalf("checkpoint = %+v", got)
	}
}

func TestAlertDeliveryLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	org := orgWatching(t, s, alice)
	ev := approval(maxU256, 1, 0x1)
	if err := s.CommitRange(ctx, app.RangeResult{ChainID: chain, Checkpoint: cp(1), Alerts: []domain.Alert{alertFor(org, ev, "unlimited_approval")}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	due, err := s.PendingAlerts(ctx, now.Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0].OrgID != org || due[0].ChainID != chain {
		t.Fatalf("PendingAlerts = %+v, %v", due, err)
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
	got, err := s.Alerts(ctx, app.AlertQuery{Org: org, Wallet: &w, Since: now.Add(-time.Hour), Limit: 10})
	if err != nil || len(got) != 1 || got[0].Status != domain.AlertDelivered || got[0].DeliveredAt == nil || got[0].Attempts != 2 {
		t.Fatalf("Alerts = %+v, %v", got, err)
	}
	other := spender
	if got, _ := s.Alerts(ctx, app.AlertQuery{Org: org, Wallet: &other, Limit: 10}); len(got) != 0 {
		t.Fatalf("wallet filter ignored: %+v", got)
	}
	if got, _ := s.Alerts(ctx, app.AlertQuery{Org: org, Since: now.Add(time.Hour), Limit: 10}); len(got) != 0 {
		t.Fatalf("since filter ignored: %+v", got)
	}
}
