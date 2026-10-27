package ar06

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// params merges the query string with, for POST, the top-level scalar fields of a JSON body (the spec wireframe allows GET or POST).
func params(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	if r.Method == http.MethodPost && r.Body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, kit.MaxBodyBytes))
		if err == nil && len(raw) > 0 {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				for k, v := range m {
					switch t := v.(type) {
					case string:
						out[k] = t
					case float64:
						out[k] = strconv.FormatFloat(t, 'f', -1, 64)
					}
				}
			}
		}
	}
	return out
}

func (m *Module) getCorrection(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out *Correction
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		h, err := getHeader(r.Context(), tx, c.TenantID, p["correction_id"], false)
		if err != nil {
			return err
		}
		out, err = loadCorrection(r.Context(), tx, c.TenantID, h)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"correction": out}
	if out.State.IsIssued() {
		resp["issued"] = map[string]any{"schema": SchemaIssuedNote, "issued_content_hash": out.IssuedContentHash, "integrity_verified": HashOf(out.issuedCanonical) == out.IssuedContentHash,
			"canonical_text": out.issuedCanonical, "representation": json.RawMessage(out.issuedCanonical)}
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}

// getCorrectableAmount reports the remaining correctable amount of an ISSUED source invoice: its issued gross minus the ISSUED credit
// notes recorded against it, derived exactly from this module's tables. Debit notes add receivable of their own and do not extend it.
func (m *Module) getCorrectableAmount(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	id := p["source_invoice_id"]
	if id == "" {
		id = p["invoice_id"]
	}
	src, err := m.source(r.Context(), c.TenantID, id)
	if err != nil {
		m.fail(w, err)
		return
	}
	var cn, cg, debits, pending kit.Amount
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if cn, cg, err = creditedTotals(r.Context(), tx, c.TenantID, id, ""); err != nil {
			return err
		}
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE(sum(gross_total) FILTER (WHERE kind = 'DEBIT_NOTE' AND state IN ('ISSUED','APPLIED','SETTLED')),0),
				COALESCE(sum(gross_total) FILTER (WHERE kind = 'CREDIT_NOTE' AND state IN ('DRAFT','PENDING_APPROVAL','APPROVED')),0)
			FROM ar06_corrections WHERE tenant_id = $1 AND source_invoice_id = $2`, c.TenantID, id).Scan(&debits, &pending); err != nil {
			return kit.MapPgErr(err)
		}
		return nil
	}); err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"source_invoice_id": id, "source_invoice_number": src.InvoiceNumber, "eligibility": EligibilityOf(src, cn, cg),
		"issued_debit_notes_gross": debits, "unissued_credit_notes_gross": pending})
}

type chainEntry struct {
	CorrectionID    string            `json:"correction_id"`
	Kind            Kind              `json:"kind"`
	State           State             `json:"lifecycle_state"`
	DocumentNumber  string            `json:"document_number,omitempty"`
	IssueDate       string            `json:"issue_date,omitempty"`
	NetTotal        kit.Amount        `json:"net_total"`
	TaxTotal        kit.Amount        `json:"tax_total"`
	GrossTotal      kit.Amount        `json:"gross_total"`
	ReasonCode      string            `json:"reason_code"`
	IssuedHash      string            `json:"issued_content_hash,omitempty"`
	OpenItemID      string            `json:"open_item_id,omitempty"`
	Replacements    []ReplacementLink `json:"replacement_invoices"`
	AccountingState string            `json:"accounting_state"`
	Version         int               `json:"version"`
}

// getCorrectionChain lists every correction raised against a source invoice (given directly, or via one of its corrections), oldest first.
func (m *Module) getCorrectionChain(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var source string
	var chain []chainEntry
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		source = p["source_invoice_id"]
		if source == "" {
			h, err := getHeader(r.Context(), tx, c.TenantID, p["correction_id"], false)
			if err != nil {
				return err
			}
			source = h.SourceInvoiceID
		} else if err := mustUUID("source_invoice_id", source); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT `+headerCols+` FROM ar06_corrections WHERE tenant_id = $1 AND source_invoice_id = $2 ORDER BY created_at, correction_id`, c.TenantID, source)
		if err != nil {
			return kit.MapPgErr(err)
		}
		var hs []*Header
		for rows.Next() {
			h, err := scanHeader(rows)
			if err != nil {
				rows.Close()
				return err
			}
			hs = append(hs, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		chain = make([]chainEntry, 0, len(hs))
		for _, h := range hs {
			reps, err := listReplacements(r.Context(), tx, c.TenantID, h.CorrectionID)
			if err != nil {
				return err
			}
			chain = append(chain, chainEntry{CorrectionID: h.CorrectionID, Kind: h.Kind, State: h.State, DocumentNumber: h.DocumentNumber, IssueDate: h.IssueDate,
				NetTotal: h.NetTotal, TaxTotal: h.TaxTotal, GrossTotal: h.GrossTotal, ReasonCode: h.Content.ReasonCode, IssuedHash: h.IssuedContentHash, OpenItemID: h.OpenItemID,
				Replacements: reps, AccountingState: h.AccountingState, Version: h.Version})
		}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"source_invoice_id": source, "corrections": chain})
}

type actionState struct {
	Action     string   `json:"action"`
	Permission []string `json:"permissions"`
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
}

// getAvailableActions is derived from state, segregation of duties, wiring and the caller's permissions, never hard-coded.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = getHeader(r.Context(), tx, c.TenantID, p["correction_id"], false)
		return err
	}); err != nil {
		m.fail(w, err)
		return
	}
	canT := func(t Transition) bool { _, e := Next(h.State, t); return e == nil }
	preparer := c.PrincipalID == h.CreatedBy || c.PrincipalID == h.SubmittedBy
	type cand struct {
		name  string
		perms []string
		legal bool
		why   string
	}
	approveLegal, approveWhy := canT(TrApprove), "the correction is not pending approval"
	if approveLegal && preparer {
		approveLegal, approveWhy = false, "a correction must be approved by a principal who neither created nor submitted it"
	}
	issueLegal, issueWhy := canT(TrIssue), "the correction is not approved (state "+string(h.State)+")"
	switch {
	case issueLegal && m.openItems == nil:
		issueLegal, issueWhy = false, "AR-07 receivables not wired"
	case issueLegal && IsMaterial(h.GrossTotal) && c.PrincipalID == h.ApprovedBy:
		issueLegal, issueWhy = false, "a material correction must be issued by a principal other than its approver"
	}
	cands := []cand{
		{"SubmitCorrectionForApproval", []string{ActionCorrectionManage}, canT(TrSubmit), "the correction is not a draft"},
		{"ApproveCorrection", []string{ActionCorrectionApprove}, approveLegal, approveWhy},
		{"IssueCreditNote", []string{ActionCorrectionIssue}, issueLegal && h.Kind == KindCredit, ifElse(h.Kind == KindCredit, issueWhy, "the correction is a debit note")},
		{"IssueDebitNote", []string{ActionCorrectionIssue}, issueLegal && h.Kind == KindDebit, ifElse(h.Kind == KindDebit, issueWhy, "the correction is a credit note")},
		{"CancelUnissuedCorrection", []string{ActionCorrectionManage}, canT(TrCancel), "an issued note is never cancelled: raise a further correction"},
		{"LinkReplacementInvoice", []string{ActionCorrectionLink}, h.Kind == KindCredit && h.State.IsIssued(), "a replacement invoice can be linked only to an issued credit note"},
	}
	allowed := map[string]bool{}
	for _, cd := range cands {
		for _, perm := range cd.perms {
			if _, done := allowed[perm]; done {
				continue
			}
			e := m.deps.Authz.CheckAllowed(r.Context(), c.PrincipalID, h.LegalEntityID, perm)
			switch {
			case e == nil:
				allowed[perm] = true
			case errors.Is(e, domain.ErrAuthorizationDenied):
				allowed[perm] = false
			default:
				m.fail(w, kit.Unavailable("authorization service unavailable"))
				return
			}
		}
	}
	out := make([]actionState, 0, len(cands))
	for _, cd := range cands {
		permitted, missing := true, ""
		for _, perm := range cd.perms {
			if !allowed[perm] {
				permitted, missing = false, perm
				break
			}
		}
		a := actionState{Action: cd.name, Permission: cd.perms, Available: cd.legal && permitted}
		switch {
		case !cd.legal:
			a.Reason = cd.why
		case !permitted:
			a.Reason = "caller lacks " + missing
		}
		out = append(out, a)
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"correction_id": h.CorrectionID, "lifecycle_state": h.State, "version": h.Version, "actions": out})
}

func ifElse(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

const (
	AcctNotApplicable = "NOT_APPLICABLE"
	AcctMissing       = "MISSING"
)

// getCorrectionAccountingStatus reports the ACC-04 posting state of the note (source_event_id = correction id). An issued note with NO
// posting request is reported MISSING and flagged as a control exception (spec section 18 #35): never silent.
func (m *Module) getCorrectionAccountingStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = getHeader(r.Context(), tx, c.TenantID, p["correction_id"], false)
		return err
	}); err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"correction_id": h.CorrectionID, "kind": h.Kind, "document_number": h.DocumentNumber, "lifecycle_state": h.State,
		"accounting_state": h.AccountingState, "source_event_id": h.CorrectionID, "control_exception": false}
	if !h.State.IsIssued() {
		resp["posting_status"] = AcctNotApplicable
		kit.WriteJSON(w, http.StatusOK, resp)
		return
	}
	status, journal, lastErr, found, err := m.deps.DB.PostingStatusFor(r.Context(), c.TenantID, h.CorrectionID)
	if err != nil {
		m.fail(w, err)
		return
	}
	switch {
	case !found:
		resp["posting_status"] = AcctMissing
		resp["control_exception"], resp["exception"] = true, "ISSUED_NOTE_WITHOUT_POSTING_REQUEST"
	default:
		resp["posting_status"], resp["journal_id"], resp["last_error"] = status, journal, lastErr
		switch status {
		case kit.PostingFailed:
			resp["control_exception"], resp["exception"] = true, "POSTING_FAILED"
		case kit.PostingQuarantined:
			resp["control_exception"], resp["exception"] = true, "POSTING_QUARANTINED"
		}
		resp["mirror_in_sync"] = h.AccountingState == status || (h.AccountingState == "QUEUED" && (status == kit.PostingPending || status == kit.PostingInProgress))
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}
