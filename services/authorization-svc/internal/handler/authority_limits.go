package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
)

// StandardReferenceFXRates provides deterministic fallback conversion rates
// between major platform currencies (ZS-IAM-001 §12, Scenario A09).
// Rates represent value relative to GBP base (1 unit of currency = X GBP).
var StandardReferenceFXRates = map[string]float64{
	"GBP": 1.0,
	"USD": 0.78,    // 1 USD = 0.78 GBP
	"EUR": 0.85,    // 1 EUR = 0.85 GBP
	"CAD": 0.58,    // 1 CAD = 0.58 GBP
	"AUD": 0.51,    // 1 AUD = 0.51 GBP
	"JPY": 0.0052,  // 1 JPY = 0.0052 GBP
	"CHF": 0.89,    // 1 CHF = 0.89 GBP
}

// evaluateAuthorityLimits evaluates monetary limits and currency conversion for the request (Phase 3.6 & 3.7, Scenario A09).
func (h *Handler) evaluateAuthorityLimits(ctx context.Context, in evalContext, evaluationEntityID string) (denied bool, basis, reason string, reasonCodes []string, negativeControls []string, err error) {
	amountStr := ""
	if in.Attributes != nil {
		amountStr = in.Attributes["amount"]
		if amountStr == "" {
			amountStr = in.Attributes["transaction_amount"]
		}
	}

	isMonetaryAction := amountStr != "" ||
		(in.Attributes != nil && (in.Attributes["authority_limit_required"] == "true" || in.Attributes["authority_type"] != "")) ||
		isReleaseAction(in.ActionType) ||
		isApprovalAction(in.ActionType)

	if !isMonetaryAction && amountStr == "" {
		return false, "", "", nil, nil, nil
	}

	// 1. Retrieve applicable authority limits from store
	limits, err := h.store.ListAuthorityLimits(ctx, in.TenantID, in.PrincipalID, "", "")
	if err != nil {
		return false, "", "", nil, nil, fmt.Errorf("list authority limits: %w", err)
	}

	// If no principal-specific limits found, look for general tenant limits
	if len(limits) == 0 {
		generalLimits, gErr := h.store.ListAuthorityLimits(ctx, in.TenantID, "", "", "")
		if gErr == nil && len(generalLimits) > 0 {
			limits = generalLimits
		}
	}

	// 2. Filter applicable limits matching scope, validity, and authority type
	applicable := filterApplicableLimits(limits, in, evaluationEntityID)

	if len(applicable) == 0 {
		if in.Attributes != nil && in.Attributes["authority_limit_required"] == "true" {
			return true, "authority_limit:no_limit_configured",
				"No valid authority limit configured for principal in this scope",
				[]string{"NO_AUTHORITY_LIMIT_CONFIGURED"},
				[]string{"authority_limit:no_limit_configured"}, nil
		}
		// If limit is not explicitly required and no limit is defined, proceed
		return false, "", "", nil, nil, nil
	}

	// Select the most specific limit
	limit := selectEffectiveLimit(applicable)

	// If no amount was provided but an authority limit was found and required
	if amountStr == "" {
		if in.Attributes != nil && in.Attributes["authority_limit_required"] == "true" {
			return true, "authority_limit:amount_required",
				"Transaction amount required for authority limit evaluation",
				[]string{"TRANSACTION_AMOUNT_REQUIRED"},
				[]string{"authority_limit:amount_required"}, nil
		}
		return false, "", "", nil, nil, nil
	}

	// 3. Parse requested amount
	reqAmount, ok := new(big.Float).SetString(amountStr)
	if !ok {
		return true, "authority_limit:invalid_amount_format",
			"Transaction amount format invalid: " + amountStr,
			[]string{"INVALID_AMOUNT_FORMAT"},
			[]string{"authority_limit:invalid_amount_format"}, nil
	}

	// 4. Currency and FX conversion handling (Phase 3.7, Scenario A09)
	reqCurrency := "GBP"
	if in.Attributes != nil && in.Attributes["currency"] != "" {
		reqCurrency = strings.ToUpper(in.Attributes["currency"])
	}
	limitCurrency := strings.ToUpper(limit.Currency)
	if limitCurrency == "" {
		limitCurrency = "GBP"
	}

	convertedAmount := new(big.Float).Copy(reqAmount)
	currenciesDiffer := reqCurrency != limitCurrency
	fxConversionApplied := false

	if currenciesDiffer {
		rate, rateFound := resolveFXRate(reqCurrency, limitCurrency, in.Attributes)
		if !rateFound {
			// Fail-closed when no deterministic FX conversion basis exists
			return true, "authority_limit:currency_conversion_missing",
				fmt.Sprintf("Currency mismatch between transaction (%s) and limit (%s) with no deterministic FX conversion basis", reqCurrency, limitCurrency),
				[]string{"CURRENCY_CONVERSION_UNAVAILABLE"},
				[]string{"authority_limit:currency_conversion_missing"}, nil
		}

		convertedAmount.Mul(convertedAmount, big.NewFloat(rate))
		fxConversionApplied = true
	}

	// 5. Upper limit evaluation
	upperLimit, ok := new(big.Float).SetString(limit.UpperLimit)
	if ok && upperLimit.Cmp(big.NewFloat(0)) > 0 {
		if convertedAmount.Cmp(upperLimit) > 0 {
			if fxConversionApplied {
				// Scenario A09: Authority limit exceeded after FX conversion
				return true, "authority_limit:exceeded:after_fx_conversion",
					"Authority limit exceeded after FX conversion (Scenario A09)",
					[]string{"AUTHORITY_LIMIT_EXCEEDED", "FX_CONVERSION_LIMIT_EXCEEDED"},
					[]string{"authority_limit:exceeded:after_fx_conversion"}, nil
			}
			return true, "authority_limit:exceeded:upper_limit",
				fmt.Sprintf("Requested amount %s %s exceeds authority upper limit %s %s", amountStr, reqCurrency, limit.UpperLimit, limit.Currency),
				[]string{"AUTHORITY_LIMIT_EXCEEDED"},
				[]string{"authority_limit:exceeded:upper_limit"}, nil
		}
	}

	// 6. Lower limit evaluation
	lowerLimit, ok := new(big.Float).SetString(limit.LowerLimit)
	if ok && lowerLimit.Cmp(big.NewFloat(0)) > 0 {
		if convertedAmount.Cmp(lowerLimit) < 0 {
			return true, "authority_limit:below_lower_limit",
				fmt.Sprintf("Requested amount %s %s is below authority lower limit %s %s", amountStr, reqCurrency, limit.LowerLimit, limit.Currency),
				[]string{"AUTHORITY_LIMIT_BELOW_MINIMUM"},
				[]string{"authority_limit:below_lower_limit"}, nil
		}
	}

	return false, "", "", nil, nil, nil
}

// evaluateQuorum evaluates quorum and dual-approval requirements (Phase 3.8).
func (h *Handler) evaluateQuorum(in evalContext) (denied bool, basis, reason string, reasonCodes []string, negativeControls []string) {
	if in.Attributes == nil {
		return false, "", "", nil, nil
	}

	isQuorumRequired := in.Attributes["dual_control_required"] == "true" ||
		in.Attributes["quorum_required"] == "true" ||
		in.Attributes["required_approvals"] != ""

	if !isQuorumRequired {
		return false, "", "", nil, nil
	}

	requiredApprovals := 2
	if reqStr := in.Attributes["required_approvals"]; reqStr != "" {
		if n, err := strconv.Atoi(reqStr); err == nil && n > 0 {
			requiredApprovals = n
		}
	}

	approvers := parseApproversList(in.Attributes)

	// Filter and deduplicate approvers
	preparerID := in.Attributes["preparer_id"]
	requestorID := in.Attributes["requestor_id"]

	distinctApprovers := make(map[string]bool)
	for _, appID := range approvers {
		appID = strings.TrimSpace(appID)
		if appID == "" {
			continue
		}
		// Maker cannot be checker: preparer and requestor cannot count as independent approver
		if (preparerID != "" && appID == preparerID) || (requestorID != "" && appID == requestorID) {
			continue
		}
		distinctApprovers[appID] = true
	}

	if len(distinctApprovers) < requiredApprovals {
		basis = fmt.Sprintf("quorum:insufficient_approvals:have=%d:required=%d", len(distinctApprovers), requiredApprovals)
		reason = fmt.Sprintf("Quorum requirement not satisfied: %d of %d required approvals", len(distinctApprovers), requiredApprovals)
		return true, basis, reason,
			[]string{"QUORUM_NOT_MET", "DUAL_APPROVAL_REQUIRED"},
			[]string{basis}
	}

	return false, "", "", nil, nil
}

// evaluateExecutionRevalidation evaluates pre-execution fact-hash and fingerprint integrity (Phase 3.9, Scenario A10).
func (h *Handler) evaluateExecutionRevalidation(in evalContext) (denied bool, basis, reason string, reasonCodes []string, negativeControls []string) {
	if in.Attributes == nil {
		return false, "", "", nil, nil
	}

	isRevalRequired := in.Attributes["execution_revalidation"] == "true" ||
		in.Attributes["approved_fact_hash"] != "" ||
		in.Attributes["approved_fingerprint"] != ""

	if !isRevalRequired {
		return false, "", "", nil, nil
	}

	approvedHash := in.Attributes["approved_fact_hash"]
	if approvedHash == "" {
		approvedHash = in.Attributes["approved_fingerprint"]
	}

	currentHash := in.Attributes["current_fact_hash"]
	if currentHash == "" {
		currentHash = in.Attributes["current_fingerprint"]
	}
	if currentHash == "" {
		currentHash = in.Attributes["fact_hash"]
	}

	// Scenario A10: Approval facts mutate after approval -> Object fingerprint changes -> Invalidate
	if approvedHash != "" && currentHash != "" && approvedHash != currentHash {
		return true, "revalidation:fact_hash_mismatch",
			"Approval facts mutate after approval: Object fingerprint changes (Scenario A10)",
			[]string{"FACT_HASH_MUTATION_DETECTED", "REAPPROVAL_REQUIRED"},
			[]string{"revalidation:fact_hash_mismatch"}
	}

	// Check if approval was invalidated or revoked
	if in.Attributes["approval_expired"] == "true" || in.Attributes["delegator_revoked"] == "true" {
		return true, "revalidation:approval_invalidated",
			"Approval invalidated due to expired or revoked authority",
			[]string{"APPROVAL_INVALIDATED"},
			[]string{"revalidation:approval_invalidated"}
	}

	return false, "", "", nil, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func filterApplicableLimits(limits []domain.AuthorityLimit, in evalContext, evaluationEntityID string) []domain.AuthorityLimit {
	var result []domain.AuthorityLimit
	now := time.Now().UTC()

	for _, al := range limits {
		// Effective period
		if now.Before(al.EffectiveFrom) {
			continue
		}
		if al.EffectiveTo != nil && now.After(*al.EffectiveTo) {
			continue
		}

		// Scope matching
		if al.LegalEntityID != nil && evaluationEntityID != PlatformScopeSentinel && *al.LegalEntityID != evaluationEntityID {
			continue
		}
		if al.BookID != nil && in.BookID != "" && *al.BookID != in.BookID {
			continue
		}
		if al.OrgUnitID != nil && in.OrgUnitID != "" && *al.OrgUnitID != in.OrgUnitID {
			continue
		}

		// Authority type matching
		if al.AuthorityType != "" && al.AuthorityType != "*" {
			match := strings.EqualFold(al.AuthorityType, in.ActionType) ||
				(isReleaseAction(in.ActionType) && strings.Contains(al.AuthorityType, "release")) ||
				(isApprovalAction(in.ActionType) && strings.Contains(al.AuthorityType, "approv"))
			if !match && in.Attributes != nil && in.Attributes["authority_type"] != "" {
				match = strings.EqualFold(al.AuthorityType, in.Attributes["authority_type"])
			}
			if !match {
				continue
			}
		}

		result = append(result, al)
	}

	return result
}

func selectEffectiveLimit(limits []domain.AuthorityLimit) domain.AuthorityLimit {
	if len(limits) == 1 {
		return limits[0]
	}

	// Score by specificity:
	// BookID + OrgUnitID: 4
	// BookID: 3
	// LegalEntityID: 2
	// Tenant: 1
	// Bonus +10 for direct principal_id
	bestIdx := 0
	bestScore := -1

	for i, l := range limits {
		score := 0
		if l.PrincipalID != nil && *l.PrincipalID != "" {
			score += 10
		}
		if l.OrgUnitID != nil && *l.OrgUnitID != "" {
			score += 2
		}
		if l.BookID != nil && *l.BookID != "" {
			score += 2
		}
		if l.LegalEntityID != nil && *l.LegalEntityID != "" {
			score += 1
		}

		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}

	return limits[bestIdx]
}

func resolveFXRate(fromCurrency, toCurrency string, attrs map[string]string) (float64, bool) {
	if attrs != nil {
		if rateStr := attrs["fx_rate"]; rateStr != "" {
			if r, err := strconv.ParseFloat(rateStr, 64); err == nil && r > 0 {
				return r, true
			}
		}
		if rateStr := attrs["exchange_rate"]; rateStr != "" {
			if r, err := strconv.ParseFloat(rateStr, 64); err == nil && r > 0 {
				return r, true
			}
		}
	}

	// Look up in StandardReferenceFXRates
	rateFrom, okFrom := StandardReferenceFXRates[fromCurrency]
	rateTo, okTo := StandardReferenceFXRates[toCurrency]

	if okFrom && okTo && rateTo > 0 {
		return rateFrom / rateTo, true
	}

	return 0, false
}

func parseApproversList(attrs map[string]string) []string {
	var approvers []string
	raw := attrs["approved_by"]
	if raw == "" {
		raw = attrs["approvers"]
	}

	if raw == "" {
		return nil
	}

	// Check if JSON array
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err == nil {
			return list
		}
	}

	// Comma-separated
	parts := strings.Split(raw, ",")
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			approvers = append(approvers, trimmed)
		}
	}
	return approvers
}
