// Package postgres implements app.Store (and the API's queries) on PostgreSQL
// with pgx. Every multi-row change runs in one transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"slices"
	"strings"
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

var _ app.Store = (*Store)(nil)

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

// --- wallets ---

// AddWallet puts addr on the watch list. created is false if it was already there
// (the existing wallet is returned unchanged).
func (s *Store) AddWallet(ctx context.Context, addr domain.Address, label string) (w domain.Wallet, created bool, err error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO wallets (address, label) VALUES ($1, $2)
		ON CONFLICT (address) DO NOTHING
		RETURNING label, created_at`, addr[:], label)
	w.Address = addr
	err = row.Scan(&w.Label, &w.CreatedAt)
	if err == nil {
		return w, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Wallet{}, false, fmt.Errorf("insert wallet %s: %w", addr, err)
	}
	w, err = s.Wallet(ctx, addr)
	return w, false, err
}

// Wallet returns one watched wallet or domain.ErrNotFound.
func (s *Store) Wallet(ctx context.Context, addr domain.Address) (domain.Wallet, error) {
	w := domain.Wallet{Address: addr}
	err := s.pool.QueryRow(ctx, "SELECT label, created_at FROM wallets WHERE address = $1", addr[:]).Scan(&w.Label, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Wallet{}, fmt.Errorf("wallet %s: %w", addr, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("select wallet %s: %w", addr, err)
	}
	return w, nil
}

// RemoveWallet takes addr off the watch list. Its history is kept.
func (s *Store) RemoveWallet(ctx context.Context, addr domain.Address) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM wallets WHERE address = $1", addr[:])
	if err != nil {
		return fmt.Errorf("delete wallet %s: %w", addr, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("wallet %s: %w", addr, domain.ErrNotFound)
	}
	return nil
}

// Wallets lists the watch list, oldest first.
func (s *Store) Wallets(ctx context.Context) ([]domain.Wallet, error) {
	rows, err := s.pool.Query(ctx, "SELECT address, label, created_at FROM wallets ORDER BY created_at, address")
	if err != nil {
		return nil, fmt.Errorf("select wallets: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Wallet, error) {
		var w domain.Wallet
		var a []byte
		if err := r.Scan(&a, &w.Label, &w.CreatedAt); err != nil {
			return w, err
		}
		return w, copyInto(w.Address[:], a)
	})
}

// --- indexing ---

// Checkpoint returns the last committed block.
func (s *Store) Checkpoint(ctx context.Context) (domain.Checkpoint, bool, error) {
	var cp domain.Checkpoint
	var h []byte
	err := s.pool.QueryRow(ctx, "SELECT block_number, block_hash FROM checkpoint").Scan(&cp.Block, &h)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Checkpoint{}, false, nil
	}
	if err != nil {
		return domain.Checkpoint{}, false, fmt.Errorf("select checkpoint: %w", err)
	}
	return cp, true, copyInto(cp.Hash[:], h)
}

const eventColumns = "tx_hash, log_index, block_number, block_hash, kind, token, from_addr, to_addr, amount::text"

// ApprovalsSince returns owner's approvals on token from fromBlock on, in chain order.
func (s *Store) ApprovalsSince(ctx context.Context, owner, token domain.Address, fromBlock uint64) ([]domain.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+` FROM events
		WHERE kind = 'approval' AND from_addr = $1 AND token = $2 AND block_number >= $3
		ORDER BY block_number, log_index`, owner[:], token[:], fromBlock)
	if err != nil {
		return nil, fmt.Errorf("select approvals of %s on %s: %w", owner, token, err)
	}
	return pgx.CollectRows(rows, scanEvent)
}

// CommitRange stores a range's events, allowances, alerts and checkpoint atomically.
func (s *Store) CommitRange(ctx context.Context, r app.RangeResult) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, e := range r.Events {
			b.Queue(`INSERT INTO events (tx_hash, log_index, block_number, block_hash, kind, token, from_addr, to_addr, amount)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::text::numeric)
				ON CONFLICT (tx_hash, log_index) DO NOTHING`,
				e.TxHash[:], e.LogIndex, e.BlockNumber, e.BlockHash[:], string(e.Kind), e.Token[:], e.From[:], e.To[:], e.Amount.String())
			if e.Kind == domain.KindApproval {
				b.Queue(`INSERT INTO allowances (owner, token, spender, amount, updated_block, updated_log_index)
					VALUES ($1, $2, $3, $4::text::numeric, $5, $6)
					ON CONFLICT (owner, token, spender) DO UPDATE
					SET amount = EXCLUDED.amount, updated_block = EXCLUDED.updated_block, updated_log_index = EXCLUDED.updated_log_index
					WHERE (allowances.updated_block, allowances.updated_log_index) <= (EXCLUDED.updated_block, EXCLUDED.updated_log_index)`,
					e.From[:], e.Token[:], e.To[:], e.Amount.String(), e.BlockNumber, e.LogIndex)
			}
		}
		for _, a := range r.Alerts {
			b.Queue(`INSERT INTO alerts (wallet, rule, severity, reason, token, tx_hash, log_index, block_number)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (rule, tx_hash, log_index) DO NOTHING`,
				a.Wallet[:], a.Rule, string(a.Severity), a.Reason, a.Token[:], a.TxHash[:], a.LogIndex, a.BlockNumber)
		}
		b.Queue(`INSERT INTO checkpoint (block_number, block_hash) VALUES ($1, $2)
			ON CONFLICT (id) DO UPDATE SET block_number = EXCLUDED.block_number, block_hash = EXCLUDED.block_hash, updated_at = now()`,
			r.Checkpoint.Block, r.Checkpoint.Hash[:])
		return tx.SendBatch(ctx, b).Close()
	})
	if err != nil {
		return fmt.Errorf("commit range up to block %d: %w", r.Checkpoint.Block, err)
	}
	return nil
}

// Rewind drops everything derived from blocks above to.Block and moves the
// checkpoint there. Allowances last set above to.Block are rebuilt from the
// latest surviving approval; delivered alerts are kept (they were already sent).
func (s *Store) Rewind(ctx context.Context, to domain.Checkpoint) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM events WHERE block_number > $1", to.Block); err != nil {
			return fmt.Errorf("delete events: %w", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM alerts WHERE block_number > $1 AND status = 'pending'", to.Block); err != nil {
			return fmt.Errorf("delete pending alerts: %w", err)
		}
		rows, err := tx.Query(ctx, "DELETE FROM allowances WHERE updated_block > $1 RETURNING owner, token, spender", to.Block)
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
				INSERT INTO allowances (owner, token, spender, amount, updated_block, updated_log_index)
				SELECT DISTINCT ON (e.from_addr, e.token, e.to_addr)
				       e.from_addr, e.token, e.to_addr, e.amount, e.block_number, e.log_index
				FROM events e
				JOIN unnest($1::bytea[], $2::bytea[], $3::bytea[]) AS k(owner, token, spender)
				  ON e.from_addr = k.owner AND e.token = k.token AND e.to_addr = k.spender
				WHERE e.kind = 'approval'
				ORDER BY e.from_addr, e.token, e.to_addr, e.block_number DESC, e.log_index DESC`,
				owners, tokens, spenders); err != nil {
				return fmt.Errorf("rebuild allowances: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO checkpoint (block_number, block_hash) VALUES ($1, $2)
			ON CONFLICT (id) DO UPDATE SET block_number = EXCLUDED.block_number, block_hash = EXCLUDED.block_hash, updated_at = now()`,
			to.Block, to.Hash[:]); err != nil {
			return fmt.Errorf("set checkpoint: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("rewind to block %d: %w", to.Block, err)
	}
	return nil
}

// Allowances returns owner's non-zero allowances, most recently changed first.
func (s *Store) Allowances(ctx context.Context, owner domain.Address) ([]domain.Allowance, error) {
	rows, err := s.pool.Query(ctx, `SELECT token, spender, amount::text, updated_block FROM allowances
		WHERE owner = $1 AND amount > 0 ORDER BY updated_block DESC, token, spender`, owner[:])
	if err != nil {
		return nil, fmt.Errorf("select allowances of %s: %w", owner, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Allowance, error) {
		a := domain.Allowance{Owner: owner}
		var token, spender []byte
		var amount string
		if err := r.Scan(&token, &spender, &amount, &a.UpdatedBlock); err != nil {
			return a, err
		}
		var err error
		if a.Amount, err = parseAmount(amount); err != nil {
			return a, err
		}
		return a, errors.Join(copyInto(a.Token[:], token), copyInto(a.Spender[:], spender))
	})
}

// --- alerts ---

const alertColumns = `id, wallet, rule, severity, reason, token, tx_hash, log_index, block_number,
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

// Alerts returns alerts matching q, newest first.
func (s *Store) Alerts(ctx context.Context, q app.AlertQuery) ([]domain.Alert, error) {
	var where []string
	var args []any
	if q.Wallet != nil {
		args = append(args, q.Wallet[:])
		where = append(where, fmt.Sprintf("wallet = $%d", len(args)))
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since)
		where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	sql := `SELECT ` + alertColumns + ` FROM alerts`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, q.Limit)
	sql += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("select alerts: %w", err)
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
	if err := r.Scan(&a.ID, &wallet, &a.Rule, &severity, &a.Reason, &token, &tx, &a.LogIndex, &a.BlockNumber,
		&status, &a.Attempts, &a.CreatedAt, &a.DeliveredAt); err != nil {
		return a, err
	}
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
