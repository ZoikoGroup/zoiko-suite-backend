package handler

// Routes whose shape is fixed by ZS-SVC-S-001 and whose behaviour this service
// already implements elsewhere. Each one here is either a new read over the
// existing version model (§3.6) or a thin adapter that delegates to the
// existing handler, so there is exactly one implementation of the business
// logic, one authorization decision, one transaction. Nothing here duplicates
// declare / release / relationship logic.
//
//	GET  /v1/document-versions/{version_id}     §3.6   (this file)
//	POST /v1/records:declare                    §9.2   -> recordsHandler.DeclareRecord
//	POST /v1/records/{id}:supersede             §9.2   -> SupersedeRecord (this file)
//	POST /v1/legal-holds/{id}:release           §9.2   -> retentionHandler.ReleaseLegalHold
//
// What is deliberately NOT here is recorded in the service README: the upload
// reservation flow, PATCH/lock/withdraw, retention:evaluate, dispositions,
// legal-hold resolve-targets, exports and DRC-05 each need a decision or an
// external capability the specification does not supply.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/domain"
)

// VersionStore is the exact-version read this service adds for §3.6, kept as its
// own narrow interface like the other DRC modules' stores.
type VersionStore interface {
	FindVersionByID(ctx context.Context, versionID string) (*domain.DocumentVersion, error)
}

type versionHandler struct {
	*Handler
	store VersionStore
}

// RegisterVersionRoutes mounts GET /v1/document-versions/{version_id}.
func RegisterVersionRoutes(r chi.Router, h *Handler, versions VersionStore) {
	vh := &versionHandler{Handler: h, store: versions}
	r.Get("/v1/document-versions/{version_id}", vh.GetDocumentVersion)
}

// GetDocumentVersion returns one exact immutable version (ZS-SVC-S-001 §3.6:
// "Read exact immutable version metadata. No 'latest' substitution.").
//
// The version is looked up by its own id, tenant-scoped by the store; its
// owning document is then fetched and authorization is decided against THAT
// document's legal entity with the same DOCUMENT_READ action the other
// document reads use, so this route can neither be used to read what the
// caller may not read nor to probe another tenant's ids. The read is access-
// logged the same best-effort way a metadata read is.
//
// The body uses the §3.3 field names (documentVersionView). Every value is
// either stored or follows from an invariant this service enforces; see the
// view's comments. encryption_key_ref is the one §3.3 field this service cannot
// supply: its storage backend has no per-object KMS reference (a local AES-GCM
// file backend under one master key), so it is omitted rather than invented.
func (h *versionHandler) GetDocumentVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	versionID := chi.URLParam(r, "version_id")
	v, err := h.store.FindVersionByID(r.Context(), versionID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	doc, err := h.Handler.store.FindDocumentByID(r.Context(), v.DocumentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentRead) {
		return
	}
	view, err := h.buildVersionView(r.Context(), v, doc)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	_ = h.recordAccess(r, actor, v.DocumentID, &v.DocumentVersionID, domain.AccessMetadata)
	writeJSON(w, http.StatusOK, view)
}

// Values that hold for every stored version because of how versions are made:
// a version row is only written, in one transaction with its outbox event, after
// the content-type check and a CLEAN malware verdict (DRC-I18/DRC-I19; a
// quarantined or failed upload stores no version). So a version that can be read
// is COMMITTED and CLEAN by construction.
const (
	malwareStatusCommitted = "CLEAN"
	commitStatusCommitted  = "COMMITTED"
)

// documentVersionView is ZS-SVC-S-001 §3.3's immutable version contract.
//
//	version_id              document_versions.document_version_id
//	document_id             document_versions.document_id
//	version_number          document_versions.version
//	content_object_id       document_versions.storage_key (the content object's locator)
//	content_hash            SHA-256 computed server-side at ingest
//	byte_length             document_versions.size_bytes
//	declared_mime_type      the type the client declared (never trusted alone)
//	detected_mime_type      the server's content sniff; null for versions written
//	                        before it was persisted (migration 000015)
//	malware_status          CLEAN (see malwareStatusCommitted)
//	region                  the document's residency region, null when none was set
//	created_from_version_id the previous version of the same document (versions are
//	                        linear here), null for version 1
//	commit_status           COMMITTED (see commitStatusCommitted)
//	committed_at            the version row's creation time, set in the commit transaction
type documentVersionView struct {
	VersionID            string    `json:"version_id"`
	DocumentID           string    `json:"document_id"`
	VersionNumber        int       `json:"version_number"`
	ContentObjectID      string    `json:"content_object_id"`
	ContentHash          string    `json:"content_hash"`
	ByteLength           int64     `json:"byte_length"`
	DeclaredMimeType     string    `json:"declared_mime_type"`
	DetectedMimeType     *string   `json:"detected_mime_type"`
	MalwareStatus        string    `json:"malware_status"`
	Region               *string   `json:"region"`
	CreatedFromVersionID *string   `json:"created_from_version_id"`
	CommitStatus         string    `json:"commit_status"`
	CommittedAt          time.Time `json:"committed_at"`
}

// ── POST /v1/records/{id}:supersede ──────────────────────────────────────────

// supersedeRecordRequest names the record that replaces the one in the URL. The
// field name is the §9.3 record_superseded anchor (new_record_id); the
// specification gives no request schema for this route.
type supersedeRecordRequest struct {
	NewRecordID string `json:"new_record_id"`
}

// SupersedeRecord is ZS-SVC-S-001 §9.2's "Create explicit supersession
// relation. New record must already exist or be atomically declared."
//
// {id} is the record being superseded (the prior record) and the body names the
// new record, which must already exist. It records exactly the relationship the
// existing POST /v1/records/{record_id}/relationships route records for
// type SUPERSEDES — the new record is the acting (source) record, the prior one
// the target — by calling the same store method, under the same authorization
// (RECORD_RELATE on the acting record's legal entity). "Atomically declared"
// is not implemented: this route takes an existing record only.
func (h *recordsHandler) SupersedeRecord(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req supersedeRecordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.NewRecordID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "new_record_id")
		return
	}
	priorRecordID := chi.URLParam(r, "record_id")

	newRecord, err := h.store.GetRecord(r.Context(), req.NewRecordID)
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, newRecord.LegalEntityID, actionRecordRelate) {
		return
	}
	rel, err := h.store.CreateRecordRelationship(r.Context(), domain.CreateRecordRelationshipParams{
		SourceRecordID:       req.NewRecordID,
		TargetRecordID:       priorRecordID,
		RelationshipType:     string(domain.RelationshipSupersedes),
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRecordsStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rel)
}

// ── POST /v1/retention:evaluate ──────────────────────────────────────────────

// actionRetentionRuleResolve is the platform permission registered for resolving
// retention rules (access-control-svc permission taxonomy, RETENTION_RULE_RESOLVE).
const actionRetentionRuleResolve = "RETENTION_RULE_RESOLVE"

// maxEvaluateRecords bounds one evaluate call. §9.1: "No bulk endpoint may degrade
// per-item authorization, retention/hold checks or audit semantics" — every record's
// owning entity is authorized individually, so a call is kept to a size where that
// is cheap. The specification gives no number; this is an operational cap.
const maxEvaluateRecords = 100

type evaluateRetentionRequest struct {
	RecordIDs []string `json:"record_ids"`
	// AsOf is the instant at which rule effective windows are tested (§5.3
	// "effective date"); absent means now.
	AsOf *time.Time `json:"as_of"`
}

// EvaluateRetention is §9.2's "Resolve schedule for record(s). Returns rule
// versions + conflict states; no silent defaults." See PgStore.EvaluateRetention
// for the resolution rules. It is read-only.
//
// Every record's legal entity is authorized separately (RETENTION_RULE_RESOLVE),
// before any rule or schedule is returned, and a record outside the caller's
// tenant fails the whole call as not found.
func (h *retentionHandler) EvaluateRetention(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req evaluateRetentionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(req.RecordIDs))
	for _, raw := range req.RecordIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "missing_field", "record_ids")
		return
	}
	if len(ids) > maxEvaluateRecords {
		writeError(w, http.StatusBadRequest, "too_many_records", "at most 100 records per call")
		return
	}
	entities, err := h.store.RecordLegalEntities(r.Context(), ids)
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	authorized := map[string]bool{}
	for _, id := range ids {
		entity := entities[id]
		if authorized[entity] {
			continue
		}
		if !h.authorize(w, r, principalID, entity, actionRetentionRuleResolve) {
			return
		}
		authorized[entity] = true
	}
	asOf := time.Now().UTC()
	if req.AsOf != nil {
		asOf = req.AsOf.UTC()
	}
	results, err := h.store.EvaluateRetentionAt(r.Context(), domain.EvaluateRetentionParams{
		RecordIDs: ids, AsOf: asOf, ActorPrincipalID: principalID})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ── POST /v1/dispositions, POST /v1/dispositions/{id}:approve ────────────────

// dispositionView is the response of both disposition routes. In this service a
// record's disposition lifecycle IS its retention state (§5.4; §8.1
// record_retention_states: "Resolved schedule, trigger, due date/condition and
// lifecycle"), so disposition_id is the retention state's id.
type dispositionView struct {
	DispositionID  string                       `json:"disposition_id"`
	RecordID       string                       `json:"record_id"`
	State          domain.RetentionState        `json:"state"`
	RetentionState *domain.RecordRetentionState `json:"retention_state"`
}

func newDispositionView(st *domain.RecordRetentionState) dispositionView {
	return dispositionView{DispositionID: st.RetentionStateID, RecordID: st.RecordID, State: st.State, RetentionState: st}
}

type createDispositionRequest struct {
	RecordID               string                `json:"record_id"`
	ExpectedRetentionState domain.RetentionState `json:"expected_retention_state"`
	ReviewNotes            string                `json:"review_notes"`
}

func validRetentionState(s domain.RetentionState) bool {
	switch s {
	case domain.RetentionWaitingForTrigger, domain.RetentionActive, domain.RetentionDue,
		domain.RetentionReviewRequired, domain.RetentionApprovedForDisposition:
		return true
	}
	return false
}

// CreateDisposition is §9.2's "Create disposition candidate/job. Hold check +
// expected retention state." It moves a DUE record to REVIEW_REQUIRED — the
// candidate awaiting an approval decision — and nothing more: it deletes,
// destroys and anonymizes nothing. See PgStore.CreateDispositionCandidate.
//
// Same grant as the other retention-state commands (RETENTION_STATE_MANAGE).
func (h *retentionHandler) CreateDisposition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionStateManage) {
		return
	}
	var req createDispositionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RecordID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "record_id")
		return
	}
	if req.ExpectedRetentionState == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "expected_retention_state")
		return
	}
	if !validRetentionState(req.ExpectedRetentionState) {
		writeError(w, http.StatusBadRequest, "invalid_request", "expected_retention_state is not a retention state")
		return
	}
	st, err := h.store.CreateDispositionCandidate(r.Context(), domain.CreateDispositionCandidateParams{
		RecordID: req.RecordID, ExpectedState: req.ExpectedRetentionState,
		ReviewNotes: req.ReviewNotes, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleDispositionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newDispositionView(st))
}

type approveDispositionRequest struct {
	ApprovalRef string `json:"approval_ref"`
}

// ApproveDisposition is §9.2's "Consume approval decision. Maker-checker from
// WFC; no self-approval logic duplicated here." The body carries the reference of
// the approval decision made by the workflow service; it is recorded as evidence
// (DRC-I13) and not verified here — this route adds no approver rules of its own.
// The transition and the legal-hold refusal are the existing
// REVIEW_REQUIRED -> APPROVED_FOR_DISPOSITION gate, unchanged. Approval is the
// dry-run boundary: nothing is deleted.
func (h *retentionHandler) ApproveDisposition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionStateManage) {
		return
	}
	var req approveDispositionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ApprovalRef == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "approval_ref")
		return
	}
	if len(req.ApprovalRef) > 255 {
		writeError(w, http.StatusBadRequest, "invalid_request", "approval_ref must be at most 255 characters")
		return
	}
	st, err := h.store.ApproveForDisposition(r.Context(), domain.ApproveForDispositionParams{
		RetentionStateID: chi.URLParam(r, "disposition_id"), ApprovedByPrincipalID: principalID, ApprovalRef: req.ApprovalRef,
	})
	if err != nil {
		h.handleDispositionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newDispositionView(st))
}

// handleDispositionError maps the disposition routes' failures. A legal-hold
// refusal is §11.1 DRC-008 LEGAL_HOLD_BLOCK, naming the hold, with nothing
// mutated; everything else uses the retention module's existing mapping.
func (h *retentionHandler) handleDispositionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRecordUnderLegalHold):
		writeError(w, http.StatusConflict, "legal_hold_block", "DRC-008: LEGAL_HOLD_BLOCK: "+err.Error())
	case errors.Is(err, domain.ErrRetentionStateMismatch):
		writeError(w, http.StatusConflict, "retention_state_mismatch", err.Error())
	default:
		h.handleRetentionStoreError(w, err)
	}
}

// buildVersionView renders a stored version as §3.3's contract. Shared by
// GET /v1/document-versions/{version_id} and the reservation completion response.
func (h *Handler) buildVersionView(ctx context.Context, v *domain.DocumentVersion, doc *domain.Document) (documentVersionView, error) {
	view := documentVersionView{
		VersionID: v.DocumentVersionID, DocumentID: v.DocumentID, VersionNumber: v.Version,
		ContentObjectID: v.StorageKey, ContentHash: v.ChecksumSHA256, ByteLength: v.SizeBytes,
		DeclaredMimeType: v.ContentType, MalwareStatus: malwareStatusCommitted,
		Region: doc.ResidencyRegionCode, CommitStatus: commitStatusCommitted, CommittedAt: v.CreatedAt,
	}
	if v.DetectedContentType != "" {
		detected := v.DetectedContentType
		view.DetectedMimeType = &detected
	}
	if v.Version > 1 {
		prev, perr := h.store.FindVersion(ctx, v.DocumentID, v.Version-1)
		switch {
		case perr == nil:
			view.CreatedFromVersionID = &prev.DocumentVersionID
		case errors.Is(perr, domain.ErrDocumentVersionNotFound):
			// No predecessor row: leave the lineage empty rather than guess it.
		default:
			return documentVersionView{}, perr
		}
	}
	return view, nil
}

// ── POST /v1/legal-holds/{id}:resolve-targets ────────────────────────────────

// maxResolveTargets bounds one resolve call (an operational cap; the
// specification gives none, and the scope query limits are open decision OD-S04).
const maxResolveTargets = 500

type resolveTargetsRequest struct {
	RecordIDs []string `json:"record_ids"`
}

// ResolveLegalHoldTargets is §9.2's "Resolve/refresh scope. Records result
// snapshot and partial failures." for the explicit-object scope: the body names
// the records the hold is to cover, the response is the immutable snapshot —
// resolved targets, unresolved targets and whether the resolution is complete.
// Unknown fields are refused: the other §5.5 scope selectors (custodians, date
// ranges, query snapshots) are not supported, so they cannot be sent and ignored.
//
// Same grant as the other hold-management commands (LEGAL_HOLD_MANAGE). See
// PgStore.ResolveLegalHoldTargets for the transactional behaviour.
func (h *retentionHandler) ResolveLegalHoldTargets(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionLegalHoldManage) {
		return
	}
	var req resolveTargetsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(req.RecordIDs))
	for _, raw := range req.RecordIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "missing_field", "record_ids")
		return
	}
	if len(ids) > maxResolveTargets {
		writeError(w, http.StatusBadRequest, "too_many_records", "at most 500 records per call")
		return
	}
	res, err := h.store.ResolveLegalHoldTargets(r.Context(), domain.ResolveLegalHoldTargetsParams{
		HoldID: chi.URLParam(r, "hold_id"), RecordIDs: ids, ResolvedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
