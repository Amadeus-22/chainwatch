package risk_test

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/risk"
)

var (
	wallet  = domain.Address{19: 0xaa}
	token   = domain.Address{19: 0x70}
	other   = domain.Address{19: 0x71}
	spender = domain.Address{19: 0x5e}
	thief   = domain.Address{19: 0xee}
	maxU256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func approval(owner, to domain.Address, amount *big.Int, block uint64) domain.Event {
	return domain.Event{Kind: domain.KindApproval, Token: token, From: owner, To: to, Amount: amount, BlockNumber: block}
}

func transfer(from, to domain.Address, amount *big.Int, block uint64) domain.Event {
	return domain.Event{Kind: domain.KindTransfer, Token: token, From: from, To: to, Amount: amount, BlockNumber: block, LogIndex: 1}
}

func ptr(a domain.Address) *domain.Address { return &a }

func rules(fs []domain.Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func TestUnlimitedApproval(t *testing.T) {
	tests := []struct {
		name string
		ev   domain.Event
		want bool
	}{
		{"max uint256", approval(wallet, spender, maxU256, 1), true},
		{"exactly 2^255", approval(wallet, spender, risk.UnlimitedThreshold, 1), true},
		{"2^255 - 1", approval(wallet, spender, new(big.Int).Sub(risk.UnlimitedThreshold, big.NewInt(1)), 1), false},
		{"small amount", approval(wallet, spender, big.NewInt(100), 1), false},
		{"approval by someone else", approval(other, wallet, maxU256, 1), false},
		{"transfer", transfer(wallet, spender, maxU256, 1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := risk.UnlimitedApproval(risk.Params{}, risk.Input{Wallet: wallet, Event: tt.ev})
			if (len(got) == 1) != tt.want {
				t.Fatalf("findings = %v, want flagged=%v", got, tt.want)
			}
			if tt.want && (got[0].Rule != risk.RuleUnlimitedApproval || got[0].Severity != domain.SeverityHigh) {
				t.Fatalf("unexpected finding %+v", got[0])
			}
		})
	}
}

func TestApprovalToEOA(t *testing.T) {
	tests := []struct {
		name       string
		ev         domain.Event
		isContract bool
		want       bool
	}{
		{"to EOA", approval(wallet, spender, big.NewInt(1), 1), false, true},
		{"to contract", approval(wallet, spender, big.NewInt(1), 1), true, false},
		{"revocation to EOA", approval(wallet, spender, big.NewInt(0), 1), false, false},
		{"approval by someone else", approval(other, spender, big.NewInt(1), 1), false, false},
		{"transfer to EOA", transfer(wallet, spender, big.NewInt(1), 1), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := risk.ApprovalToEOA(risk.Params{}, risk.Input{Wallet: wallet, Event: tt.ev, SpenderIsContract: tt.isContract})
			if (len(got) == 1) != tt.want {
				t.Fatalf("findings = %v, want flagged=%v", got, tt.want)
			}
		})
	}
}

func TestLargeOutflow(t *testing.T) {
	p := risk.Params{LargeOutflow: map[domain.Address]*big.Int{token: big.NewInt(1000)}}
	otherToken := transfer(wallet, thief, big.NewInt(1_000_000), 1)
	otherToken.Token = other
	tests := []struct {
		name string
		ev   domain.Event
		want bool
	}{
		{"above threshold", transfer(wallet, thief, big.NewInt(1001), 1), true},
		{"at threshold", transfer(wallet, thief, big.NewInt(1000), 1), false},
		{"below threshold", transfer(wallet, thief, big.NewInt(999), 1), false},
		{"inflow", transfer(thief, wallet, big.NewInt(5000), 1), false},
		{"self transfer", transfer(wallet, wallet, big.NewInt(5000), 1), false},
		{"token without threshold", otherToken, false},
		{"approval", approval(wallet, thief, big.NewInt(5000), 1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := risk.LargeOutflow(p, risk.Input{Wallet: wallet, Event: tt.ev})
			if (len(got) == 1) != tt.want {
				t.Fatalf("findings = %v, want flagged=%v", got, tt.want)
			}
			if tt.want && got[0].Severity != domain.SeverityMedium {
				t.Fatalf("severity = %s", got[0].Severity)
			}
		})
	}
}

func TestDrainPattern(t *testing.T) {
	p := risk.Params{DrainWindow: 10}
	approved := approval(wallet, spender, maxU256, 100)
	revoked := approval(wallet, spender, big.NewInt(0), 101)
	otherTokenApproval := approved
	otherTokenApproval.Token = other

	tests := []struct {
		name      string
		ev        domain.Event
		approvals []domain.Event
		sender    *domain.Address
		want      bool
	}{
		{"spender pulls to itself", transfer(wallet, spender, big.NewInt(5), 105), []domain.Event{approved}, ptr(spender), true},
		{"spender pulls to a third address", transfer(wallet, thief, big.NewInt(5), 105), []domain.Event{approved}, ptr(spender), true},
		{"sent to spender, sender unknown", transfer(wallet, spender, big.NewInt(5), 105), []domain.Event{approved}, nil, true},
		{"same block, later log", transfer(wallet, spender, big.NewInt(5), 100), []domain.Event{approved}, ptr(spender), true},
		{"at the window edge", transfer(wallet, spender, big.NewInt(5), 110), []domain.Event{approved}, ptr(spender), true},
		{"outside the window", transfer(wallet, spender, big.NewInt(5), 111), []domain.Event{approved}, ptr(spender), false},
		{"wallet sent it itself", transfer(wallet, spender, big.NewInt(5), 105), []domain.Event{approved}, ptr(wallet), false},
		{"unrelated sender and recipient", transfer(wallet, thief, big.NewInt(5), 105), []domain.Event{approved}, ptr(thief), false},
		{"approval was revoked", transfer(wallet, spender, big.NewInt(5), 105), []domain.Event{approved, revoked}, ptr(spender), false},
		{"approval on another token", transfer(wallet, spender, big.NewInt(5), 105), []domain.Event{otherTokenApproval}, ptr(spender), false},
		{"no approvals", transfer(wallet, spender, big.NewInt(5), 105), nil, ptr(spender), false},
		{"inflow", transfer(spender, wallet, big.NewInt(5), 105), []domain.Event{approved}, ptr(spender), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := risk.DrainPattern(p, risk.Input{Wallet: wallet, Event: tt.ev, RecentApprovals: tt.approvals, TxSender: tt.sender})
			if (len(got) == 1) != tt.want {
				t.Fatalf("findings = %v, want flagged=%v", got, tt.want)
			}
			if tt.want && got[0].Severity != domain.SeverityCritical {
				t.Fatalf("severity = %s", got[0].Severity)
			}
		})
	}
}

func TestEvaluateDrainScenario(t *testing.T) {
	p := risk.Params{DrainWindow: 10, LargeOutflow: map[domain.Address]*big.Int{token: big.NewInt(100)}}
	approve := approval(wallet, thief, maxU256, 2)

	got := rules(risk.Evaluate(p, risk.Input{Wallet: wallet, Event: approve, SpenderIsContract: false}))
	if want := []string{risk.RuleUnlimitedApproval, risk.RuleApprovalToEOA}; !reflect.DeepEqual(got, want) {
		t.Errorf("approval: rules = %v, want %v", got, want)
	}

	drain := transfer(wallet, thief, big.NewInt(1000), 3)
	got = rules(risk.Evaluate(p, risk.Input{Wallet: wallet, Event: drain, RecentApprovals: []domain.Event{approve}, TxSender: ptr(thief)}))
	if want := []string{risk.RuleLargeOutflow, risk.RuleDrainPattern}; !reflect.DeepEqual(got, want) {
		t.Errorf("drain: rules = %v, want %v", got, want)
	}

	benign := approval(wallet, spender, big.NewInt(50), 4)
	if got := risk.Evaluate(p, risk.Input{Wallet: wallet, Event: benign, SpenderIsContract: true}); len(got) != 0 {
		t.Errorf("benign approval: findings = %v, want none", got)
	}
}

func TestAllowanceRisk(t *testing.T) {
	for _, tt := range []struct {
		amount *big.Int
		want   domain.Severity
	}{
		{big.NewInt(1), domain.SeverityMedium},
		{new(big.Int).Sub(risk.UnlimitedThreshold, big.NewInt(1)), domain.SeverityMedium},
		{risk.UnlimitedThreshold, domain.SeverityHigh},
		{maxU256, domain.SeverityHigh},
	} {
		if got := risk.AllowanceRisk(tt.amount); got != tt.want {
			t.Errorf("AllowanceRisk(%s) = %s, want %s", tt.amount, got, tt.want)
		}
	}
}
