package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/erc20"
	"github.com/Amadeus-22/chainwatch/internal/domain/risk"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

var (
	victim   = domain.Address{19: 0x01}
	scammer  = domain.Address{19: 0x02}
	stranger = domain.Address{19: 0x03}
	router   = domain.Address{19: 0x04} // a contract
	token    = domain.Address{19: 0x70}
	maxU256  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func erc20Log(topic domain.Hash, from, to domain.Address, amount *big.Int, block uint64, tx byte, idx uint) domain.Log {
	data := make([]byte, 32)
	amount.FillBytes(data)
	return domain.Log{
		Address:     token,
		Topics:      []domain.Hash{topic, erc20.AddressTopic(from), erc20.AddressTopic(to)},
		Data:        data,
		BlockNumber: block,
		TxHash:      domain.Hash{0: tx, 31: byte(block)},
		LogIndex:    idx,
	}
}

func approveLog(owner, spender domain.Address, amount *big.Int, block uint64, tx byte) domain.Log {
	return erc20Log(erc20.TopicApproval, owner, spender, amount, block, tx, 0)
}

func transferLog(from, to domain.Address, amount *big.Int, block uint64, tx byte) domain.Log {
	return erc20Log(erc20.TopicTransfer, from, to, amount, block, tx, 1)
}

type harness struct {
	chain *fakeChain
	store *fakeStore
	reg   *prometheus.Registry
	m     *metrics.Metrics
	cfg   app.IndexerConfig
}

func newHarness(head uint64) *harness {
	reg := prometheus.NewRegistry()
	start := uint64(0)
	return &harness{
		chain: newFakeChain(head),
		store: newFakeStore(victim),
		reg:   reg,
		m:     metrics.New(reg),
		cfg: app.IndexerConfig{
			Confirmations: 0,
			ReorgWindow:   4,
			MaxBlockRange: 100,
			StartBlock:    &start,
			PollInterval:  time.Millisecond,
			Risk: risk.Params{
				DrainWindow:  50,
				LargeOutflow: map[domain.Address]*big.Int{token: big.NewInt(500)},
			},
		},
	}
}

func (h *harness) indexer() *app.Indexer {
	return app.NewIndexer(h.chain, h.store, h.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), h.m)
}

func (h *harness) sync(t *testing.T) {
	t.Helper()
	if err := h.indexer().Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func alertRules(alerts []storedAlert) []string {
	out := []string{}
	for _, a := range alerts {
		out = append(out, a.Rule)
	}
	slices.Sort(out)
	return out
}

func TestSyncDetectsDrainScenario(t *testing.T) {
	h := newHarness(10)
	h.chain.code[router] = true
	h.chain.addLog(transferLog(stranger, victim, big.NewInt(1000), 1, 0xa1), stranger) // funding: inflow, no risk
	h.chain.addLog(approveLog(victim, router, big.NewInt(10), 2, 0xa2), victim)        // benign approval to a contract
	h.chain.addLog(approveLog(victim, scammer, maxU256, 3, 0xa3), victim)              // phishing signature
	h.chain.addLog(transferLog(victim, scammer, big.NewInt(1000), 5, 0xa4), scammer)   // transferFrom by the scammer
	h.chain.addLog(transferLog(stranger, scammer, big.NewInt(7), 6, 0xa5), stranger)   // unrelated to the victim
	h.chain.addLog(approveLog(stranger, scammer, maxU256, 6, 0xa6), stranger)          // unrelated to the victim

	h.sync(t)

	events, alerts, cp := h.store.snapshot()
	if len(events) != 4 {
		t.Fatalf("indexed %d events, want 4 (unrelated logs must be filtered)", len(events))
	}
	want := []string{risk.RuleApprovalToEOA, risk.RuleDrainPattern, risk.RuleLargeOutflow, risk.RuleUnlimitedApproval}
	if got := alertRules(alerts); !slices.Equal(got, want) {
		t.Fatalf("alerts = %v, want %v", got, want)
	}
	for _, a := range alerts {
		if a.Wallet != victim || a.Status != domain.AlertPending {
			t.Errorf("alert %+v: want pending alert for the victim", a.Alert)
		}
	}
	if cp == nil || cp.Block != 10 || cp.Hash != h.chain.hashAt(10) {
		t.Fatalf("checkpoint = %+v, want block 10 with its hash", cp)
	}
	if got := testutil.ToFloat64(h.m.IndexedBlock); got != 10 {
		t.Errorf("indexed_block = %v, want 10", got)
	}
	if got := testutil.ToFloat64(h.m.Events.WithLabelValues("approval")); got != 2 {
		t.Errorf("events_total{kind=approval} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(h.m.Findings.WithLabelValues(risk.RuleDrainPattern)); got != 1 {
		t.Errorf("findings_total{rule=drain_pattern} = %v, want 1", got)
	}
}

func TestSyncStaysConfirmationsBehindHead(t *testing.T) {
	h := newHarness(10)
	h.cfg.Confirmations = 3
	h.chain.addLog(approveLog(victim, scammer, maxU256, 8, 0xb1), victim)

	h.sync(t)

	events, _, cp := h.store.snapshot()
	if cp.Block != 7 {
		t.Fatalf("checkpoint = %d, want 7 (head 10 - 3 confirmations)", cp.Block)
	}
	if len(events) != 0 {
		t.Fatalf("indexed %d events from unconfirmed blocks", len(events))
	}

	h.chain.head = 11
	h.sync(t)
	if events, _, _ := h.store.snapshot(); len(events) != 1 {
		t.Fatalf("after one more block: %d events, want 1", len(events))
	}
}

func TestSyncDetectsDrainAcrossRanges(t *testing.T) {
	h := newHarness(30)
	h.cfg.MaxBlockRange = 5 // approval and drain land in different committed ranges
	h.chain.addLog(approveLog(victim, scammer, maxU256, 2, 0xc1), victim)
	h.chain.addLog(transferLog(victim, stranger, big.NewInt(1), 27, 0xc2), scammer)

	h.sync(t)

	_, alerts, _ := h.store.snapshot()
	if !slices.Contains(alertRules(alerts), risk.RuleDrainPattern) {
		t.Fatalf("alerts = %v, want drain_pattern using the approval from an earlier range", alertRules(alerts))
	}
}

func TestSyncHalvesRangeWhenNodeRefuses(t *testing.T) {
	h := newHarness(40)
	h.cfg.MaxBlockRange = 32
	h.chain.maxSpan = 5
	for b := uint64(0); b <= 40; b += 4 {
		h.chain.addLog(transferLog(stranger, victim, big.NewInt(1), b, byte(b)), stranger)
	}

	h.sync(t)

	events, _, cp := h.store.snapshot()
	if cp.Block != 40 {
		t.Fatalf("checkpoint = %d, want 40", cp.Block)
	}
	if len(events) != 11 {
		t.Fatalf("indexed %d events, want 11: halving must not skip blocks", len(events))
	}
	if h.chain.tooLarge == 0 {
		t.Fatal("node limit never hit; test is not exercising halving")
	}
	// Successful queries (two per range) tile [0, 40] with no gap or overlap.
	next := uint64(0)
	for _, q := range h.chain.queries {
		if q.ToBlock-q.FromBlock+1 > h.chain.maxSpan || q.Topics[1] == nil {
			continue // refused attempt, or the second (recipient) query of a range
		}
		if q.FromBlock != next {
			t.Fatalf("range %d-%d, want it to start at %d", q.FromBlock, q.ToBlock, next)
		}
		next = q.ToBlock + 1
	}
	if next != 41 {
		t.Fatalf("ranges ended at %d, want 40", next-1)
	}
}

func TestSyncFailsWhenSingleBlockIsTooLarge(t *testing.T) {
	h := newHarness(10)
	h.chain.maxSpan = 0
	h.chain.logsErr = domain.ErrRangeTooLarge
	err := h.indexer().Sync(context.Background())
	if !errors.Is(err, domain.ErrRangeTooLarge) {
		t.Fatalf("err = %v, want ErrRangeTooLarge after shrinking to one block", err)
	}
	if last := h.chain.queries[len(h.chain.queries)-1]; last.FromBlock != last.ToBlock {
		t.Fatalf("last query %d-%d, want a single block", last.FromBlock, last.ToBlock)
	}
	if _, _, cp := h.store.snapshot(); cp != nil {
		t.Fatalf("checkpoint moved to %+v on failure", cp)
	}
}

func TestSyncRPCErrorLeavesCheckpoint(t *testing.T) {
	h := newHarness(10)
	h.sync(t)
	h.chain.head = 20
	h.chain.logsErr = errors.New("connection reset")

	if err := h.indexer().Sync(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if _, _, cp := h.store.snapshot(); cp.Block != 10 {
		t.Fatalf("checkpoint = %d, want 10 (unchanged)", cp.Block)
	}

	h.chain.logsErr = nil
	h.sync(t)
	if _, _, cp := h.store.snapshot(); cp.Block != 20 {
		t.Fatalf("after recovery checkpoint = %d, want 20", cp.Block)
	}
}

func TestSyncRewindsOnReorg(t *testing.T) {
	h := newHarness(10)
	h.chain.addLog(approveLog(victim, router, big.NewInt(5), 3, 0xd0), victim) // below the reorg: must survive
	h.chain.addLog(approveLog(victim, scammer, maxU256, 9, 0xd1), victim)      // will be reorged out
	h.sync(t)
	if _, alerts, _ := h.store.snapshot(); len(alerts) != 3 { // router EOA + scammer unlimited + scammer EOA
		t.Fatalf("before reorg: %d alerts, want 3", len(alerts))
	}

	// Blocks 8..10 are replaced; the new fork has a different transfer at block 9.
	h.chain.reorg(8, []domain.Log{transferLog(stranger, victim, big.NewInt(3), 9, 0xd2)})
	h.chain.head = 12
	h.sync(t)

	events, alerts, cp := h.store.snapshot()
	var txs []byte
	for _, e := range events {
		txs = append(txs, e.TxHash[0])
	}
	if !slices.Equal(txs, []byte{0xd0, 0xd2}) {
		t.Fatalf("events after reorg are from txs %x, want d0 d2", txs)
	}
	if len(alerts) != 1 || alerts[0].TxHash[0] != 0xd0 {
		t.Fatalf("alerts after reorg = %v, want only the one below the reorg", alertRules(alerts))
	}
	if cp.Block != 12 || cp.Hash != h.chain.hashAt(12) {
		t.Fatalf("checkpoint = %+v, want block 12 on the new fork", cp)
	}
	if got := testutil.ToFloat64(h.m.Reorgs); got != 1 {
		t.Fatalf("reorgs_total = %v, want 1", got)
	}
	// Rewind went ReorgWindow (4) blocks back from the checkpoint at 10.
	var rewound bool
	for _, q := range h.chain.queries {
		if q.FromBlock == 7 {
			rewound = true
		}
	}
	if !rewound {
		t.Fatal("no query restarted at block 7 (checkpoint 10 - reorg window 4 + 1)")
	}
}

func TestReprocessingIsIdempotent(t *testing.T) {
	h := newHarness(10)
	h.chain.addLog(approveLog(victim, scammer, maxU256, 3, 0xe1), victim)
	h.chain.addLog(transferLog(victim, scammer, big.NewInt(900), 4, 0xe2), scammer)
	h.sync(t)
	events1, alerts1, _ := h.store.snapshot()

	// Move the checkpoint back without deleting anything, as when an operator
	// replays a range or two instances briefly overlap: every row already exists.
	h.store.cp = &domain.Checkpoint{Block: 0, Hash: h.chain.hashAt(0)}
	h.sync(t)
	h.sync(t) // and a no-op sync on top

	events2, alerts2, _ := h.store.snapshot()
	if len(events2) != len(events1) || len(alerts2) != len(alerts1) {
		t.Fatalf("reprocessing changed state: events %d -> %d, alerts %d -> %d",
			len(events1), len(events2), len(alerts1), len(alerts2))
	}
}

func TestRestartResumesFromCheckpoint(t *testing.T) {
	h := newHarness(10)
	h.sync(t)

	// A new process: fresh Indexer, same store, StartBlock no longer relevant.
	h.chain.head = 25
	h.chain.addLog(approveLog(victim, scammer, maxU256, 20, 0xf1), victim)
	h.chain.queries = nil
	h.cfg.StartBlock = nil
	h.sync(t)

	if first := h.chain.queries[0]; first.FromBlock != 11 {
		t.Fatalf("restart queried from block %d, want 11", first.FromBlock)
	}
	if events, _, cp := h.store.snapshot(); len(events) != 1 || cp.Block != 25 {
		t.Fatalf("after restart: %d events, checkpoint %d; want 1 and 25", len(events), cp.Block)
	}
}

func TestFirstRunWithoutStartBlockBeginsAtConfirmedHead(t *testing.T) {
	h := newHarness(100)
	h.cfg.StartBlock = nil
	h.cfg.Confirmations = 2
	h.sync(t)
	if len(h.chain.queries) != 2 || h.chain.queries[0].FromBlock != 98 {
		t.Fatalf("queries = %+v, want one range starting at 98", h.chain.queries)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(5)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.indexer().Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if _, _, cp := h.store.snapshot(); cp == nil || cp.Block != 5 {
		t.Fatalf("Run did not sync: checkpoint %+v", cp)
	}
}
