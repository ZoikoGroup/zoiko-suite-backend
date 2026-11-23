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
//
// These are the DEFAULT reference basis. ZS-IAM-001 §31 leaves "authoritative
// FX basis for approval-limit comparisons" as a pre-production decision, so
// the operator sets the table with AUTHZ_FX_REFERENCE_RATES
// (SetFXReferenceRates); every decision that converts names the basis it used.
var StandardReferenceFXRates = map[string]float64{
	"GBP": 1.0,
	"USD": 0.78,   // 1 USD = 0.78 GBP
	"EUR": 0.85,   // 1 EUR = 0.85 GBP
	"CAD": 0.58,   // 1 CAD = 0.58 GBP
	"AUD": 0.51,   // 1 AUD = 0.51 GBP
	"JPY": 0.0052, // 1 JPY = 0.0052 GBP
	"CHF": 0.89,   // 1 CHF = 0.89 GBP
}

// FXBasisReferenceRates names the conversion basis in reason codes.
const FXBasisReferenceRates = "REFERENCE_RATES"

// SetFXReferenceRates replaces the reference table (AUTHZ_FX_REFERENCE_RATES).
// nil or empty keeps StandardReferenceFXRates.
func (h *Handler) SetFXReferenceRates(rates map[string]float64) {
	if len(rates) == 0 {
		h.fxRates = nil
		return
	}
	h.fxRates = make(map[string]float64, len(rates))
	for k, v := range rates {
		h.fxRates[strings.ToUpper(strings.TrimSpace(k))] = v
	}
}

// fxReferenceRates is the table every conversion and limit authoring reads.
func (h *Handler) fxReferenceRates() map[string]float64 {
	if len(h.fxRates) > 0 {
		return h.fxRates
	}
	return StandardReferenceFXRates
}

// ParseFXReferenceRates reads "GBP=1,USD=0.78,EUR=0.85": units of the base
// currency per one unit of each. Every rate must be a positive number. An
// empty string is (nil, nil): keep the default table.
func ParseFXReferenceRates(raw string) (map[string]float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	rates := map[string]float64{}
	for _, pair := range strings.Split(raw, ",") {
		code, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
		code = strings.ToUpper(strings.TrimSpace(code))
		if !ok || code == "" {
			return nil, fmt.Errorf("fx reference rate %q is not CODE=rate", pair)
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil || rate <= 0 {
			return nil, fmt.Errorf("fx reference rate for %s must be a positive number, got %q", code, val)
		}
		rates[code] = rate
	}
	return rates, nil
}

// evaluateAuthorityLimits evaluates monetary limits and currency conversion for the request (Phase 3.6 & 3.7, Scenario A09).
func (h *Handler) evaluateAuthorityLimits(ctx context.Context, in evalContext, evaluationEntityID string) (denied bool, basis, reason string, reasonCodes []string, negativeControls []string, err error) {
	amountStr, quantityStr := "", ""
	if in.Attributes != nil {
		amountStr = in.Attributes["amount"]
		if amountStr == "" {
			amountStr = in.Attributes["transaction_amount"]
		}
		quantityStr = strings.TrimSpace(in.Attributes["quantity"])
	}

	isMonetaryAction := amountStr != "" ||
		(in.Attributes != nil && (in.Attributes["authority_limit_required"] == "true" || in.Attributes["authority_type"] != "")) ||
		isReleaseAction(in.ActionType) ||
		isApprovalAction(in.ActionType)

	if !isMonetaryAction && amountStr == "" && quantityStr == "" {
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

	// A QUANTITY limit (000040, S8-D1) bounds the request's quantity, a
	// MONETARY one its amount; each kind is judged on its own.
	monetary, quantity := splitLimitsByKind(applicable)
	if len(quantity) > 0 {
		required := in.Attributes != nil && in.Attributes["authority_limit_required"] == "true"
		if denied, basis, reason, codes, controls := checkQuantityLimit(selectEffectiveLimit(quantity), quantityStr, required); denied {
			return true, basis, reason, codes, controls, nil
		}
	}
	if len(monetary) == 0 {
		return false, "", "", nil, nil, nil
	}

	// Select the most specific limit
	limit := selectEffectiveLimit(monetary)

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
	limitCurrency := strings.ToUpper(limit.Currency)
	if limitCurrency == "" {
		limitCurrency = "GBP"
	}
	// No currency is no comparison. It used to be read as GBP, so an amount in
	// yen or dollars sent without its currency was judged as pounds.
	reqCurrency := ""
	if in.Attributes != nil {
		reqCurrency = strings.ToUpper(strings.TrimSpace(in.Attributes["currency"]))
	}
	if reqCurrency == "" {
		return true, "authority_limit:currency_required",
			fmt.Sprintf("The amount states no currency; the applicable authority limit is in %s", limitCurrency),
			[]string{"CURRENCY_REQUIRED"},
			[]string{"authority_limit:currency_required"}, nil
	}

	convertedAmount := new(big.Float).Copy(reqAmount)
	currenciesDiffer := reqCurrency != limitCurrency
	fxConversionApplied := false
	// §20 "SoD/authority result": a converted comparison names its basis, so
	// the decision can be reconstructed against the table that was in force.
	var fxBasis []string

	if currenciesDiffer {
		rate, rateFound := h.resolveFXRate(reqCurrency, limitCurrency)
		if !rateFound {
			// Fail-closed when no deterministic FX conversion basis exists
			return true, "authority_limit:currency_conversion_missing",
				fmt.Sprintf("Currency mismatch between transaction (%s) and limit (%s) with no deterministic FX conversion basis", reqCurrency, limitCurrency),
				[]string{"CURRENCY_CONVERSION_UNAVAILABLE"},
				[]string{"authority_limit:currency_conversion_missing"}, nil
		}

		convertedAmount.Mul(convertedAmount, big.NewFloat(rate))
		fxConversionApplied = true
		fxBasis = []string{"FX_BASIS:" + FXBasisReferenceRates + ":" + reqCurrency + "/" + limitCurrency,
			"FX_RATE:" + strconv.FormatFloat(rate, 'f', -1, 64)}
	}

	// 5. Upper limit evaluation
	upperLimit, ok := new(big.Float).SetString(limit.UpperLimit)
	if ok && upperLimit.Cmp(big.NewFloat(0)) > 0 {
		if convertedAmount.Cmp(upperLimit) > 0 {
			if fxConversionApplied {
				// Scenario A09: Authority limit exceeded after FX conversion
				return true, "authority_limit:exceeded:after_fx_conversion",
					"Authority limit exceeded after FX conversion (Scenario A09)",
					append([]string{"AUTHORITY_LIMIT_EXCEEDED", "FX_CONVERSION_LIMIT_EXCEEDED"}, fxBasis...),
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
				append([]string{"AUTHORITY_LIMIT_BELOW_MINIMUM"}, fxBasis...),
				[]string{"authority_limit:below_lower_limit"}, nil
		}
	}

	// Within the limit: no denial, but the FX basis (if any) travels with the
	// PERMIT as evidence.
	return false, "", "", fxBasis, nil, nil
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

	// Fail closed (10 Nov 2026): a revalidation that is asked for, or implied by
	// an approval fingerprint, needs BOTH hashes. Either one missing used to
	// skip the comparison, so a PEP that sent the approved hash and forgot the
	// current one executed on facts nobody had compared.
	if approvedHash == "" {
		return true, "revalidation:approved_fact_hash_missing",
			"Execution revalidation requested but the approved fact hash was not supplied",
			[]string{"APPROVED_FACT_HASH_REQUIRED", "REAPPROVAL_REQUIRED"},
			[]string{"revalidation:approved_fact_hash_missing"}
	}
	if currentHash == "" {
		return true, "revalidation:current_fact_hash_missing",
			"An approval fingerprint was supplied without the current fact hash to compare it with",
			[]string{"CURRENT_FACT_HASH_REQUIRED", "REAPPROVAL_REQUIRED"},
			[]string{"revalidation:current_fact_hash_missing"}
	}

	// Scenario A10: Approval facts mutate after approval -> Object fingerprint changes -> Invalidate
	if approvedHash != currentHash {
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
		// Only a limit in force limits (000035, AU-03): a pending proposal,
		// a rejected one, and a retired or superseded version do not. Empty
		// is a row read without the status column.
		if al.Status != "" && al.Status != domain.AuthorityLimitActive {
			continue
		}
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

func splitLimitsByKind(limits []domain.AuthorityLimit) (monetary, quantity []domain.AuthorityLimit) {
	for _, l := range limits {
		if l.LimitKind == domain.AuthorityLimitQuantity {
			quantity = append(quantity, l)
		} else {
			monetary = append(monetary, l)
		}
	}
	return monetary, quantity
}

// checkQuantityLimit judges the request's `quantity` attribute — the one the
// delegation ceiling reads — against a QUANTITY limit: a count, so nothing to
// convert. No quantity is a denial only when the caller marks a limit
// required, the rule a missing amount follows.
func checkQuantityLimit(limit domain.AuthorityLimit, quantityStr string, required bool) (denied bool, basis, reason string, reasonCodes, negativeControls []string) {
	if quantityStr == "" {
		if required {
			return true, "authority_limit:quantity_required",
				"Quantity required for authority limit evaluation",
				[]string{"QUANTITY_REQUIRED"},
				[]string{"authority_limit:quantity_required"}
		}
		return false, "", "", nil, nil
	}
	q, ok := new(big.Rat).SetString(quantityStr)
	if !ok || q.Sign() < 0 {
		return true, "authority_limit:invalid_quantity",
			"Quantity is not a non-negative number: " + quantityStr,
			[]string{"INVALID_QUANTITY_FORMAT"},
			[]string{"authority_limit:invalid_quantity"}
	}
	if upper, ok := new(big.Rat).SetString(limit.UpperLimit); ok && upper.Sign() > 0 && q.Cmp(upper) > 0 {
		return true, "authority_limit:quantity_exceeded",
			fmt.Sprintf("Requested quantity %s exceeds authority upper limit %s", quantityStr, limit.UpperLimit),
			[]string{"AUTHORITY_LIMIT_EXCEEDED"},
			[]string{"authority_limit:quantity_exceeded"}
	}
	if lower, ok := new(big.Rat).SetString(limit.LowerLimit); ok && lower.Sign() > 0 && q.Cmp(lower) < 0 {
		return true, "authority_limit:quantity_below_lower_limit",
			fmt.Sprintf("Requested quantity %s is below authority lower limit %s", quantityStr, limit.LowerLimit),
			[]string{"AUTHORITY_LIMIT_BELOW_MINIMUM"},
			[]string{"authority_limit:quantity_below_lower_limit"}
	}
	return false, "", "", nil, nil
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

// resolveFXRate converts with the platform's reference rates ONLY.
//
// It used to honour an fx_rate / exchange_rate supplied in the request's
// attributes first — by the caller whose amount is being checked. A caller
// could send fx_rate=0.0001 and bring any amount under any limit: the limit
// was whatever the caller said it was. A request-supplied rate is now ignored
// (decision 5 Oct 2026); a pair the reference table cannot convert fails closed
// in the caller, as it always did.
func (h *Handler) resolveFXRate(fromCurrency, toCurrency string) (float64, bool) {
	rates := h.fxReferenceRates()
	rateFrom, okFrom := rates[fromCurrency]
	rateTo, okTo := rates[toCurrency]

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
