package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// --- nonces and sessions ---

// CreateNonce stores a sign-in nonce and purges expired ones, which keeps the
// unauthenticated nonce endpoint from growing the table without bound.
func (s *Store) CreateNonce(ctx context.Context, nonce string, expiresAt time.Time) error {
	b := &pgx.Batch{}
	b.Queue("DELETE FROM siwe_nonces WHERE expires_at < now() - interval '1 hour'")
	b.Queue("INSERT INTO siwe_nonces (nonce, expires_at) VALUES ($1, $2)", nonce, expiresAt)
	if err := s.pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("insert nonce: %w", err)
	}
	return nil
}

// ConsumeNonce marks nonce used in one conditional UPDATE, so of two concurrent
// sign-ins with the same nonce exactly one succeeds.
func (s *Store) ConsumeNonce(ctx context.Context, nonce string, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE siwe_nonces SET used_at = $2
		WHERE nonce = $1 AND used_at IS NULL AND expires_at > $2`, nonce, now)
	if err != nil {
		return fmt.Errorf("consume nonce: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("nonce: %w", domain.ErrNotFound)
	}
	return nil
}

// CreateSession stores a session and purges expired ones.
func (s *Store) CreateSession(ctx context.Context, sess domain.Session) error {
	b := &pgx.Batch{}
	b.Queue("DELETE FROM sessions WHERE expires_at < now()")
	b.Queue("INSERT INTO sessions (token_hash, account, expires_at) VALUES ($1, $2, $3)",
		sess.TokenHash[:], sess.Address[:], sess.ExpiresAt)
	if err := s.pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// Session returns the unexpired session with that token hash.
func (s *Store) Session(ctx context.Context, tokenHash [32]byte, now time.Time) (domain.Session, error) {
	sess := domain.Session{TokenHash: tokenHash}
	var account []byte
	err := s.pool.QueryRow(ctx, "SELECT account, expires_at FROM sessions WHERE token_hash = $1 AND expires_at > $2",
		tokenHash[:], now).Scan(&account, &sess.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, fmt.Errorf("session: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.Session{}, fmt.Errorf("select session: %w", err)
	}
	return sess, copyInto(sess.Address[:], account)
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash [32]byte) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1", tokenHash[:]); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// --- organizations and members ---

// Memberships lists account's organizations, oldest first.
func (s *Store) Memberships(ctx context.Context, account domain.Address) ([]domain.Membership, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.id, o.name, o.created_at, m.role FROM memberships m
		JOIN organizations o ON o.id = m.org_id WHERE m.account = $1 ORDER BY o.id`, account[:])
	if err != nil {
		return nil, fmt.Errorf("select memberships of %s: %w", account, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Membership, error) {
		var m domain.Membership
		var id int64
		var role string
		err := r.Scan(&id, &m.Org.Name, &m.Org.CreatedAt, &role)
		m.Org.ID, m.Role = domain.OrgID(id), domain.Role(role)
		return m, err
	})
}

// Role returns account's role in org.
func (s *Store) Role(ctx context.Context, org domain.OrgID, account domain.Address) (domain.Role, error) {
	var role string
	err := s.pool.QueryRow(ctx, "SELECT role FROM memberships WHERE org_id = $1 AND account = $2",
		int64(org), account[:]).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("membership of %s in organization %d: %w", account, org, domain.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("select role: %w", err)
	}
	return domain.Role(role), nil
}

// CreateOrg creates an organization and its first owner in one transaction.
func (s *Store) CreateOrg(ctx context.Context, name string, owner domain.Address) (domain.Organization, error) {
	o := domain.Organization{Name: name}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, "INSERT INTO organizations (name) VALUES ($1) RETURNING id, created_at",
			name).Scan(&id, &o.CreatedAt); err != nil {
			return err
		}
		o.ID = domain.OrgID(id)
		_, err := tx.Exec(ctx, "INSERT INTO memberships (org_id, account, role) VALUES ($1, $2, 'owner')", id, owner[:])
		return err
	})
	if err != nil {
		return domain.Organization{}, fmt.Errorf("create organization %q: %w", name, err)
	}
	return o, nil
}

// Members lists org's members, owners first.
func (s *Store) Members(ctx context.Context, org domain.OrgID) ([]domain.Member, error) {
	rows, err := s.pool.Query(ctx, `SELECT account, role, created_at FROM memberships WHERE org_id = $1
		ORDER BY role = 'owner' DESC, created_at, account`, int64(org))
	if err != nil {
		return nil, fmt.Errorf("select members of organization %d: %w", org, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Member, error) {
		var m domain.Member
		var account []byte
		var role string
		if err := r.Scan(&account, &role, &m.CreatedAt); err != nil {
			return m, err
		}
		m.Role = domain.Role(role)
		return m, copyInto(m.Address[:], account)
	})
}

// AddMember adds account to org unless it already is a member.
func (s *Store) AddMember(ctx context.Context, org domain.OrgID, account domain.Address, role domain.Role) (domain.Member, bool, error) {
	m := domain.Member{Address: account}
	var r string
	err := s.pool.QueryRow(ctx, `INSERT INTO memberships (org_id, account, role) VALUES ($1, $2, $3)
		ON CONFLICT (org_id, account) DO NOTHING RETURNING role, created_at`,
		int64(org), account[:], string(role)).Scan(&r, &m.CreatedAt)
	if err == nil {
		m.Role = domain.Role(r)
		return m, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Member{}, false, fmt.Errorf("insert member %s into organization %d: %w", account, org, err)
	}
	err = s.pool.QueryRow(ctx, "SELECT role, created_at FROM memberships WHERE org_id = $1 AND account = $2",
		int64(org), account[:]).Scan(&r, &m.CreatedAt)
	if err != nil {
		return domain.Member{}, false, fmt.Errorf("select member %s of organization %d: %w", account, org, err)
	}
	m.Role = domain.Role(r)
	return m, false, nil
}

// RemoveMember deletes a membership, refusing to remove the last owner. The
// organization's owner rows are locked first so two owners removing each other
// concurrently cannot leave it ownerless.
func (s *Store) RemoveMember(ctx context.Context, org domain.OrgID, account domain.Address) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT account FROM memberships WHERE org_id = $1 AND role = 'owner' FOR UPDATE", int64(org))
		if err != nil {
			return fmt.Errorf("lock owners of organization %d: %w", org, err)
		}
		owners, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return fmt.Errorf("lock owners of organization %d: %w", org, err)
		}
		var role string
		err = tx.QueryRow(ctx, "DELETE FROM memberships WHERE org_id = $1 AND account = $2 RETURNING role",
			int64(org), account[:]).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("member %s of organization %d: %w", account, org, domain.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("delete member %s of organization %d: %w", account, org, err)
		}
		if role == string(domain.RoleOwner) && len(owners) <= 1 {
			return fmt.Errorf("%w: %s is the last owner of organization %d", domain.ErrConflict, account, org)
		}
		return nil
	})
}

// --- watch list ---

const walletColumns = "org_id, chain_id, address, label, created_at"

func scanWallet(r pgx.CollectableRow) (domain.Wallet, error) {
	var w domain.Wallet
	var org, chain int64
	var a []byte
	if err := r.Scan(&org, &chain, &a, &w.Label, &w.CreatedAt); err != nil {
		return w, err
	}
	w.OrgID, w.ChainID = domain.OrgID(org), domain.ChainID(chain)
	return w, copyInto(w.Address[:], a)
}

// AddWallet puts w on its organization's watch list unless it is already there.
func (s *Store) AddWallet(ctx context.Context, w domain.Wallet) (domain.Wallet, bool, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO watched_wallets (org_id, chain_id, address, label) VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id, chain_id, address) DO NOTHING RETURNING created_at`,
		int64(w.OrgID), int64(w.ChainID), w.Address[:], w.Label).Scan(&w.CreatedAt)
	if err == nil {
		return w, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Wallet{}, false, fmt.Errorf("insert wallet %s on chain %d: %w", w.Address, w.ChainID, err)
	}
	existing, err := s.OrgWallet(ctx, w.OrgID, w.ChainID, w.Address)
	return existing, false, err
}

// OrgWallet returns one wallet of org's watch list.
func (s *Store) OrgWallet(ctx context.Context, org domain.OrgID, chain domain.ChainID, addr domain.Address) (domain.Wallet, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+walletColumns+` FROM watched_wallets
		WHERE org_id = $1 AND chain_id = $2 AND address = $3`, int64(org), int64(chain), addr[:])
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("select wallet %s: %w", addr, err)
	}
	w, err := pgx.CollectExactlyOneRow(rows, scanWallet)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Wallet{}, fmt.Errorf("wallet %s on chain %d: %w", addr, chain, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("select wallet %s: %w", addr, err)
	}
	return w, nil
}

// RemoveWallet takes a wallet off org's watch list. Chain history is kept.
func (s *Store) RemoveWallet(ctx context.Context, org domain.OrgID, chain domain.ChainID, addr domain.Address) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM watched_wallets WHERE org_id = $1 AND chain_id = $2 AND address = $3",
		int64(org), int64(chain), addr[:])
	if err != nil {
		return fmt.Errorf("delete wallet %s: %w", addr, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("wallet %s on chain %d: %w", addr, chain, domain.ErrNotFound)
	}
	return nil
}

// OrgWallets lists org's watch list, oldest first.
func (s *Store) OrgWallets(ctx context.Context, org domain.OrgID) ([]domain.Wallet, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+walletColumns+` FROM watched_wallets
		WHERE org_id = $1 ORDER BY created_at, chain_id, address`, int64(org))
	if err != nil {
		return nil, fmt.Errorf("select wallets of organization %d: %w", org, err)
	}
	return pgx.CollectRows(rows, scanWallet)
}

// --- organization-scoped reads ---

// Allowances returns the non-zero allowances of the wallets q.Org watches, most
// recently changed first. The join on watched_wallets is the tenant boundary.
func (s *Store) Allowances(ctx context.Context, q app.AllowanceQuery) ([]domain.Allowance, error) {
	args := []any{int64(q.Org)}
	where := []string{"w.org_id = $1", "a.amount > 0"}
	if q.ChainID != nil {
		args = append(args, int64(*q.ChainID))
		where = append(where, fmt.Sprintf("a.chain_id = $%d", len(args)))
	}
	if q.Owner != nil {
		args = append(args, q.Owner[:])
		where = append(where, fmt.Sprintf("a.owner = $%d", len(args)))
	}
	rows, err := s.pool.Query(ctx, `SELECT a.chain_id, a.owner, a.token, a.spender, a.amount::text, a.updated_block
		FROM allowances a JOIN watched_wallets w ON w.chain_id = a.chain_id AND w.address = a.owner
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY a.updated_block DESC, a.chain_id, a.owner, a.token, a.spender`, args...)
	if err != nil {
		return nil, fmt.Errorf("select allowances of organization %d: %w", q.Org, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Allowance, error) {
		var a domain.Allowance
		var chain int64
		var owner, token, spender []byte
		var amount string
		if err := r.Scan(&chain, &owner, &token, &spender, &amount, &a.UpdatedBlock); err != nil {
			return a, err
		}
		a.ChainID = domain.ChainID(chain)
		var err error
		if a.Amount, err = parseAmount(amount); err != nil {
			return a, err
		}
		return a, errors.Join(copyInto(a.Owner[:], owner), copyInto(a.Token[:], token), copyInto(a.Spender[:], spender))
	})
}

// Alerts returns q.Org's alerts, newest first.
func (s *Store) Alerts(ctx context.Context, q app.AlertQuery) ([]domain.Alert, error) {
	args := []any{int64(q.Org)}
	where := []string{"org_id = $1"}
	if q.ChainID != nil {
		args = append(args, int64(*q.ChainID))
		where = append(where, fmt.Sprintf("chain_id = $%d", len(args)))
	}
	if q.Wallet != nil {
		args = append(args, q.Wallet[:])
		where = append(where, fmt.Sprintf("wallet = $%d", len(args)))
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since)
		where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	args = append(args, q.Limit)
	rows, err := s.pool.Query(ctx, `SELECT `+alertColumns+` FROM alerts WHERE `+strings.Join(where, " AND ")+
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("select alerts of organization %d: %w", q.Org, err)
	}
	return pgx.CollectRows(rows, scanAlert)
}
