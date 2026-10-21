package ar05

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// cmdBase is the common body of the commands on an existing submission. legal_entity_id is optional: the entity is derived
// from the stored submission and a body entity that disagrees is refused (spec invariant 16).
type cmdBase struct {
	SubmissionID    string `json:"submission_id"`
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	ExpectedVersion *int   `json:"expected_version"`
	ReasonCode      string `json:"reason_code,omitempty"`
	EvidenceID      string `json:"evidence_id,omitempty"`
}

// subResponse is the shape of every command answer.
type subResponse struct {
	Submission          *Submission     `json:"submission"`
	AvailableActions    []string        `json:"available_actions"`
	Existing            bool            `json:"existing,omitempty"`
	DuplicateSuppressed bool            `json:"duplicate_suppressed,omitempty"`
	Outcome             string          `json:"outcome,omitempty"`
	Code                string          `json:"code,omitempty"`
	Detail              string          `json:"detail,omitempty"`
	ExternalAttemptID   string          `json:"external_attempt_id,omitempty"`
	Retried             *bool           `json:"retried,omitempty"`
	Reconciliation      *Reconciliation `json:"reconciliation,omitempty"`
}

func respond(s *Submission) subResponse {
	return subResponse{Submission: s, AvailableActions: available(s.State)}
}

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

// loadSubmission identifies the caller, loads the submission to learn its entity, then runs Guard (tenant + entity
// reconciliation before authz) for the action.
func (m *Module) loadSubmission(w http.ResponseWriter, r *http.Request, b *cmdBase, action string, versionRequired bool) (kit.Caller, *Submission, bool) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	if err := mustUUID("submission_id", b.SubmissionID); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if versionRequired && b.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return kit.Caller{}, nil, false
	}
	var s *Submission
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		s, err = getSubmission(r.Context(), tx, c.TenantID, b.SubmissionID, false)
		return err
	}); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.LegalEntityID != "" && b.LegalEntityID != s.LegalEntityID {
		m.fail(w, kit.Forbidden(kit.CodeTenantScope, "legal_entity_id does not match the submission's legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok = m.deps.Guard(w, r, s.LegalEntityID, action)
	if !ok {
		return kit.Caller{}, nil, false
	}
	return c, s, true
}

// change is one atomic submission change: the update, its history row, extra audit detail and events.
type change struct {
	upd    subUpdate
	h      hist
	events func(*Submission) []kit.Event
}

// commit applies a change inside the caller's transaction: the row (optimistic concurrency), history, audit, the mirror
// onto the document's e-invoice dimension (ONLY that dimension) and the outbox events - all in one transaction.
func (m *Module) commit(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Submission, ch change) (*Submission, error) {
	c.LegalEntityID = cur.LegalEntityID
	ch.upd.At = ch.h.At
	nh, err := updateSubmission(ctx, tx, cur, ch.upd)
	if err != nil {
		return nil, err
	}
	ch.h.Before = cur.State
	if err := writeHistory(ctx, tx, c, nh, ch.h); err != nil {
		return nil, err
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: nh.SubmissionID, Command: ch.h.Command, ReasonCode: ch.h.Reason,
		EvidenceID: ch.h.Evidence, Before: map[string]any{"state": cur.State, "version": cur.Version},
		After:  map[string]any{"state": nh.State, "version": nh.Version},
		Detail: mergeDetail(ch.h.Detail, map[string]any{"document_id": nh.DocumentID, "profile_code": nh.ProfileCode, "content_hash": nh.ContentHash})}); err != nil {
		return nil, err
	}
	if before, after := cur.State.Dimension(true), nh.State.Dimension(ch.upd.StillRequired); before != after {
		if err := m.mirror(ctx, tx, c, nh, after); err != nil {
			return nil, err
		}
	}
	if ch.events != nil {
		if err := kit.Emit(ctx, tx, c, ch.events(nh)...); err != nil {
			return nil, err
		}
	}
	return nh, nil
}

func mergeDetail(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range b {
		out[k] = v
	}
	for k, v := range a {
		out[k] = v
	}
	return out
}

// mirror writes the e-invoice dimension of the source document in the caller's transaction. It is the ONLY thing AR-05
// ever writes to the document's owner, and only through the port.
func (m *Module) mirror(ctx context.Context, tx pgx.Tx, c kit.Caller, s *Submission, dimension string) error {
	src, err := m.source(s.DocumentType)
	if err != nil {
		return err
	}
	return src.SetEInvoiceState(ctx, tx, c, s.DocumentID, dimension)
}

// binding re-reads the source document and refuses when it no longer matches the content hash the submission was bound
// to at prepare time (or when it is no longer an issued document).
func (m *Module) binding(ctx context.Context, c kit.Caller, s *Submission) (*SourceDocument, error) {
	src, err := m.source(s.DocumentType)
	if err != nil {
		return nil, err
	}
	doc, err := src.Get(ctx, c.TenantID, s.DocumentID)
	if err != nil {
		return nil, err
	}
	if doc.ContentHash == "" || doc.ContentHash != s.ContentHash {
		p := validationFailed("the source document no longer matches the content hash this submission is bound to: it is not transmitted")
		m.refusal(ctx, c, s.SubmissionID, "contentBinding", p, map[string]any{"bound_hash": s.ContentHash, "document_hash": doc.ContentHash})
		return nil, p
	}
	return doc, nil
}

// ── PrepareEInvoice ──────────────────────────────────────────────────────────

type prepareReq struct {
	DocumentID     string `json:"document_id"`
	InvoiceID      string `json:"invoice_id,omitempty"` // alias of document_id for invoices
	DocumentType   string `json:"document_type,omitempty"`
	LegalEntityID  string `json:"legal_entity_id,omitempty"`
	JurisdictionID string `json:"jurisdiction_id"`
	ProfileCode    string `json:"profile_code,omitempty"`
	Network        string `json:"network,omitempty"`
	EndpointID     string `json:"endpoint_id,omitempty"`
	ParticipantID  string `json:"participant_id,omitempty"`
	Purpose        string `json:"purpose,omitempty"`
}

func (m *Module) prepareEInvoice(w http.ResponseWriter, r *http.Request) {
	var req prepareReq
	if !kit.Decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	docType := upperTrim(req.DocumentType)
	if docType == "" {
		docType = DocInvoice
	}
	docID := trimmed(req.DocumentID)
	if req.InvoiceID != "" {
		if docID != "" && docID != trimmed(req.InvoiceID) {
			m.fail(w, kit.Invalid("document_id and invoice_id disagree"))
			return
		}
		docID = trimmed(req.InvoiceID)
	}
	jur := trimmed(req.JurisdictionID)
	switch {
	case docType != DocInvoice && docType != DocCreditNote && docType != DocDebitNote:
		m.fail(w, kit.Invalid("document_type must be INVOICE, CREDIT_NOTE or DEBIT_NOTE"))
		return
	case jur == "" || len(jur) > 32:
		m.fail(w, kit.Invalid("jurisdiction_id is required (max 32 characters)"))
		return
	case tooLong(req.ProfileCode, 128) || tooLong(req.Network, 64) || tooLong(req.EndpointID, 255) || tooLong(req.ParticipantID, 255) || tooLong(req.Purpose, 64):
		m.fail(w, kit.Invalid("profile_code (128), network (64), endpoint_id (255), participant_id (255) or purpose (64) is too long"))
		return
	}
	if err := mustUUID("document_id", docID); err != nil {
		m.fail(w, err)
		return
	}
	src, err := m.source(docType)
	if err != nil {
		m.fail(w, err)
		return
	}
	doc, err := src.Get(ctx, c.TenantID, docID) // NOT_FOUND for anything not issued
	if err != nil {
		m.fail(w, err)
		return
	}
	if req.LegalEntityID != "" && req.LegalEntityID != doc.LegalEntityID {
		m.fail(w, kit.Forbidden(kit.CodeTenantScope, "legal_entity_id does not match the document's legal entity"))
		return
	}
	c, ok = m.deps.Guard(w, r, doc.LegalEntityID, ActionSubmit)
	if !ok {
		return
	}
	refuse := func(p *kit.Problem, detail map[string]any) {
		m.refusal(ctx, c, docID, "prepareEInvoice", p, detail)
		m.fail(w, p)
	}

	// The profile code: configured for the jurisdiction (deployment config) and/or stated by the caller; they must agree.
	code := trimmed(req.ProfileCode)
	if m.catalog != nil {
		if cc, ok := m.catalog.CodeFor(jur, docType); ok {
			if code != "" && code != cc {
				refuse(validationFailed("profile %s is not the e-invoice profile configured for jurisdiction %s (%s)", code, jur, cc), map[string]any{"requested": code, "configured": cc})
				return
			}
			code = cc
		}
	}
	if code == "" {
		refuse(validationFailed("no e-invoice profile is configured for jurisdiction %s and document type %s: profile_code is required", jur, docType), nil)
		return
	}

	// One logical submission per (document, profile): a second prepare returns the existing one.
	var existing *Submission
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
		existing, err = activeFor(ctx, tx, c.TenantID, docType, docID, code)
		return err
	}); err != nil {
		m.fail(w, err)
		return
	}
	if existing != nil {
		resp := respond(existing)
		resp.Existing = true
		kit.WriteJSON(w, http.StatusOK, resp)
		return
	}

	// The profile must belong to the entity's jurisdiction: never a guessed default.
	if m.entities == nil {
		m.fail(w, kit.Unavailable("entity jurisdiction service not wired: the e-invoice profile cannot be verified"))
		return
	}
	entJur, err := m.entities.PrimaryJurisdiction(ctx, c.TenantID, doc.LegalEntityID)
	if err != nil {
		m.deps.Log.Error("ar05 entity jurisdiction unavailable - failing closed", zap.Error(err))
		m.fail(w, kit.Unavailable("the legal entity's jurisdiction could not be determined"))
		return
	}
	if !strings.EqualFold(entJur, jur) {
		refuse(validationFailed("jurisdiction %s is not the jurisdiction of the invoice's legal entity (%s): the e-invoice profile does not apply", jur, entJur),
			map[string]any{"requested_jurisdiction": jur, "entity_jurisdiction": entJur})
		return
	}
	if m.profiles == nil {
		m.fail(w, kit.Unavailable("jurisdiction profile service not wired"))
		return
	}
	prof, err := m.profiles.Resolve(ctx, c, ProfileQuery{Jurisdiction: jur, ProfileCode: code, DocumentType: docType, At: m.now()})
	switch {
	case err == nil:
	case errors.Is(err, ErrNoProfile), errors.Is(err, ErrProfileInvalid):
		refuse(validationFailed("no usable e-invoice profile %s is in force for jurisdiction %s: %v", code, jur, err), nil)
		return
	default:
		m.deps.Log.Error("ar05 profile resolution unavailable - failing closed", zap.Error(err))
		m.fail(w, kit.Unavailable("the e-invoice profile could not be resolved"))
		return
	}
	if err := prof.MatchRequest(jur, docType, code, trimmed(req.Network)); err != nil {
		var p *kit.Problem
		errors.As(err, &p)
		refuse(p, map[string]any{"profile_network": prof.Network, "profile_document_type": prof.DocumentType})
		return
	}
	rend, err := m.renderers.For(prof.Syntax)
	if err != nil {
		var p *kit.Problem
		errors.As(err, &p)
		refuse(p, map[string]any{"syntax": prof.Syntax})
		return
	}
	artifact, err := rend.Render(doc, prof)
	if err != nil {
		m.deps.Log.Error("ar05 render failed", zap.Error(err))
		m.fail(w, kit.Unavailable("the artifact could not be rendered"))
		return
	}
	snapshot, _ := json.Marshal(prof)
	now := m.now()
	s := &Submission{SubmissionID: uuid.NewString(), TenantID: c.TenantID, LegalEntityID: doc.LegalEntityID, DocumentType: docType, DocumentID: docID, DocumentNumber: doc.Number,
		ContentHash: doc.ContentHash, JurisdictionCode: jur, ProfileCode: code, ProfileVersion: prof.ProfileVersion, Network: prof.Network, Syntax: prof.Syntax, Mode: prof.Mode,
		PackVersionID: prof.PackVersionID, PackArtifactDigest: prof.PackDigest, RuleID: prof.RuleID, RuleContentDigest: prof.RuleDigest, ResolutionDecisionID: prof.DecisionID,
		EndpointID: trimmed(req.EndpointID), ParticipantID: trimmed(req.ParticipantID), Purpose: trimmed(req.Purpose), ArtifactHash: Sha256Hex(artifact),
		PreparedBy: c.PrincipalID, CreatedAt: now}
	s.ArtifactRef = "ar05://artifacts/" + s.SubmissionID

	var out *Submission
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		parent, err := latestRejected(ctx, tx, c.TenantID, docType, docID, code)
		if err != nil {
			return err
		}
		s.ParentSubmissionID = parent
		if err := insertSubmission(ctx, tx, s, snapshot, artifact, rend.ContentType()); err != nil {
			return err
		}
		s.State, s.ValidationStatus, s.Version, s.UpdatedAt = StatePrepared, "NOT_RUN", 1, now
		cc := c
		cc.LegalEntityID = s.LegalEntityID
		if err := writeHistory(ctx, tx, cc, s, hist{At: now, Command: "prepareEInvoice", Detail: map[string]any{"profile_code": code, "profile_version": s.ProfileVersion,
			"artifact_hash": s.ArtifactHash, "parent_submission_id": parent}}); err != nil {
			return err
		}
		if err := kit.RecordAudit(ctx, tx, cc, kit.Audit{AggregateType: AggregateType, AggregateID: s.SubmissionID, Command: "prepareEInvoice",
			After: map[string]any{"state": s.State, "version": 1},
			Detail: map[string]any{"document_id": docID, "document_type": docType, "content_hash": s.ContentHash, "artifact_hash": s.ArtifactHash, "profile_code": code,
				"profile_version": s.ProfileVersion, "network": s.Network, "syntax": s.Syntax, "pack_version_id": s.PackVersionID, "parent_submission_id": parent}}); err != nil {
			return err
		}
		if err := m.mirror(ctx, tx, cc, s, s.State.Dimension(true)); err != nil {
			return err
		}
		if err := kit.Emit(ctx, tx, cc, kit.Event{AggregateType: AggregateType, AggregateID: s.SubmissionID, EventType: EventPrepared,
			Payload: evPayload(c, s, map[string]any{"parent_submission_id": parent})}); err != nil {
			return err
		}
		out = s
		return nil
	})
	if kit.IsUniqueViolation(err, "uq_ar05_active_submission") {
		// A concurrent prepare won the race: return the winner.
		if rerr := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) {
			existing, e = activeFor(ctx, tx, c.TenantID, docType, docID, code)
			return e
		}); rerr == nil && existing != nil {
			resp := respond(existing)
			resp.Existing = true
			kit.WriteJSON(w, http.StatusOK, resp)
			return
		}
	}
	if err != nil {
		m.fail(w, kit.MapPgErr(err))
		return
	}
	kit.WriteJSON(w, http.StatusCreated, respond(out))
}

// ── ValidateEInvoice ─────────────────────────────────────────────────────────

func (m *Module) validateEInvoice(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	c, s, ok := m.loadSubmission(w, r, &req, ActionSubmit, true)
	if !ok {
		return
	}
	ctx := r.Context()
	if s.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if _, err := Next(s.State, TrValidate); err != nil {
		m.fail(w, err)
		return
	}
	doc, err := m.binding(ctx, c, s)
	if err != nil {
		m.fail(w, err)
		return
	}
	prof, err := s.Profile()
	if err != nil {
		m.fail(w, kit.Unavailable("the stored profile snapshot is unreadable"))
		return
	}
	var out *Submission
	var rep ValidationReport
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getSubmission(ctx, tx, c.TenantID, s.SubmissionID, true)
		if err != nil {
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		to, err := Next(cur.State, TrValidate)
		if err != nil {
			return err
		}
		artifact, _, err := loadArtifact(ctx, tx, c.TenantID, cur.SubmissionID)
		if err != nil {
			return err
		}
		rep = Validate(doc, prof, artifact, cur.ArtifactHash)
		now := m.now()
		cc := c
		cc.LegalEntityID = cur.LegalEntityID
		run, err := insertValidation(ctx, tx, cc, cur, rep, now)
		if err != nil {
			return err
		}
		status, state := "FAILED", cur.State
		if rep.Passed {
			status, state = "PASSED", to
		}
		out, err = m.commit(ctx, tx, c, cur, change{upd: subUpdate{State: state, ValidationStatus: &status},
			h: hist{At: now, Command: "validateEInvoice", Detail: map[string]any{"run_no": run, "passed": rep.Passed, "issue_count": len(rep.Issues)}}})
		return err
	})
	if err != nil {
		m.fail(w, kit.MapPgErr(err))
		return
	}
	if !rep.Passed {
		kit.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "einvoice_validation_failed", "code": kit.CodeEInvoiceValidationFailed,
			"detail": fmt.Sprintf("%d validation error(s): see getValidationErrors", len(rep.Issues)), "issues": rep.Issues, "submission": out})
		return
	}
	kit.WriteJSON(w, http.StatusOK, respond(out))
}

// ── SubmitEInvoice / RetrySubmission ─────────────────────────────────────────

// callStatus is how a transmission call ended, as classified for the state machine.
type callStatus struct {
	outcome   string // OutAcknowledged | OutAccepted | OutRejected | OutAmbiguous | OutNotSent
	res       *SubmitResult
	err       error
	downgrade bool // a synchronous ACCEPTED without a receipt was kept as an acknowledgement
}

func classifyCall(res *SubmitResult, err error) callStatus {
	if err != nil {
		if errors.Is(err, ErrNotSent) {
			return callStatus{outcome: OutNotSent, err: err}
		}
		return callStatus{outcome: OutAmbiguous, err: err} // timeout, reset, 5xx, anything else: the provider may hold the document
	}
	if res == nil {
		return callStatus{outcome: OutAmbiguous, err: errors.New("the transport returned no answer")}
	}
	switch res.Disposition {
	case DispAcknowledged:
		return callStatus{outcome: OutAcknowledged, res: res}
	case DispAccepted:
		if strings.TrimSpace(res.ReceiptID) == "" {
			return callStatus{outcome: OutAcknowledged, res: res, downgrade: true} // no false ACCEPTED without a receipt
		}
		return callStatus{outcome: OutAccepted, res: res}
	case DispRejected:
		return callStatus{outcome: OutRejected, res: res}
	}
	return callStatus{outcome: OutAmbiguous, res: res, err: fmt.Errorf("the transport answered an unknown disposition %q", res.Disposition)}
}

// transportFor resolves the wire; "no provider" is a clean 503 with nothing changed.
func (m *Module) transportFor(p *Profile) (Transport, error) {
	t, err := m.transports.For(p)
	if err != nil {
		var np *NoProviderError
		if errors.As(err, &np) {
			return nil, kit.Err(http.StatusServiceUnavailable, CodeProviderNotConfigured, err.Error())
		}
		return nil, kit.Unavailable("the e-invoice transport could not be resolved")
	}
	if t == nil {
		return nil, kit.Err(http.StatusServiceUnavailable, CodeProviderNotConfigured, "no e-invoice provider is configured for this profile")
	}
	return t, nil
}

// register registers the submission in the regulatory registry once (idempotent on a per-submission key). A registry that
// cannot answer stops the submission BEFORE anything is sent: nothing has happened yet, so failing closed is free.
func (m *Module) register(ctx context.Context, c kit.Caller, s *Submission) (*string, error) {
	if m.registry == nil || s.RegistrySubmissionID != "" {
		return nil, nil
	}
	id, err := m.registry.Register(ctx, c, RegistryRegistration{Jurisdiction: s.JurisdictionCode, ProfileCode: s.ProfileCode, EffectiveAt: m.now(),
		SubjectRef: s.DocumentType + ":" + s.DocumentID, PayloadHash: "sha256:" + s.ArtifactHash, Key: "ar05-reg-" + s.SubmissionID})
	if err != nil {
		m.deps.Log.Error("ar05 regulatory registry unavailable - nothing was sent", zap.Error(err))
		return nil, kit.Unavailable("the regulatory submission registry is unavailable: nothing was sent")
	}
	return &id, nil
}

func (m *Module) submitEInvoice(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	c, s, ok := m.loadSubmission(w, r, &req, ActionSubmit, true)
	if !ok {
		return
	}
	if s.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	switch s.State {
	case StateSubmitted, StateAccepted:
		// Duplicate-submission suppression: the document is already with the provider.
		resp := respond(s)
		resp.DuplicateSuppressed = true
		kit.WriteJSON(w, http.StatusOK, resp)
		return
	case StateUnknown:
		m.fail(w, stateUnknown("a transmission attempt is already pending with an unknown outcome: use retrySubmission (reconciles first) or reconcileSubmissionState"))
		return
	}
	if _, err := Next(s.State, TrBeginAttempt); err != nil {
		m.fail(w, err)
		return
	}
	m.transmit(w, r, c, s, req.ExpectedVersion, nil, "submitEInvoice", req.ReasonCode)
}

// transmit runs one attempt: bind to the content hash, resolve the wire, register, record the attempt durably (state
// PENDING_UNKNOWN) in tx1, make the call OUTSIDE any transaction, then record the classified outcome in tx2. A crash
// between the two leaves PENDING_UNKNOWN with an in-flight attempt: the safe, reconcilable state.
func (m *Module) transmit(w http.ResponseWriter, r *http.Request, c kit.Caller, s *Submission, expected *int, retry *retryPlan, command, reason string) {
	ctx := r.Context()
	if _, err := m.binding(ctx, c, s); err != nil {
		m.fail(w, err)
		return
	}
	prof, err := s.Profile()
	if err != nil {
		m.fail(w, kit.Unavailable("the stored profile snapshot is unreadable"))
		return
	}
	tr, err := m.transportFor(prof)
	if err != nil {
		m.fail(w, err)
		return
	}
	var artifact []byte
	var ctype string
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) {
		artifact, ctype, e = loadArtifact(ctx, tx, c.TenantID, s.SubmissionID)
		return e
	}); err != nil {
		m.fail(w, err)
		return
	}
	if Sha256Hex(artifact) != s.ArtifactHash {
		m.fail(w, validationFailed("the stored artifact no longer hashes to the recorded artifact hash: it is not transmitted"))
		return
	}
	regID, err := m.register(ctx, c, s)
	if err != nil {
		m.fail(w, err)
		return
	}

	// tx1: the durable attempt, BEFORE the call.
	attemptID := uuid.NewString()
	var started *Submission
	var att *Attempt
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getSubmission(ctx, tx, c.TenantID, s.SubmissionID, true)
		if err != nil {
			return err
		}
		tran := TrBeginAttempt
		parent, recon := "", ""
		if retry != nil {
			tran, parent, recon = TrRetryAttempt, retry.ParentAttemptID, retry.ReconciliationID
			last, err := latestAttempt(ctx, tx, c.TenantID, cur.SubmissionID)
			if err != nil {
				return err
			}
			// Only the attempt that reconciliation proved was not received may be followed.
			if last == nil || last.ExternalAttemptID != parent || last.Outcome != OutNotReceived {
				return stateUnknown("the earlier attempt is not confirmed as not received: nothing is re-sent")
			}
			if err := appendResult(ctx, tx, c, cur, parent, resultWrite{Outcome: OutSuperseded, Source: SrcSystem, At: m.now(), Detail: map[string]any{"superseded_by": attemptID}}); err != nil {
				return err
			}
		} else if expected != nil && cur.Version != *expected {
			return kit.ErrStaleVersion
		}
		to, err := Next(cur.State, tran)
		if err != nil {
			return err
		}
		now := m.now()
		prov := tr.Name()
		digest := Sha256Hex([]byte(cur.ArtifactHash + "|" + cur.EndpointID + "|" + cur.ParticipantID + "|" + attemptID))
		a, err := insertAttempt(ctx, tx, c, cur, attemptStart{ExternalID: attemptID, Provider: prov, ArtifactHash: cur.ArtifactHash, RequestDigest: digest, ParentID: parent,
			ReconciliationID: recon, At: now})
		if err != nil {
			return err
		}
		att = a
		started, err = m.commit(ctx, tx, c, cur, change{upd: subUpdate{State: to, Provider: &prov, RegistryID: regID, AttemptsDelta: 1},
			h: hist{At: now, Command: command, Reason: reason, Detail: map[string]any{"external_attempt_id": attemptID, "attempt_no": a.AttemptNo, "provider": prov, "parent_attempt_id": parent}}})
		return err
	})
	if err != nil {
		m.fail(w, kit.MapPgErr(err))
		return
	}

	// The call. Its context is NOT the request's: a client disconnect must not turn an in-flight send into "failed".
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.SubmitTimeout)
	res, callErr := tr.Submit(callCtx, SubmitRequest{ExternalAttemptID: attemptID, SubmissionID: s.SubmissionID, TenantID: c.TenantID, LegalEntityID: s.LegalEntityID,
		CorrelationID: c.CorrelationID, ProfileCode: s.ProfileCode, ProfileVersion: s.ProfileVersion, Network: s.Network, Syntax: s.Syntax, Mode: s.Mode, DocumentType: s.DocumentType,
		DocumentNumber: s.DocumentNumber, EndpointID: s.EndpointID, ParticipantID: s.ParticipantID, Purpose: s.Purpose, ContentType: ctype, Artifact: artifact, ArtifactHash: s.ArtifactHash})
	cancel()
	st := classifyCall(res, callErr)

	final, err := m.finishAttempt(context.WithoutCancel(ctx), c, started, att, st)
	if err != nil {
		// The result could not be recorded: the attempt stays IN_FLIGHT and the state stays PENDING_UNKNOWN (safe, reconcilable).
		m.deps.Log.Error("ar05 could not record the outcome of a transmission attempt: left PENDING_UNKNOWN", zap.Error(err), zap.String("external_attempt_id", attemptID))
		m.fail(w, kit.Err(http.StatusServiceUnavailable, kit.CodeDependencyUnavailable, "the outcome could not be recorded; the submission is PENDING_UNKNOWN until reconciled"))
		return
	}
	m.syncRegistry(context.WithoutCancel(ctx), c, final, st, attemptID)
	resp := respond(final)
	resp.ExternalAttemptID = attemptID
	switch st.outcome {
	case OutAmbiguous:
		resp.Outcome, resp.Code = string(StateUnknown), kit.CodeExternalStateUnknown
		resp.Detail = "the provider's answer is unknown; the submission is PENDING_UNKNOWN and nothing is re-sent until it is reconciled"
		kit.WriteJSON(w, http.StatusAccepted, resp)
	case OutNotSent:
		m.fail(w, kit.Err(http.StatusServiceUnavailable, kit.CodeDependencyUnavailable, "the document could not be sent (nothing reached the provider); the submission is VALIDATED and may be submitted again"))
	default:
		resp.Outcome = st.outcome
		kit.WriteJSON(w, http.StatusOK, resp)
	}
}

// finishAttempt is tx2: append the result and, if the submission is still waiting on this attempt, move the state.
func (m *Module) finishAttempt(ctx context.Context, c kit.Caller, started *Submission, att *Attempt, st callStatus) (*Submission, error) {
	var out *Submission
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getSubmission(ctx, tx, c.TenantID, started.SubmissionID, true)
		if err != nil {
			return err
		}
		now := m.now()
		cc := c
		cc.LegalEntityID = cur.LegalEntityID
		w := resultWrite{Outcome: st.outcome, Source: SrcTransport, At: now, Detail: map[string]any{}}
		if st.res != nil {
			w.ProviderRef, w.ResponseDigest = st.res.ProviderRef, st.res.ResponseDigest
			if st.res.ReasonCode != "" {
				w.Detail["reason_code"] = st.res.ReasonCode
			}
			if st.res.Message != "" {
				w.Detail["message"] = truncate(st.res.Message, maxText)
			}
			if st.res.ReceiptID != "" {
				w.Detail["receipt_id"] = st.res.ReceiptID
			}
		}
		if st.downgrade {
			w.Detail["accepted_without_receipt"] = true
		}
		if st.err != nil {
			w.Detail["error"] = truncate(st.err.Error(), 500)
		}
		if err := appendResult(ctx, tx, cc, cur, att.ExternalAttemptID, w); err != nil {
			return err
		}
		if cur.State != StateUnknown {
			// A callback or reconciliation already moved the submission: the result is recorded, the state is not touched.
			out = cur
			return nil
		}
		var tran Transition
		var events func(*Submission) []kit.Event
		switch st.outcome {
		case OutAcknowledged:
			tran = TrAck
			events = func(n *Submission) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: n.SubmissionID, EventType: EventSubmitted,
					Payload: evPayload(c, n, map[string]any{"external_attempt_id": att.ExternalAttemptID, "provider": att.Provider, "provider_reference": w.ProviderRef})}}
			}
		case OutAccepted:
			tran = TrAccept
			events = func(n *Submission) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: n.SubmissionID, EventType: EventAccepted,
					Payload: evPayload(c, n, map[string]any{"external_attempt_id": att.ExternalAttemptID, "provider_reference": w.ProviderRef, "receipt_id": st.res.ReceiptID})}}
			}
		case OutRejected:
			tran = TrReject
			events = func(n *Submission) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: n.SubmissionID, EventType: EventRejected,
					Payload: evPayload(c, n, map[string]any{"external_attempt_id": att.ExternalAttemptID, "reason_code": st.res.ReasonCode, "message": truncate(st.res.Message, 500)})}}
			}
		case OutNotSent:
			tran = TrNotSent
		default: // ambiguous: stay PENDING_UNKNOWN
			events = func(n *Submission) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: n.SubmissionID, EventType: EventStateUnknown,
					Payload: evPayload(c, n, map[string]any{"external_attempt_id": att.ExternalAttemptID, "provider": att.Provider})}}
			}
		}
		to := cur.State
		if tran != "" {
			if to, err = Next(cur.State, tran); err != nil {
				return err
			}
		}
		upd := subUpdate{State: to}
		if w.ProviderRef != "" {
			upd.LastProviderRef = &w.ProviderRef
		}
		out, err = m.commit(ctx, tx, c, cur, change{upd: upd, events: events,
			h: hist{At: now, Command: "attemptOutcome", Detail: map[string]any{"external_attempt_id": att.ExternalAttemptID, "outcome": st.outcome, "provider_ref": w.ProviderRef}}})
		return err
	})
	return out, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// syncRegistry mirrors a final provider answer to the regulatory registry. Best effort AFTER the local commit: AR's own
// records are authoritative, a registry hiccup is logged and does not undo the evidence.
func (m *Module) syncRegistry(ctx context.Context, c kit.Caller, s *Submission, st callStatus, attemptID string) {
	if m.registry == nil || s == nil || s.RegistrySubmissionID == "" {
		return
	}
	var status, receipt string
	switch st.outcome {
	case OutAccepted:
		status, receipt = "ACCEPTED", st.res.ReceiptID
	case OutRejected:
		status = "REJECTED"
	default:
		return
	}
	m.pushRegistry(ctx, c, s, status, receipt, attemptID+":"+st.outcome)
}

func (m *Module) pushRegistry(ctx context.Context, c kit.Caller, s *Submission, status, receipt, eventID string) {
	prof, err := s.Profile()
	if err != nil || !prof.HasLifecycleState(status) {
		return
	}
	if err := m.registry.RecordEvent(ctx, c, s.RegistrySubmissionID, RegistryEvent{ProviderEventID: eventID, Status: status, ReceiptID: receipt, OccurredAt: m.now(),
		Detail: map[string]any{"external_submission_id": s.SubmissionID}}); err != nil {
		m.deps.Log.Warn("ar05 could not mirror the outcome to the regulatory registry", zap.Error(err), zap.String("submission_id", s.SubmissionID))
	}
}

// retryPlan is the proof that an earlier attempt was not received.
type retryPlan struct{ ParentAttemptID, ReconciliationID string }

func (m *Module) retrySubmission(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	c, s, ok := m.loadSubmission(w, r, &req, ActionSubmit, true)
	if !ok {
		return
	}
	if s.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	switch s.State {
	case StateValidated:
		// Nothing in flight (a definite NOT_SENT earlier): a plain submit.
		m.transmit(w, r, c, s, req.ExpectedVersion, nil, "retrySubmission", req.ReasonCode)
		return
	case StateUnknown:
	case StateSubmitted:
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, "the provider has the document and is processing it: nothing to retry, use reconcileSubmissionState"))
		return
	default:
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("retrySubmission is not allowed from state %s", s.State)))
		return
	}
	prof, err := s.Profile()
	if err != nil {
		m.fail(w, kit.Unavailable("the stored profile snapshot is unreadable"))
		return
	}
	tr, err := m.transportFor(prof)
	if err != nil {
		m.fail(w, err)
		return
	}
	// Reconcile FIRST. A retry never re-sends on a guess.
	rec, err := m.reconcile(r.Context(), c, s, tr, reconcileInput{Method: MethodQuery, Trigger: "RETRY", Reason: req.ReasonCode})
	if err != nil {
		m.fail(w, err)
		return
	}
	if rec.ProviderState != ProviderNotReceived {
		f := false
		resp := respond(rec.Submission)
		resp.Retried, resp.Reconciliation = &f, rec.Record
		resp.Detail = "the provider confirmed an earlier attempt (" + rec.ProviderState + "): the state was reconciled and nothing was re-sent"
		kit.WriteJSON(w, http.StatusOK, resp)
		return
	}
	// The provider confirmed the earlier attempt was NOT received: only now may a new attempt be created.
	m.transmit(w, r, c, rec.Submission, nil, &retryPlan{ParentAttemptID: rec.AttemptID, ReconciliationID: rec.Record.ReconciliationID}, "retrySubmission", req.ReasonCode)
}

// ── ReconcileSubmissionState ─────────────────────────────────────────────────

type reconcileReq struct {
	cmdBase
	Method        string `json:"method"`                   // QUERY_PROVIDER (default) | ADMIN_EVIDENCE
	ResolvedState string `json:"resolved_state,omitempty"` // ADMIN_EVIDENCE: ACCEPTED | REJECTED | NOT_RECEIVED
	ReceiptID     string `json:"receipt_id,omitempty"`     // ADMIN_EVIDENCE with ACCEPTED
	ProviderRef   string `json:"provider_reference,omitempty"`
}

func (m *Module) reconcileSubmissionState(w http.ResponseWriter, r *http.Request) {
	var req reconcileReq
	if !kit.Decode(w, r, &req) {
		return
	}
	method := req.Method
	if method == "" {
		method = MethodQuery
	}
	switch method {
	case MethodQuery:
	case MethodAdmin:
		// The controlled manual path: privileged action + reason_code + evidence_id are all mandatory.
		if !kit.RequireReason(w, req.ReasonCode) {
			return
		}
		if trimmed(req.EvidenceID) == "" {
			m.fail(w, evidenceRequired())
			return
		}
		switch req.ResolvedState {
		case ProviderAccepted, ProviderRejected, ProviderNotReceived:
		default:
			m.fail(w, kit.Invalid("resolved_state must be ACCEPTED, REJECTED or NOT_RECEIVED"))
			return
		}
		if req.ResolvedState == ProviderAccepted && trimmed(req.ReceiptID) == "" && trimmed(req.ProviderRef) == "" {
			m.fail(w, kit.Invalid("receipt_id (or provider_reference) is required to record ACCEPTED: no acceptance without a receipt"))
			return
		}
	default:
		m.fail(w, kit.Invalid("method must be QUERY_PROVIDER or ADMIN_EVIDENCE"))
		return
	}
	if tooLong(req.ReasonCode, 128) || tooLong(req.EvidenceID, 255) || tooLong(req.ReceiptID, 256) || tooLong(req.ProviderRef, 255) {
		m.fail(w, kit.Invalid("reason_code (128), evidence_id (255), receipt_id (256) or provider_reference (255) is too long"))
		return
	}
	c, s, ok := m.loadSubmission(w, r, &req.cmdBase, ActionReconcile, true)
	if !ok {
		return
	}
	if method == MethodAdmin && !m.deps.Authorize(w, r, c, ActionAdapterAdmin) {
		return
	}
	if s.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if s.State != StateUnknown && s.State != StateSubmitted {
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("only a SUBMITTED or PENDING_UNKNOWN submission can be reconciled (state %s)", s.State)))
		return
	}
	var rec *reconResult
	var err error
	if method == MethodAdmin {
		rec, err = m.reconcile(r.Context(), c, s, nil, reconcileInput{Method: MethodAdmin, Trigger: "RECONCILE", Reason: req.ReasonCode, Evidence: req.EvidenceID,
			Admin: &StatusResult{State: req.ResolvedState, ReceiptID: trimmed(req.ReceiptID), ProviderRef: trimmed(req.ProviderRef)}})
	} else {
		prof, perr := s.Profile()
		if perr != nil {
			m.fail(w, kit.Unavailable("the stored profile snapshot is unreadable"))
			return
		}
		var tr Transport
		if tr, err = m.transportFor(prof); err != nil {
			m.fail(w, err)
			return
		}
		rec, err = m.reconcile(r.Context(), c, s, tr, reconcileInput{Method: MethodQuery, Trigger: "RECONCILE", Reason: req.ReasonCode, Evidence: req.EvidenceID})
	}
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := respond(rec.Submission)
	resp.Reconciliation = rec.Record
	kit.WriteJSON(w, http.StatusOK, resp)
}

type reconcileInput struct {
	Method, Trigger, Reason, Evidence string
	Admin                             *StatusResult // ADMIN_EVIDENCE: the stated resolution
}

type reconResult struct {
	Submission    *Submission
	Record        *Reconciliation
	ProviderState string
	AttemptID     string
}

// reconcile settles what the provider holds for the latest attempt and records it as evidence. For QUERY_PROVIDER the
// provider is asked OUTSIDE any transaction (bounded by SubmitTimeout); an unknown or failed answer is recorded and
// returned as EXTERNAL_STATE_UNKNOWN: it never moves the state and never permits a re-send.
func (m *Module) reconcile(ctx context.Context, c kit.Caller, s *Submission, tr Transport, in reconcileInput) (*reconResult, error) {
	var att *Attempt
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) {
		att, e = latestAttempt(ctx, tx, c.TenantID, s.SubmissionID)
		return e
	}); err != nil {
		return nil, err
	}
	if att == nil {
		return nil, kit.Conflict(kit.CodeInvalidTransition, "the submission has no transmission attempt to reconcile")
	}
	// SoD of the manual path: whoever started the attempt does not resolve it by hand.
	if in.Method == MethodAdmin && att.StartedBy == c.PrincipalID {
		p := kit.Forbidden(kit.CodeSegregationOfDuties, "the principal who started the transmission attempt cannot resolve it manually")
		m.refusal(ctx, c, s.SubmissionID, "reconcileSubmissionState", p, map[string]any{"external_attempt_id": att.ExternalAttemptID})
		return nil, p
	}
	// An attempt still inside its call window may simply be slow: asking now could answer "not received" while the
	// request is on the wire, and a re-send would then duplicate it.
	if in.Method == MethodQuery && att.Outcome == OutInFlight && m.now().Sub(att.StartedAt) < 2*m.SubmitTimeout+10*time.Second {
		return nil, stateUnknown("the latest attempt %s is still within its call window: it is not reconciled or re-sent yet", att.ExternalAttemptID)
	}
	var st *StatusResult
	var qerr error
	if in.Method == MethodAdmin {
		st = in.Admin
	} else {
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.SubmitTimeout)
		st, qerr = tr.QueryStatus(qctx, QueryRequest{ExternalAttemptID: att.ExternalAttemptID, ProviderRef: att.ProviderRef, SubmissionID: s.SubmissionID, TenantID: c.TenantID,
			CorrelationID: c.CorrelationID, ProfileCode: s.ProfileCode, Network: s.Network})
		cancel()
	}
	providerState := ProviderUnknown
	if qerr != nil {
		providerState = "ERROR"
	} else if st != nil {
		providerState = st.State
	}
	switch providerState {
	case ProviderReceived, ProviderAccepted, ProviderRejected, ProviderNotReceived, ProviderUnknown, "ERROR":
	default:
		providerState = ProviderUnknown
	}
	if providerState == ProviderAccepted && in.Method == MethodQuery && st != nil && strings.TrimSpace(st.ReceiptID) == "" && strings.TrimSpace(st.ProviderRef) == "" {
		providerState = ProviderReceived // no ACCEPTED without a receipt
	}

	var out *Submission
	rec := &Reconciliation{ReconciliationID: uuid.NewString(), SubmissionID: s.SubmissionID, ExternalAttemptID: att.ExternalAttemptID, Method: in.Method, Trigger: in.Trigger,
		ProviderState: providerState, ReasonCode: in.Reason, EvidenceID: in.Evidence, PerformedBy: c.PrincipalID}
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getSubmission(ctx, tx, c.TenantID, s.SubmissionID, true)
		if err != nil {
			return err
		}
		cc := c
		cc.LegalEntityID = cur.LegalEntityID
		now := m.now()
		rec.PerformedAt, rec.StateBefore, rec.StateAfter = now, string(cur.State), string(cur.State)
		if st != nil {
			rec.ProviderRef, rec.ReceiptID = st.ProviderRef, st.ReceiptID
		}
		if qerr != nil {
			rec.Detail = map[string]any{"error": truncate(qerr.Error(), 500)}
		}
		src := SrcReconciliation
		if in.Method == MethodAdmin {
			src = SrcAdmin
		}
		rw := resultWrite{Source: src, At: now, Detail: map[string]any{"reconciliation_id": rec.ReconciliationID}}
		if st != nil {
			rw.ProviderRef, rw.ResponseDigest = st.ProviderRef, st.ResponseDigest
		}
		var tran Transition
		var ev []kit.Event
		ack := ""
		switch providerState {
		case ProviderAccepted:
			rw.Outcome, tran, ack = pick(in.Method == MethodAdmin, OutAdminAccept, OutAccepted), TrAccept, "ACCEPTED"
		case ProviderRejected:
			rw.Outcome, tran, ack = pick(in.Method == MethodAdmin, OutAdminReject, OutRejected), TrReject, "REJECTED"
		case ProviderReceived:
			rw.Outcome, tran = OutReceived, TrReceived
		case ProviderNotReceived:
			rw.Outcome = pick(in.Method == MethodAdmin, OutAdminNotRecv, OutNotReceived)
		default:
			rw.Outcome = OutUnresolved
		}
		to := cur.State
		applied := false
		if tran != "" {
			if nx, err := Next(cur.State, tran); err == nil {
				to, applied = nx, true
			} else if cur.State == StateSubmitted && tran == TrReceived {
				applied = false // already SUBMITTED: nothing to move
			}
		}
		rec.StateAfter = string(to)
		if err := appendResult(ctx, tx, cc, cur, att.ExternalAttemptID, rw); err != nil {
			return err
		}
		if err := insertReconciliation(ctx, tx, cur, rec); err != nil {
			return err
		}
		if providerState != ProviderUnknown && providerState != "ERROR" {
			ev = append(ev, kit.Event{AggregateType: AggregateType, AggregateID: cur.SubmissionID, EventType: EventReconciled,
				Payload: evPayload(cc, cur, map[string]any{"reconciliation_id": rec.ReconciliationID, "method": in.Method, "provider_state": providerState, "state_before": cur.State,
					"state_after": to, "external_attempt_id": att.ExternalAttemptID})})
		}
		if ack != "" && applied {
			a := &Ack{SubmissionID: cur.SubmissionID, ExternalAttemptID: att.ExternalAttemptID, Provider: att.Provider, ProviderEventID: "recon:" + rec.ReconciliationID, Status: ack,
				ReceiptID: rec.ReceiptID, ProviderReference: rec.ProviderRef, OccurredAt: now, ReceivedAt: now, Source: pick(in.Method == MethodAdmin, "ADMIN", "RECONCILIATION"),
				Disposition: "APPLIED", StateAfter: string(to), RecordedBy: c.PrincipalID}
			if err := insertAck(ctx, tx, cur, a); err != nil {
				return err
			}
		}
		if !applied {
			// The state does not move, but the evidence of the reconciliation is written (own audit row, no version bump).
			if err := kit.RecordAudit(ctx, tx, cc, kit.Audit{AggregateType: AggregateType, AggregateID: cur.SubmissionID, Command: "reconcileSubmissionState", ReasonCode: in.Reason,
				EvidenceID: in.Evidence, Before: map[string]any{"state": cur.State}, After: map[string]any{"state": cur.State},
				Detail: map[string]any{"reconciliation_id": rec.ReconciliationID, "provider_state": providerState, "method": in.Method, "external_attempt_id": att.ExternalAttemptID}}); err != nil {
				return err
			}
			if len(ev) > 0 {
				if err := kit.Emit(ctx, tx, cc, ev...); err != nil {
					return err
				}
			}
			out = cur
			return nil
		}
		if providerState == ProviderAccepted {
			ev = append(ev, kit.Event{AggregateType: AggregateType, AggregateID: cur.SubmissionID, EventType: EventAccepted,
				Payload: evPayload(cc, cur, map[string]any{"external_attempt_id": att.ExternalAttemptID, "receipt_id": rec.ReceiptID, "provider_reference": rec.ProviderRef, "via": in.Method})})
		}
		if providerState == ProviderRejected {
			ev = append(ev, kit.Event{AggregateType: AggregateType, AggregateID: cur.SubmissionID, EventType: EventRejected,
				Payload: evPayload(cc, cur, map[string]any{"external_attempt_id": att.ExternalAttemptID, "via": in.Method})})
		}
		if providerState == ProviderReceived {
			ev = append(ev, kit.Event{AggregateType: AggregateType, AggregateID: cur.SubmissionID, EventType: EventSubmitted,
				Payload: evPayload(cc, cur, map[string]any{"external_attempt_id": att.ExternalAttemptID, "via": in.Method})})
		}
		upd := subUpdate{State: to}
		if rec.ProviderRef != "" {
			upd.LastProviderRef = &rec.ProviderRef
		}
		out, err = m.commit(ctx, tx, c, cur, change{upd: upd, events: func(n *Submission) []kit.Event {
			for i := range ev {
				if p, ok := ev[i].Payload.(map[string]any); ok {
					p["state"], p["version"] = n.State, n.Version
				}
			}
			return ev
		}, h: hist{At: now, Command: "reconcileSubmissionState", Reason: in.Reason, Evidence: in.Evidence,
			Detail: map[string]any{"reconciliation_id": rec.ReconciliationID, "provider_state": providerState, "method": in.Method, "external_attempt_id": att.ExternalAttemptID}}})
		return err
	})
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	if out.State == StateAccepted || out.State == StateRejected {
		m.pushRegistry(context.WithoutCancel(ctx), c, out, string(out.State), rec.ReceiptID, "recon:"+rec.ReconciliationID)
	}
	if providerState == ProviderUnknown || providerState == "ERROR" {
		return nil, stateUnknown("the provider could not state whether it holds attempt %s: the submission stays PENDING_UNKNOWN and nothing is re-sent (reconciliation %s)", att.ExternalAttemptID, rec.ReconciliationID)
	}
	return &reconResult{Submission: out, Record: rec, ProviderState: providerState, AttemptID: att.ExternalAttemptID}, nil
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// ── CancelSubmissionWherePermitted ───────────────────────────────────────────

func (m *Module) cancelSubmission(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if tooLong(req.ReasonCode, 128) {
		m.fail(w, kit.Invalid("reason_code is too long"))
		return
	}
	c, s, ok := m.loadSubmission(w, r, &req, ActionSubmit, true)
	if !ok {
		return
	}
	ctx := r.Context()
	if s.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	// Whether the document still needs an e-invoice once this submission is gone (computed outside the transaction).
	still := true
	if req, err := m.Required(ctx, c.TenantID, s.LegalEntityID, s.JurisdictionCode, s.DocumentType); err == nil {
		still = req
	}
	var out *Submission
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getSubmission(ctx, tx, c.TenantID, s.SubmissionID, true)
		if err != nil {
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		to, err := Next(cur.State, TrCancel)
		if err != nil {
			return kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("a %s submission cannot be cancelled: after transmission a correction document or the provider's own cancellation applies", cur.State))
		}
		if cur.AttemptCount > 0 {
			return kit.Conflict(kit.CodeInvalidTransition, "the document was already transmitted: the submission cannot be cancelled")
		}
		out, err = m.commit(ctx, tx, c, cur, change{upd: subUpdate{State: to, StillRequired: still},
			h: hist{At: m.now(), Command: "cancelSubmissionWherePermitted", Reason: req.ReasonCode, Evidence: req.EvidenceID}})
		return err
	})
	if err != nil {
		m.fail(w, kit.MapPgErr(err))
		return
	}
	kit.WriteJSON(w, http.StatusOK, respond(out))
}
