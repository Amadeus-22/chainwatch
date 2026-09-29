// Package risk holds chainwatch's risk rules. Rules are pure functions of an Input:
// everything that needs I/O (spender code, transaction sender, earlier approvals) is
// looked up by the caller and passed in.
package risk

import (
	"fmt"
	"math/big"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// Rule names, as stored on alerts and used as the `rule` metric label.
const (
	RuleUnlimitedApproval = "unlimited_approval"
	RuleApprovalToEOA     = "approval_to_eoa"
	RuleLargeOutflow      = "large_outflow"
	RuleDrainPattern      = "drain_pattern"
)

// UnlimitedThreshold is 2^255. Wallets and dapps write "unlimited" as 2^256-1, but
// some use other huge values; anything at or above half the uint256 range is
// treated as unlimited.
var UnlimitedThreshold = new(big.Int).Lsh(big.NewInt(1), 255)

// Params configures the rules that need thresholds.
type Params struct {
	// LargeOutflow maps a token to the amount (in base units) above which an
	// outflow is flagged. Tokens without an entry are not checked: a threshold is
	// meaningless without knowing the token's decimals and price.
	LargeOutflow map[domain.Address]*big.Int
	// DrainWindow is how many blocks after an approval an outflow to or by that
	// spender counts as a drain.
	DrainWindow uint64
}

// Input is one event seen from the point of view of one watched wallet.
type Input struct {
	Wallet domain.Address
	Event  domain.Event

	// SpenderIsContract is set for approvals: whether the spender has code.
	SpenderIsContract bool

	// RecentApprovals is set for outflows: the wallet's approvals on the same token
	// within the drain window before Event, in chain order.
	RecentApprovals []domain.Event
	// TxSender is set for outflows when RecentApprovals is non-empty: the account
	// that sent the transaction containing Event (for transferFrom, the spender).
	TxSender *domain.Address
}

// Evaluate runs every rule and returns the findings in rule order.
func Evaluate(p Params, in Input) []domain.Finding {
	var out []domain.Finding
	for _, rule := range []func(Params, Input) []domain.Finding{
		UnlimitedApproval, ApprovalToEOA, LargeOutflow, DrainPattern,
	} {
		out = append(out, rule(p, in)...)
	}
	return out
}

func isApprovalBy(in Input) bool {
	return in.Event.Kind == domain.KindApproval && in.Event.From == in.Wallet
}

func isOutflow(in Input) bool {
	return in.Event.Kind == domain.KindTransfer && in.Event.From == in.Wallet && in.Event.To != in.Wallet
}

// UnlimitedApproval flags an approval of at least 2^255: the spender can move any
// balance the wallet will ever hold of that token.
func UnlimitedApproval(_ Params, in Input) []domain.Finding {
	if !isApprovalBy(in) || in.Event.Amount.Cmp(UnlimitedThreshold) < 0 {
		return nil
	}
	return []domain.Finding{{
		Rule:     RuleUnlimitedApproval,
		Severity: domain.SeverityHigh,
		Reason: fmt.Sprintf("wallet %s granted unlimited allowance on token %s to %s",
			in.Wallet, in.Event.Token, in.Event.To),
	}}
}

// ApprovalToEOA flags a non-zero approval to an address without code. Legitimate
// spenders are almost always contracts (routers, vaults); an approval to an
// externally owned account is the typical drainer pattern.
func ApprovalToEOA(_ Params, in Input) []domain.Finding {
	if !isApprovalBy(in) || in.Event.Amount.Sign() == 0 || in.SpenderIsContract {
		return nil
	}
	return []domain.Finding{{
		Rule:     RuleApprovalToEOA,
		Severity: domain.SeverityHigh,
		Reason: fmt.Sprintf("wallet %s approved %s of token %s to %s, which has no code (externally owned account)",
			in.Wallet, in.Event.Amount, in.Event.Token, in.Event.To),
	}}
}

// LargeOutflow flags a transfer out of the wallet above the token's threshold.
func LargeOutflow(p Params, in Input) []domain.Finding {
	if !isOutflow(in) {
		return nil
	}
	limit, ok := p.LargeOutflow[in.Event.Token]
	if !ok || in.Event.Amount.Cmp(limit) <= 0 {
		return nil
	}
	return []domain.Finding{{
		Rule:     RuleLargeOutflow,
		Severity: domain.SeverityMedium,
		Reason: fmt.Sprintf("%s of token %s left wallet %s to %s (threshold %s)",
			in.Event.Amount, in.Event.Token, in.Wallet, in.Event.To, limit),
	}}
}

// DrainPattern flags an outflow moved by, or sent to, a spender the wallet approved
// on the same token within the last DrainWindow blocks. Only the latest approval to
// each spender counts, so a revocation (approve 0) clears it. A transfer the wallet
// sent itself is never a drain.
func DrainPattern(p Params, in Input) []domain.Finding {
	if !isOutflow(in) || len(in.RecentApprovals) == 0 {
		return nil
	}
	if in.TxSender != nil && *in.TxSender == in.Wallet {
		return nil
	}
	latest := make(map[domain.Address]domain.Event)
	var order []domain.Address
	for _, a := range in.RecentApprovals {
		if a.Kind != domain.KindApproval || a.From != in.Wallet || a.Token != in.Event.Token || !a.Before(in.Event) {
			continue
		}
		if in.Event.BlockNumber-a.BlockNumber > p.DrainWindow {
			continue
		}
		if _, seen := latest[a.To]; !seen {
			order = append(order, a.To)
		}
		latest[a.To] = a
	}
	for _, spender := range order {
		a := latest[spender]
		if a.Amount.Sign() == 0 {
			continue
		}
		movedBySpender := in.TxSender != nil && *in.TxSender == spender
		if !movedBySpender && in.Event.To != spender {
			continue
		}
		return []domain.Finding{{
			Rule:     RuleDrainPattern,
			Severity: domain.SeverityCritical,
			Reason: fmt.Sprintf("%s of token %s left wallet %s at block %d, moved by or sent to %s, which the wallet approved at block %d (tx %s)",
				in.Event.Amount, in.Event.Token, in.Wallet, in.Event.BlockNumber, spender, a.BlockNumber, a.TxHash),
		}}
	}
	return nil
}
