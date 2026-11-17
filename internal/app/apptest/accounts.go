// Package apptest provides an in-memory implementation of app.AuthStore and
// app.OrgStore for tests of the app and httpapi packages. It mirrors the
// Postgres adapter's rules (single-use nonces, last-owner protection, queries
// scoped to the organization's watch list).
package apptest

import (
	"context"
	"sync"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// Accounts is an in-memory AuthStore and OrgStore. Seed chain data through
// SeedAllowances and SeedAlerts; reads are scoped like the SQL.
type Accounts struct {
	mu             sync.Mutex
	nonces         map[string]nonceRow
	sessions       map[[32]byte]domain.Session
	orgs           map[domain.OrgID]domain.Organization
	members        map[domain.OrgID]map[domain.Address]domain.Role
	wallets        []domain.Wallet
	SeedAllowances []domain.Allowance // chain data, shared by every organization
	SeedAlerts     []domain.Alert
	nextOrg        domain.OrgID
}

var (
	_ app.AuthStore = (*Accounts)(nil)
	_ app.OrgStore  = (*Accounts)(nil)
)

type nonceRow struct {
	expires time.Time
	used    bool
}

// NewAccounts returns an empty store.
func NewAccounts() *Accounts {
	return &Accounts{
		nonces:   map[string]nonceRow{},
		sessions: map[[32]byte]domain.Session{},
		orgs:     map[domain.OrgID]domain.Organization{},
		members:  map[domain.OrgID]map[domain.Address]domain.Role{},
	}
}

func (f *Accounts) CreateNonce(_ context.Context, n string, exp time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonces[n] = nonceRow{expires: exp}
	return nil
}

func (f *Accounts) ConsumeNonce(_ context.Context, n string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.nonces[n]
	if !ok || r.used || !now.Before(r.expires) {
		return domain.ErrNotFound
	}
	r.used = true
	f.nonces[n] = r
	return nil
}

func (f *Accounts) CreateSession(_ context.Context, s domain.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.TokenHash] = s
	return nil
}

func (f *Accounts) Session(_ context.Context, h [32]byte, now time.Time) (domain.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[h]
	if !ok || !now.Before(s.ExpiresAt) {
		return domain.Session{}, domain.ErrNotFound
	}
	return s, nil
}

func (f *Accounts) DeleteSession(_ context.Context, h [32]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, h)
	return nil
}

func (f *Accounts) Memberships(_ context.Context, a domain.Address) ([]domain.Membership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Membership
	for id := domain.OrgID(1); id <= f.nextOrg; id++ {
		if r, ok := f.members[id][a]; ok {
			out = append(out, domain.Membership{Org: f.orgs[id], Role: r})
		}
	}
	return out, nil
}

func (f *Accounts) Role(_ context.Context, org domain.OrgID, a domain.Address) (domain.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.members[org][a]
	if !ok {
		return "", domain.ErrNotFound
	}
	return r, nil
}

func (f *Accounts) CreateOrg(_ context.Context, name string, owner domain.Address) (domain.Organization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextOrg++
	o := domain.Organization{ID: f.nextOrg, Name: name}
	f.orgs[o.ID] = o
	f.members[o.ID] = map[domain.Address]domain.Role{owner: domain.RoleOwner}
	return o, nil
}

func (f *Accounts) Members(_ context.Context, org domain.OrgID) ([]domain.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Member
	for a, r := range f.members[org] {
		out = append(out, domain.Member{Address: a, Role: r})
	}
	return out, nil
}

func (f *Accounts) AddMember(_ context.Context, org domain.OrgID, a domain.Address, role domain.Role) (domain.Member, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.members[org][a]; ok {
		return domain.Member{Address: a, Role: r}, false, nil
	}
	f.members[org][a] = role
	return domain.Member{Address: a, Role: role}, true, nil
}

func (f *Accounts) RemoveMember(_ context.Context, org domain.OrgID, a domain.Address) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.members[org][a]
	if !ok {
		return domain.ErrNotFound
	}
	if r == domain.RoleOwner {
		owners := 0
		for _, x := range f.members[org] {
			if x == domain.RoleOwner {
				owners++
			}
		}
		if owners == 1 {
			return domain.ErrConflict
		}
	}
	delete(f.members[org], a)
	return nil
}

func (f *Accounts) AddWallet(_ context.Context, w domain.Wallet) (domain.Wallet, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.wallets {
		if x.OrgID == w.OrgID && x.ChainID == w.ChainID && x.Address == w.Address {
			return x, false, nil
		}
	}
	f.wallets = append(f.wallets, w)
	return w, true, nil
}

func (f *Accounts) OrgWallet(_ context.Context, org domain.OrgID, chain domain.ChainID, a domain.Address) (domain.Wallet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.wallets {
		if x.OrgID == org && x.ChainID == chain && x.Address == a {
			return x, nil
		}
	}
	return domain.Wallet{}, domain.ErrNotFound
}

func (f *Accounts) RemoveWallet(_ context.Context, org domain.OrgID, chain domain.ChainID, a domain.Address) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.wallets {
		if x.OrgID == org && x.ChainID == chain && x.Address == a {
			f.wallets = append(f.wallets[:i], f.wallets[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound
}

func (f *Accounts) OrgWallets(_ context.Context, org domain.OrgID) ([]domain.Wallet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Wallet
	for _, x := range f.wallets {
		if x.OrgID == org {
			out = append(out, x)
		}
	}
	return out, nil
}

func (f *Accounts) watches(org domain.OrgID, chain domain.ChainID, a domain.Address) bool {
	for _, x := range f.wallets {
		if x.OrgID == org && x.ChainID == chain && x.Address == a {
			return true
		}
	}
	return false
}

func (f *Accounts) Allowances(_ context.Context, q app.AllowanceQuery) ([]domain.Allowance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Allowance
	for _, a := range f.SeedAllowances {
		if !f.watches(q.Org, a.ChainID, a.Owner) || (q.ChainID != nil && *q.ChainID != a.ChainID) ||
			(q.Owner != nil && *q.Owner != a.Owner) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *Accounts) Alerts(_ context.Context, q app.AlertQuery) ([]domain.Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Alert
	for _, a := range f.SeedAlerts {
		if a.OrgID == q.Org && (q.Wallet == nil || *q.Wallet == a.Wallet) && (q.ChainID == nil || *q.ChainID == a.ChainID) {
			out = append(out, a)
		}
	}
	return out, nil
}

// SessionCount returns how many sessions are stored.
func (f *Accounts) SessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

// HasSessionKey reports whether a stored session is keyed by exactly b (used to
// check that raw tokens are never stored).
func (f *Accounts) HasSessionKey(b []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for h := range f.sessions {
		if string(h[:]) == string(b) {
			return true
		}
	}
	return false
}
