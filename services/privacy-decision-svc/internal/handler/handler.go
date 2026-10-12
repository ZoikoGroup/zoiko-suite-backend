// Package handler exposes privacy-decision-svc's REST API — PRV-03.
package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/consentregistry"
	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/events"
	svcmiddleware "zoiko.io/privacy-decision-svc/internal/middleware"
	"zoiko.io/privacy-decision-svc/internal/purposeregistry"
	"zoiko.io/privacy-decision-svc/internal/retentionregistry"
	"zoiko.io/privacy-decision-svc/internal/store"
	"zoiko.io/privacy-decision-svc/internal/transferregistry"
)

// PurposeChecker, ConsentChecker, HoldChecker and TransferChecker are the narrow
// interfaces the handler depends on.
type PurposeChecker interface {
	ResolveActivity(ctx context.Context, tenantID, activityID string) (*purposeregistry.ActivityVersion, error)
	ResolvePurpose(ctx context.Context, tenantID, purposeID string) (*purposeregistry.PurposeVersion, error)
}

type ConsentChecker interface {
	ResolveStatus(ctx context.Context, tenantID, subjectRef, purposeID string) (*consentregistry.ConsentResolution, error)
}

type HoldChecker interface {
	Resolve(ctx context.Context, tenantID, recordClass, entityRef string) (*retentionregistry.RetentionResolution, error)
}

type TransferChecker interface {
	EvaluateTransfer(ctx context.Context, tenantID, principalID string, req *transferregistry.EvaluateTransferRequest) (*transferregistry.TransferDecision, error)
}

type Handler struct {
	store     store.Store
	pub       events.Publisher
	purposes  PurposeChecker
	consents  ConsentChecker
	holds     HoldChecker
	transfers TransferChecker
	log       *zap.Logger
}

func New(
	st store.Store,
	pub events.Publisher,
	purposes PurposeChecker,
	consents ConsentChecker,
	holds HoldChecker,
	transfers TransferChecker,
	log *zap.Logger,
) *Handler {
	return &Handler{
		store:     st,
		pub:       pub,
		purposes:  purposes,
		consents:  consents,
		holds:     holds,
		transfers: transfers,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	// Canonical routes (§18)
	r.Route("/privacy/decisions", func(r chi.Router) {
		r.Post("/", h.EvaluateDecision)
		r.Get("/{decisionID}", h.GetDecision)
	})

	// Backwards-compatible /v1/ routes
	r.Route("/v1/privacy/decisions", func(r chi.Router) {
		r.Post("/", h.EvaluateDecision)
		r.Get("/{decisionID}", h.GetDecision)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// dedupeReasons preserves order while removing duplicate reason codes —
// e.g. a MINOR subject with a SENSITIVE data flag would otherwise report
// PRV-010 twice.
func dedupeReasons(reasons []string) []string {
	seen := make(map[string]bool, len(reasons))
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func hashBody(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func readBodyBytes(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return []byte{}, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func (h *Handler) checkIdempotency(w http.ResponseWriter, r *http.Request, tenantID, principalID string, body []byte) (*domain.IdempotencyRecord, bool, string, string) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, false, "", ""
	}
	reqHash := hashBody(body)
	existing, err := h.store.GetIdempotency(r.Context(), tenantID, key)
	if err != nil {
		h.log.Warn("idempotency lookup error", zap.Error(err))
		return nil, false, key, reqHash
	}
	if existing != nil {
		if existing.RequestHash != reqHash {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyConflict.Error())
			return existing, true, key, reqHash
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replay", "true")
		w.WriteHeader(existing.ResponseCode)
		_, _ = w.Write(existing.ResponseBody)
		return existing, true, key, reqHash
	}
	return nil, false, key, reqHash
}

func (h *Handler) writeJSONWithIdempotency(ctx context.Context, w http.ResponseWriter, tenantID, principalID, endpoint, key, reqHash string, status int, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal response")
		return
	}
	if key != "" {
		_ = h.store.SaveIdempotency(ctx, domain.IdempotencyRecord{
			Key:          key,
			TenantID:     tenantID,
			Endpoint:     endpoint,
			RequestHash:  reqHash,
			ResponseCode: status,
			ResponseBody: body,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// EvaluateDecision handles POST /privacy/decisions and POST /v1/privacy/decisions —
// the runtime purpose-binding decision described in ZS-SVC-W-001 §12/§13.
func (h *Handler) EvaluateDecision(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	var req domain.EvaluateDecisionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubjectRef == "" || req.ProcessingActivityID == "" || req.PurposeID == "" {
		writeError(w, http.StatusBadRequest, "subject_ref, processing_activity_id and purpose_id are required")
		return
	}
	if !req.ProposedOperation.Valid() {
		writeError(w, http.StatusBadRequest, "proposed_operation is missing or not a recognized value")
		return
	}

	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeError(w, http.StatusForbidden, "tenant_id does not match the verified X-Tenant-Id")
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = verifiedTenant
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	decision := &domain.PrivacyDecision{
		InputFingerprint:     domain.ComputeInputFingerprint(&req, tenantID),
		SubjectRef:           req.SubjectRef,
		SubjectContext:       req.SubjectContext,
		DataContext:          req.DataContext,
		ProcessingActivityID: req.ProcessingActivityID,
		PurposeID:            req.PurposeID,
		ProposedOperation:    req.ProposedOperation,
		RecipientContext:     req.RecipientContext,
		ActorPrincipalID:     principalID,
		CorrelationID:        correlationID,
		Constraints:          []domain.DecisionConstraint{},
	}

	if req.SecondaryPurposeID != "" {
		decision.SecondaryPurposeID = &req.SecondaryPurposeID
	} else if req.ProposedSecondaryPurpose != "" {
		decision.SecondaryPurposeID = &req.ProposedSecondaryPurpose
	}

	result, reasonCodes, constraints := h.evaluate(r.Context(), tenantID, principalID, &req, decision)
	decision.Result = result
	decision.ReasonCodes = reasonCodes
	if constraints != nil {
		decision.Constraints = constraints
	}

	if err := h.store.RecordDecision(r.Context(), tenantID, decision); err != nil {
		h.log.Error("EvaluateDecision: failed to record decision", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.decision.evaluated", EntityID: decision.DecisionID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: correlationID, Payload: decision,
	})

	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "EvaluateDecision", idemKey, reqHash, http.StatusOK, decision)
}

// evaluate implements the runtime privacy decision sequence from ZS-SVC-W-001 §13.
// Any material dependency failure fails closed as INDETERMINATE (§12.2).
func (h *Handler) evaluate(
	ctx context.Context,
	tenantID, principalID string,
	req *domain.EvaluateDecisionRequest,
	decision *domain.PrivacyDecision,
) (domain.DecisionResult, []string, []domain.DecisionConstraint) {
	// Step 1: Resolve the processing activity — must be ACTIVE (§13 step 1, §32 PRV-002).
	activity, err := h.purposes.ResolveActivity(ctx, tenantID, req.ProcessingActivityID)
	if err != nil {
		h.log.Error("evaluate: purpose registry unavailable (activity)", zap.Error(err))
		return domain.ResultIndeterminate, []string{domain.PRV019PrivacyContextIndeterminate}, nil
	}
	if activity == nil || activity.VersionStatus != "ACTIVE" {
		return domain.ResultBlock, []string{domain.PRV002ProcessingActivityInactive}, nil
	}
	decision.ActivityVersionID = &activity.ActivityVersionID

	// Step 1b: Purpose limitation (§13 step 1, PRV-C01, §13.1 Safeguard 1: No implicit same-tenant exemption).
	boundToActivity := false
	for _, p := range activity.PurposeIDs {
		if p == req.PurposeID {
			boundToActivity = true
			break
		}
	}
	if !boundToActivity {
		return domain.ResultBlock, []string{domain.PRV009PurposeIncompatible}, nil
	}

	// Resolve the purpose itself — must be PUBLISHED (§13 step 1, §32 PRV-001).
	purpose, err := h.purposes.ResolvePurpose(ctx, tenantID, req.PurposeID)
	if err != nil {
		h.log.Error("evaluate: purpose registry unavailable (purpose)", zap.Error(err))
		return domain.ResultIndeterminate, []string{domain.PRV019PrivacyContextIndeterminate}, nil
	}
	if purpose == nil || purpose.VersionStatus != "PUBLISHED" {
		return domain.ResultBlock, []string{domain.PRV001PurposeNotRegistered}, nil
	}
	decision.PurposeVersionID = &purpose.PurposeVersionID

	// Step 2: Assessment requirement (§12.2 REVIEW_REQUIRED, §32 PRV-016).
	if activity.DpiaTiaStatus == "REVIEW_REQUIRED" || activity.DpiaTiaStatus == "UNDER_REVIEW" {
		return domain.ResultReviewRequired, []string{domain.PRV016AssessmentRequired}, nil
	}

	// Step 3: §13.1 Mandatory Safeguards for proposed operations.
	//
	// Safeguard 4 ("no model-training/analytics purpose inherited from
	// telemetry unless separately registered and permitted") is already
	// enforced structurally by Step 1b above: TRAIN_MODEL can only proceed
	// against a purpose_id that is one of the activity's own registered,
	// PUBLISHED purposes. A separate purpose-name/ID substring heuristic
	// (e.g. checking for "TRAIN"/"MODEL"/"AI" in the purpose_id) would be
	// an invented rule with no basis in the registered purpose data or in
	// §13.1's text — this service does not fabricate PDC's job, same
	// doctrine as the rest of this domain (see domain/types.go).

	// Safeguard 6: No anonymous-data claim without approved de-identification control.
	if req.ProposedOperation == domain.OperationAnonymize || (req.DataContext != nil && req.DataContext.Classification == "ANONYMOUS") {
		if req.DeidentificationControlRef == "" {
			return domain.ResultBlock, []string{domain.PRV010DataCategoryRestricted}, nil
		}
	}

	// Step 4: Consent evaluation (§12.1 + PRV-C04 + Activity Dependency).
	consentRequired := (req.ConsentCheck != nil && req.ConsentCheck.Required) || activity.NoticeConsentDependency == "REQUIRED"
	if consentRequired {
		resolution, err := h.consents.ResolveStatus(ctx, tenantID, req.SubjectRef, req.PurposeID)
		if err != nil {
			h.log.Error("evaluate: consent registry unavailable", zap.Error(err))
			return domain.ResultIndeterminate, []string{domain.PRV019PrivacyContextIndeterminate}, nil
		}
		if resolution.LatestReceipt != nil {
			decision.ConsentReceiptID = &resolution.LatestReceipt.ConsentReceiptID
			if resolution.LatestReceipt.NoticeVersionID != "" {
				decision.NoticeVersionID = &resolution.LatestReceipt.NoticeVersionID
			}
		}
		if resolution.Status == "WITHDRAWN" {
			return domain.ResultBlock, []string{domain.PRV007ConsentWithdrawn}, nil
		}
		if resolution.Status != "GRANTED" {
			return domain.ResultBlock, []string{domain.PRV006ConsentRequiredMissing}, nil
		}
	}

	// Step 5: Legal hold evaluation (§13 step 4, §32 PRV-014).
	if req.LegalHoldCheck != nil && req.LegalHoldCheck.RecordClass != "" {
		resolution, err := h.holds.Resolve(ctx, tenantID, req.LegalHoldCheck.RecordClass, req.LegalHoldCheck.EntityRef)
		if err != nil {
			h.log.Error("evaluate: retention registry unavailable", zap.Error(err))
			return domain.ResultIndeterminate, []string{domain.PRV019PrivacyContextIndeterminate}, nil
		}
		if resolution.Blocked {
			if resolution.MatchedHold != nil {
				decision.LegalHoldID = &resolution.MatchedHold.LegalHoldID
			}
			return domain.ResultBlock, []string{domain.PRV014RetentionOrHoldBlock}, nil
		}
	}

	// Step 6: Transfer state evaluation (PRV-05 integration, §12.1 + §13 step 4, §32 PRV-015).
	isTransfer := req.ProposedOperation == domain.OperationExport ||
		(req.ProposedOperation == domain.OperationDisclose && req.RecipientContext != nil && req.RecipientContext.DestinationJurisdiction != "") ||
		req.TransferCheck != nil

	var transferConstraints []domain.DecisionConstraint
	if isTransfer {
		if req.TransferCheck == nil {
			// Cross-border export without transfer mechanism details is prohibited (§13.1, §17.2)
			if req.ProposedOperation == domain.OperationExport {
				return domain.ResultBlock, []string{domain.PRV015TransferNotAuthorized}, nil
			}
		} else if h.transfers != nil {
			tResp, err := h.transfers.EvaluateTransfer(ctx, tenantID, principalID, &transferregistry.EvaluateTransferRequest{
				TenantID:                tenantID,
				RelationshipID:          req.TransferCheck.RelationshipID,
				TransferMechanismID:     req.TransferCheck.TransferMechanismID,
				DestinationJurisdiction: req.TransferCheck.DestinationJurisdiction,
				AssessmentRequired:      req.TransferCheck.AssessmentCheck,
			})
			if err != nil {
				h.log.Error("evaluate: transfer service unavailable", zap.Error(err))
				return domain.ResultIndeterminate, []string{domain.PRV019PrivacyContextIndeterminate}, nil
			}
			decision.TransferDecisionID = &tResp.DecisionID
			if tResp.Result == "BLOCKED" {
				return domain.ResultBlock, []string{domain.PRV015TransferNotAuthorized}, nil
			}
			if tResp.Result == "REVIEW_REQUIRED" {
				return domain.ResultReviewRequired, []string{domain.PRV016AssessmentRequired}, nil
			}
			if tResp.Result == "CONDITIONAL" {
				transferConstraints = append(transferConstraints, domain.DecisionConstraint{
					Type:        "RECIPIENT_LIMITATION",
					Description: "Transfer conditional on recipient adhering to processor contractual obligations and destination jurisdiction safeguards",
					Parameters: map[string]interface{}{
						"destination_jurisdiction": req.TransferCheck.DestinationJurisdiction,
						"mechanism_id":             req.TransferCheck.TransferMechanismID,
					},
				})
			}
		}
	}

	// Step 7: Minimization / sensitive-data / secondary-purpose review gate.
	//
	// §13.1 Safeguard 5 (no sensitive-data downgrade), I14 (secondary
	// purpose requires explicit compatibility/policy evaluation), and the
	// HR domain-adoption pattern's "special/sensitive categories require
	// explicit category/condition references" (§21.1) all name conditions
	// this service can detect from caller-declared context but cannot
	// itself adjudicate — doing so would mean inventing the exact
	// minimization/compatibility rules §13 step 5 assigns to PDC (no PDC
	// exists anywhere in this codebase; same documented gap as PRV-01/05).
	// The platform's own fail-closed doctrine for this situation is
	// explicit (§26 runbooks: "otherwise INDETERMINATE/REVIEW_REQUIRED —
	// never 'best guess'"), so a detected-but-unresolvable condition here
	// routes to REVIEW_REQUIRED with a real §32 reason code, not a
	// fabricated RESTRICT constraint.
	var reviewReasons []string
	if req.SecondaryPurposeID != "" || req.ProposedSecondaryPurpose != "" {
		reviewReasons = append(reviewReasons, domain.PRV009PurposeIncompatible)
	}
	if req.SubjectContext != nil && strings.ToUpper(req.SubjectContext.AgeBand) == "MINOR" {
		reviewReasons = append(reviewReasons, domain.PRV010DataCategoryRestricted)
	}
	if req.DataContext != nil {
		for _, flag := range req.DataContext.SensitivityFlags {
			u := strings.ToUpper(flag)
			if u == "SENSITIVE" || u == "SPECIAL_CATEGORY" || u == "HEALTH" || u == "BIOMETRIC" || u == "HIGH_RISK" {
				reviewReasons = append(reviewReasons, domain.PRV010DataCategoryRestricted)
				break
			}
		}
	}
	if len(reviewReasons) > 0 {
		return domain.ResultReviewRequired, dedupeReasons(reviewReasons), nil
	}

	// A real CONDITIONAL transfer authorization is the one legitimate
	// source of RESTRICT constraint content this version has: it comes
	// from privacy-transfer-svc's own already-evaluated decision, not
	// invented here.
	if len(transferConstraints) > 0 {
		return domain.ResultRestrict, []string{domain.PRV011MinimizationRequired}, transferConstraints
	}

	// Step 8: All conditions satisfied — PERMIT (§12.2).
	return domain.ResultPermit, []string{}, nil
}

// GetDecision handles GET /privacy/decisions/{decisionID} and GET /v1/privacy/decisions/{decisionID} —
// retrieving a past decision's durable evidence record per §13.2.
func (h *Handler) GetDecision(w http.ResponseWriter, r *http.Request) {
	decisionID := chi.URLParam(r, "decisionID")
	d, err := h.store.FindDecision(r.Context(), decisionID)
	if err != nil {
		if errors.Is(err, domain.ErrDecisionNotFound) {
			writeError(w, http.StatusNotFound, "privacy decision not found")
			return
		}
		h.log.Error("GetDecision: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, d)
}
