package app_test

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/sync/errgroup"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/risk"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

const (
	ethereum domain.ChainID = 1
	base     domain.ChainID = 8453
	orgA     domain.OrgID   = 10
	orgB     domain.OrgID   = 20
)

// twoChains runs one indexer per chain against one store, as main does.
type twoChains struct {
	store  *fakeStore
	chains map[domain.ChainID]*fakeChain
	m      *metrics.Metrics
}

func newTwoChains() *twoChains {
	return &twoChains{
		store:  newFakeStore(),
		chains: map[domain.ChainID]*fakeChain{ethereum: newFakeChain(20), base: newFakeChain(50)},
		m:      metrics.New(prometheus.NewRegistry()),
	}
}

func (tc *twoChains) indexer(id domain.ChainID) *app.Indexer {
	start := uint64(0)
	return app.NewIndexer(tc.chains[id], tc.store, app.IndexerConfig{
		ChainID: id, Confirmations: 0, ReorgWindow: 4, MaxBlockRange: 100, StartBlock: &start,
		PollInterval: time.Millisecond, Risk: risk.Params{DrainWindow: 50},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), tc.m)
}

func TestIndexersKeepChainsApart(t *testing.T) {
	tc := newTwoChains()
	// The same address is watched on both chains: org A on Ethereum, org B on Base.
	tc.store.watch(orgA, ethereum, victim)
	tc.store.watch(orgB, base, victim)
	// The same tx hash and log index on both chains must not collide.
	tc.chains[ethereum].addLog(approveLog(victim, scammer, maxU256, 3, 0xe1), victim)
	tc.chains[base].addLog(approveLog(victim, scammer, big.NewInt(5), 3, 0xe1), victim)
	tc.chains[base].addLog(transferLog(victim, scammer, big.NewInt(5), 40, 0xe2), scammer)

	for _, id := range []domain.ChainID{ethereum, base} {
		if err := tc.indexer(id).Sync(context.Background()); err != nil {
			t.Fatalf("sync chain %d: %v", id, err)
		}
	}

	ethEvents, ethAlerts, ethCP := tc.store.snapshotChain(ethereum)
	baseEvents, baseAlerts, baseCP := tc.store.snapshotChain(base)
	if ethCP.Block != 20 || baseCP.Block != 50 {
		t.Fatalf("checkpoints = %d/%d, want 20/50 (one per chain)", ethCP.Block, baseCP.Block)
	}
	if len(ethEvents) != 1 || len(baseEvents) != 2 {
		t.Fatalf("events = %d on Ethereum, %d on Base; want 1 and 2", len(ethEvents), len(baseEvents))
	}
	for _, a := range ethAlerts {
		if a.OrgID != orgA {
			t.Errorf("Ethereum alert %s for org %d, want only org A", a.Rule, a.OrgID)
		}
	}
	for _, a := range baseAlerts {
		if a.OrgID != orgB {
			t.Errorf("Base alert %s for org %d, want only org B", a.Rule, a.OrgID)
		}
	}
	if got := alertRules(ethAlerts); len(got) != 2 { // unlimited + approval to EOA
		t.Errorf("Ethereum alerts = %v", got)
	}
	if got := alertRules(baseAlerts); len(got) != 2 { // approval to EOA + drain
		t.Errorf("Base alerts = %v", got)
	}
	if got := testutil.ToFloat64(tc.m.IndexedBlock.WithLabelValues("8453")); got != 50 {
		t.Errorf("indexed_block{chain_id=8453} = %v", got)
	}
}

func TestWalletWatchedByTwoOrgsAlertsBoth(t *testing.T) {
	tc := newTwoChains()
	tc.store.watch(orgA, ethereum, victim)
	tc.store.watch(orgB, ethereum, victim)
	tc.chains[ethereum].addLog(approveLog(victim, scammer, maxU256, 3, 0xf1), victim)

	if err := tc.indexer(ethereum).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, alerts, _ := tc.store.snapshotChain(ethereum)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (the log is stored once)", len(events))
	}
	perOrg := map[domain.OrgID]int{}
	for _, a := range alerts {
		perOrg[a.OrgID]++
	}
	if perOrg[orgA] != 2 || perOrg[orgB] != 2 {
		t.Fatalf("alerts per org = %v, want 2 each", perOrg)
	}
	// Only one eth_getLogs topic per address, however many orgs watch it.
	for _, q := range tc.chains[ethereum].queries {
		for _, topics := range q.Topics {
			if len(topics) > 2 {
				t.Fatalf("query has duplicate wallet topics: %v", q.Topics)
			}
		}
	}
}

func TestIndexersRunConcurrently(t *testing.T) {
	tc := newTwoChains()
	tc.store.watch(orgA, ethereum, victim)
	tc.store.watch(orgA, base, victim)
	tc.chains[ethereum].addLog(approveLog(victim, scammer, maxU256, 3, 0xa1), victim)
	tc.chains[base].addLog(approveLog(victim, scammer, maxU256, 4, 0xa2), victim)

	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	for _, id := range []domain.ChainID{ethereum, base} {
		ix := tc.indexer(id)
		g.Go(func() error { return ix.Run(gctx) })
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, e := tc.store.snapshotChain(ethereum)
		_, _, b := tc.store.snapshotChain(base)
		if e != nil && e.Block == 20 && b != nil && b.Block == 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("indexers did not reach both heads")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}
