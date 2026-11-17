package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/erc20"
	"github.com/Amadeus-22/chainwatch/internal/domain/risk"
	"github.com/Amadeus-22/chainwatch/internal/platform/backoff"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

// IndexerConfig tunes the indexer. See the README configuration table.
type IndexerConfig struct {
	ChainID       domain.ChainID
	Confirmations uint64
	ReorgWindow   uint64
	MaxBlockRange uint64
	StartBlock    *uint64 // nil: start at the confirmed head on first run
	PollInterval  time.Duration
	Risk          risk.Params
}

// Indexer follows one chain `Confirmations` blocks behind head and turns ERC-20
// logs that involve wallets watched on that chain into events, allowances and
// alerts. The process runs one Indexer per configured chain.
type Indexer struct {
	chain Chain
	store Store
	cfg   IndexerConfig
	log   *slog.Logger
	m     *metrics.Metrics
	retry backoff.Policy
	label string // chain id as a metric label value

	// span is the current eth_getLogs range size. It halves when the node refuses
	// a range and doubles back towards MaxBlockRange after each success.
	span uint64
}

// NewIndexer wires an Indexer.
func NewIndexer(chain Chain, store Store, cfg IndexerConfig, log *slog.Logger, m *metrics.Metrics) *Indexer {
	return &Indexer{
		chain: chain,
		store: store,
		cfg:   cfg,
		log:   log,
		m:     m,
		retry: backoff.Policy{Base: cfg.PollInterval, Max: 2 * time.Minute},
		span:  cfg.MaxBlockRange,
		label: strconv.FormatUint(uint64(cfg.ChainID), 10),
	}
}

// Run syncs every PollInterval until ctx is cancelled. A failed sync leaves the
// checkpoint where it was and is retried with jittered exponential backoff.
func (ix *Indexer) Run(ctx context.Context) error {
	failures := 0
	for {
		wait := ix.cfg.PollInterval
		if err := ix.Sync(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			wait = ix.retry.Delay(failures)
			ix.log.Error("sync failed", "err", err, "attempt", failures, "retry_in", wait.String())
		} else {
			failures = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Sync indexes every confirmed block not yet committed, one range at a time.
// Each range is committed with its checkpoint, so an interrupted Sync resumes
// where it stopped.
func (ix *Indexer) Sync(ctx context.Context) error {
	head, err := ix.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("get head: %w", err)
	}
	if head < ix.cfg.Confirmations {
		return nil
	}
	safe := head - ix.cfg.Confirmations

	next, err := ix.nextBlock(ctx, safe)
	if err != nil {
		return err
	}
	for next <= safe {
		if err := ctx.Err(); err != nil {
			return err
		}
		to := min(safe, next+ix.span-1)
		done, err := ix.indexRange(ctx, next, to)
		if err != nil {
			return err
		}
		ix.m.IndexedBlock.WithLabelValues(ix.label).Set(float64(done))
		ix.m.HeadLag.WithLabelValues(ix.label).Set(float64(head - done))
		next = done + 1
	}
	return nil
}

// nextBlock returns the first block to index: after the checkpoint, rewinding
// first if the checkpoint block is no longer canonical.
func (ix *Indexer) nextBlock(ctx context.Context, safe uint64) (uint64, error) {
	cp, ok, err := ix.store.Checkpoint(ctx, ix.cfg.ChainID)
	if err != nil {
		return 0, fmt.Errorf("load checkpoint: %w", err)
	}
	if !ok {
		if ix.cfg.StartBlock != nil {
			return *ix.cfg.StartBlock, nil
		}
		return safe, nil
	}
	canonical, err := ix.chain.BlockHash(ctx, cp.Block)
	if err != nil {
		return 0, fmt.Errorf("get hash of checkpoint block %d: %w", cp.Block, err)
	}
	if canonical == cp.Hash {
		return cp.Block + 1, nil
	}

	// The block we last committed was reorged out. Everything derived from the
	// last ReorgWindow blocks is suspect: drop it and index those blocks again.
	target := cp.Block - min(ix.cfg.ReorgWindow, cp.Block)
	targetHash, err := ix.chain.BlockHash(ctx, target)
	if err != nil {
		return 0, fmt.Errorf("get hash of rewind block %d: %w", target, err)
	}
	if err := ix.store.Rewind(ctx, ix.cfg.ChainID, domain.Checkpoint{Block: target, Hash: targetHash}); err != nil {
		return 0, fmt.Errorf("rewind to block %d: %w", target, err)
	}
	ix.m.Reorgs.WithLabelValues(ix.label).Inc()
	ix.log.Warn("reorg detected, rewound",
		"block", cp.Block, "stored_hash", cp.Hash.String(), "canonical_hash", canonical.String(), "rewound_to", target)
	return target + 1, nil
}

// indexRange indexes [from, to] and returns the last block it committed, which is
// below to when the node forced the range to shrink.
func (ix *Indexer) indexRange(ctx context.Context, from, to uint64) (uint64, error) {
	wallets, err := ix.store.WatchedWallets(ctx, ix.cfg.ChainID)
	if err != nil {
		return 0, fmt.Errorf("load wallets: %w", err)
	}
	for {
		// Read the hash of `to` before the logs: if a reorg lands in between, the
		// stored hash is the stale one and the next Sync detects the mismatch. The
		// other order could pair stale logs with a fresh hash and miss the reorg.
		toHash, err := ix.chain.BlockHash(ctx, to)
		if err != nil {
			return 0, fmt.Errorf("get hash of block %d: %w", to, err)
		}
		logs, err := ix.fetchLogs(ctx, from, to, wallets)
		if errors.Is(err, domain.ErrRangeTooLarge) && to > from {
			to = from + (to-from)/2
			ix.span = to - from + 1
			ix.log.Info("log range too large, halving", "from", from, "to", to)
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("get logs %d-%d: %w", from, to, err)
		}
		result, err := ix.derive(ctx, from, logs, wallets)
		if err != nil {
			return 0, fmt.Errorf("evaluate blocks %d-%d: %w", from, to, err)
		}
		result.ChainID = ix.cfg.ChainID
		result.Checkpoint = domain.Checkpoint{Block: to, Hash: toHash}
		if err := ix.store.CommitRange(ctx, result); err != nil {
			return 0, fmt.Errorf("commit blocks %d-%d: %w", from, to, err)
		}
		ix.record(result)
		ix.span = min(ix.span*2, ix.cfg.MaxBlockRange)
		return to, nil
	}
}

// fetchLogs runs two filters: Approval/Transfer with a watched wallet as topic 1
// (owner or sender), and Transfer with a watched wallet as topic 2 (recipient).
func (ix *Indexer) fetchLogs(ctx context.Context, from, to uint64, wallets []domain.Wallet) ([]domain.Log, error) {
	if len(wallets) == 0 {
		return nil, nil
	}
	var walletTopics []domain.Hash
	seenWallet := map[domain.Address]bool{}
	for _, w := range wallets {
		if !seenWallet[w.Address] { // several organizations may watch one address
			seenWallet[w.Address] = true
			walletTopics = append(walletTopics, erc20.AddressTopic(w.Address))
		}
	}
	queries := []LogQuery{
		{FromBlock: from, ToBlock: to, Topics: [][]domain.Hash{{erc20.TopicApproval, erc20.TopicTransfer}, walletTopics}},
		{FromBlock: from, ToBlock: to, Topics: [][]domain.Hash{{erc20.TopicTransfer}, nil, walletTopics}},
	}
	type key struct {
		tx  domain.Hash
		idx uint
	}
	seen := map[key]bool{}
	var out []domain.Log
	for _, q := range queries {
		logs, err := ix.chain.Logs(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, l := range logs {
			k := key{l.TxHash, l.LogIndex}
			if l.Removed || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, l)
		}
	}
	slices.SortFunc(out, func(a, b domain.Log) int {
		if c := cmp.Compare(a.BlockNumber, b.BlockNumber); c != 0 {
			return c
		}
		return cmp.Compare(a.LogIndex, b.LogIndex)
	})
	return out, nil
}

// derive decodes logs and runs the risk rules on the events of watched wallets.
// Each finding becomes one alert per organization that watches the wallet.
func (ix *Indexer) derive(ctx context.Context, from uint64, logs []domain.Log, wallets []domain.Wallet) (RangeResult, error) {
	watched := make(map[domain.Address][]domain.OrgID, len(wallets))
	for _, w := range wallets {
		watched[w.Address] = append(watched[w.Address], w.OrgID)
	}
	ev := newEvaluator(ix, from)

	var r RangeResult
	for _, l := range logs {
		e, err := erc20.Decode(l)
		if errors.Is(err, erc20.ErrNotERC20) {
			continue // e.g. an ERC-721 Transfer, which shares the topic hash
		}
		if err != nil {
			ix.log.Warn("skipping malformed log", "err", err, "tx", l.TxHash.String(), "log_index", l.LogIndex)
			continue
		}
		r.Events = append(r.Events, e)

		var wallet domain.Address
		switch {
		case e.Kind == domain.KindApproval && len(watched[e.From]) > 0:
			wallet = e.From
		case e.Kind == domain.KindTransfer && len(watched[e.From]) > 0:
			wallet = e.From
		default:
			continue // inflows are recorded but carry no outflow risk
		}
		findings, err := ev.evaluate(ctx, wallet, e, r.Events)
		if err != nil {
			return RangeResult{}, err
		}
		for _, f := range findings {
			for _, org := range watched[wallet] {
				r.Alerts = append(r.Alerts, domain.NewAlert(org, ix.cfg.ChainID, wallet, e, f))
			}
		}
	}
	return r, nil
}

// record updates metrics after a successful commit, so a retried range is not
// counted twice by the process that failed it.
func (ix *Indexer) record(r RangeResult) {
	for _, e := range r.Events {
		ix.m.Events.WithLabelValues(ix.label, string(e.Kind)).Inc()
	}
	for _, a := range r.Alerts {
		ix.m.Findings.WithLabelValues(ix.label, a.Rule).Inc()
	}
}

// evaluator gathers the I/O-dependent inputs of the pure risk rules for one
// range, caching chain lookups.
type evaluator struct {
	ix        *Indexer
	from      uint64
	hasCode   map[domain.Address]bool
	senders   map[domain.Hash]domain.Address
	approvals map[[2]domain.Address][]domain.Event // (owner, token) -> stored approvals
}

func newEvaluator(ix *Indexer, from uint64) *evaluator {
	return &evaluator{
		ix:        ix,
		from:      from,
		hasCode:   map[domain.Address]bool{},
		senders:   map[domain.Hash]domain.Address{},
		approvals: map[[2]domain.Address][]domain.Event{},
	}
}

func (v *evaluator) evaluate(ctx context.Context, wallet domain.Address, e domain.Event, batch []domain.Event) ([]domain.Finding, error) {
	in := risk.Input{Wallet: wallet, Event: e}
	switch e.Kind {
	case domain.KindApproval:
		if e.Amount.Sign() > 0 { // revocations need no code lookup
			isContract, err := v.spenderIsContract(ctx, e.To)
			if err != nil {
				return nil, err
			}
			in.SpenderIsContract = isContract
		}
	case domain.KindTransfer:
		if e.To == wallet {
			return nil, nil
		}
		approvals, err := v.recentApprovals(ctx, wallet, e, batch)
		if err != nil {
			return nil, err
		}
		if len(approvals) > 0 {
			sender, err := v.txSender(ctx, e.TxHash)
			if err != nil {
				return nil, err
			}
			in.RecentApprovals, in.TxSender = approvals, &sender
		}
	}
	return risk.Evaluate(v.ix.cfg.Risk, in), nil
}

func (v *evaluator) spenderIsContract(ctx context.Context, a domain.Address) (bool, error) {
	if c, ok := v.hasCode[a]; ok {
		return c, nil
	}
	c, err := v.ix.chain.HasCode(ctx, a)
	if err != nil {
		return false, fmt.Errorf("get code of %s: %w", a, err)
	}
	v.hasCode[a] = c
	return c, nil
}

func (v *evaluator) txSender(ctx context.Context, tx domain.Hash) (domain.Address, error) {
	if s, ok := v.senders[tx]; ok {
		return s, nil
	}
	s, err := v.ix.chain.TxSender(ctx, tx)
	if err != nil {
		return domain.Address{}, fmt.Errorf("get sender of %s: %w", tx, err)
	}
	v.senders[tx] = s
	return s, nil
}

// recentApprovals returns wallet's approvals on e.Token that precede e within the
// drain window: committed ones from the store plus earlier ones from this range.
func (v *evaluator) recentApprovals(ctx context.Context, wallet domain.Address, e domain.Event, batch []domain.Event) ([]domain.Event, error) {
	window := v.ix.cfg.Risk.DrainWindow
	key := [2]domain.Address{wallet, e.Token}
	stored, ok := v.approvals[key]
	if !ok {
		since := v.from - min(window, v.from)
		var err error
		stored, err = v.ix.store.ApprovalsSince(ctx, v.ix.cfg.ChainID, wallet, e.Token, since)
		if err != nil {
			return nil, fmt.Errorf("load approvals of %s on %s: %w", wallet, e.Token, err)
		}
		v.approvals[key] = stored
	}
	var out []domain.Event
	for _, a := range stored {
		if a.BlockNumber < v.from { // the batch is authoritative from v.from on
			out = append(out, a)
		}
	}
	for _, a := range batch {
		if a.Kind == domain.KindApproval && a.From == wallet && a.Token == e.Token && a.Before(e) {
			out = append(out, a)
		}
	}
	return out, nil
}
