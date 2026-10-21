package handler

import (
	"context"
	"errors"
	"strings"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

const (
	actionRecordDeclare = "RECORD_DECLARE"
	actionRecordRelate  = "RECORD_RELATE"
	actionRecordRead    = "RECORD_READ"
)

// RecordsStore is DRC-02's own persistence contract, composed onto the
// existing Handler the same way AUD-06 composed EvidenceStore — kept
// separate from the existing Store interface so this file's scope is
// self-contained.
type RecordsStore interface {
	DeclareRecordV2(ctx context.Context, p domain.DeclareRecordV2Params) (*domain.Record, bool, error)
	GetRecord(ctx context.Context, recordID string) (*domain.Record, error)
	GetRecordByDocumentVersion(ctx context.Context, documentVersionID string) (*domain.Record, error)
	CreateRecordRelationship(ctx context.Context, p domain.CreateRecordRelationshipParams) (*domain.RecordRelationship, error)
	ListRecordRelationships(ctx context.Context, recordID string) ([]domain.RecordRelationship, error)
}

func RegisterRecordsRoutes(r chi.Router, h *Handler, recordsStore RecordsStore) {
	rh := &recordsHandler{Handler: h, store: recordsStore}
	r.Route("/v1/records", func(r chi.Router) {
		r.Post("/", rh.DeclareRecord)
		r.Get("/{record_id}", rh.GetRecord)
		// ZS-SVC-S-001 §9.2: POST /v1/records/{id}:supersede.
		r.Post("/{record_id}:supersede", rh.SupersedeRecord)
		r.Get("/by-version/{document_version_id}", rh.GetRecordByDocumentVersion)
		r.Post("/{record_id}/relationships", rh.CreateRecordRelationship)
		r.Get("/{record_id}/relationships", rh.ListRecordRelationships)
	})
	// ZS-SVC-S-001 §9.2: POST /v1/records:declare — the same handler as POST
	// /v1/records, so there is one declaration path.
	r.Post("/v1/records:declare", rh.DeclareRecord)
}

type recordsHandler struct {
	*Handler
	store RecordsStore
}

type declareRecordRequest struct {
	DocumentID           string `json:"document_id"`
	DocumentVersionID    string `json:"document_version_id"`
	LegalEntityID        string `json:"legal_entity_id"`
	RecordClass          string `json:"record_class"`
	JurisdictionScope    string `json:"jurisdiction_scope"`
	BusinessContext      string `json:"business_context"`
	RetentionScheduleRef string `json:"retention_schedule_ref"`
	DeclarationReason    string `json:"declaration_reason"`
	CorrelationID        string `json:"correlation_id"`
}

// DeclareRecord converts one exact, already-committed document version
// into a governed record — DRC-02's primary command. record_class and
// jurisdiction_scope are mandatory: without them there is nothing for
// a future retention rule (DRC-03) to bind to.
func (h *recordsHandler) DeclareRecord(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req declareRecordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DocumentID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "document_id")
		return
	}

	// The grant that matters is the one on the entity that OWNS the document.
	// It used to be the caller's own legal_entity_id whenever one was supplied,
	// so a principal with the declare grant on entity A could declare a record
	// over entity B's document by naming A. The owning entity is resolved from
	// the document (inherently before authorization — the scope lives on the
	// resource) and is the only scope checked.
	doc, err := h.Handler.store.FindDocumentByID(r.Context(), req.DocumentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, doc.LegalEntityID, actionRecordDeclare) {
		return
	}
	if req.LegalEntityID != "" && req.LegalEntityID != doc.LegalEntityID {
		writeError(w, http.StatusBadRequest, "legal_entity_mismatch",
			"legal_entity_id does not match the legal entity that owns the document")
		return
	}

	if req.DocumentVersionID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "document_version_id")
		return
	}
	if req.RecordClass == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "DRC-005: RECORD_DECLARATION_INCOMPLETE: record_class")
		return
	}
	if req.JurisdictionScope == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "DRC-005: RECORD_DECLARATION_INCOMPLETE: jurisdiction_scope")
		return
	}

	// §4.2 "Business context: domain object, record purpose and declared record
	// class are complete" -> Block; §4.3 makes declaration_reason a mandatory
	// field. DRC-005 RECORD_DECLARATION_INCOMPLETE.
	if strings.TrimSpace(req.BusinessContext) == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "DRC-005: RECORD_DECLARATION_INCOMPLETE: business_context")
		return
	}
	if strings.TrimSpace(req.DeclarationReason) == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "DRC-005: RECORD_DECLARATION_INCOMPLETE: declaration_reason")
		return
	}

	// §4.2 "Residency": the document's storage region must be allowed for the
	// tenant at declaration time (the same registry check upload performs).
	// Fail closed on a mismatch or an unreachable registry. Whether the record
	// CLASS further restricts regions is not defined by the specification.
	if doc.ResidencyRegionCode != nil && *doc.ResidencyRegionCode != "" {
		if err := h.Handler.residency.CheckRegion(r.Context(), tenantID, *doc.ResidencyRegionCode); err != nil {
			h.handleResidencyError(w, err)
			return
		}
	}

	rec, _, err := h.store.DeclareRecordV2(r.Context(), domain.DeclareRecordV2Params{
		DocumentID: req.DocumentID, DocumentVersionID: req.DocumentVersionID, LegalEntityID: doc.LegalEntityID,
		RecordClass: req.RecordClass, JurisdictionScope: req.JurisdictionScope, BusinessContext: req.BusinessContext,
		RetentionScheduleRef: req.RetentionScheduleRef, DeclaredByPrincipalID: principalID,
		DeclarationReason: req.DeclarationReason, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *recordsHandler) GetRecord(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	rec, err := h.store.GetRecord(r.Context(), chi.URLParam(r, "record_id"))
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, rec.LegalEntityID, actionRecordRead) {
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *recordsHandler) GetRecordByDocumentVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	rec, err := h.store.GetRecordByDocumentVersion(r.Context(), chi.URLParam(r, "document_version_id"))
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, rec.LegalEntityID, actionRecordRead) {
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

type createRecordRelationshipRequest struct {
	TargetRecordID   string `json:"target_record_id"`
	RelationshipType string `json:"relationship_type"`
}

// CreateRecordRelationship records one evidentiary link (§4.5) from the
// record named in the URL to another record. For SUPERSEDES, the
// target moves to SUPERSEDED in the same transaction.
func (h *recordsHandler) CreateRecordRelationship(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createRecordRelationshipRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TargetRecordID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "target_record_id")
		return
	}
	if req.RelationshipType == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "relationship_type")
		return
	}
	sourceRecordID := chi.URLParam(r, "record_id")

	source, err := h.store.GetRecord(r.Context(), sourceRecordID)
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, source.LegalEntityID, actionRecordRelate) {
		return
	}

	rel, err := h.store.CreateRecordRelationship(r.Context(), domain.CreateRecordRelationshipParams{
		SourceRecordID: sourceRecordID, TargetRecordID: req.TargetRecordID,
		RelationshipType: req.RelationshipType, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rel)
}

func (h *recordsHandler) ListRecordRelationships(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	recordID := chi.URLParam(r, "record_id")
	rec, err := h.store.GetRecord(r.Context(), recordID)
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, rec.LegalEntityID, actionRecordRead) {
		return
	}
	rels, err := h.store.ListRecordRelationships(r.Context(), recordID)
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	if rels == nil {
		rels = []domain.RecordRelationship{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": rels, "count": len(rels)})
}

func (h *recordsHandler) handleRecordsStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRecordNotFound), errors.Is(err, domain.ErrDocumentVersionNotFoundForRecord):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidRecordClass), errors.Is(err, domain.ErrJurisdictionScopeRequired),
		errors.Is(err, domain.ErrInvalidRelationshipType), errors.Is(err, domain.ErrRecordRelationshipSelfReference):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, domain.ErrDocumentVersionAlreadyDeclared), errors.Is(err, domain.ErrRecordAlreadySuperseded),
		errors.Is(err, domain.ErrDuplicateRecordRelationship):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		h.handleStoreError(w, err)
	}
}
