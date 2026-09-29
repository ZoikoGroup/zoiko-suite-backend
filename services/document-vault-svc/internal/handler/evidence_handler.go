package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

const (
	actionAuditEvidenceRegister = "AUDIT_EVIDENCE_REGISTER"
	actionAuditEvidenceManage   = "AUDIT_EVIDENCE_MANAGE"
	actionAuditEvidenceRead     = "AUDIT_EVIDENCE_READ"
)

// EvidenceStore is AUD-06's own persistence contract, composed onto the
// existing Handler the same way AUD-05 composed PBCStore onto
// evidence-requirements-svc's handler — kept separate from the existing
// Store interface so this file's scope is self-contained.
type EvidenceStore interface {
	RegisterEvidence(ctx context.Context, p domain.RegisterEvidenceParams) (*domain.AuditEvidence, bool, error)
	GetEvidence(ctx context.Context, tenantID, evidenceID string) (*domain.AuditEvidence, error)
	VerifyIntegrity(ctx context.Context, p domain.VerifyIntegrityParams) (*domain.AuditEvidence, bool, error)
	AssessReliability(ctx context.Context, p domain.AssessReliabilityParams) (*domain.EvidenceReliabilityAssessment, error)
	GetReliabilityAssessments(ctx context.Context, tenantID, evidenceID string) ([]*domain.EvidenceReliabilityAssessment, error)
	LinkToProcedure(ctx context.Context, p domain.LinkToProcedureParams) (*domain.EvidenceProcedureLink, error)
	GetProcedureLinks(ctx context.Context, tenantID, evidenceID string) ([]*domain.EvidenceProcedureLink, error)
	RecordContradiction(ctx context.Context, p domain.RecordContradictionParams) (*domain.EvidenceContradiction, error)
	ListContradictions(ctx context.Context, tenantID, evidenceID string) ([]*domain.EvidenceContradiction, error)
	RestrictEvidence(ctx context.Context, p domain.RestrictEvidenceParams) (*domain.AuditEvidence, error)
	QuarantineEvidence(ctx context.Context, p domain.QuarantineEvidenceParams) (*domain.AuditEvidence, error)
	SupersedeEvidence(ctx context.Context, p domain.SupersedeEvidenceParams) (*domain.EvidenceVersion, error)
	GetCustodyHistory(ctx context.Context, tenantID, evidenceID string) ([]*domain.CustodyEntry, error)
}

func RegisterEvidenceRoutes(r chi.Router, h *Handler, evidenceStore EvidenceStore) {
	eh := &evidenceHandler{Handler: h, store: evidenceStore}
	r.Route("/v1/audit-evidence", func(r chi.Router) {
		r.Post("/", eh.RegisterEvidence)
		r.Get("/{evidence_id}", eh.GetEvidence)
		r.Post("/{evidence_id}/verify-integrity", eh.VerifyIntegrity)
		r.Post("/{evidence_id}/assess-reliability", eh.AssessReliability)
		r.Get("/{evidence_id}/reliability", eh.GetReliabilityAssessments)
		r.Post("/{evidence_id}/link", eh.LinkToProcedure)
		r.Get("/{evidence_id}/links", eh.GetProcedureLinks)
		r.Post("/{evidence_id}/contradictions", eh.RecordContradiction)
		r.Get("/{evidence_id}/contradictions", eh.ListContradictions)
		r.Post("/{evidence_id}/restrict", eh.RestrictEvidence)
		r.Post("/{evidence_id}/quarantine", eh.QuarantineEvidence)
		r.Post("/{evidence_id}/supersede", eh.SupersedeEvidence)
		r.Get("/{evidence_id}/custody", eh.GetCustodyHistory)
	})
}

type evidenceHandler struct {
	*Handler
	store EvidenceStore
}

type registerEvidenceRequest struct {
	DocumentID        string `json:"document_id"`
	DocumentVersionID string `json:"document_version_id"`
	LegalEntityID     string `json:"legal_entity_id"`
	EngagementID      string `json:"engagement_id"`
	EvidenceSource    string `json:"evidence_source"`
	AcquisitionMethod string `json:"acquisition_method"`
	CorrelationID     string `json:"correlation_id"`
}

// RegisterEvidence requires evidence_source/acquisition_method — the real
// enforcement of "evidence source and acquisition method mandatory,"
// backed at the DB layer by migration 000003's own CHECK constraints.
func (h *evidenceHandler) RegisterEvidence(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req registerEvidenceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.EvidenceSource == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "evidence_source")
		return
	}
	if req.AcquisitionMethod == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "acquisition_method")
		return
	}
	if req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "correlation_id")
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, actionAuditEvidenceRegister) {
		return
	}
	evidence, created, err := h.store.RegisterEvidence(r.Context(), domain.RegisterEvidenceParams{
		DocumentID: req.DocumentID, TenantID: tenantID, LegalEntityID: req.LegalEntityID, EngagementID: req.EngagementID,
		EvidenceSource: req.EvidenceSource, AcquisitionMethod: req.AcquisitionMethod, RegisteredByPrincipalID: principalID,
		CorrelationID: req.CorrelationID, DocumentVersionID: req.DocumentVersionID,
	})
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, evidence)
}

func (h *evidenceHandler) getEvidenceForAccess(w http.ResponseWriter, r *http.Request, action string) (*domain.AuditEvidence, bool) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	evidence, err := h.store.GetEvidence(r.Context(), tenantID, chi.URLParam(r, "evidence_id"))
	if err != nil {
		h.writeEvidenceErr(w, err)
		return nil, false
	}
	if !h.authorize(w, r, principalID, evidence.LegalEntityID, action) {
		return nil, false
	}
	return evidence, true
}

func (h *evidenceHandler) GetEvidence(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, evidence)
}

// VerifyIntegrity recomputes against the document's own stored checksum
// — see PgStore.VerifyIntegrity's own doc comment.
func (h *evidenceHandler) VerifyIntegrity(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceManage)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	updated, matches, err := h.store.VerifyIntegrity(r.Context(), domain.VerifyIntegrityParams{EvidenceID: evidence.EvidenceID, ActorPrincipalID: principalID})
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": updated, "integrity_verified": matches})
}

type assessReliabilityRequest struct {
	ReliabilityRating string `json:"reliability_rating"`
	Rationale         string `json:"rationale"`
}

func (h *evidenceHandler) AssessReliability(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceManage)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	var req assessReliabilityRequest
	if !decodeJSON(w, r, &req) || req.ReliabilityRating == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reliability_rating")
		return
	}
	assessment, err := h.store.AssessReliability(r.Context(), domain.AssessReliabilityParams{
		EvidenceID: evidence.EvidenceID, AssessedByPrincipalID: principalID, ReliabilityRating: req.ReliabilityRating, Rationale: req.Rationale,
	})
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, assessment)
}

func (h *evidenceHandler) GetReliabilityAssessments(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceRead)
	if !ok {
		return
	}
	assessments, err := h.store.GetReliabilityAssessments(r.Context(), evidence.TenantID, evidence.EvidenceID)
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, assessments)
}

type linkToProcedureRequest struct {
	ProcedureRef string `json:"procedure_ref"`
}

func (h *evidenceHandler) LinkToProcedure(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceManage)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	var req linkToProcedureRequest
	if !decodeJSON(w, r, &req) || req.ProcedureRef == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "procedure_ref")
		return
	}
	link, err := h.store.LinkToProcedure(r.Context(), domain.LinkToProcedureParams{EvidenceID: evidence.EvidenceID, ProcedureRef: req.ProcedureRef, LinkedByPrincipalID: principalID})
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, link)
}

func (h *evidenceHandler) GetProcedureLinks(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceRead)
	if !ok {
		return
	}
	links, err := h.store.GetProcedureLinks(r.Context(), evidence.TenantID, evidence.EvidenceID)
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, links)
}

type recordContradictionRequest struct {
	Description             string  `json:"description"`
	ContradictingEvidenceID *string `json:"contradicting_evidence_id,omitempty"`
}

// RecordContradiction never removes or hides prior evidence — "contradictory
// evidence retained." There is no corresponding delete route anywhere in
// this handler.
func (h *evidenceHandler) RecordContradiction(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceManage)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	var req recordContradictionRequest
	if !decodeJSON(w, r, &req) || req.Description == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "description")
		return
	}
	contradiction, err := h.store.RecordContradiction(r.Context(), domain.RecordContradictionParams{
		EvidenceID: evidence.EvidenceID, Description: req.Description, RecordedByPrincipalID: principalID, ContradictingEvidenceID: req.ContradictingEvidenceID,
	})
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, contradiction)
}

func (h *evidenceHandler) ListContradictions(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceRead)
	if !ok {
		return
	}
	contradictions, err := h.store.ListContradictions(r.Context(), evidence.TenantID, evidence.EvidenceID)
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, contradictions)
}

func (h *evidenceHandler) RestrictEvidence(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceManage)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	updated, err := h.store.RestrictEvidence(r.Context(), domain.RestrictEvidenceParams{EvidenceID: evidence.EvidenceID, ActorPrincipalID: principalID})
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type quarantineEvidenceRequest struct {
	Reason string `json:"reason"`
}

func (h *evidenceHandler) QuarantineEvidence(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceManage)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	var req quarantineEvidenceRequest
	if !decodeJSON(w, r, &req) || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	updated, err := h.store.QuarantineEvidence(r.Context(), domain.QuarantineEvidenceParams{EvidenceID: evidence.EvidenceID, Reason: req.Reason, ActorPrincipalID: principalID})
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type supersedeEvidenceRequest struct {
	NewDocumentVersionID string `json:"new_document_version_id"`
}

func (h *evidenceHandler) SupersedeEvidence(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceManage)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	var req supersedeEvidenceRequest
	if !decodeJSON(w, r, &req) || req.NewDocumentVersionID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "new_document_version_id")
		return
	}
	version, err := h.store.SupersedeEvidence(r.Context(), domain.SupersedeEvidenceParams{EvidenceID: evidence.EvidenceID, NewDocumentVersionID: req.NewDocumentVersionID, ActorPrincipalID: principalID})
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

func (h *evidenceHandler) GetCustodyHistory(w http.ResponseWriter, r *http.Request) {
	evidence, ok := h.getEvidenceForAccess(w, r, actionAuditEvidenceRead)
	if !ok {
		return
	}
	history, err := h.store.GetCustodyHistory(r.Context(), evidence.TenantID, evidence.EvidenceID)
	if err != nil {
		h.writeEvidenceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, history)
}

func (h *evidenceHandler) writeEvidenceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrEvidenceNotFound):
		writeError(w, http.StatusNotFound, "evidence_not_found", "")
	case errors.Is(err, domain.ErrEvidenceVersionNotFound):
		writeError(w, http.StatusNotFound, "evidence_version_not_found", "")
	case errors.Is(err, domain.ErrDocumentNotFound):
		writeError(w, http.StatusUnprocessableEntity, "document_not_found", "")
	case errors.Is(err, domain.ErrEvidenceSourceRequired):
		writeError(w, http.StatusBadRequest, "evidence_source_required", "")
	case errors.Is(err, domain.ErrAcquisitionMethodRequired):
		writeError(w, http.StatusBadRequest, "acquisition_method_required", "")
	default:
		h.log.Error("audit evidence store error")
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}
