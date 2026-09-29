package app_test

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// fakeChain is an in-memory chain. Tests mutate it between syncs to simulate new
// blocks, reorgs, node limits and outages.
type fakeChain struct {
	mu       sync.Mutex
	head     uint64
	fork     byte // part of every block hash; bump it to reorg blocks >= reorgFrom
	forkFrom uint64
	logs     []domain.Log
	code     map[domain.Address]bool
	senders  map[domain.Hash]domain.Address

	maxSpan  uint64 // eth_getLogs ranges wider than this fail with ErrRangeTooLarge; 0 = no limit
	logsErr  error  // returned by every Logs call when set
	queries  []app.LogQuery
	tooLarge int
}

func newFakeChain(head uint64) *fakeChain {
	return &fakeChain{head: head, code: map[domain.Address]bool{}, senders: map[domain.Hash]domain.Address{}}
}

func (c *fakeChain) hashAt(n uint64) domain.Hash {
	var h domain.Hash
	if n >= c.forkFrom {
		h[0] = c.fork
	}
	binary.BigEndian.PutUint64(h[24:], n)
	return h
}

// reorg replaces every block from `from` on with a new fork and swaps the logs
// in those blocks for newLogs.
func (c *fakeChain) reorg(from uint64, newLogs []domain.Log) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fork++
	c.forkFrom = from
	kept := c.logs[:0]
	for _, l := range c.logs {
		if l.BlockNumber < from {
			kept = append(kept, l)
		}
	}
	c.logs = kept
	for _, l := range newLogs {
		l.BlockHash = c.hashAt(l.BlockNumber)
		c.logs = append(c.logs, l)
	}
}

func (c *fakeChain) addLog(l domain.Log, sender domain.Address) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l.BlockHash = c.hashAt(l.BlockNumber)
	c.logs = append(c.logs, l)
	c.senders[l.TxHash] = sender
}

func (c *fakeChain) BlockNumber(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head, nil
}

func (c *fakeChain) BlockHash(_ context.Context, n uint64) (domain.Hash, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > c.head {
		return domain.Hash{}, errors.New("block not found")
	}
	return c.hashAt(n), nil
}

func (c *fakeChain) Logs(_ context.Context, q app.LogQuery) ([]domain.Log, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, q)
	if c.logsErr != nil {
		return nil, c.logsErr
	}
	if c.maxSpan > 0 && q.ToBlock-q.FromBlock+1 > c.maxSpan {
		c.tooLarge++
		return nil, domain.ErrRangeTooLarge
	}
	var out []domain.Log
	for _, l := range c.logs {
		if l.BlockNumber >= q.FromBlock && l.BlockNumber <= q.ToBlock && topicsMatch(q.Topics, l.Topics) {
			out = append(out, l)
		}
	}
	return out, nil
}

func topicsMatch(filter [][]domain.Hash, topics []domain.Hash) bool {
	for i, accepted := range filter {
		if accepted == nil {
			continue
		}
		if i >= len(topics) || !slices.Contains(accepted, topics[i]) {
			return false
		}
	}
	return true
}

func (c *fakeChain) HasCode(_ context.Context, a domain.Address) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code[a], nil
}

func (c *fakeChain) TxSender(_ context.Context, tx domain.Hash) (domain.Address, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.senders[tx]
	if !ok {
		return domain.Address{}, errors.New("tx not found")
	}
	return s, nil
}

// fakeStore mirrors the Postgres adapter's uniqueness rules so idempotency can be
// tested without a database.
type fakeStore struct {
	mu      sync.Mutex
	wallets []domain.Wallet
	cp      *domain.Checkpoint
	events  map[eventKey]domain.Event
	alerts  []storedAlert
	commits int
}

type eventKey struct {
	tx  domain.Hash
	idx uint
}

type storedAlert struct {
	domain.Alert
	nextAttempt time.Time
	lastError   string
}

func newFakeStore(wallets ...domain.Address) *fakeStore {
	s := &fakeStore{events: map[eventKey]domain.Event{}}
	for _, w := range wallets {
		s.wallets = append(s.wallets, domain.Wallet{Address: w})
	}
	return s
}

func (s *fakeStore) Wallets(context.Context) ([]domain.Wallet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.wallets), nil
}

func (s *fakeStore) Checkpoint(context.Context) (domain.Checkpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cp == nil {
		return domain.Checkpoint{}, false, nil
	}
	return *s.cp, true, nil
}

func (s *fakeStore) ApprovalsSince(_ context.Context, owner, token domain.Address, from uint64) ([]domain.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Event
	for _, e := range s.sortedEvents() {
		if e.Kind == domain.KindApproval && e.From == owner && e.Token == token && e.BlockNumber >= from {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) CommitRange(_ context.Context, r app.RangeResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	for _, e := range r.Events {
		k := eventKey{e.TxHash, e.LogIndex}
		if _, dup := s.events[k]; !dup {
			s.events[k] = e
		}
	}
	for _, a := range r.Alerts {
		if s.findAlert(a) >= 0 {
			continue
		}
		a.ID = int64(len(s.alerts) + 1)
		s.alerts = append(s.alerts, storedAlert{Alert: a})
	}
	cp := r.Checkpoint
	s.cp = &cp
	return nil
}

func (s *fakeStore) findAlert(a domain.Alert) int {
	for i, x := range s.alerts {
		if x.Rule == a.Rule && x.TxHash == a.TxHash && x.LogIndex == a.LogIndex {
			return i
		}
	}
	return -1
}

func (s *fakeStore) Rewind(_ context.Context, to domain.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.events {
		if e.BlockNumber > to.Block {
			delete(s.events, k)
		}
	}
	kept := s.alerts[:0]
	for _, a := range s.alerts {
		if a.BlockNumber <= to.Block || a.Status == domain.AlertDelivered {
			kept = append(kept, a)
		}
	}
	s.alerts = kept
	s.cp = &to
	return nil
}

func (s *fakeStore) PendingAlerts(_ context.Context, now time.Time, limit int) ([]domain.Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Alert
	for _, a := range s.alerts {
		if a.Status == domain.AlertPending && !a.nextAttempt.After(now) && len(out) < limit {
			out = append(out, a.Alert)
		}
	}
	return out, nil
}

func (s *fakeStore) MarkAlertDelivered(_ context.Context, id int64, at time.Time) error {
	return s.update(id, func(a *storedAlert) {
		a.Status = domain.AlertDelivered
		a.Attempts++
		a.DeliveredAt = &at
	})
}

func (s *fakeStore) MarkAlertFailed(_ context.Context, id int64, next time.Time, reason string) error {
	return s.update(id, func(a *storedAlert) {
		a.Attempts++
		a.nextAttempt = next
		a.lastError = reason
	})
}

func (s *fakeStore) update(id int64, f func(*storedAlert)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.alerts {
		if s.alerts[i].ID == id {
			f(&s.alerts[i])
			return nil
		}
	}
	return domain.ErrNotFound
}

func (s *fakeStore) CountPendingAlerts(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.alerts {
		if a.Status == domain.AlertPending {
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) sortedEvents() []domain.Event {
	out := make([]domain.Event, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b domain.Event) int {
		if a.Before(b) {
			return -1
		}
		if b.Before(a) {
			return 1
		}
		return 0
	})
	return out
}

func (s *fakeStore) snapshot() (events []domain.Event, alerts []storedAlert, cp *domain.Checkpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedEvents(), slices.Clone(s.alerts), s.cp
}
