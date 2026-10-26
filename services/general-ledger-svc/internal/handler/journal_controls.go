package handler

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// Manual-journal controls of the accounting kernel (standard §9). Manual journals are the high-risk path, so each control
// is enforced here on the server rather than left to the client.
const (
	actionWithdrawJournal   = "GL_JOURNAL_WITHDRAW"
	actionRecurringManage   = "GL_RECURRING_MANAGE"
	actionRecurringGenerate = "GL_RECURRING_GENERATE"
)

// JournalControlStore is what the manual-journal controls need beyond the journal Store. PgStore implements it.
type JournalControlStore interface {
	WithdrawJournal(ctx context.Context, tenantID, journalID, principalID, reason string) error
	CreateRecurringTemplate(ctx context.Context, t *domain.RecurringTemplate) error
	GetRecurringTemplate(ctx context.Context, tenantID, id string) (*domain.RecurringTemplate, error)
	ListRecurringTemplates(ctx context.Context, tenantID, legalEntityID string) ([]domain.RecurringTemplate, error)
	SetRecurringTemplateStatus(ctx context.Context, tenantID, id, status string) error
	GenerateRecurringJournal(ctx context.Context, tenantID, templateID string, runDate domain.Date, principalID string) (*domain.JournalHeader, []domain.JournalLine, bool, error)
	AccountControls(ctx context.Context, tenantID string, codes []string) (map[string]domain.AccountControl, error)
	CheckReconciliationCase(ctx context.Context, tenantID, caseID, legalEntityID string) (bookCode string, err error)
}

// ManualJournalPolicy is the configured evidence rule of §9: evidence is mandatory above a threshold, or for designated
// journal types. The zero value requires nothing.
type ManualJournalPolicy struct {
	// EvidenceThresholdCents is the journal total above which a manual journal must carry evidence; 0 disables it.
	EvidenceThresholdCents int64
	// EvidenceTypes are the journal types that always need evidence.
	EvidenceTypes map[domain.JournalType]bool
}

func (p ManualJournalPolicy) evidenceRequired(h *domain.JournalHeader, totalCents int64) bool {
	if len(h.EvidenceRefs) > 0 || h.SourceEventID != nil || h.SourceAccountingEventID != nil {
		return false
	}
	return (p.EvidenceThresholdCents > 0 && totalCents > p.EvidenceThresholdCents) || p.EvidenceTypes[h.JournalType]
}

// WithJournalControls wires the manual-journal controls. Without it the withdraw and recurring routes answer 501 and the
// evidence rule is not applied.
func (h *Handler) WithJournalControls(c JournalControlStore, p ManualJournalPolicy) *Handler {
	h.ctl, h.policy = c, p
	return h
}

func optionalString(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func (h *Handler) controls(w http.ResponseWriter) bool {
	if h.ctl == nil {
		writeError(w, http.StatusNotImplemented, "journal_controls_not_configured", "")
		return false
	}
	return true
}

// requireEvidence applies the configured evidence rule to a manual journal about to be submitted for approval.
func (h *Handler) requireEvidence(w http.ResponseWriter, header *domain.JournalHeader, totalCents int64) bool {
	if h.policy.evidenceRequired(header, totalCents) {
		writeError(w, http.StatusUnprocessableEntity, "evidence_required",
			"this manual journal needs evidence (a document or reference) before it can be submitted for approval")
		return false
	}
	return true
}

// refuseFutureDated refuses to post a manual journal before its posting date (§9): a future-dated journal may be prepared
// and approved ahead of time, but cannot post until its date. System journals are not delayed.
func (h *Handler) refuseFutureDated(w http.ResponseWriter, header *domain.JournalHeader) bool {
	if header.SourceEventID != nil || header.SourceAccountingEventID != nil {
		return true
	}
	today := h.now().UTC().Truncate(24 * time.Hour)
	if header.PostingDate.After(today) {
		writeError(w, http.StatusConflict, "posting_date_in_future",
			"this journal is dated "+header.PostingDate.String()+" and cannot post before then")
		return false
	}
	return true
}

// ── POST /v1/journals/{id}/withdraw ─────────────────────────────────────────────

type withdrawRequest struct {
	Reason string `json:"reason"`
}

// WithdrawJournal withdraws a draft (§9). A submitted or posted journal is not withdrawn: it is rejected or reversed.
func (h *Handler) WithdrawJournal(w http.ResponseWriter, r *http.Request) {
	var req withdrawRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	if !h.controls(w) {
		return
	}
	journalID := chi.URLParam(r, "journal_id")
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("WithdrawJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if header == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionWithdrawJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.ctl.WithdrawJournal(r.Context(), tenantID, journalID, principalID, req.Reason); err != nil {
		switch {
		case errors.Is(err, domain.ErrJournalNotFound):
			writeError(w, http.StatusNotFound, "journal_not_found", "")
		case errors.Is(err, domain.ErrNotWithdrawable):
			writeError(w, http.StatusUnprocessableEntity, "not_withdrawable", err.Error())
		default:
			h.log.Error("WithdrawJournal: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		}
		return
	}
	updated, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusOK, map[string]string{"journal_id": journalID, "approval_status": string(domain.ApprovalStatusCancelled)})
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/journals/{id}/copy ─────────────────────────────────────────────────

type copyRequest struct {
	CorrelationID   string      `json:"correlation_id"`
	FiscalPeriod    string      `json:"fiscal_period,omitempty"`
	Description     string      `json:"description,omitempty"`
	TransactionDate domain.Date `json:"transaction_date,omitempty"`
	PostingDate     domain.Date `json:"posting_date,omitempty"`

	OverrideControlAccountRestriction bool   `json:"override_control_account_restriction,omitempty"`
	OverrideSoftClose                 bool   `json:"override_soft_close,omitempty"`
	SoftCloseOverrideReason           string `json:"soft_close_override_reason,omitempty"`
}

// CopyJournal creates a new draft from an existing journal (§9). The copy has a new identity and provenance and runs the
// same checks as any new journal; it never inherits posting identifiers, numbers or approvals.
func (h *Handler) CopyJournal(w http.ResponseWriter, r *http.Request) {
	var req copyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.CorrelationID) == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "correlation_id")
		return
	}
	journalID := chi.URLParam(r, "journal_id")
	src, lines, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("CopyJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if src == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}
	cr := domain.CreateJournalRequest{
		TenantID: src.TenantID, LegalEntityID: src.LegalEntityID, FiscalPeriod: src.FiscalPeriod, Description: src.Description,
		CorrelationID: req.CorrelationID, JournalType: src.JournalType, TransactionDate: src.TransactionDate, PostingDate: src.PostingDate,
		CurrencyCode: src.CurrencyCode, BookID: src.BookID, ReportingBasis: src.ReportingBasis, EvidenceRefs: src.EvidenceRefs,
		OverrideControlAccountRestriction: req.OverrideControlAccountRestriction, OverrideSoftClose: req.OverrideSoftClose,
		SoftCloseOverrideReason: req.SoftCloseOverrideReason,
	}
	if src.JournalType == domain.JournalTypeReversal {
		// a copy of a reversal would be an unattached reversal; the journal being copied is a fresh adjustment instead
		cr.JournalType = domain.JournalTypeAdjustment
	}
	if src.JournalPurpose != nil {
		cr.JournalPurpose = *src.JournalPurpose
	}
	if req.FiscalPeriod != "" {
		cr.FiscalPeriod = req.FiscalPeriod
	}
	if strings.TrimSpace(req.Description) != "" {
		cr.Description = req.Description
	}
	if !req.TransactionDate.IsZero() {
		cr.TransactionDate = req.TransactionDate
	}
	if !req.PostingDate.IsZero() {
		cr.PostingDate = req.PostingDate
	}
	for _, l := range lines {
		cr.Lines = append(cr.Lines, domain.CreateJournalLineInput{
			AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount, Description: l.Description,
			TaxCode: l.TaxCode, TaxLogicSnapshotID: l.TaxLogicSnapshotID, Dimensions: l.Dimensions, CounterpartyID: l.CounterpartyID,
		})
	}
	if cr.JournalType == domain.JournalTypeAdjustment && cr.JournalPurpose == "" {
		cr.JournalPurpose = "OTHER"
	}
	h.createJournal(w, r, cr, &journalID)
}

// ── recurring templates ─────────────────────────────────────────────────────────

type templateRequest struct {
	LegalEntityID  string                 `json:"legal_entity_id"`
	Name           string                 `json:"name"`
	JournalType    domain.JournalType     `json:"journal_type"`
	JournalPurpose string                 `json:"journal_purpose"`
	CurrencyCode   string                 `json:"currency_code"`
	BookID         *string                `json:"book_id,omitempty"`
	Description    string                 `json:"description"`
	Lines          []domain.RecurringLine `json:"lines"`
	Frequency      string                 `json:"frequency"`
	FirstRunDate   domain.Date            `json:"first_run_date"`
	EndDate        *domain.Date           `json:"end_date,omitempty"`
}

func (h *Handler) CreateRecurringTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.LegalEntityID == "":
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id")
		return
	case strings.TrimSpace(req.Name) == "" || len(req.Name) > 160:
		writeError(w, http.StatusBadRequest, "missing_field", "name")
		return
	case req.FirstRunDate.IsZero():
		writeError(w, http.StatusBadRequest, "missing_field", "first_run_date")
		return
	case !domain.ValidJournalType(req.JournalType) || req.JournalType == domain.JournalTypeReversal:
		writeError(w, http.StatusBadRequest, "invalid_journal_type", domain.ErrInvalidJournalType.Error())
		return
	case !domain.ValidJournalPurpose(req.JournalPurpose):
		writeError(w, http.StatusBadRequest, "invalid_journal_purpose", "journal_purpose must be one of "+strings.Join(domain.JournalPurposes(), ", "))
		return
	case !domain.ValidCurrencyCode(req.CurrencyCode):
		writeError(w, http.StatusBadRequest, "invalid_currency_code", domain.ErrInvalidCurrency.Error())
		return
	case req.Frequency != "WEEKLY" && req.Frequency != "MONTHLY" && req.Frequency != "QUARTERLY" && req.Frequency != "YEARLY":
		writeError(w, http.StatusBadRequest, "invalid_frequency", "frequency is WEEKLY, MONTHLY, QUARTERLY or YEARLY")
		return
	case len(req.Lines) < 2:
		writeError(w, http.StatusBadRequest, "no_lines", domain.ErrNoLines.Error())
		return
	}
	var debit, credit int64
	for _, l := range req.Lines {
		if !exactlyOneNonZero(l.DebitAmount, l.CreditAmount) {
			writeError(w, http.StatusBadRequest, "invalid_line", domain.ErrInvalidLine.Error())
			return
		}
		debit += int64(l.DebitAmount*100 + 0.5)
		credit += int64(l.CreditAmount*100 + 0.5)
	}
	if debit != credit {
		writeError(w, http.StatusUnprocessableEntity, "unbalanced_journal", domain.ErrUnbalancedJournal.Error())
		return
	}
	if !h.controls(w) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRecurringManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	// A template is no way round the control-account rule: its lines are held to the same restriction as a manual journal.
	probe := domain.CreateJournalRequest{TenantID: tenantID, LegalEntityID: req.LegalEntityID}
	for _, l := range req.Lines {
		probe.Lines = append(probe.Lines, domain.CreateJournalLineInput{AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount})
	}
	if !h.checkAccountRestrictions(w, r, probe, principalID) {
		return
	}
	t := &domain.RecurringTemplate{
		TemplateID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID, Name: strings.TrimSpace(req.Name), JournalType: req.JournalType,
		JournalPurpose: req.JournalPurpose, CurrencyCode: req.CurrencyCode, BookID: req.BookID, Description: req.Description, Lines: req.Lines,
		Frequency: req.Frequency, NextRunDate: req.FirstRunDate, EndDate: req.EndDate, CreatedBy: principalID,
	}
	if err := h.ctl.CreateRecurringTemplate(r.Context(), t); err != nil {
		h.writeTemplateErr(w, err)
		return
	}
	got, err := h.ctl.GetRecurringTemplate(r.Context(), tenantID, t.TemplateID)
	if err != nil {
		writeJSON(w, http.StatusCreated, t)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (h *Handler) GetRecurringTemplate(w http.ResponseWriter, r *http.Request) {
	if !h.controls(w) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	t, err := h.ctl.GetRecurringTemplate(r.Context(), tenantID, chi.URLParam(r, "template_id"))
	if err != nil {
		h.writeTemplateErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *Handler) ListRecurringTemplates(w http.ResponseWriter, r *http.Request) {
	if !h.controls(w) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	list, err := h.ctl.ListRecurringTemplates(r.Context(), tenantID, r.URL.Query().Get("legal_entity_id"))
	if err != nil {
		h.writeTemplateErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": list, "total": len(list)})
}

// SetRecurringStatus pauses, resumes or ends a template.
func (h *Handler) SetRecurringStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.controls(w) {
			return
		}
		principalID, ok := h.requirePrincipal(w, r)
		if !ok {
			return
		}
		tenantID, ok := h.requireTenant(w, r)
		if !ok {
			return
		}
		id := chi.URLParam(r, "template_id")
		t, err := h.ctl.GetRecurringTemplate(r.Context(), tenantID, id)
		if err != nil {
			h.writeTemplateErr(w, err)
			return
		}
		if err := h.authz.CheckAllowed(r.Context(), principalID, t.LegalEntityID, actionRecurringManage); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
		if err := h.ctl.SetRecurringTemplateStatus(r.Context(), tenantID, id, status); err != nil {
			h.writeTemplateErr(w, err)
			return
		}
		got, err := h.ctl.GetRecurringTemplate(r.Context(), tenantID, id)
		if err != nil {
			h.writeTemplateErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, got)
	}
}

type generateRequest struct {
	RunDate domain.Date `json:"run_date"`
}

// GenerateRecurringJournal creates the draft journal of one run of a template. The template is not posted; the draft
// follows the ordinary submit, approve and post path.
func (h *Handler) GenerateRecurringJournal(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RunDate.IsZero() {
		writeError(w, http.StatusBadRequest, "missing_field", "run_date")
		return
	}
	if !h.controls(w) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "template_id")
	t, err := h.ctl.GetRecurringTemplate(r.Context(), tenantID, id)
	if err != nil {
		h.writeTemplateErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, t.LegalEntityID, actionRecurringGenerate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	// The chart can change after a template was made: the restriction is checked again for this run.
	probe := domain.CreateJournalRequest{TenantID: tenantID, LegalEntityID: t.LegalEntityID}
	for _, l := range t.Lines {
		probe.Lines = append(probe.Lines, domain.CreateJournalLineInput{AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount})
	}
	if !h.checkAccountRestrictions(w, r, probe, principalID) {
		return
	}
	header, lines, created, err := h.ctl.GenerateRecurringJournal(r.Context(), tenantID, id, req.RunDate, principalID)
	if err != nil {
		h.writeTemplateErr(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, domain.JournalWithLines{JournalHeader: *header, Lines: lines})
}

func (h *Handler) writeTemplateErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrTemplateNotFound):
		writeError(w, http.StatusNotFound, "template_not_found", "")
	case errors.Is(err, domain.ErrTemplateExists):
		writeError(w, http.StatusConflict, "template_exists", err.Error())
	case errors.Is(err, domain.ErrTemplateNotActive):
		writeError(w, http.StatusConflict, "template_not_active", err.Error())
	case errors.Is(err, domain.ErrTemplateOutOfOrder):
		writeError(w, http.StatusConflict, "run_out_of_order", err.Error())
	case errors.Is(err, domain.ErrInvalidIdentifier):
		writeError(w, http.StatusBadRequest, "invalid_field", "tenant_id and legal_entity_id must both be UUIDs")
	default:
		h.log.Error("recurring journal request failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// ParseManualJournalPolicy reads the configured evidence rule. A threshold that is not a non-negative amount is an error:
// a control that silently turns itself off because of a typo is worse than a service that will not start.
func ParseManualJournalPolicy(threshold, types string) (ManualJournalPolicy, error) {
	var p ManualJournalPolicy
	if t := strings.TrimSpace(threshold); t != "" {
		r, ok := new(big.Rat).SetString(t)
		if !ok || r.Sign() < 0 {
			return p, fmt.Errorf("MANUAL_JOURNAL_EVIDENCE_THRESHOLD %q is not a non-negative amount", threshold)
		}
		cents := new(big.Rat).Mul(r, big.NewRat(100, 1))
		p.EvidenceThresholdCents = new(big.Int).Quo(cents.Num(), cents.Denom()).Int64()
	}
	for _, s := range strings.Split(types, ",") {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !domain.ValidJournalType(domain.JournalType(s)) {
			return p, fmt.Errorf("MANUAL_JOURNAL_EVIDENCE_TYPES: %q is not a journal type", s)
		}
		if p.EvidenceTypes == nil {
			p.EvidenceTypes = map[domain.JournalType]bool{}
		}
		p.EvidenceTypes[domain.JournalType(s)] = true
	}
	return p, nil
}


// checkWave2Controls applies the subledger-aware rules of §12 and §17 to a manual journal: a line on an AR or AP control
// account (or any account flagged for it) names its counterparty, and a reconciliation adjustment names the open case it settles.
// Writes its own response and returns false on a refusal.
func (h *Handler) checkWave2Controls(w http.ResponseWriter, r *http.Request, req domain.CreateJournalRequest) (reconBook string, ok bool) {
	adjustment := req.JournalPurpose == "RECONCILIATION_ADJUSTMENT"
	if req.ReconciliationCaseID != "" && !adjustment {
		writeError(w, http.StatusBadRequest, "invalid_reconciliation_case", "reconciliation_case_id belongs only to a RECONCILIATION_ADJUSTMENT journal")
		return "", false
	}
	if h.ctl == nil {
		if adjustment {
			writeError(w, http.StatusNotImplemented, "journal_controls_not_configured", "")
			return "", false
		}
		return "", true
	}
	if adjustment {
		if req.ReconciliationCaseID == "" {
			writeError(w, http.StatusUnprocessableEntity, "reconciliation_case_required", "a reconciliation adjustment names the open reconciliation case it settles")
			return "", false
		}
		book, err := h.ctl.CheckReconciliationCase(r.Context(), req.TenantID, req.ReconciliationCaseID, req.LegalEntityID)
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrReconciliationCaseNotOpen):
				writeError(w, http.StatusUnprocessableEntity, "reconciliation_case_not_open", err.Error())
			case errors.Is(err, domain.ErrJournalNotFound):
				writeError(w, http.StatusUnprocessableEntity, "reconciliation_case_unknown", "no such reconciliation case for this entity")
			default:
				h.log.Error("CheckReconciliationCase failed", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
			}
			return "", false
		}
		reconBook = book
	}
	codes := make([]string, 0, len(req.Lines))
	for _, l := range req.Lines {
		codes = append(codes, l.AccountCode)
	}
	controls, err := h.ctl.AccountControls(r.Context(), req.TenantID, codes)
	if err != nil {
		h.log.Error("AccountControls failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return "", false
	}
	for i, l := range req.Lines {
		c, ok := controls[l.AccountCode]
		if !ok {
			continue
		}
		if (c.CounterpartyRequired || c.ControlType == "AR" || c.ControlType == "AP") && (l.CounterpartyID == nil || strings.TrimSpace(*l.CounterpartyID) == "") {
			writeError(w, http.StatusUnprocessableEntity, "counterparty_required",
				fmt.Sprintf("line %d posts to account %s, which needs the counterparty (customer or supplier)", i+1, l.AccountCode))
			return "", false
		}
	}
	return reconBook, true
}
