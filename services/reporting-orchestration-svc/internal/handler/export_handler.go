package handler

import (
	"net/http"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// AUD-10 audit export routes (/v1/audit-exports). The lifecycle is
// documented in internal/domain/export.go and ROUTES.md.

// archiveIDPattern restricts archive ids to characters that are safe to
// place in the upstream URL path (no "/", "..", "?" or "#").
var archiveIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

const (
	maxPurposeLength = 1000
	maxReasonLength  = 2000
	maxShortField    = 256
)

type createExportRequest struct {
	TenantID      string `json:"tenant_id,omitempty"`
	LegalEntityID string `json:"legal_entity_id"`
	ArchiveID     string `json:"archive_id"`
	Purpose       string `json:"purpose"`
}

type redactRequest struct {
	FieldOrScope string `json:"field_or_scope"`
	Reason       string `json:"reason"`
}

type deliverRequest struct {
	DeliveredToPrincipalID string `json:"delivered_to_principal_id"`
	DeliveryChannel        string `json:"delivery_channel"`
}

type revokeRequest struct {
	Reason string `json:"reason"`
}

// CreateExport handles POST /v1/audit-exports.
//
// Validation: legal_entity_id (defaults to / must agree with
// X-Legal-Entity-Id), archive_id (safe id characters), purpose (required:
// AUD-10 "authorized export purpose"); tenant_id optional, must match.
// Writes: audit_export_requests row in REQUESTED.
// Event: audit.export.requested.v1.
// Statuses: 201; 400; 401; 403 mismatch/authz denied; 503 authz unavailable.
func (h *Handler) CreateExport(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}

	var request createExportRequest
	if !requireJSONBody(w, r, &request) {
		return
	}
	if !requireTenantAgreement(w, r, caller, request.TenantID) {
		return
	}
	legalEntityID, ok := resolveLegalEntity(w, r, caller, request.LegalEntityID)
	if !ok {
		return
	}
	if legalEntityID == "" {
		validationProblem(w, r, "legal_entity_id is required")
		return
	}
	archiveID := strings.TrimSpace(request.ArchiveID)
	if archiveID == "" {
		validationProblem(w, r, "archive_id is required")
		return
	}
	if !archiveIDPattern.MatchString(archiveID) {
		validationProblem(w, r, "archive_id may contain only letters, digits, '.', '_', ':' and '-' (max 128)")
		return
	}
	purpose := strings.TrimSpace(request.Purpose)
	if purpose == "" {
		validationProblem(w, r, "purpose is required: an export must state its authorized purpose")
		return
	}
	if len(purpose) > maxPurposeLength {
		validationProblem(w, r, "purpose must be at most 1000 characters")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, caller.PrincipalID, legalEntityID, actionExportRequest); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}

	created, err := h.exports.CreateExportRequest(r.Context(), domain.CreateExportRequestParams{
		TenantID:               caller.TenantID,
		LegalEntityID:          legalEntityID,
		ArchiveID:              archiveID,
		Purpose:                purpose,
		RequestedByPrincipalID: caller.PrincipalID,
		Event:                  newEvent(caller, domain.EventExportRequested, domain.AggregateAuditExport, "", legalEntityID),
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, created.Version)
	writeJSON(w, http.StatusCreated, created)
}

// GetExport handles GET /v1/audit-exports/{id}.
//
// Statuses: 200 with ETag; 400 bad id; 401; 404 unknown/another tenant's.
func (h *Handler) GetExport(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	exportID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}

	export, err := h.exports.GetExportRequest(r.Context(), caller.TenantID, exportID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, export.Version)
	writeJSON(w, http.StatusOK, export)
}

// ListExports handles GET /v1/audit-exports.
//
// Query: status, legal_entity_id (filters); limit, offset.
// Statuses: 200 {data, count, limit, offset}; 400 invalid query; 401.
func (h *Handler) ListExports(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	filter := store.ExportFilter{
		Status:        strings.ToUpper(query.Get("status")),
		LegalEntityID: query.Get("legal_entity_id"),
	}
	exports, err := h.exports.ListExportRequests(r.Context(), caller.TenantID, filter, page)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{Data: exports, Count: len(exports), Limit: page.Limit, Offset: page.Offset})
}

// ApproveExport handles POST /v1/audit-exports/{id}/approve.
//
// Rule: REQUESTED -> APPROVED. Segregation of duties: the approver must not
// be the requester (403 SELF_APPROVAL_FORBIDDEN).
// Event: audit.export.approved.v1.
// Statuses: 200 with ETag; 400; 401; 403 authz denied / self-approval;
// 404; 409 wrong state; 412 stale If-Match.
func (h *Handler) ApproveExport(w http.ResponseWriter, r *http.Request) {
	caller, existing, expectedVersion, ok := h.loadExportForCommand(w, r, actionExportApprove)
	if !ok {
		return
	}

	approved, err := h.exports.ApproveExport(r.Context(), domain.ApproveExportParams{
		ExportID:              existing.ExportID,
		TenantID:              caller.TenantID,
		ApprovedByPrincipalID: caller.PrincipalID,
		ExpectedVersion:       expectedVersion,
		Event:                 newEvent(caller, domain.EventExportApproved, domain.AggregateAuditExport, existing.ExportID, existing.LegalEntityID),
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, approved.Version)
	writeJSON(w, http.StatusOK, approved)
}

// BuildExport handles POST /v1/audit-exports/{id}/build (BuildExportPackage).
//
// Steps:
//  1. APPROVED -> BUILDING (committed; a concurrent build gets 409);
//  2. fetch the archive record from audit-event-store-svc;
//  3. the archive digest must be a SHA-256 hex digest;
//  4. record the manifest entry and move BUILDING -> VALIDATING.
//
// Any failure in 2-3 moves the export to FAILED (never seals fabricated
// content). Events: audit.export.built.v1 / audit.export.build_failed.v1.
// Statuses: 200 with ETag; 400; 401; 403; 404; 409 wrong state; 412;
// 424 archive unavailable or digest invalid.
func (h *Handler) BuildExport(w http.ResponseWriter, r *http.Request) {
	caller, existing, expectedVersion, ok := h.loadExportForCommand(w, r, actionExportBuild)
	if !ok {
		return
	}

	// Step 1: claim the build.
	if _, err := h.exports.StartBuild(r.Context(), domain.StartBuildParams{
		ExportID: existing.ExportID, TenantID: caller.TenantID, ExpectedVersion: expectedVersion,
	}); err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	// Step 2: fetch the real archive record.
	archive, err := h.archives.GetArchive(r.Context(), existing.ArchiveID, caller.PrincipalID, caller.CorrelationID)
	if err != nil {
		h.failBuild(w, r, caller, existing, "archive fetch failed: "+err.Error(), domain.ErrArchiveFetchFailed)
		return
	}

	// Step 3: the digest is what the seal will vouch for — it must be real.
	if !domain.IsSHA256Hex(archive.ArchiveDigest) {
		h.failBuild(w, r, caller, existing, "archive digest is not a SHA-256 hex digest", domain.ErrArchiveDigestInvalid)
		return
	}

	// Step 4: record the manifest entry and move to VALIDATING.
	validating, err := h.exports.CompleteBuild(r.Context(), domain.CompleteBuildParams{
		ExportID:     existing.ExportID,
		TenantID:     caller.TenantID,
		ArtifactName: "audit-archive-" + archive.ArchiveID,
		Sha256:       strings.ToLower(archive.ArchiveDigest),
		Event:        newEvent(caller, domain.EventExportBuilt, domain.AggregateAuditExport, existing.ExportID, existing.LegalEntityID),
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, validating.Version)
	writeJSON(w, http.StatusOK, validating)
}

// failBuild moves a BUILDING export to FAILED and writes the 424 problem.
func (h *Handler) failBuild(w http.ResponseWriter, r *http.Request, caller requestContext, export *domain.AuditExportRequest, reason string, problem error) {
	_, err := h.exports.FailBuild(r.Context(), domain.FailBuildParams{
		ExportID: export.ExportID,
		TenantID: caller.TenantID,
		Reason:   reason,
		Event:    newEvent(caller, domain.EventExportBuildFailed, domain.AggregateAuditExport, export.ExportID, export.LegalEntityID),
	})
	if err != nil {
		h.logger.Error("failed to mark export build failed", zap.String("export_id", export.ExportID), zap.Error(err))
	}
	h.writeDomainError(w, r, problem)
}

// RedactExport handles POST /v1/audit-exports/{id}/redact (ApplyRedaction).
//
// Validation: body {field_or_scope, reason} both required.
// Rule: only before the seal (REQUESTED, APPROVED, VALIDATING); a sealed,
// delivered, revoked or failed package is immutable (409).
// Writes: append-only redaction_decisions row; export version bump.
// Event: audit.export.redaction_applied.v1.
// Statuses: 201 decision; 400; 401; 403; 404; 409; 412.
func (h *Handler) RedactExport(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	exportID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}
	expectedVersion, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	var request redactRequest
	if !requireJSONBody(w, r, &request) {
		return
	}
	fieldOrScope := strings.TrimSpace(request.FieldOrScope)
	reason := strings.TrimSpace(request.Reason)
	if fieldOrScope == "" || reason == "" {
		validationProblem(w, r, "field_or_scope and reason are required")
		return
	}
	if len(fieldOrScope) > maxShortField || len(reason) > maxReasonLength {
		validationProblem(w, r, "field_or_scope must be at most 256 and reason at most 2000 characters")
		return
	}

	existing, err := h.exports.GetExportRequest(r.Context(), caller.TenantID, exportID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, caller.PrincipalID, existing.LegalEntityID, actionExportRedact); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}

	decision, err := h.exports.RecordRedaction(r.Context(), domain.RecordRedactionParams{
		ExportID:             exportID,
		TenantID:             caller.TenantID,
		FieldOrScope:         fieldOrScope,
		Reason:               reason,
		DecidedByPrincipalID: caller.PrincipalID,
		ExpectedVersion:      expectedVersion,
		Event:                newEvent(caller, domain.EventExportRedactionApplied, domain.AggregateAuditExport, exportID, existing.LegalEntityID),
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, decision)
}

// SealExport handles POST /v1/audit-exports/{id}/seal (SealPackage).
//
// Rule: VALIDATING -> SEALED; at least one manifest entry. The seal digest
// (SHA-256 over manifest + redaction log) is stored; the package is
// immutable from here on.
// Event: audit.export.sealed.v1.
// Statuses: 200 with ETag; 400; 401; 403; 404; 409 wrong state / empty
// manifest; 412.
func (h *Handler) SealExport(w http.ResponseWriter, r *http.Request) {
	caller, existing, expectedVersion, ok := h.loadExportForCommand(w, r, actionExportSeal)
	if !ok {
		return
	}

	sealed, err := h.exports.SealExport(r.Context(), domain.SealExportParams{
		ExportID:            existing.ExportID,
		TenantID:            caller.TenantID,
		SealedByPrincipalID: caller.PrincipalID,
		ExpectedVersion:     expectedVersion,
		Event:               newEvent(caller, domain.EventExportSealed, domain.AggregateAuditExport, existing.ExportID, existing.LegalEntityID),
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, sealed.Version)
	writeJSON(w, http.StatusOK, sealed)
}

// DeliverExport handles POST /v1/audit-exports/{id}/deliver (DeliverPackage).
//
// Validation: body {delivered_to_principal_id, delivery_channel} required.
// Gates, in order, all before any write:
//  1. export must be SEALED (409 otherwise);
//  2. caller authorized to deliver (AUDIT_EXPORT_DELIVER);
//  3. recipient authorized to receive (AUDIT_EXPORT_RECEIVE);
//  4. no legal hold (retention-registry-svc; unreachable = 503 fail closed,
//     hold = 409).
//
// Writes: SEALED -> DELIVERED and an append-only delivery receipt.
// Event: audit.export.delivered.v1.
// Statuses: 200 with ETag; 400; 401; 403; 404; 409; 412; 503.
func (h *Handler) DeliverExport(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	exportID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}
	expectedVersion, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	var request deliverRequest
	if !requireJSONBody(w, r, &request) {
		return
	}
	recipientID := strings.TrimSpace(request.DeliveredToPrincipalID)
	channel := strings.TrimSpace(request.DeliveryChannel)
	if recipientID == "" || channel == "" {
		validationProblem(w, r, "delivered_to_principal_id and delivery_channel are required")
		return
	}
	if len(recipientID) > 128 || len(channel) > maxShortField {
		validationProblem(w, r, "delivered_to_principal_id must be at most 128 and delivery_channel at most 256 characters")
		return
	}

	existing, err := h.exports.GetExportRequest(r.Context(), caller.TenantID, exportID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	// Gate 1: state (re-checked under lock by the store).
	if existing.Status != domain.ExportSealed {
		h.writeDomainError(w, r, domain.ErrExportInvalidState)
		return
	}
	// Gate 2: the caller may deliver.
	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, caller.PrincipalID, existing.LegalEntityID, actionExportDeliver); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	// Gate 3: the recipient may receive (AUD-10 "recipient authorization").
	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, recipientID, existing.LegalEntityID, actionExportReceive); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	// Gate 4: legal hold / retention (fail closed).
	blocked, err := h.retentions.IsBlocked(r.Context(), caller.TenantID, "AUDIT_EXPORT", existing.ArchiveID, caller.PrincipalID, caller.CorrelationID)
	if err != nil {
		h.logger.Error("retention-registry-svc unavailable during export delivery", zap.Error(err))
		h.writeDomainError(w, r, domain.ErrRetentionServiceDown)
		return
	}
	if blocked {
		h.writeDomainError(w, r, domain.ErrLegalHoldActive)
		return
	}

	delivered, err := h.exports.DeliverExport(r.Context(), domain.DeliverExportParams{
		ExportID:               exportID,
		TenantID:               caller.TenantID,
		DeliveredToPrincipalID: recipientID,
		DeliveryChannel:        channel,
		ExpectedVersion:        expectedVersion,
		Event:                  newEvent(caller, domain.EventExportDelivered, domain.AggregateAuditExport, exportID, existing.LegalEntityID),
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, delivered.Version)
	writeJSON(w, http.StatusOK, delivered)
}

// RevokeExport handles POST /v1/audit-exports/{id}/revoke (RevokeDeliveryAccess).
//
// Validation: body {reason} required.
// Rule: SEALED or DELIVERED -> REVOKED (terminal); records who and why.
// Event: audit.export.access_revoked.v1.
// Statuses: 200 with ETag; 400; 401; 403; 404; 409 wrong state; 412.
func (h *Handler) RevokeExport(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	exportID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}
	expectedVersion, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	var request revokeRequest
	if !requireJSONBody(w, r, &request) {
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		validationProblem(w, r, "reason is required to revoke an export")
		return
	}
	if len(reason) > maxReasonLength {
		validationProblem(w, r, "reason must be at most 2000 characters")
		return
	}

	existing, err := h.exports.GetExportRequest(r.Context(), caller.TenantID, exportID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, caller.PrincipalID, existing.LegalEntityID, actionExportRevoke); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}

	revoked, err := h.exports.RevokeExport(r.Context(), domain.RevokeExportParams{
		ExportID:             exportID,
		TenantID:             caller.TenantID,
		RevokedByPrincipalID: caller.PrincipalID,
		Reason:               reason,
		ExpectedVersion:      expectedVersion,
		Event:                newEvent(caller, domain.EventExportAccessRevoked, domain.AggregateAuditExport, exportID, existing.LegalEntityID),
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	setETag(w, revoked.Version)
	writeJSON(w, http.StatusOK, revoked)
}

// GetManifest handles GET /v1/audit-exports/{id}/manifest (GetArchiveManifest /
// ListPackageContents / VerifyArtifactDigest).
//
// Returns the manifest entries, the redaction log, the digest recomputed
// now, and — once sealed — the stored seal digest with integrity_verified
// (true when the recomputed digest still equals the seal).
// Statuses: 200; 400 bad id; 401; 404 unknown/another tenant's.
func (h *Handler) GetManifest(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	exportID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}

	export, entries, redactions, err := h.exports.GetPackageManifest(r.Context(), caller.TenantID, exportID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	view := domain.PackageManifestView{
		ExportID:       export.ExportID,
		Status:         export.Status,
		Entries:        entries,
		Redactions:     redactions,
		ComputedDigest: domain.ComputePackageDigest(export, entries, redactions),
		SealDigest:     export.SealDigest,
	}
	if export.SealDigest != "" {
		verified := view.ComputedDigest == export.SealDigest
		view.IntegrityVerified = &verified
	}
	writeJSON(w, http.StatusOK, view)
}

// ListRedactions handles GET /v1/audit-exports/{id}/redactions (GetRedactionLog).
//
// Statuses: 200 {data, count}; 400 bad id; 401; 404.
func (h *Handler) ListRedactions(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	exportID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}
	redactions, err := h.exports.ListRedactions(r.Context(), caller.TenantID, exportID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": redactions, "count": len(redactions)})
}

// ListDeliveries handles GET /v1/audit-exports/{id}/deliveries (GetDeliveryHistory).
//
// Statuses: 200 {data, count}; 400 bad id; 401; 404.
func (h *Handler) ListDeliveries(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	exportID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}
	receipts, err := h.exports.ListDeliveries(r.Context(), caller.TenantID, exportID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": receipts, "count": len(receipts)})
}

// loadExportForCommand is the shared prologue of body-less export commands
// (approve, build, seal): caller, UUID id, If-Match, load the export (404),
// and the authorization check for action.
func (h *Handler) loadExportForCommand(w http.ResponseWriter, r *http.Request, action string) (requestContext, *domain.AuditExportRequest, int, bool) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return caller, nil, 0, false
	}
	exportID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return caller, nil, 0, false
	}
	expectedVersion, ok := parseIfMatch(w, r)
	if !ok {
		return caller, nil, 0, false
	}
	var ignoredBody struct{}
	if !optionalJSONBody(w, r, &ignoredBody) {
		return caller, nil, 0, false
	}

	existing, err := h.exports.GetExportRequest(r.Context(), caller.TenantID, exportID)
	if err != nil {
		h.writeDomainError(w, r, err)
		return caller, nil, 0, false
	}
	if err := h.authz.CheckAllowed(r.Context(), caller.TenantID, caller.PrincipalID, existing.LegalEntityID, action); err != nil {
		h.writeAuthzError(w, r, err)
		return caller, nil, 0, false
	}
	return caller, existing, expectedVersion, true
}
