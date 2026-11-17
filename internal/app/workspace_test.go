package app_test

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/app/apptest"
	"github.com/Amadeus-22/chainwatch/internal/domain"
)

var (
	alice     = domain.Principal{Address: domain.Address{19: 0xa1}} // owner of org A
	bob       = domain.Principal{Address: domain.Address{19: 0xb0}} // owner of org B
	carol     = domain.Principal{Address: domain.Address{19: 0xc0}} // member of org A
	treasuryA = domain.Address{19: 0xaa}
	treasuryB = domain.Address{19: 0xbb}
)

type tenants struct {
	ws    *app.Workspace
	store *apptest.Accounts
	a, b  domain.OrgID
}

// newTenants sets up two organizations, each watching its own treasury on
// Ethereum, with allowances and alerts for both.
func newTenants(t *testing.T) *tenants {
	t.Helper()
	ctx := context.Background()
	store := apptest.NewAccounts()
	ws := app.NewWorkspace(store, []domain.Chain{{ID: ethereum, Name: "Ethereum"}, {ID: base, Name: "Base"}})
	a, _ := ws.CreateOrganization(ctx, alice, "Fund A")
	b, _ := ws.CreateOrganization(ctx, bob, "DAO B")
	if _, _, err := ws.AddMember(ctx, alice, a.ID, carol.Address, domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		p   domain.Principal
		org domain.OrgID
		w   domain.Address
	}{{alice, a.ID, treasuryA}, {bob, b.ID, treasuryB}} {
		if _, _, err := ws.AddWallet(ctx, x.p, x.org, ethereum, x.w, "treasury"); err != nil {
			t.Fatal(err)
		}
	}
	store.SeedAllowances = []domain.Allowance{allowance(ethereum, treasuryA), allowance(ethereum, treasuryB)}
	store.SeedAlerts = []domain.Alert{
		{ID: 1, OrgID: a.ID, ChainID: ethereum, Wallet: treasuryA, Rule: "unlimited_approval"},
		{ID: 2, OrgID: b.ID, ChainID: ethereum, Wallet: treasuryB, Rule: "drain_pattern"},
	}
	return &tenants{ws: ws, store: store, a: a.ID, b: b.ID}
}

func TestOrgCannotSeeAnotherOrgsData(t *testing.T) {
	tn := newTenants(t)
	ctx := context.Background()
	chain := ethereum

	// Alice (org A) asking for org B's data is refused, whatever the endpoint.
	checks := map[string]error{}
	_, checks["wallets"] = tn.ws.Wallets(ctx, alice, tn.b)
	_, checks["allowances"] = tn.ws.Allowances(ctx, alice, app.AllowanceQuery{Org: tn.b})
	_, checks["alerts"] = tn.ws.Alerts(ctx, alice, app.AlertQuery{Org: tn.b})
	_, checks["members"] = tn.ws.Members(ctx, alice, tn.b)
	_, _, checks["add wallet"] = tn.ws.AddWallet(ctx, alice, tn.b, ethereum, treasuryA, "")
	checks["remove wallet"] = tn.ws.RemoveWallet(ctx, alice, tn.b, ethereum, treasuryB)
	_, _, checks["add member"] = tn.ws.AddMember(ctx, alice, tn.b, alice.Address, domain.RoleOwner)
	checks["remove member"] = tn.ws.RemoveMember(ctx, alice, tn.b, bob.Address)
	for name, err := range checks {
		if !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s of org B by org A: err = %v, want ErrForbidden", name, err)
		}
	}
	if _, err := tn.ws.Wallets(ctx, bob, tn.b); err != nil {
		t.Fatal(err)
	}
	if ws, _ := tn.store.OrgWallets(ctx, tn.b); len(ws) != 1 {
		t.Fatalf("org B's watch list changed: %+v", ws)
	}

	// Within her own org, Alice sees only org A's rows.
	wallets, err := tn.ws.Wallets(ctx, alice, tn.a)
	if err != nil || len(wallets) != 1 || wallets[0].Address != treasuryA {
		t.Fatalf("org A wallets = %+v, %v", wallets, err)
	}
	allowances, err := tn.ws.Allowances(ctx, alice, app.AllowanceQuery{Org: tn.a})
	if err != nil || len(allowances) != 1 || allowances[0].Owner != treasuryA {
		t.Fatalf("org A allowances = %+v, %v", allowances, err)
	}
	alerts, err := tn.ws.Alerts(ctx, alice, app.AlertQuery{Org: tn.a})
	if err != nil || len(alerts) != 1 || alerts[0].ID != 1 {
		t.Fatalf("org A alerts = %+v, %v", alerts, err)
	}

	// Filtering her own org's allowances by org B's treasury does not leak it.
	owner := treasuryB
	if _, err := tn.ws.Allowances(ctx, alice, app.AllowanceQuery{Org: tn.a, ChainID: &chain, Owner: &owner}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("allowances of a wallet org A does not watch: %v, want ErrNotFound", err)
	}
	if got, _ := tn.ws.Allowances(ctx, alice, app.AllowanceQuery{Org: tn.a, Owner: &owner}); len(got) != 0 {
		t.Fatalf("owner filter without chain leaked %+v", got)
	}
	wallet := treasuryB
	if got, _ := tn.ws.Alerts(ctx, alice, app.AlertQuery{Org: tn.a, Wallet: &wallet}); len(got) != 0 {
		t.Fatalf("alert filter by org B's wallet leaked %+v", got)
	}
}

func TestSameWalletInTwoOrgsIsIndependent(t *testing.T) {
	tn := newTenants(t)
	ctx := context.Background()
	if _, created, err := tn.ws.AddWallet(ctx, bob, tn.b, ethereum, treasuryA, "counterparty"); err != nil || !created {
		t.Fatalf("org B watching treasury A: created=%v err=%v", created, err)
	}
	if err := tn.ws.RemoveWallet(ctx, bob, tn.b, ethereum, treasuryA); err != nil {
		t.Fatal(err)
	}
	if ws, _ := tn.ws.Wallets(ctx, alice, tn.a); len(ws) != 1 {
		t.Fatalf("org B's removal affected org A: %+v", ws)
	}
}

func TestRoles(t *testing.T) {
	tn := newTenants(t)
	ctx := context.Background()
	dave := domain.Address{19: 0xd0}

	// Members manage the watch list but not membership.
	if _, _, err := tn.ws.AddWallet(ctx, carol, tn.a, base, treasuryA, ""); err != nil {
		t.Fatalf("member adding a wallet: %v", err)
	}
	if _, _, err := tn.ws.AddMember(ctx, carol, tn.a, dave, domain.RoleMember); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member adding a member: %v, want ErrForbidden", err)
	}
	if err := tn.ws.RemoveMember(ctx, carol, tn.a, alice.Address); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member removing the owner: %v, want ErrForbidden", err)
	}
	// A member may leave.
	if err := tn.ws.RemoveMember(ctx, carol, tn.a, carol.Address); err != nil {
		t.Fatalf("member leaving: %v", err)
	}
	if _, err := tn.ws.Wallets(ctx, carol, tn.a); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("former member reading wallets: %v, want ErrForbidden", err)
	}
	// The last owner cannot leave.
	if err := tn.ws.RemoveMember(ctx, alice, tn.a, alice.Address); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("last owner leaving: %v, want ErrConflict", err)
	}
}

func TestWorkspaceValidation(t *testing.T) {
	tn := newTenants(t)
	ctx := context.Background()
	if _, _, err := tn.ws.AddWallet(ctx, alice, tn.a, 10, treasuryA, ""); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("unconfigured chain: %v", err)
	}
	long := make([]byte, app.MaxLabelLen+1)
	if _, _, err := tn.ws.AddWallet(ctx, alice, tn.a, ethereum, treasuryA, string(long)); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("long label: %v", err)
	}
	if _, err := tn.ws.CreateOrganization(ctx, alice, "   "); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("blank org name: %v", err)
	}
	ms, err := tn.ws.Memberships(ctx, carol)
	if err != nil || len(ms) != 1 || ms[0].Org.ID != tn.a || ms[0].Role != domain.RoleMember {
		t.Fatalf("carol's memberships = %+v, %v", ms, err)
	}
}

func allowance(chain domain.ChainID, owner domain.Address) domain.Allowance {
	return domain.Allowance{ChainID: chain, Owner: owner, Token: token, Spender: scammer, Amount: big.NewInt(1)}
}
