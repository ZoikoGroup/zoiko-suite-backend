package handler

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/purposeregistry"
	"zoiko.io/privacy-decision-svc/internal/transferregistry"
)

// outcome is the result of one evaluation.
type outcome struct {
	result      domain.DecisionResult
	reasons     []string
	constraints []domain.DecisionConstraint
	expiresAt   *time.Time
}

func stop(result domain.DecisionResult, reasons ...string) outcome {
	return outcome{result: result, reasons: reasons}
}

func up(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(v)) {
			return true
		}
	}
	return false
}

// earliest returns the earlier of two optional instants.
func earliest(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.Before(*a):
		return b
	}
	return a
}

// requestedCategories is the union of data_categories and data_category.
func requestedCategories(dc *domain.DataContext) []string {
	if dc == nil {
		return nil
	}
	var out []string
	for _, c := range dc.DataCategories {
		if up(c) != "" {
			out = append(out, up(c))
		}
	}
	if up(dc.DataCategory) != "" {
		out = append(out, up(dc.DataCategory))
	}
	return out
}

// evaluate implements the runtime privacy decision sequence from ZS-SVC-W-001 §13.
// Every uncertain or unrecognised input resolves to a non-PERMIT result (I13).
func (h *Handler) evaluate(
	ctx context.Context,
	tenantID, principalID string,
	req *domain.EvaluateDecisionRequest,
	decision *domain.PrivacyDecision,
) outcome {
	indeterminate := func(extra ...string) outcome {
		return stop(domain.ResultIndeterminate, append(extra, domain.PRV019PrivacyContextIndeterminate)...)
	}

	// Step 1: Resolve the exact processing-activity version (§13 step 1, I29, PRV-002).
	// A requested activity_version_id is resolved exactly; it never silently
	// becomes the current version.
	requestedActivityVersion := strings.TrimSpace(req.ActivityVersionID)
	var activity *purposeregistry.ActivityVersion
	var err error
	if requestedActivityVersion != "" {
		activity, err = h.purposes.ResolveActivityVersion(ctx, tenantID, principalID, req.ProcessingActivityID, requestedActivityVersion)
	} else {
		activity, err = h.purposes.ResolveActivity(ctx, tenantID, principalID, req.ProcessingActivityID)
	}
	if err != nil {
		h.log.Error("evaluate: purpose registry unavailable (activity)", zap.Error(err))
		return indeterminate()
	}
	if activity == nil || up(activity.VersionStatus) != "ACTIVE" {
		return stop(domain.ResultBlock, domain.PRV002ProcessingActivityInactive)
	}
	if activity.ActivityID != "" && activity.ActivityID != req.ProcessingActivityID {
		return stop(domain.ResultBlock, domain.PRV002ProcessingActivityInactive)
	}
	if requestedActivityVersion != "" && activity.ActivityVersionID != requestedActivityVersion {
		return indeterminate()
	}
	decision.ActivityVersionID = &activity.ActivityVersionID

	// Step 1b: Purpose limitation (§13.1 Safeguard 1: no implicit same-tenant exemption).
	if !containsFold(activity.PurposeIDs, req.PurposeID) {
		return stop(domain.ResultBlock, domain.PRV009PurposeIncompatible)
	}

	// Resolve the purpose itself — must be PUBLISHED (PRV-001).
	purpose, err := h.purposes.ResolvePurpose(ctx, tenantID, principalID, req.PurposeID)
	if err != nil {
		h.log.Error("evaluate: purpose registry unavailable (purpose)", zap.Error(err))
		return indeterminate()
	}
	if purpose == nil || up(purpose.VersionStatus) != "PUBLISHED" {
		return stop(domain.ResultBlock, domain.PRV001PurposeNotRegistered)
	}
	// The registry resolves only the current PUBLISHED purpose version. An
	// exact historical purpose_version_id cannot be resolved, so a request for
	// any other version fails closed instead of becoming the current one.
	if v := strings.TrimSpace(req.PurposeVersionID); v != "" && v != purpose.PurposeVersionID {
		return indeterminate()
	}
	decision.PurposeVersionID = &purpose.PurposeVersionID

	// Step 1c: Secondary purpose (I14, N02). Free text is never a purpose.
	if strings.TrimSpace(req.ProposedSecondaryPurpose) != "" {
		return stop(domain.ResultBlock, domain.PRV001PurposeNotRegistered)
	}
	secondaryRegistered := false
	if sp := strings.TrimSpace(req.SecondaryPurposeID); sp != "" {
		secondary, err := h.purposes.ResolvePurpose(ctx, tenantID, principalID, sp)
		if err != nil {
			h.log.Error("evaluate: purpose registry unavailable (secondary purpose)", zap.Error(err))
			return indeterminate()
		}
		if secondary == nil || up(secondary.VersionStatus) != "PUBLISHED" {
			return stop(domain.ResultBlock, domain.PRV001PurposeNotRegistered)
		}
		if !containsFold(activity.PurposeIDs, sp) {
			return stop(domain.ResultBlock, domain.PRV009PurposeIncompatible)
		}
		secondaryRegistered = true
	}

	// Step 2: Policy Resolution (§13 step 3 & PRV-005)
	decision.PolicyRefs = append(decision.PolicyRefs, purpose.LawfulBasisRefs...)
	decision.PolicyRefs = append(decision.PolicyRefs, purpose.SpecialConditionRefs...)
	if len(decision.PolicyRefs) == 0 {
		return stop(domain.ResultIndeterminate, domain.PRV005PolicyUnavailable)
	}

	// Step 3: Jurisdiction Evaluation (N12 & I13)
	subjJurisdiction := ""
	if req.SubjectContext != nil {
		subjJurisdiction = strings.TrimSpace(req.SubjectContext.Jurisdiction)
		if subjJurisdiction == "" {
			subjJurisdiction = strings.TrimSpace(req.SubjectContext.Residency)
		}
	}
	if subjJurisdiction == "" || up(subjJurisdiction) == "UNKNOWN" {
		return stop(domain.ResultIndeterminate, domain.PRV004JurisdictionUnresolved)
	}
	if len(activity.Jurisdictions) > 0 {
		if !containsFold(activity.Jurisdictions, subjJurisdiction) && !containsFold(activity.Jurisdictions, "GLOBAL") {
			return stop(domain.ResultBlock, domain.PRV004JurisdictionUnresolved)
		}
	}

	// Step 4: Registered-scope enforcement and unknown-value handling (I13, I15).
	// The caller's declarations are checked against the REGISTERED activity
	// scope; caller sensitivity assertions can only add review, never remove it.
	sc := req.SubjectContext
	class := up(sc.SubjectClass)
	switch {
	case class == "":
		return indeterminate()
	case class == "UNKNOWN":
		return stop(domain.ResultReviewRequired, domain.PRV019PrivacyContextIndeterminate)
	case len(activity.SubjectClasses) == 0:
		return indeterminate()
	case !containsFold(activity.SubjectClasses, class):
		return stop(domain.ResultBlock, domain.PRV009PurposeIncompatible)
	}
	if ab := up(sc.AgeBand); ab != "" {
		if !domain.IsValidAgeBand(ab) {
			return indeterminate()
		}
		if ab == "UNKNOWN" {
			return stop(domain.ResultReviewRequired, domain.PRV019PrivacyContextIndeterminate)
		}
	}

	registered := make([]string, 0, len(activity.DataCategories))
	for _, c := range activity.DataCategories {
		if up(c) != "" {
			registered = append(registered, up(c))
		}
	}
	if len(registered) == 0 {
		return indeterminate()
	}
	requested := requestedCategories(req.DataContext)
	for _, c := range requested {
		if !containsFold(registered, c) {
			return stop(domain.ResultBlock, domain.PRV010DataCategoryRestricted)
		}
	}

	sensitive := false
	unclassified := false
	for _, c := range append(append([]string{}, registered...), requested...) {
		switch {
		case domain.IsSensitiveCategory(c):
			sensitive = true
		case !domain.IsOrdinaryCategory(c):
			unclassified = true
		}
	}
	restricted := false
	if dc := req.DataContext; dc != nil {
		if cl := up(dc.Classification); cl != "" {
			if !domain.IsValidClassification(cl) {
				return indeterminate()
			}
			if cl == "UNKNOWN" {
				return stop(domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted)
			}
			restricted = cl == "RESTRICTED"
		}
		if dc.Source != "" && !domain.IsValidDataSource(dc.Source) {
			return indeterminate()
		}
		for _, f := range dc.SensitivityFlags {
			if !domain.IsValidSensitivityFlag(f) {
				return indeterminate()
			}
			sensitive = true
		}
	}
	if rc := req.RecipientContext; rc != nil && rc.RecipientType != "" && !domain.IsValidRecipientType(rc.RecipientType) {
		return indeterminate()
	}
	if unclassified {
		return stop(domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted)
	}

	// Step 5: Assessment requirement (§12.2 REVIEW_REQUIRED, PRV-016).
	switch up(activity.DpiaTiaStatus) {
	case "RESOLVED", "NOT_REQUIRED":
	case "REVIEW_REQUIRED", "UNDER_REVIEW":
		return stop(domain.ResultReviewRequired, domain.PRV016AssessmentRequired)
	default:
		return indeterminate()
	}

	// Step 6: Safeguard 6: no anonymous-data claim without an approved de-identification control.
	if req.ProposedOperation == domain.OperationAnonymize || (req.DataContext != nil && up(req.DataContext.Classification) == "ANONYMOUS") {
		if strings.TrimSpace(req.DeidentificationControlRef) == "" {
			return stop(domain.ResultBlock, domain.PRV010DataCategoryRestricted)
		}
	}

	// Step 7: Consent evaluation (N10 employee imbalance, terminal states).
	var consentExpiry *time.Time
	consentRequired := req.ConsentCheck != nil && req.ConsentCheck.Required
	switch up(activity.NoticeConsentDependency) {
	case "REQUIRED":
		consentRequired = true
	case "CONDITIONAL":
		// The condition is policy-owned (PDC, not available): fail closed.
		consentRequired = true
	case "NOT_REQUIRED":
	default:
		return indeterminate()
	}
	if consentRequired {
		if class == "EMPLOYEE" {
			return stop(domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted)
		}
		if h.consents == nil {
			return indeterminate()
		}
		resolution, err := h.consents.ResolveStatus(ctx, tenantID, principalID, req.SubjectRef, req.PurposeID)
		if err != nil {
			h.log.Error("evaluate: consent registry unavailable", zap.Error(err))
			return indeterminate()
		}
		if resolution == nil {
			return stop(domain.ResultBlock, domain.PRV006ConsentRequiredMissing)
		}
		if resolution.LatestReceipt != nil {
			decision.ConsentReceiptID = &resolution.LatestReceipt.ConsentReceiptID
			if resolution.LatestReceipt.NoticeVersionID != "" {
				decision.NoticeVersionID = &resolution.LatestReceipt.NoticeVersionID
			}
		}
		switch up(resolution.Status) {
		case "WITHDRAWN":
			return stop(domain.ResultBlock, domain.PRV007ConsentWithdrawn)
		case "GRANTED":
			if resolution.LatestReceipt == nil || resolution.LatestReceipt.ConsentReceiptID == "" {
				return indeterminate()
			}
			if req.ConsentCheck != nil && strings.TrimSpace(req.ConsentCheck.ReceiptID) != "" &&
				strings.TrimSpace(req.ConsentCheck.ReceiptID) != resolution.LatestReceipt.ConsentReceiptID {
				return stop(domain.ResultBlock, domain.PRV006ConsentRequiredMissing)
			}
			if exp := resolution.LatestReceipt.ExpiresAt; exp != nil {
				if !exp.After(time.Now()) {
					return stop(domain.ResultBlock, domain.PRV006ConsentRequiredMissing)
				}
				consentExpiry = exp
			}
		default: // EXPIRED, INVALIDATED, DENIED, NOT_REQUESTED, anything unrecognised
			return stop(domain.ResultBlock, domain.PRV006ConsentRequiredMissing)
		}
	}

	// Step 8: Legal hold / retention (§13, PRV-014, I19, N25). The caller must
	// identify the record_class; this service never derives one.
	op := req.ProposedOperation
	hc := req.LegalHoldCheck
	hasHoldCheck := hc != nil && strings.TrimSpace(hc.RecordClass) != ""
	if !hasHoldCheck && (op == domain.OperationDelete || op == domain.OperationAnonymize) {
		// Erasure cannot proceed on unknown hold state.
		return indeterminate(domain.PRV014RetentionOrHoldBlock)
	}
	if hasHoldCheck {
		if h.holds == nil {
			return indeterminate()
		}
		res, err := h.holds.Resolve(ctx, tenantID, principalID, strings.TrimSpace(hc.RecordClass), strings.TrimSpace(hc.EntityRef))
		if err != nil {
			h.log.Error("evaluate: retention registry unavailable", zap.Error(err))
			return indeterminate()
		}
		if res == nil {
			return indeterminate()
		}
		if res.MatchedHold != nil && res.MatchedHold.LegalHoldID != "" {
			decision.LegalHoldID = &res.MatchedHold.LegalHoldID
		}
		if !res.Blocked && res.MatchedHold != nil {
			// Not blocked yet a hold matched: inconsistent upstream state.
			return indeterminate()
		}
		if res.Blocked && op != domain.OperationRetain {
			return stop(domain.ResultBlock, domain.PRV014RetentionOrHoldBlock)
		}
	}

	// Step 9: Transfer (PRV-05, §12.1, §13 step 4, PRV-015). Unknown results fail closed.
	isExport := op == domain.OperationExport
	isForeignDisclose := op == domain.OperationDisclose &&
		req.RecipientContext != nil &&
		req.RecipientContext.DestinationJurisdiction != "" &&
		!strings.EqualFold(req.RecipientContext.DestinationJurisdiction, subjJurisdiction)
	var transferConstraints []domain.DecisionConstraint
	var transferExpiry *time.Time
	if isExport || isForeignDisclose || req.TransferCheck != nil {
		if req.TransferCheck == nil {
			return stop(domain.ResultBlock, domain.PRV015TransferNotAuthorized)
		}
		if h.transfers == nil {
			return indeterminate()
		}
		tResp, err := h.transfers.EvaluateTransfer(ctx, tenantID, principalID, &transferregistry.EvaluateTransferRequest{
			TenantID:                tenantID,
			RelationshipID:          req.TransferCheck.RelationshipID,
			TransferMechanismID:     req.TransferCheck.TransferMechanismID,
			DestinationJurisdiction: req.TransferCheck.DestinationJurisdiction,
			AssessmentRequired:      req.TransferCheck.AssessmentCheck,
		})
		if err != nil {
			h.log.Error("evaluate: transfer service unavailable", zap.Error(err))
			return indeterminate()
		}
		if tResp == nil {
			return indeterminate()
		}
		if tResp.DecisionID != "" {
			decision.TransferDecisionID = &tResp.DecisionID
		}
		switch up(tResp.Result) {
		case "AUTHORIZED":
		case "CONDITIONAL":
			transferConstraints = append(transferConstraints, domain.DecisionConstraint{
				Type:        "RECIPIENT_LIMITATION",
				Description: "Transfer conditional on recipient adhering to processor contractual obligations and destination jurisdiction safeguards",
				Parameters: map[string]interface{}{
					"destination_jurisdiction": req.TransferCheck.DestinationJurisdiction,
					"mechanism_id":             req.TransferCheck.TransferMechanismID,
				},
			})
		case "BLOCKED":
			return stop(domain.ResultBlock, domain.PRV015TransferNotAuthorized)
		case "REVIEW_REQUIRED":
			return stop(domain.ResultReviewRequired, domain.PRV016AssessmentRequired)
		default: // empty or unrecognised result
			return indeterminate()
		}
		transferExpiry = tResp.ExpiresAt
	}

	// expires_at: no specification rule or default TTL exists, so none is
	// invented. The decision is only valid until the earliest upstream
	// evidence it relied on expires (consent receipt, transfer decision).
	expiry := earliest(consentExpiry, transferExpiry)

	// Step 10: Review gate — secondary purpose, model training, minors,
	// restricted/sensitive data.
	var reviewReasons []string
	if secondaryRegistered {
		// Registered, but the compatibility/policy evaluation (PDC) is not
		// available, so a secondary use is never auto-permitted (I14).
		reviewReasons = append(reviewReasons, domain.PRV009PurposeIncompatible)
	}
	if op == domain.OperationTrainModel {
		// §13.1 / N50 / PRV-C19: training needs an explicit, human-approved
		// policy that cannot be verified here, so it is never auto-permitted.
		reviewReasons = append(reviewReasons, domain.PRV009PurposeIncompatible)
	}
	if up(sc.AgeBand) == "MINOR" || class == "MINOR" {
		reviewReasons = append(reviewReasons, domain.PRV010DataCategoryRestricted)
	}
	if restricted || sensitive {
		reviewReasons = append(reviewReasons, domain.PRV010DataCategoryRestricted)
	}
	if len(reviewReasons) > 0 {
		return outcome{result: domain.ResultReviewRequired, reasons: dedupeReasons(reviewReasons), expiresAt: expiry}
	}

	if len(transferConstraints) > 0 {
		return outcome{result: domain.ResultRestrict, reasons: []string{domain.PRV011MinimizationRequired}, constraints: transferConstraints, expiresAt: expiry}
	}

	// Step 11: All conditions satisfied — PERMIT (§12.2).
	return outcome{result: domain.ResultPermit, reasons: []string{}, expiresAt: expiry}
}
