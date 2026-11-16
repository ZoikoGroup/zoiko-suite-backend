package handler

// The register summary, per-decision evidence, the tenant-wide replay history
// and the G-07 evidence ledger (verify, export, seal, packages).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	svcmiddleware "zoiko.io/governance-decision-log-svc/internal/middleware"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

// maxSummaryWindow bounds GET /v1/decisions/summary: the trend is one point
// per day, and a year of points is the most a chart can show.
const maxSummaryWindow = 366 * 24 * time.Hour

func problem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	apicontract.WriteProblem(w, r, apicontract.Problem{Status: status, Code: code, Title: title, Detail: detail})
}

func (h *Handler) storeUnavailable(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.log.Error(op+": store unavailable", zap.Error(err))
	problem(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service Unavailable", "Store unavailable")
}

// tenantAndReader resolves the tenant and the reading principal, answering
// 400/401 itself when either is missing.
func (h *Handler) tenantAndReader(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		problem(w, r, http.StatusBadRequest, "TENANT_SCOPE_MISSING", "Tenant Scope Missing", "X-Tenant-Id is required")
		return "", "", false
	}
	principalID, ok := h.readPrincipal(w, r)
	return tenantID, principalID, ok
}

// pageParams reads limit/offset, refusing malformed values.
func pageParams(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	limit, offset := 0, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Limit", "limit must be a positive integer")
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Offset", "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// DecisionSummary handles GET /v1/decisions/summary.
//
// Query: from, to (RFC3339; default the 30 days to now) or days=N (1–366,
// ending now); entity narrows the decision figures to one legal entity (and is
// then authorized on it). The window is [from, to); the previous window is
// the same length immediately before it.
//
//	200 → domain.DecisionSummary
//	400 → bad window
//	401/403/503 → as every read
func (h *Handler) DecisionSummary(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.tenantAndReader(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	entity := strings.TrimSpace(q.Get("entity"))
	if !h.authorizeReadOf(w, r, principalID, entity) {
		return
	}

	to := h.now()
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Timestamp", "to must be a valid RFC3339 timestamp")
			return
		}
		to = t.UTC()
	}
	from := to.Add(-30 * 24 * time.Hour)
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 366 {
			problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Window", "days must be an integer from 1 to 366")
			return
		}
		from = to.Add(-time.Duration(n) * 24 * time.Hour)
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Timestamp", "from must be a valid RFC3339 timestamp")
			return
		}
		from = t.UTC()
	}
	if !from.Before(to) || to.Sub(from) > maxSummaryWindow {
		problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Window", "from must be before to, and the window at most 366 days")
		return
	}

	sum, err := h.store.Summary(r.Context(), store.SummaryParams{TenantID: tenantID, LegalEntityID: entity, From: from, To: to})
	if err != nil {
		h.storeUnavailable(w, r, "DecisionSummary", err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// GetDecisionEvidence handles GET /v1/decisions/{decision_id}/evidence: the
// decision's chain link re-computed, the package that sealed it, and the
// events queued for it. Authorized like GetDecision.
func (h *Handler) GetDecisionEvidence(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.tenantAndReader(w, r)
	if !ok {
		return
	}
	decisionID := chi.URLParam(r, "decision_id")
	d, err := h.store.FindByID(r.Context(), tenantID, decisionID)
	if errors.Is(err, domain.ErrDecisionNotFound) {
		if h.authorizeReadOf(w, r, principalID, "") {
			problem(w, r, http.StatusNotFound, "NOT_FOUND", "Decision Not Found", "No decision with this decision_id for this tenant")
		}
		return
	}
	if err != nil {
		h.storeUnavailable(w, r, "GetDecisionEvidence", err)
		return
	}
	if !h.authorizeReadOf(w, r, principalID, d.LegalEntityID) {
		return
	}
	ev, err := h.store.DecisionEvidence(r.Context(), tenantID, decisionID)
	if err != nil {
		h.storeUnavailable(w, r, "GetDecisionEvidence", err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// ListAllReplayManifests handles GET /v1/replay-manifests: every replay in the
// tenant, newest first (limit, offset, mismatches=true). Tenant-wide read.
func (h *Handler) ListAllReplayManifests(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.tenantAndReader(w, r)
	if !ok {
		return
	}
	if !h.authorizeReadOf(w, r, principalID, "") {
		return
	}
	limit, offset, ok := pageParams(w, r)
	if !ok {
		return
	}
	mismatches := strings.EqualFold(r.URL.Query().Get("mismatches"), "true")
	records, total, err := h.store.ListReplayManifests(r.Context(), store.ReplayListParams{
		TenantID: tenantID, MismatchesOnly: mismatches, Limit: limit, Offset: offset,
	})
	if err != nil {
		h.storeUnavailable(w, r, "ListAllReplayManifests", err)
		return
	}
	if records == nil {
		records = []*domain.ReplayManifest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records, "total_count": total, "limit": limit, "offset": offset})
}

// VerifyChain handles GET /v1/evidence/chain/verify: re-computes every link of
// the tenant's chain and every sealed package. 200 whether or not the chain
// verifies — valid=false with first_break and the failing packages is the
// answer, not an error. Tenant-wide read.
func (h *Handler) VerifyChain(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.tenantAndReader(w, r)
	if !ok {
		return
	}
	if !h.authorizeReadOf(w, r, principalID, "") {
		return
	}
	v, err := h.store.VerifyChain(r.Context(), tenantID)
	if err != nil {
		h.storeUnavailable(w, r, "VerifyChain", err)
		return
	}
	if !v.Valid {
		h.log.Warn("evidence chain does not verify", zap.String("tenant_id", tenantID), zap.Any("first_break", v.FirstBreak))
	}
	writeJSON(w, http.StatusOK, v)
}

// ExportChain handles GET /v1/evidence/chain/export: the tenant's evidence
// bundle (every decision with its canonical form, packages, replays and the
// verification result), as one JSON document to download. Built before the
// first byte is written, so a failure is a 503 and never a truncated bundle.
func (h *Handler) ExportChain(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.tenantAndReader(w, r)
	if !ok {
		return
	}
	if !h.authorizeReadOf(w, r, principalID, "") {
		return
	}
	bundle, err := h.store.ExportChain(r.Context(), tenantID)
	if err != nil {
		h.storeUnavailable(w, r, "ExportChain", err)
		return
	}
	body, err := json.Marshal(bundle)
	if err != nil {
		h.storeUnavailable(w, r, "ExportChain", err)
		return
	}
	h.log.Info("evidence bundle exported", zap.String("tenant_id", tenantID), zap.String("principal_id", principalID),
		zap.Int("decisions", len(bundle.Decisions)))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="governance-evidence-%s.json"`,
		bundle.ExportedAt.Format("20060102T150405Z")))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

type sealRequest struct {
	ThroughDecisionID string  `json:"through_decision_id,omitempty"`
	Label             *string `json:"label,omitempty"`
	Reason            *string `json:"reason,omitempty"`
}

// SealPackage handles POST /v1/evidence/packages/seal: seals the chain through
// through_decision_id (the head when omitted) into an append-only package.
// Needs GOVERNANCE_EVIDENCE_SEAL on the platform scope and an Idempotency-Key.
//
//	201 → sealed
//	200 → retry of a seal already made under this Idempotency-Key
//	400 → missing key, bad JSON, over-long label/reason
//	404 → through_decision_id is not a decision of this tenant
//	409 → IDEMPOTENCY_MISMATCH, NOTHING_TO_SEAL, CHAIN_BROKEN
//	401/403/503
func (h *Handler) SealPackage(w http.ResponseWriter, r *http.Request) {
	// Principal first, then tenant — the same order and statuses as
	// CreateDecision: an unidentified caller is 401 whatever else it omitted.
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		problem(w, r, http.StatusUnauthorized, "TENANT_SCOPE_MISSING", "Tenant Scope Missing", domain.ErrTenantScopeMissing.Error())
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		problem(w, r, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency Key Required", "Idempotency-Key is required for material state changes")
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	var req sealRequest
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid JSON", err.Error())
			return
		}
	}
	trimmed := func(p *string) *string {
		if p == nil || strings.TrimSpace(*p) == "" {
			return nil
		}
		v := strings.TrimSpace(*p)
		return &v
	}
	req.Label, req.Reason = trimmed(req.Label), trimmed(req.Reason)
	req.ThroughDecisionID = strings.TrimSpace(req.ThroughDecisionID)
	limits := []field{
		{"Idempotency-Key", key, 256},
		{"X-Principal-Id", principalID, 64},
		{"through_decision_id", req.ThroughDecisionID, 64},
		{"label", ptr(req.Label), 200},
		{"reason", ptr(req.Reason), 500},
	}
	if name := tooLong(limits...); name != "" {
		writeTooLong(w, r, name, limits)
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSealEvidence) {
		return
	}

	hash := requestHash(principalID, "", "", "evidence-seal", raw)
	pkg, created, err := h.store.SealPackage(r.Context(), store.SealRequest{
		TenantID: tenantID, PrincipalID: principalID, ThroughDecisionID: req.ThroughDecisionID,
		Label: req.Label, Reason: req.Reason, IdempotencyKey: key, RequestHash: hash,
	})
	switch {
	case errors.Is(err, domain.ErrDecisionNotFound):
		problem(w, r, http.StatusNotFound, "NOT_FOUND", "Decision Not Found", "through_decision_id is not a decision of this tenant")
		return
	case errors.Is(err, domain.ErrNothingToSeal):
		problem(w, r, http.StatusConflict, "NOTHING_TO_SEAL", "Nothing To Seal", "every decision through that point is already sealed (or the ledger is empty)")
		return
	case errors.Is(err, domain.ErrChainBroken):
		problem(w, r, http.StatusConflict, "CHAIN_BROKEN", "Evidence Chain Broken", "the chain does not verify through that point; sealing it would certify tampered evidence — run /v1/evidence/chain/verify")
		return
	case err != nil:
		h.storeUnavailable(w, r, "SealPackage", err)
		return
	}
	if !created {
		if string(pkg.RequestHash) != string(hash) {
			problem(w, r, http.StatusConflict, "IDEMPOTENCY_MISMATCH", "Idempotency Mismatch", "Idempotency-Key reused with a different request")
			return
		}
		writeJSON(w, http.StatusOK, pkg)
		return
	}
	h.log.Info("evidence package sealed", zap.String("tenant_id", tenantID), zap.String("package_id", pkg.PackageID),
		zap.Int64("to_seq", pkg.ToSeq), zap.String("merkle_root", pkg.MerkleRoot))
	writeJSON(w, http.StatusCreated, pkg)
}

// ListPackages handles GET /v1/evidence/packages, newest first. Tenant-wide read.
func (h *Handler) ListPackages(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.tenantAndReader(w, r)
	if !ok {
		return
	}
	if !h.authorizeReadOf(w, r, principalID, "") {
		return
	}
	pkgs, err := h.store.ListPackages(r.Context(), tenantID)
	if err != nil {
		h.storeUnavailable(w, r, "ListPackages", err)
		return
	}
	if pkgs == nil {
		pkgs = []*domain.EvidencePackage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": pkgs, "total_count": len(pkgs)})
}

// GetPackage handles GET /v1/evidence/packages/{package_id}. Tenant-wide read:
// a package covers every entity's decisions.
func (h *Handler) GetPackage(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.tenantAndReader(w, r)
	if !ok {
		return
	}
	if !h.authorizeReadOf(w, r, principalID, "") {
		return
	}
	pkg, err := h.store.GetPackage(r.Context(), tenantID, chi.URLParam(r, "package_id"))
	if errors.Is(err, domain.ErrPackageNotFound) {
		problem(w, r, http.StatusNotFound, "NOT_FOUND", "Package Not Found", "No evidence package with this id for this tenant")
		return
	}
	if err != nil {
		h.storeUnavailable(w, r, "GetPackage", err)
		return
	}
	writeJSON(w, http.StatusOK, pkg)
}

// VerifyPackage handles GET /v1/evidence/packages/{package_id}/verify:
// re-computes the package's Merkle root and chain hash from the decisions as
// stored now. 200 with valid=false is a finding, not an error.
func (h *Handler) VerifyPackage(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.tenantAndReader(w, r)
	if !ok {
		return
	}
	if !h.authorizeReadOf(w, r, principalID, "") {
		return
	}
	check, err := h.store.VerifyPackage(r.Context(), tenantID, chi.URLParam(r, "package_id"))
	if errors.Is(err, domain.ErrPackageNotFound) {
		problem(w, r, http.StatusNotFound, "NOT_FOUND", "Package Not Found", "No evidence package with this id for this tenant")
		return
	}
	if err != nil {
		h.storeUnavailable(w, r, "VerifyPackage", err)
		return
	}
	writeJSON(w, http.StatusOK, check)
}
