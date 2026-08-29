// Package handler exposes privacy-decision-svc's REST API — PRV-03.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/consentregistry"
	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/events"
	svcmiddleware "zoiko.io/privacy-decision-svc/internal/middleware"
	"zoiko.io/privacy-decision-svc/internal/purposeregistry"
	"zoiko.io/privacy-decision-svc/internal/retentionregistry"
	"zoiko.io/privacy-decision-svc/internal/store"
)

// PurposeChecker, ConsentChecker and HoldChecker are the narrow
// interfaces the handler depends on — satisfied by the real HTTP clients
// and stubbable in tests.
type PurposeChecker interface {
	ResolveActivity(ctx context.Context, activityID string) (*purposeregistry.ActivityVersion, error)
	ResolvePurpose(ctx context.Context, purposeID string) (*purposeregistry.PurposeVersion, error)
}

type ConsentChecker interface {
	ResolveStatus(ctx context.Context, subjectRef, purposeID string) (*consentregistry.ConsentResolution, error)
}

type HoldChecker interface {
	Resolve(ctx context.Context, tenantID, recordClass, entityRef string) (*retentionregistry.RetentionResolution, error)
}

type Handler struct {
	store    store.Store
	pub      events.Publisher
	purposes PurposeChecker
	consents ConsentChecker
	holds    HoldChecker
	log      *zap.Logger
}

func New(st store.Store, pub events.Publisher, purposes PurposeChecker, consents ConsentChecker, holds HoldChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, purposes: purposes, consents: consents, holds: holds, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
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

// EvaluateDecision handles POST /v1/privacy/decisions — the runtime
// purpose-binding decision described in ZS-SVC-W-001 §12.
//
// Deliberately NOT gated by an authorization-svc check: this is a
// read-oriented evaluation over evidence the caller already has a
// legitimate reason to ask about (its own proposed operation), the same
// posture already established for retention-registry-svc's Resolve route
// — "a read-only check every service needs cheaply and often." The
// caller's own domain-level authorization is a separate, prior concern
// (§12.2's PERMIT result note: "domain/IAM authorization is still
// separately required").
func (h *Handler) EvaluateDecision(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.EvaluateDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
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
		SubjectRef:           req.SubjectRef,
		ProcessingActivityID: req.ProcessingActivityID,
		PurposeID:            req.PurposeID,
		ProposedOperation:    req.ProposedOperation,
		ActorPrincipalID:     principalID,
		CorrelationID:        correlationID,
	}

	result, reasonCodes := h.evaluate(r.Context(), &req, decision)
	decision.Result = result
	decision.ReasonCodes = reasonCodes

	if err := h.store.RecordDecision(r.Context(), tenantID, decision); err != nil {
		h.log.Error("EvaluateDecision: failed to record decision", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.decision.evaluated", EntityID: decision.DecisionID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: correlationID, Payload: decision,
	})

	writeJSON(w, http.StatusOK, decision)
}

// evaluate is the actual decision sequence from ZS-SVC-W-001 Figure 6,
// scoped to what this version can determine from real evidence — see the
// domain package's doc comment for exactly which of the five steps are
// implemented and which (minimization/compatibility evaluation) are not.
// Any dependency being unreachable fails the WHOLE decision closed as
// INDETERMINATE — §12.2: "Material processing fails closed."
func (h *Handler) evaluate(ctx context.Context, req *domain.EvaluateDecisionRequest, decision *domain.PrivacyDecision) (domain.DecisionResult, []string) {
	// Step 1: resolve the processing activity — must be ACTIVE.
	activity, err := h.purposes.ResolveActivity(ctx, req.ProcessingActivityID)
	if err != nil {
		h.log.Error("evaluate: purpose registry unavailable (activity)", zap.Error(err))
		return domain.ResultIndeterminate, []string{domain.ReasonDependencyUnavailable}
	}
	if activity == nil || activity.VersionStatus != "ACTIVE" {
		return domain.ResultBlock, []string{domain.ReasonActivityNotActive}
	}
	decision.ActivityVersionID = &activity.ActivityVersionID

	// Step 1b (PRV-C01, purpose limitation): the proposed purpose must be
	// one this activity actually declares — enforced directly from PRV-01's
	// own registered PurposeIDs, not a rule this service invents.
	boundToActivity := false
	for _, p := range activity.PurposeIDs {
		if p == req.PurposeID {
			boundToActivity = true
			break
		}
	}
	if !boundToActivity {
		return domain.ResultBlock, []string{domain.ReasonPurposeNotBoundToActivity}
	}

	// Resolve the purpose itself — must be PUBLISHED.
	purpose, err := h.purposes.ResolvePurpose(ctx, req.PurposeID)
	if err != nil {
		h.log.Error("evaluate: purpose registry unavailable (purpose)", zap.Error(err))
		return domain.ResultIndeterminate, []string{domain.ReasonDependencyUnavailable}
	}
	if purpose == nil || purpose.VersionStatus != "PUBLISHED" {
		return domain.ResultBlock, []string{domain.ReasonPurposeNotPublished}
	}
	decision.PurposeVersionID = &purpose.PurposeVersionID

	// Step 4a (PRV-C04, consent): opt-in only — see the domain package's
	// doc comment on why this service never infers the requirement itself.
	if req.ConsentCheck != nil && req.ConsentCheck.Required {
		resolution, err := h.consents.ResolveStatus(ctx, req.SubjectRef, req.PurposeID)
		if err != nil {
			h.log.Error("evaluate: consent registry unavailable", zap.Error(err))
			return domain.ResultIndeterminate, []string{domain.ReasonDependencyUnavailable}
		}
		if resolution.LatestReceipt != nil {
			decision.ConsentReceiptID = &resolution.LatestReceipt.ConsentReceiptID
		}
		if resolution.Status != "GRANTED" {
			return domain.ResultBlock, []string{domain.ReasonConsentNotGranted}
		}
	}

	// Step 4b (legal hold): opt-in and caller-supplied record_class only.
	if req.LegalHoldCheck != nil && req.LegalHoldCheck.RecordClass != "" {
		tenantID := svcmiddleware.TenantFromContext(ctx)
		resolution, err := h.holds.Resolve(ctx, tenantID, req.LegalHoldCheck.RecordClass, req.LegalHoldCheck.EntityRef)
		if err != nil {
			h.log.Error("evaluate: retention registry unavailable", zap.Error(err))
			return domain.ResultIndeterminate, []string{domain.ReasonDependencyUnavailable}
		}
		if resolution.Blocked {
			if resolution.MatchedHold != nil {
				decision.LegalHoldID = &resolution.MatchedHold.LegalHoldID
			}
			return domain.ResultBlock, []string{domain.ReasonLegalHoldBlocksUse}
		}
	}

	return domain.ResultPermit, []string{}
}

// GetDecision handles GET /v1/privacy/decisions/{decisionID} — retrieving
// a past decision's evidence record, per §13.2.
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
