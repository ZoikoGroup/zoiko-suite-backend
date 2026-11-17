package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/glclient"
	"zoiko.io/migration-integrity-svc/internal/middleware"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// notConfigured answers 501 when WithControlPlane was never called, the
// same shape general-ledger-svc's own kernel handler uses for a repo that
// does not implement MigrationRepo (see its h.migration()).
func (h *Handler) notConfigured(w http.ResponseWriter) bool {
	if h.cp == nil || h.gl == nil {
		h.errJSON(w, http.StatusNotImplemented, "migration control plane is not configured on this instance")
		return true
	}
	return false
}

// ─── Identity Crosswalk ─────────────────────────────────────────────────────

// UpsertCrosswalk handles POST /v1/migrations/crosswalk. It is authorized at
// the platform scope (the gateway-verified tenant, no single legal entity):
// a crosswalk entry can concern any entity a source system reports records
// for, and is checked for conflicts against the whole tenant's prior
// mappings regardless of which entity first established them.
func (h *Handler) UpsertCrosswalk(w http.ResponseWriter, r *http.Request) {
	if h.notConfigured(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.checkAllowed(w, r, principalID, tenantID, ActionMigrationCrosswalkWrite) {
		return
	}

	var req domain.UpsertCrosswalkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	entries := make([]domain.IdentityCrosswalk, len(req.Entries))
	for i, e := range req.Entries {
		entries[i] = domain.IdentityCrosswalk{
			SourceSystem: strings.TrimSpace(e.SourceSystem), SourceType: strings.TrimSpace(e.SourceType), SourceID: strings.TrimSpace(e.SourceID),
			CanonicalID: strings.TrimSpace(e.CanonicalID), MatchMethod: strings.ToUpper(strings.TrimSpace(e.MatchMethod)),
			MatchEvidence: e.MatchEvidence, MatchedBy: principalID, FirstJobID: e.JobID,
		}
	}
	out, err := h.cp.UpsertCrosswalkEntries(r.Context(), tenantID, entries, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		if errors.Is(err, store.ErrCrosswalkConflict) {
			h.errJSON(w, http.StatusConflict, err.Error())
			return
		}
		h.logger.Error("failed to upsert crosswalk entries", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to persist crosswalk entries")
		return
	}
	h.okJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

// GetCrosswalkEntry handles GET
// /v1/migrations/crosswalk?source_system=&source_type=&source_id=.
func (h *Handler) GetCrosswalkEntry(w http.ResponseWriter, r *http.Request) {
	if h.notConfigured(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.checkAllowed(w, r, principalID, tenantID, ActionMigrationCrosswalkView) {
		return
	}
	q := r.URL.Query()
	sourceSystem, sourceType, sourceID := q.Get("source_system"), q.Get("source_type"), q.Get("source_id")
	if sourceSystem == "" || sourceType == "" || sourceID == "" {
		h.errJSON(w, http.StatusBadRequest, "source_system, source_type and source_id are all required")
		return
	}
	e, err := h.cp.GetCrosswalkBySource(r.Context(), tenantID, sourceSystem, sourceType, sourceID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, err.Error())
		return
	}
	h.okJSON(w, http.StatusOK, e)
}

// ─── Promote ────────────────────────────────────────────────────────────────

// PromoteBatch handles POST /v1/migrations/{id}/promote. It is the wiring
// this service was missing entirely: given a validation job that passed
// (no unresolved MQ0/MQ1 blockers) and whose records' identities have all
// been resolved in the crosswalk, it drives general-ledger-svc's existing
// kernel migration endpoints -- Stage, Approve, Post, Reconcile -- as a
// client, in that order, and records the result. general-ledger-svc's own
// kernel logic is never modified; this only calls it.
func (h *Handler) PromoteBatch(w http.ResponseWriter, r *http.Request) {
	if h.notConfigured(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	jobID := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "migration job not found")
		return
	}
	if !h.checkAllowed(w, r, principalID, job.LegalEntityID, ActionMigrationBatchPromote) {
		return
	}

	// Idempotent replay: a job that already reached STAGED or further
	// returns its existing batch ref rather than attempting GL calls a
	// second time. A FAILED ref (a prior attempt that never reached GL, or
	// that GL itself refused -- e.g. an unknown account) is retried: the
	// extract was corrected and nothing in GL for this job ever succeeded,
	// so there is no GL state to replay instead of redo.
	var existingRef *domain.MigrationBatchRef
	if existing, err := h.cp.GetBatchRefByJobID(r.Context(), tenantID, jobID); err == nil {
		if existing.Status != domain.BatchRefFailed {
			w.Header().Set("Idempotent-Replay", "true")
			h.okJSON(w, http.StatusOK, existing)
			return
		}
		existingRef = existing
	}

	// §16 disposition: an MQ0/MQ1 that is not remediated/accepted blocks
	// promotion. A job that scored FAILED can still carry no blockers at
	// all (e.g. the integrity-score threshold alone failed it) -- what
	// gates promotion specifically is an unresolved MQ0 or MQ1, matching
	// the task's "no unresolved MQ0/MQ1 blockers" condition rather than
	// re-deriving the job's own PASS/FAIL.
	if blocking := unresolvedBlockers(job.AuditEntries); len(blocking) > 0 {
		h.errJSON(w, http.StatusConflict, fmt.Sprintf(
			"%d unresolved MQ0/MQ1 entr(y/ies) block promotion; remediate them first", len(blocking)))
		return
	}

	var req domain.PromoteBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	// Every line's source identity must already be resolved in the
	// permanent crosswalk (MIG-INV-14) before this service hands it to
	// general-ledger-svc as a real posting line.
	var unresolved []string
	for _, l := range req.Lines {
		if _, err := h.cp.GetCrosswalkBySource(r.Context(), tenantID, job.SourceSystem, strings.TrimSpace(l.SourceType), strings.TrimSpace(l.SourceRef)); err != nil {
			unresolved = append(unresolved, l.SourceRef)
		}
	}
	if len(unresolved) > 0 {
		h.errJSON(w, http.StatusConflict, fmt.Sprintf(
			"%d line(s) have no resolved identity crosswalk entry and cannot be promoted: %s", len(unresolved), strings.Join(unresolved, ", ")))
		return
	}

	var ref *domain.MigrationBatchRef
	if existingRef != nil {
		ref, err = h.cp.ResetBatchRefForRetry(r.Context(), tenantID, existingRef.ID, req.BatchCode, req.BookID, principalID, r.Header.Get("X-Correlation-ID"))
	} else {
		ref, err = h.cp.CreateBatchRef(r.Context(), tenantID, &domain.MigrationBatchRef{
			LegalEntityID: job.LegalEntityID, JobID: jobID, BatchCode: req.BatchCode, GLBookID: req.BookID,
		}, principalID, r.Header.Get("X-Correlation-ID"))
	}
	if err != nil {
		h.logger.Error("failed to record migration batch ref", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to record batch promotion")
		return
	}

	glID := glclient.Identity{TenantID: tenantID, LegalEntityID: job.LegalEntityID, PrincipalID: req.PreparerPrincipalID, CorrelationID: r.Header.Get("X-Correlation-ID")}
	idemSeed := ref.ID // stable per batch ref: a retried promote step replays GL's own idempotent response

	lines := make([]glclient.LineInput, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = glclient.LineInput{
			SourceRef: l.SourceRef, SourceAccount: l.SourceAccount, AccountCode: l.AccountCode, Debit: l.Debit, Credit: l.Credit,
			CounterpartyID: l.CounterpartyID, TransactionAmount: l.TransactionAmount, TransactionCurrency: l.TransactionCurrency,
			ExchangeRate: l.ExchangeRate, Dimensions: l.Dimensions, Description: l.Description,
		}
	}
	controlTotals := make(map[string]glclient.ControlTotal, len(req.ControlTotals))
	for k, v := range req.ControlTotals {
		controlTotals[k] = glclient.ControlTotal{Total: v.Total, Count: v.Count, Ref: v.Ref}
	}

	stageReq := glclient.StageRequest{
		LegalEntityID: job.LegalEntityID, BookID: req.BookID, BatchCode: req.BatchCode, Kind: "OPENING",
		CutoverDate: req.CutoverDate, SourceSystem: job.SourceSystem, SourceExtractRef: req.SourceExtractRef,
		SourceExtractHash: req.SourceExtractHash, MappingRef: req.MappingRef,
		SourceTotals: glclient.Totals{Debit: req.SourceTotals.Debit, Credit: req.SourceTotals.Credit},
		ControlTotals: controlTotals, Lines: lines,
	}

	fail := func(step string, err error) {
		h.cp.UpdateBatchRef(r.Context(), tenantID, ref.ID, domain.BatchRefFailed, "", "", "", "", step+": "+err.Error(), principalID, r.Header.Get("X-Correlation-ID"))
		h.logger.Error("promotion step to general-ledger-svc failed", zap.String("step", step), zap.Error(err))
		h.errJSON(w, http.StatusBadGateway, fmt.Sprintf("general-ledger-svc %s failed: %v", step, err))
	}

	// Stage (preparer identity).
	stageGLID := glID
	stageGLID.PrincipalID = req.PreparerPrincipalID
	batch, err := h.gl.Stage(r.Context(), stageGLID, idemSeed, stageReq)
	if err != nil {
		fail("stage", err)
		return
	}
	ref, _ = h.cp.UpdateBatchRef(r.Context(), tenantID, ref.ID, domain.BatchRefStaged, batch.BatchID, "", "", "", "", principalID, r.Header.Get("X-Correlation-ID"))

	// Validate (preparer identity -- same action as Stage in GL).
	if _, issues, err := h.gl.Validate(r.Context(), stageGLID, idemSeed, batch.BatchID); err != nil {
		fail("validate", err)
		return
	} else if len(issues) > 0 {
		fail("validate", fmt.Errorf("%d issue(s), e.g. %s: %s", len(issues), issues[0].Code, issues[0].Detail))
		return
	}

	// Approve (the promoting principal -- must differ from the preparer;
	// general-ledger-svc enforces ErrSelfApproval itself).
	approveGLID := glID
	approveGLID.PrincipalID = principalID
	if _, err := h.gl.Approve(r.Context(), approveGLID, idemSeed, batch.BatchID, req.AuthorityRef, req.WindowStart, req.WindowEnd); err != nil {
		fail("approve", err)
		return
	}
	ref, _ = h.cp.UpdateBatchRef(r.Context(), tenantID, ref.ID, domain.BatchRefApproved, batch.BatchID, "", "", "", "", principalID, r.Header.Get("X-Correlation-ID"))

	// Post.
	posted, err := h.gl.Post(r.Context(), approveGLID, idemSeed, batch.BatchID)
	if err != nil {
		fail("post", err)
		return
	}
	ref, _ = h.cp.UpdateBatchRef(r.Context(), tenantID, ref.ID, domain.BatchRefPosted, batch.BatchID, posted.JournalID, posted.JournalNumber, "", "", principalID, r.Header.Get("X-Correlation-ID"))

	// Reconcile.
	reconciled, err := h.gl.Reconcile(r.Context(), approveGLID, idemSeed, batch.BatchID)
	if err != nil {
		fail("reconcile", err)
		return
	}
	recStatus := ""
	if reconciled.Reconciliation != nil {
		recStatus = reconciled.Reconciliation.Status
	}
	finalStatus := domain.BatchRefPosted
	if recStatus == "MATCHED" {
		finalStatus = domain.BatchRefReconciled
	}
	ref, err = h.cp.UpdateBatchRef(r.Context(), tenantID, ref.ID, finalStatus, batch.BatchID, posted.JournalID, posted.JournalNumber, recStatus, "", principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.logger.Error("failed to record reconciliation result", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to record reconciliation result")
		return
	}

	h.okJSON(w, http.StatusCreated, ref)
}

// unresolvedBlockers returns the job's MQ0/MQ1 audit entries that have not
// been remediated.
func unresolvedBlockers(entries []domain.AuditEntry) []domain.AuditEntry {
	var out []domain.AuditEntry
	for _, e := range entries {
		if (e.Severity == domain.SeverityMQ0 || e.Severity == domain.SeverityMQ1) && !e.IsRemediated {
			out = append(out, e)
		}
	}
	return out
}

func (h *Handler) GetBatchRef(w http.ResponseWriter, r *http.Request) {
	if h.notConfigured(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	jobID := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	job, err := h.store.GetJobByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "migration job not found")
		return
	}
	if !h.checkAllowed(w, r, principalID, job.LegalEntityID, ActionMigrationBatchView) {
		return
	}
	ref, err := h.cp.GetBatchRefByJobID(r.Context(), tenantID, jobID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "no batch has been promoted for this job")
		return
	}
	h.okJSON(w, http.StatusOK, ref)
}

// ─── Certificate ────────────────────────────────────────────────────────────

// IssueCertificate handles POST /v1/migrations/{id}/certificate. It reads
// general-ledger-svc's OWN evidence for the posted batch -- fresh, at
// issuance time -- rather than trusting what PromoteBatch recorded earlier:
// the certificate's claim must be backed by GL's live reconciliation
// result, not an assertion this service makes about its own prior call.
func (h *Handler) IssueCertificate(w http.ResponseWriter, r *http.Request) {
	if h.notConfigured(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	jobID := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "migration job not found")
		return
	}
	if !h.checkAllowed(w, r, principalID, job.LegalEntityID, ActionMigrationCertificateIssue) {
		return
	}

	var req domain.IssueCertificateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	blocking := unresolvedBlockers(job.AuditEntries)
	if len(blocking) > 0 {
		h.errJSON(w, http.StatusConflict, fmt.Sprintf("%d unresolved MQ0/MQ1 entr(y/ies): a certificate cannot be issued", len(blocking)))
		return
	}

	ref, err := h.cp.GetBatchRefByJobID(r.Context(), tenantID, jobID)
	if err != nil {
		h.errJSON(w, http.StatusConflict, "no batch has been promoted and posted for this job yet")
		return
	}
	if ref.GLBatchID == "" {
		h.errJSON(w, http.StatusConflict, "the batch has not reached general-ledger-svc yet")
		return
	}
	if existing, err := h.cp.GetCertificateByBatchRefID(r.Context(), tenantID, ref.ID); err == nil {
		w.Header().Set("Idempotent-Replay", "true")
		h.okJSON(w, http.StatusOK, existing)
		return
	}

	// The live read: general-ledger-svc's own evidence endpoint, asked now,
	// not the response PromoteBatch happened to get earlier.
	glID := glclient.Identity{TenantID: tenantID, LegalEntityID: job.LegalEntityID, PrincipalID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID")}
	ev, err := h.gl.Evidence(r.Context(), glID, ref.GLBatchID)
	if err != nil {
		h.logger.Error("failed to read general-ledger-svc evidence", zap.Error(err))
		h.errJSON(w, http.StatusBadGateway, "could not read general-ledger-svc's reconciliation evidence")
		return
	}
	if ev.Batch == nil || ev.Batch.Reconciliation == nil || ev.Batch.Reconciliation.Status != "MATCHED" || !ev.Verified {
		h.errJSON(w, http.StatusConflict, "general-ledger-svc reports the batch is not reconciled MATCHED with verified evidence; no certificate can be issued")
		return
	}

	cert, err := h.cp.CreateCertificate(r.Context(), tenantID, &domain.MigrationCertificate{
		LegalEntityID: job.LegalEntityID, Scope: req.Scope, BatchRefID: ref.ID, JobID: jobID,
		UnresolvedExceptions: len(blocking), GLBatchID: ref.GLBatchID, GLJournalID: ev.Batch.JournalID, GLJournalNumber: ev.Batch.JournalNumber,
		GLReconciliationStatus: ev.Batch.Reconciliation.Status, GLEvidenceHash: ev.Batch.EvidenceHash, GLEvidenceVerified: ev.Verified,
		SignerPrincipalID: principalID,
	}, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.logger.Error("failed to issue migration certificate", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to issue certificate")
		return
	}
	h.okJSON(w, http.StatusCreated, cert)
}

func (h *Handler) GetCertificate(w http.ResponseWriter, r *http.Request) {
	if h.notConfigured(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	jobID := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	job, err := h.store.GetJobByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "migration job not found")
		return
	}
	if !h.checkAllowed(w, r, principalID, job.LegalEntityID, ActionMigrationCertificateView) {
		return
	}
	ref, err := h.cp.GetBatchRefByJobID(r.Context(), tenantID, jobID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, "no batch has been promoted for this job")
		return
	}
	cert, err := h.cp.GetCertificateByBatchRefID(r.Context(), tenantID, ref.ID)
	if err != nil {
		h.errJSON(w, http.StatusNotFound, err.Error())
		return
	}
	h.okJSON(w, http.StatusOK, cert)
}
