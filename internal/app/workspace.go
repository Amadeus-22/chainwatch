package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// MaxLabelLen bounds wallet labels (bytes).
const MaxLabelLen = 200

// Workspace is the organization-scoped API use case. Every method takes the
// signed-in principal and checks its membership (and role, where needed) before
// touching data; the store is never asked for another organization's rows.
type Workspace struct {
	store  OrgStore
	chains []domain.Chain
}

// NewWorkspace wires a Workspace for the configured chains.
func NewWorkspace(store OrgStore, chains []domain.Chain) *Workspace {
	return &Workspace{store: store, chains: chains}
}

// Chains lists the configured networks.
func (w *Workspace) Chains() []domain.Chain { return slices.Clone(w.chains) }

func (w *Workspace) chainKnown(id domain.ChainID) bool {
	return slices.ContainsFunc(w.chains, func(c domain.Chain) bool { return c.ID == id })
}

// authorize returns ErrForbidden unless p is a member of org with at least need.
func (w *Workspace) authorize(ctx context.Context, p domain.Principal, org domain.OrgID, need domain.Role) error {
	role, err := w.store.Role(ctx, org, p.Address)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("%w: %s is not a member of organization %d", domain.ErrForbidden, p.Address, org)
		}
		return fmt.Errorf("load role: %w", err)
	}
	if need == domain.RoleOwner && role != domain.RoleOwner {
		return fmt.Errorf("%w: only owners of organization %d may do this", domain.ErrForbidden, org)
	}
	return nil
}

// Memberships lists the organizations p belongs to.
func (w *Workspace) Memberships(ctx context.Context, p domain.Principal) ([]domain.Membership, error) {
	return w.store.Memberships(ctx, p.Address)
}

// CreateOrganization creates an organization owned by p.
func (w *Workspace) CreateOrganization(ctx context.Context, p domain.Principal, name string) (domain.Organization, error) {
	name, err := domain.ValidateOrgName(name)
	if err != nil {
		return domain.Organization{}, err
	}
	return w.store.CreateOrg(ctx, name, p.Address)
}

// Members lists org's members. Any member may read it.
func (w *Workspace) Members(ctx context.Context, p domain.Principal, org domain.OrgID) ([]domain.Member, error) {
	if err := w.authorize(ctx, p, org, domain.RoleMember); err != nil {
		return nil, err
	}
	return w.store.Members(ctx, org)
}

// AddMember adds an account to org. Owners only.
func (w *Workspace) AddMember(ctx context.Context, p domain.Principal, org domain.OrgID, account domain.Address, role domain.Role) (domain.Member, bool, error) {
	if err := w.authorize(ctx, p, org, domain.RoleOwner); err != nil {
		return domain.Member{}, false, err
	}
	return w.store.AddMember(ctx, org, account, role)
}

// RemoveMember removes an account from org. Owners may remove anyone; a member
// may remove only itself (leave). The last owner cannot be removed.
func (w *Workspace) RemoveMember(ctx context.Context, p domain.Principal, org domain.OrgID, account domain.Address) error {
	need := domain.RoleOwner
	if account == p.Address {
		need = domain.RoleMember
	}
	if err := w.authorize(ctx, p, org, need); err != nil {
		return err
	}
	return w.store.RemoveMember(ctx, org, account)
}

// AddWallet watches (chain, addr) for org.
func (w *Workspace) AddWallet(ctx context.Context, p domain.Principal, org domain.OrgID, chain domain.ChainID, addr domain.Address, label string) (domain.Wallet, bool, error) {
	if err := w.authorize(ctx, p, org, domain.RoleMember); err != nil {
		return domain.Wallet{}, false, err
	}
	if !w.chainKnown(chain) {
		return domain.Wallet{}, false, fmt.Errorf("%w: chain id %d is not configured", domain.ErrInvalidInput, chain)
	}
	if len(label) > MaxLabelLen {
		return domain.Wallet{}, false, fmt.Errorf("%w: label must be at most %d bytes", domain.ErrInvalidInput, MaxLabelLen)
	}
	return w.store.AddWallet(ctx, domain.Wallet{OrgID: org, ChainID: chain, Address: addr, Label: label})
}

// RemoveWallet stops watching (chain, addr) for org. Indexed history is kept.
func (w *Workspace) RemoveWallet(ctx context.Context, p domain.Principal, org domain.OrgID, chain domain.ChainID, addr domain.Address) error {
	if err := w.authorize(ctx, p, org, domain.RoleMember); err != nil {
		return err
	}
	return w.store.RemoveWallet(ctx, org, chain, addr)
}

// Wallets lists org's watch list.
func (w *Workspace) Wallets(ctx context.Context, p domain.Principal, org domain.OrgID) ([]domain.Wallet, error) {
	if err := w.authorize(ctx, p, org, domain.RoleMember); err != nil {
		return nil, err
	}
	return w.store.OrgWallets(ctx, org)
}

// Allowances returns the open allowances of org's wallets. Filtering by a wallet
// that org does not watch is ErrNotFound, even if another organization watches
// it: allowances are shared chain data, access to them is not.
func (w *Workspace) Allowances(ctx context.Context, p domain.Principal, q AllowanceQuery) ([]domain.Allowance, error) {
	if err := w.authorize(ctx, p, q.Org, domain.RoleMember); err != nil {
		return nil, err
	}
	if q.Owner != nil && q.ChainID != nil {
		if _, err := w.store.OrgWallet(ctx, q.Org, *q.ChainID, *q.Owner); err != nil {
			return nil, err
		}
	}
	return w.store.Allowances(ctx, q)
}

// Alerts returns org's alerts.
func (w *Workspace) Alerts(ctx context.Context, p domain.Principal, q AlertQuery) ([]domain.Alert, error) {
	if err := w.authorize(ctx, p, q.Org, domain.RoleMember); err != nil {
		return nil, err
	}
	return w.store.Alerts(ctx, q)
}
