// Package app holds chainwatch's use cases: the Indexer turns confirmed chain logs
// into events, allowances and alerts; the Alerter delivers alerts. It defines the
// ports it needs and never imports adapters.
package app

import (
	"context"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// LogQuery is an eth_getLogs filter. Topics[i] lists the accepted values for topic
// position i (OR); a nil entry matches anything.
type LogQuery struct {
	FromBlock uint64
	ToBlock   uint64
	Topics    [][]domain.Hash
}

// Chain is read access to an EVM chain through JSON-RPC.
type Chain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	// BlockHash returns the canonical hash at height n.
	BlockHash(ctx context.Context, n uint64) (domain.Hash, error)
	// Logs returns the matching logs. It returns domain.ErrRangeTooLarge when the
	// node refuses the range as too large or too many results.
	Logs(ctx context.Context, q LogQuery) ([]domain.Log, error)
	// HasCode reports whether addr is a contract at the latest block.
	HasCode(ctx context.Context, addr domain.Address) (bool, error)
	// TxSender returns the account that sent the transaction.
	TxSender(ctx context.Context, tx domain.Hash) (domain.Address, error)
}

// RangeResult is everything derived from one block range, committed atomically
// together with the new checkpoint.
type RangeResult struct {
	Checkpoint domain.Checkpoint
	Events     []domain.Event // in chain order
	Alerts     []domain.Alert
}

// Store persists chainwatch's state. Implementations must make CommitRange
// idempotent: events are unique on (tx hash, log index) and alerts on
// (rule, tx hash, log index), so re-running a range changes nothing.
type Store interface {
	Wallets(ctx context.Context) ([]domain.Wallet, error)
	// Checkpoint returns the last committed block; ok is false before the first commit.
	Checkpoint(ctx context.Context) (cp domain.Checkpoint, ok bool, err error)
	// ApprovalsSince returns owner's Approval events on token at or after
	// fromBlock, in chain order.
	ApprovalsSince(ctx context.Context, owner, token domain.Address, fromBlock uint64) ([]domain.Event, error)
	// CommitRange stores events, updates allowances from the approvals among them,
	// inserts new alerts and moves the checkpoint, in one transaction.
	CommitRange(ctx context.Context, r RangeResult) error
	// Rewind deletes events above to.Block, recomputes the allowances they
	// touched, drops still-pending alerts above to.Block, and sets the checkpoint
	// to to, in one transaction.
	Rewind(ctx context.Context, to domain.Checkpoint) error

	// PendingAlerts returns up to limit pending alerts due at or before now, oldest first.
	PendingAlerts(ctx context.Context, now time.Time, limit int) ([]domain.Alert, error)
	MarkAlertDelivered(ctx context.Context, id int64, at time.Time) error
	// MarkAlertFailed records a failed attempt and when to try again.
	MarkAlertFailed(ctx context.Context, id int64, next time.Time, reason string) error
	CountPendingAlerts(ctx context.Context) (int, error)
}

// AlertQuery filters alerts for the API. A zero Wallet or Since means no filter.
type AlertQuery struct {
	Wallet *domain.Address
	Since  time.Time
	Limit  int
}

// Notifier delivers one alert to a human-facing channel. Implementations should
// send alert.DedupKey() so receivers can drop redeliveries.
type Notifier interface {
	Notify(ctx context.Context, alert domain.Alert) error
}
