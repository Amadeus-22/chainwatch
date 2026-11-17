// Package postgres implements app.Store, app.AuthStore and app.OrgStore on
// PostgreSQL with pgx. Every multi-row change runs in one transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/migrations"
)

// Store is a PostgreSQL-backed store.
type Store struct {
	pool *pgxpool.Pool
}

var (
	_ app.Store     = (*Store)(nil)
	_ app.AuthStore = (*Store)(nil)
	_ app.OrgStore  = (*Store)(nil)
)

// Open connects and verifies the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases all connections.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the database is reachable (used by /readyz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// migrationLockID serialises migrations when several replicas start at once.
const migrationLockID = 0x636861696e77 // "chainw"

// Migrate applies pending migrations from the embedded migrations package.
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	slices.Sort(names)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
			return fmt.Errorf("take migration lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		for _, name := range names {
			var applied bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", name).Scan(&applied); err != nil {
				return fmt.Errorf("check migration %s: %w", name, err)
			}
			if applied {
				continue
			}
			body, err := fs.ReadFile(migrations.FS, name)
			if err != nil {
				return fmt.Errorf("read migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
				return fmt.Errorf("record migration %s: %w", name, err)
			}
		}
		return nil
	})
}

// WatchedWallets returns every (organization, address) pair watched on chain.
func (s *Store) WatchedWallets(ctx context.Context, chain domain.ChainID) ([]domain.Wallet, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+walletColumns+` FROM watched_wallets
		WHERE chain_id = $1 ORDER BY address, org_id`, int64(chain))
	if err != nil {
		return nil, fmt.Errorf("select wallets on chain %d: %w", chain, err)
	}
	return pgx.CollectRows(rows, scanWallet)
}

// --- indexing ---

// Checkpoint returns the chain's last committed block.
func (s *Store) Checkpoint(ctx context.Context, chain domain.ChainID) (domain.Checkpoint, bool, error) {
	var cp domain.Checkpoint
	var h []byte
	err := s.pool.QueryRow(ctx, "SELECT block_number, block_hash FROM checkpoints WHERE chain_id = $1", int64(chain)).Scan(&cp.Block, &h)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Checkpoint{}, false, nil
	}
	if err != nil {
		return domain.Checkpoint{}, false, fmt.Errorf("select checkpoint of chain %d: %w", chain, err)
	}
	return cp, true, copyInto(cp.Hash[:], h)
}

const eventColumns = "tx_hash, log_index, block_number, block_hash, kind, token, from_addr, to_addr, amount::text"

// ApprovalsSince returns owner's approvals on token from fromBlock on, in chain order.
func (s *Store) ApprovalsSince(ctx context.Context, chain domain.ChainID, owner, token domain.Address, fromBlock uint64) ([]domain.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+` FROM events
		WHERE chain_id = $1 AND kind = 'approval' AND from_addr = $2 AND token = $3 AND block_number >= $4
		ORDER BY block_number, log_index`, int64(chain), owner[:], token[:], fromBlock)
	if err != nil {
		return nil, fmt.Errorf("select approvals of %s on %s: %w", owner, token, err)
	}
	return pgx.CollectRows(rows, scanEvent)
}

// CommitRange stores a range's events, allowances, alerts and checkpoint atomically.
func (s *Store) CommitRange(ctx context.Context, r app.RangeResult) error {
	chain := int64(r.ChainID)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, e := range r.Events {
			b.Queue(`INSERT INTO events (chain_id, tx_hash, log_index, block_number, block_hash, kind, token, from_addr, to_addr, amount)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text::numeric)
				ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`,
				chain, e.TxHash[:], e.LogIndex, e.BlockNumber, e.BlockHash[:], string(e.Kind), e.Token[:], e.From[:], e.To[:], e.Amount.String())
			if e.Kind == domain.KindApproval {
				b.Queue(`INSERT INTO allowances (chain_id, owner, token, spender, amount, updated_block, updated_log_index)
					VALUES ($1, $2, $3, $4, $5::text::numeric, $6, $7)
					ON CONFLICT (chain_id, owner, token, spender) DO UPDATE
					SET amount = EXCLUDED.amount, updated_block = EXCLUDED.updated_block, updated_log_index = EXCLUDED.updated_log_index
					WHERE (allowances.updated_block, allowances.updated_log_index) <= (EXCLUDED.updated_block, EXCLUDED.updated_log_index)`,
					chain, e.From[:], e.Token[:], e.To[:], e.Amount.String(), e.BlockNumber, e.LogIndex)
			}
		}
		for _, a := range r.Alerts {
			// The organization may have been deleted since the range started:
			// insert only while it exists, instead of failing the whole range.
			b.Queue(`INSERT INTO alerts (org_id, chain_id, wallet, rule, severity, reason, token, tx_hash, log_index, block_number)
				SELECT $1::bigint, $2::bigint, $3::bytea, $4::text, $5::text, $6::text, $7::bytea, $8::bytea, $9::integer, $10::bigint
				WHERE EXISTS (SELECT 1 FROM organizations WHERE id = $1::bigint)
				ON CONFLICT (org_id, chain_id, rule, tx_hash, log_index) DO NOTHING`,
				int64(a.OrgID), int64(a.ChainID), a.Wallet[:], a.Rule, string(a.Severity), a.Reason, a.Token[:], a.TxHash[:], a.LogIndex, a.BlockNumber)
		}
		b.Queue(upsertCheckpoint, chain, r.Checkpoint.Block, r.Checkpoint.Hash[:])
		return tx.SendBatch(ctx, b).Close()
	})
	if err != nil {
		return fmt.Errorf("commit chain %d range up to block %d: %w", r.ChainID, r.Checkpoint.Block, err)
	}
	return nil
}

const upsertCheckpoint = `INSERT INTO checkpoints (chain_id, block_number, block_hash) VALUES ($1, $2, $3)
	ON CONFLICT (chain_id) DO UPDATE SET block_number = EXCLUDED.block_number, block_hash = EXCLUDED.block_hash, updated_at = now()`

// Rewind drops everything derived from blocks above to.Block and moves the
// checkpoint there. Allowances last set above to.Block are rebuilt from the
// latest surviving approval; delivered alerts are kept (they were already sent).
func (s *Store) Rewind(ctx context.Context, chainID domain.ChainID, to domain.Checkpoint) error {
	chain := int64(chainID)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM events WHERE chain_id = $1 AND block_number > $2", chain, to.Block); err != nil {
			return fmt.Errorf("delete events: %w", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM alerts WHERE chain_id = $1 AND block_number > $2 AND status = 'pending'", chain, to.Block); err != nil {
			return fmt.Errorf("delete pending alerts: %w", err)
		}
		rows, err := tx.Query(ctx, "DELETE FROM allowances WHERE chain_id = $1 AND updated_block > $2 RETURNING owner, token, spender", chain, to.Block)
		if err != nil {
			return fmt.Errorf("delete allowances: %w", err)
		}
		var owners, tokens, spenders [][]byte
		for rows.Next() {
			var o, t, sp []byte
			if err := rows.Scan(&o, &t, &sp); err != nil {
				rows.Close()
				return fmt.Errorf("read deleted allowances: %w", err)
			}
			owners, tokens, spenders = append(owners, o), append(tokens, t), append(spenders, sp)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read deleted allowances: %w", err)
		}
		if len(owners) > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO allowances (chain_id, owner, token, spender, amount, updated_block, updated_log_index)
				SELECT DISTINCT ON (e.from_addr, e.token, e.to_addr)
				       e.chain_id, e.from_addr, e.token, e.to_addr, e.amount, e.block_number, e.log_index
				FROM events e
				JOIN unnest($2::bytea[], $3::bytea[], $4::bytea[]) AS k(owner, token, spender)
				  ON e.from_addr = k.owner AND e.token = k.token AND e.to_addr = k.spender
				WHERE e.chain_id = $1 AND e.kind = 'approval'
				ORDER BY e.from_addr, e.token, e.to_addr, e.block_number DESC, e.log_index DESC`,
				chain, owners, tokens, spenders); err != nil {
				return fmt.Errorf("rebuild allowances: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, upsertCheckpoint, chain, to.Block, to.Hash[:]); err != nil {
			return fmt.Errorf("set checkpoint: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("rewind chain %d to block %d: %w", chainID, to.Block, err)
	}
	return nil
}

// --- alerts ---

const alertColumns = `id, org_id, chain_id, wallet, rule, severity, reason, token, tx_hash, log_index, block_number,
	status, attempts, created_at, delivered_at`

// PendingAlerts returns due pending alerts, oldest first.
func (s *Store) PendingAlerts(ctx context.Context, now time.Time, limit int) ([]domain.Alert, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+alertColumns+` FROM alerts
		WHERE status = 'pending' AND next_attempt_at <= $1 ORDER BY id LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select pending alerts: %w", err)
	}
	return pgx.CollectRows(rows, scanAlert)
}

// MarkAlertDelivered records a successful delivery.
func (s *Store) MarkAlertDelivered(ctx context.Context, id int64, at time.Time) error {
	return s.updateAlert(ctx, id, `UPDATE alerts SET status = 'delivered', attempts = attempts + 1,
		delivered_at = $2, last_error = '' WHERE id = $1 AND status = 'pending'`, at)
}

// MarkAlertFailed records a failed attempt and when to retry.
func (s *Store) MarkAlertFailed(ctx context.Context, id int64, next time.Time, reason string) error {
	return s.updateAlert(ctx, id, `UPDATE alerts SET attempts = attempts + 1, next_attempt_at = $2,
		last_error = $3 WHERE id = $1 AND status = 'pending'`, next, reason)
}

func (s *Store) updateAlert(ctx context.Context, id int64, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, append([]any{id}, args...)...)
	if err != nil {
		return fmt.Errorf("update alert %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("pending alert %d: %w", id, domain.ErrNotFound)
	}
	return nil
}

// CountPendingAlerts counts alerts not yet delivered.
func (s *Store) CountPendingAlerts(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM alerts WHERE status = 'pending'").Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending alerts: %w", err)
	}
	return n, nil
}

// --- scanning helpers ---

func scanEvent(r pgx.CollectableRow) (domain.Event, error) {
	var e domain.Event
	var tx, blockHash, token, from, to []byte
	var kind, amount string
	if err := r.Scan(&tx, &e.LogIndex, &e.BlockNumber, &blockHash, &kind, &token, &from, &to, &amount); err != nil {
		return e, err
	}
	e.Kind = domain.EventKind(kind)
	var err error
	if e.Amount, err = parseAmount(amount); err != nil {
		return e, err
	}
	return e, errors.Join(copyInto(e.TxHash[:], tx), copyInto(e.BlockHash[:], blockHash),
		copyInto(e.Token[:], token), copyInto(e.From[:], from), copyInto(e.To[:], to))
}

func scanAlert(r pgx.CollectableRow) (domain.Alert, error) {
	var a domain.Alert
	var wallet, token, tx []byte
	var severity, status string
	var org, chain int64
	if err := r.Scan(&a.ID, &org, &chain, &wallet, &a.Rule, &severity, &a.Reason, &token, &tx, &a.LogIndex, &a.BlockNumber,
		&status, &a.Attempts, &a.CreatedAt, &a.DeliveredAt); err != nil {
		return a, err
	}
	a.OrgID, a.ChainID = domain.OrgID(org), domain.ChainID(chain)
	a.Severity, a.Status = domain.Severity(severity), domain.AlertStatus(status)
	return a, errors.Join(copyInto(a.Wallet[:], wallet), copyInto(a.Token[:], token), copyInto(a.TxHash[:], tx))
}

func copyInto(dst, src []byte) error {
	if len(src) != len(dst) {
		return fmt.Errorf("stored value has %d bytes, want %d", len(src), len(dst))
	}
	copy(dst, src)
	return nil
}

func parseAmount(s string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("stored amount %q is not an integer", s)
	}
	return n, nil
}
