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
	"zoiko.io/privacy-decision-svc/internal/purposeregistry"
	"zoiko.io/privacy-decision-svc/internal/retentionregistry"
	"zoiko.io/privacy-decision-svc/internal/store"
	"zoiko.io/privacy-decision-svc/internal/transferregistry"
)

// PurposeChecker, ConsentChecker, HoldChecker and TransferChecker are the narrow
// interfaces the handler depends on.
type PurposeChecker interface {
	ResolveActivity(ctx context.Context, tenantID, principalID, activityID string) (*purposeregistry.ActivityVersion, error)
	ResolveActivityVersion(ctx context.Context, tenantID, principalID, activityID, versionID string) (*purposeregistry.ActivityVersion, error)
	ResolvePurpose(ctx context.Context, tenantID, principalID, purposeID string) (*purposeregistry.PurposeVersion, error)
}

type ConsentChecker interface {
	ResolveStatus(ctx context.Context, tenantID, principalID, subjectRef, purposeID string) (*consentregistry.ConsentResolution, error)
}

type HoldChecker interface {
	Resolve(ctx context.Context, tenantID, principalID, recordClass, entityRef string) (*retentionregistry.RetentionResolution, error)
}

type TransferChecker interface {
	EvaluateTransfer(ctx context.Context, tenantID, principalID string, req *transferregistry.EvaluateTransferRequest) (*transferregistry.TransferDecision, error)
}

type Handler struct {
	store     store.Store
	purposes  PurposeChecker
	consents  ConsentChecker
	holds     HoldChecker
	transfers TransferChecker
	log       *zap.Logger
}

func New(
	st store.Store,
	purposes PurposeChecker,
	consents ConsentChecker,
	holds HoldChecker,
	transfers TransferChecker,
	log *zap.Logger,
) *Handler {
	return &Handler{
		store:     st,
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

type errorResponse struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeCoded(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg, ErrorCode: code})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principalID == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
	if tenantID == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeTenantScopeRequired, "X-Tenant-Id header is required")
		return "", false
	}
	return tenantID, true
}

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

// maxBodyBytes bounds a request body (1 MiB), the same limit the sibling privacy
// services apply. A decision request is a handful of identifiers and short enums.
const maxBodyBytes = 1 << 20

// readBodyBytes reads the request body through http.MaxBytesReader, so a body
// larger than maxBodyBytes is refused after at most maxBodyBytes+1 bytes have
// been read instead of being buffered whole; the server also stops reading the
// connection. The caller answers a read error as 400 VALIDATION_FAILED, the
// established refusal for an unreadable body.
func readBodyBytes(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return []byte{}, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// EvaluateDecision handles POST /privacy/decisions and POST /v1/privacy/decisions —
// the runtime purpose-binding decision described in ZS-SVC-W-001 §12/§13, §18.
//
// Error contract (status / error_code), all JSON {"error","error_code"}:
//
//	401 AUTHENTICATION_FAILED        missing X-Principal-Id
//	400 TENANT_SCOPE_REQUIRED        missing X-Tenant-Id
//	400 CORRELATION_ID_REQUIRED      missing X-Correlation-ID
//	400 IDEMPOTENCY_KEY_REQUIRED     missing Idempotency-Key
//	400 VALIDATION_FAILED            unreadable body (incl. larger than 1 MiB), invalid body or required field
//	403 TENANT_SCOPE_MISMATCH        body tenant_id != X-Tenant-Id
//	409 PRV-020                      Idempotency-Key reused with a different payload
//	503 STORE_UNAVAILABLE            lookup or the atomic decision+idempotency commit failed
//	500 INTERNAL_ERROR               response could not be rendered
func (h *Handler) EvaluateDecision(w http.ResponseWriter, r *http.Request) {
	// 1. Mandatory Actor Authentication
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// 2. Mandatory Tenant Scope
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	// 3. Mandatory Correlation ID (§18.1), echoed on every response from here on.
	correlationID := strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
	if correlationID == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeCorrelationRequired, "X-Correlation-ID header is required")
		return
	}
	w.Header().Set("X-Correlation-ID", correlationID)

	// 4. Mandatory Idempotency Key (§18.1)
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeIdempotencyRequired, "Idempotency-Key header is required")
		return
	}

	bodyBytes, err := readBodyBytes(w, r)
	if err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "failed to read request body (the body must not exceed 1 MiB)")
		return
	}
	reqHash := hashBody(bodyBytes)

	// 5. Idempotency replay check before processing (§18.1). Scope is
	// (tenant, principal, key): a key never replays another principal's decision.
	existing, err := h.store.GetIdempotency(r.Context(), tenantID, principalID, idemKey)
	if err != nil {
		h.log.Error("EvaluateDecision: idempotency lookup failed", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "store unavailable")
		return
	}
	if existing != nil {
		h.writeReplay(w, existing, reqHash)
		return
	}

	// 6. Request payload validation
	var req domain.EvaluateDecisionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if req.TenantID != "" && req.TenantID != tenantID {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeTenantScopeMismatch, "tenant_id does not match the verified X-Tenant-Id")
		return
	}
	if strings.TrimSpace(req.SubjectRef) == "" || strings.TrimSpace(req.ProcessingActivityID) == "" || strings.TrimSpace(req.PurposeID) == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "subject_ref, processing_activity_id and purpose_id are required")
		return
	}
	if !req.ProposedOperation.Valid() {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "proposed_operation is missing or not a recognized value")
		return
	}

	fingerprint := domain.ComputeInputFingerprint(&req, tenantID)
	decision := &domain.PrivacyDecision{
		InputFingerprint:     fingerprint,
		ContextHash:          fingerprint,
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
		PolicyRefs:           []string{},
	}
	// Only a registered secondary-purpose ID is ever recorded; free text is
	// never stored as a purpose identifier (I14, N02).
	if sp := strings.TrimSpace(req.SecondaryPurposeID); sp != "" {
		decision.SecondaryPurposeID = &sp
	}

	// 7. Core runtime evaluation sequence
	// The correlation id and idempotency key of THIS request travel with the
	// evaluation, so the call to privacy-transfer-svc (a mutating route there)
	// carries the §18.1 context it requires.
	evalCtx := transferregistry.WithRequestContext(r.Context(), correlationID, idemKey)
	out := h.evaluate(evalCtx, tenantID, principalID, &req, decision)
	decision.Result = out.result
	decision.ReasonCodes = out.reasons
	if out.constraints != nil {
		decision.Constraints = out.constraints
	}
	decision.ExpiresAt = out.expiresAt

	// 8. Durable record + idempotency record in ONE transaction (§13.2, §18.1).
	claim := domain.IdempotencyRecord{
		Key: idemKey, TenantID: tenantID, PrincipalID: principalID,
		Endpoint: "EvaluateDecision", RequestHash: reqHash,
	}
	var renderErr error
	existing, err = h.store.CommitDecision(r.Context(), claim, decision, func(d *domain.PrivacyDecision) (int, []byte, error) {
		body, mErr := json.Marshal(d)
		if mErr != nil {
			renderErr = mErr
		}
		return http.StatusOK, body, mErr
	})
	if err != nil {
		if renderErr != nil {
			writeCoded(w, http.StatusInternalServerError, domain.ErrCodeInternal, "failed to marshal response")
			return
		}
		h.log.Error("EvaluateDecision: atomic commit failed; nothing persisted, safe to retry", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "store unavailable")
		return
	}
	if existing != nil {
		// A concurrent request with the same key won the claim.
		h.writeReplay(w, existing, reqHash)
		return
	}

	respBody, err := json.Marshal(decision)
	if err != nil {
		writeCoded(w, http.StatusInternalServerError, domain.ErrCodeInternal, "failed to marshal response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)
}

// writeReplay returns the stored original response, or 409 PRV-020 when the
// key was used with a different payload.
func (h *Handler) writeReplay(w http.ResponseWriter, existing *domain.IdempotencyRecord, reqHash string) {
	if existing.RequestHash != reqHash {
		writeCoded(w, http.StatusConflict, domain.ErrCodeImmutableEvidenceConflict, domain.ErrIdempotencyConflict.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotency-Replay", "true")
	w.WriteHeader(existing.ResponseCode)
	_, _ = w.Write(existing.ResponseBody)
}

// GetDecision handles GET /privacy/decisions/{decisionID} and GET /v1/privacy/decisions/{decisionID} —
// retrieving a past decision's durable evidence record per §13.2.
// Authenticated and strictly scoped to caller's verified tenant to prevent cross-tenant access.
func (h *Handler) GetDecision(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if c := strings.TrimSpace(r.Header.Get("X-Correlation-ID")); c != "" {
		w.Header().Set("X-Correlation-ID", c)
	}

	decisionID := chi.URLParam(r, "decisionID")
	d, err := h.store.FindDecision(r.Context(), tenantID, decisionID)
	if err != nil {
		if errors.Is(err, domain.ErrDecisionNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodeNotFound, "privacy decision not found")
			return
		}
		h.log.Error("GetDecision: store unavailable", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, d)
}
