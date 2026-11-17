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

// RangeResult is everything derived from one block range of one chain, committed
// atomically together with the new checkpoint.
type RangeResult struct {
	ChainID    domain.ChainID
	Checkpoint domain.Checkpoint
	Events     []domain.Event // in chain order
	Alerts     []domain.Alert
}

// Store persists the indexer's and alerter's state. Everything chain-derived is
// keyed by chain id. Implementations must make CommitRange idempotent: events are
// unique on (chain, tx hash, log index) and alerts on (org, chain, rule, tx hash,
// log index), so re-running a range changes nothing.
type Store interface {
	// WatchedWallets returns every (organization, address) pair watched on chain.
	// An address watched by two organizations appears twice.
	WatchedWallets(ctx context.Context, chain domain.ChainID) ([]domain.Wallet, error)
	// Checkpoint returns the chain's last committed block; ok is false before the
	// first commit.
	Checkpoint(ctx context.Context, chain domain.ChainID) (cp domain.Checkpoint, ok bool, err error)
	// ApprovalsSince returns owner's Approval events on token at or after
	// fromBlock, in chain order.
	ApprovalsSince(ctx context.Context, chain domain.ChainID, owner, token domain.Address, fromBlock uint64) ([]domain.Event, error)
	// CommitRange stores events, updates allowances from the approvals among them,
	// inserts new alerts and moves the chain's checkpoint, in one transaction.
	CommitRange(ctx context.Context, r RangeResult) error
	// Rewind deletes the chain's events above to.Block, recomputes the allowances
	// they touched, drops the chain's still-pending alerts above to.Block, and sets
	// its checkpoint to to, in one transaction.
	Rewind(ctx context.Context, chain domain.ChainID, to domain.Checkpoint) error

	// PendingAlerts returns up to limit pending alerts due at or before now, oldest first.
	PendingAlerts(ctx context.Context, now time.Time, limit int) ([]domain.Alert, error)
	MarkAlertDelivered(ctx context.Context, id int64, at time.Time) error
	// MarkAlertFailed records a failed attempt and when to try again.
	MarkAlertFailed(ctx context.Context, id int64, next time.Time, reason string) error
	CountPendingAlerts(ctx context.Context) (int, error)
}

// AuthStore persists sign-in nonces and sessions.
type AuthStore interface {
	CreateNonce(ctx context.Context, nonce string, expiresAt time.Time) error
	// ConsumeNonce marks nonce used. It returns domain.ErrNotFound when the nonce
	// is unknown, already used or expired at now; two concurrent calls with the
	// same nonce must not both succeed.
	ConsumeNonce(ctx context.Context, nonce string, now time.Time) error
	CreateSession(ctx context.Context, s domain.Session) error
	// Session returns the unexpired session with that token hash or domain.ErrNotFound.
	Session(ctx context.Context, tokenHash [32]byte, now time.Time) (domain.Session, error)
	DeleteSession(ctx context.Context, tokenHash [32]byte) error
}

// OrgStore persists organizations, memberships and each organization's watch
// list, and answers the organization-scoped read queries. It does not check
// permissions: Workspace does, before every call.
type OrgStore interface {
	Memberships(ctx context.Context, account domain.Address) ([]domain.Membership, error)
	// Role returns account's role in org, or domain.ErrNotFound if it is not a member.
	Role(ctx context.Context, org domain.OrgID, account domain.Address) (domain.Role, error)
	// CreateOrg creates an organization with owner as its first owner.
	CreateOrg(ctx context.Context, name string, owner domain.Address) (domain.Organization, error)
	Members(ctx context.Context, org domain.OrgID) ([]domain.Member, error)
	// AddMember adds account with role; created is false (and the existing member
	// is returned unchanged) if it already is a member.
	AddMember(ctx context.Context, org domain.OrgID, account domain.Address, role domain.Role) (m domain.Member, created bool, err error)
	// RemoveMember returns domain.ErrNotFound if account is not a member and
	// domain.ErrConflict if it is the organization's last owner.
	RemoveMember(ctx context.Context, org domain.OrgID, account domain.Address) error

	// AddWallet puts w on its organization's watch list; created is false (and the
	// existing wallet is returned unchanged) if it was already there.
	AddWallet(ctx context.Context, w domain.Wallet) (domain.Wallet, bool, error)
	// OrgWallet returns one watched wallet or domain.ErrNotFound.
	OrgWallet(ctx context.Context, org domain.OrgID, chain domain.ChainID, addr domain.Address) (domain.Wallet, error)
	// RemoveWallet returns domain.ErrNotFound if the wallet is not watched by org.
	RemoveWallet(ctx context.Context, org domain.OrgID, chain domain.ChainID, addr domain.Address) error
	OrgWallets(ctx context.Context, org domain.OrgID) ([]domain.Wallet, error)
	// Allowances returns the non-zero allowances of the wallets org watches.
	Allowances(ctx context.Context, q AllowanceQuery) ([]domain.Allowance, error)
	// Alerts returns org's alerts matching q, newest first.
	Alerts(ctx context.Context, q AlertQuery) ([]domain.Alert, error)
}

// AllowanceQuery selects allowances of the wallets Org watches. Nil fields mean
// no filter.
type AllowanceQuery struct {
	Org     domain.OrgID
	ChainID *domain.ChainID
	Owner   *domain.Address
}

// AlertQuery selects Org's alerts. Nil fields and a zero Since mean no filter.
type AlertQuery struct {
	Org     domain.OrgID
	ChainID *domain.ChainID
	Wallet  *domain.Address
	Since   time.Time
	Limit   int
}

// Notifier delivers one alert to a human-facing channel. Implementations should
// send alert.DedupKey() so receivers can drop redeliveries.
type Notifier interface {
	Notify(ctx context.Context, alert domain.Alert) error
}
