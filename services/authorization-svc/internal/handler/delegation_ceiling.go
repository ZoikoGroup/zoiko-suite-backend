package handler

import (
	"context"
	"math"
	"math/big"
	"strings"

	"zoiko.io/authorization-svc/internal/domain"
)

// delegationCeilingFinder is the store capability the ceiling layer needs;
// *store.PgStore and *cache.Store provide it.
type delegationCeilingFinder interface {
	FindDelegationCeilings(ctx context.Context, principalID, legalEntityID, tenantID, actionType string) ([]domain.DelegationCeiling, error)
}

// minorUnitExponent mirrors delegated-authority-svc: ISO 4217 minor units for
// the currencies that differ from two.
var minorUnitExponent = map[string]int{
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "UGX": 0, "XAF": 0, "XOF": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3, "LYD": 3, "IQD": 3,
}

// evaluateDelegationCeiling applies the ceiling a DELEGATION places on the
// authority it confers, for a decision granted through that delegation
// (ORG-06 AuthorityLimit). The delegate's own role grants are not touched:
// this runs only when delegation is the basis of the grant.
//
// It used to be absent: delegated-authority-svc published authority_limit_*,
// this service dropped them, and a delegation capped at 500.00 granted any
// amount at decision time.
//
// Among the delegations that confer the action, the most permissive decides:
// one without a ceiling of a kind leaves that kind unconstrained. A capped
// delegation is not exercised blind — no amount (or quantity) against a cap
// is a denial, as is an amount whose currency cannot be converted. The
// conversion uses the platform's reference rates only, never a rate supplied
// in the request, which would let a caller choose its own ceiling.
func (h *Handler) evaluateDelegationCeiling(ctx context.Context, in evalContext, evaluationEntityID string) (denied bool, basis, reason string, err error) {
	finder, ok := h.store.(delegationCeilingFinder)
	if !ok {
		return false, "", "", nil
	}
	ceilings, err := finder.FindDelegationCeilings(ctx, in.PrincipalID, evaluationEntityID, in.TenantID, in.ActionType)
	if err != nil {
		return false, "", "", err
	}
	if len(ceilings) == 0 {
		return false, "", "", nil
	}
	attrs := in.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}

	// ── monetary ──
	monetaryCapped := true
	for _, c := range ceilings {
		if c.LimitMinor == nil || c.LimitCurrency == nil {
			monetaryCapped = false
			break
		}
	}
	if monetaryCapped {
		amountStr := attrs["amount"]
		if amountStr == "" {
			amountStr = attrs["transaction_amount"]
		}
		if amountStr == "" {
			return true, "delegation_limit:amount_required",
				"the delegation conferring this action is capped; the request must state the amount", nil
		}
		amount, ok := new(big.Float).SetString(amountStr)
		if !ok || amount.Sign() < 0 {
			return true, "delegation_limit:invalid_amount", "amount is not a non-negative number: " + amountStr, nil
		}
		reqCurrency := strings.ToUpper(strings.TrimSpace(attrs["currency"]))
		within := false
		for _, c := range ceilings {
			cur := strings.ToUpper(*c.LimitCurrency)
			rc := reqCurrency
			if rc == "" {
				rc = cur
			}
			rate := 1.0
			if rc != cur {
				from, okFrom := StandardReferenceFXRates[rc]
				to, okTo := StandardReferenceFXRates[cur]
				if !okFrom || !okTo || to <= 0 {
					continue
				}
				rate = from / to
			}
			exp, ok := minorUnitExponent[cur]
			if !ok {
				exp = 2
			}
			inMinor := new(big.Float).Mul(amount, big.NewFloat(rate*math.Pow10(exp)))
			if inMinor.Cmp(new(big.Float).SetInt64(*c.LimitMinor)) <= 0 {
				within = true
				break
			}
		}
		if !within {
			return true, "delegation_limit:exceeded",
				"the amount exceeds the ceiling of the delegation conferring this action", nil
		}
	}

	// ── quantity ──
	quantityCapped := true
	for _, c := range ceilings {
		if c.LimitQuantity == nil {
			quantityCapped = false
			break
		}
	}
	if quantityCapped {
		qStr := attrs["quantity"]
		if qStr == "" {
			return true, "delegation_limit:quantity_required",
				"the delegation conferring this action caps quantity; the request must state it", nil
		}
		q, ok := new(big.Float).SetString(qStr)
		if !ok || q.Sign() < 0 {
			return true, "delegation_limit:invalid_quantity", "quantity is not a non-negative number: " + qStr, nil
		}
		within := false
		for _, c := range ceilings {
			if q.Cmp(new(big.Float).SetInt64(*c.LimitQuantity)) <= 0 {
				within = true
				break
			}
		}
		if !within {
			return true, "delegation_limit:quantity_exceeded",
				"the quantity exceeds the ceiling of the delegation conferring this action", nil
		}
	}
	return false, "", "", nil
}
