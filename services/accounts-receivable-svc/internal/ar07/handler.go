package ar07

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

type createReq struct {
	LegalEntityID      string     `json:"legal_entity_id"`
	DocumentKind       string     `json:"document_kind"`
	SourceDocumentID   string     `json:"source_document_id"`
	DocumentNumber     string     `json:"document_number"`
	CustomerPartyID    string     `json:"customer_party_id"`
	Currency           string     `json:"currency"`
	OriginalAmount     kit.Amount `json:"original_amount"`
	IssueDate          string     `json:"issue_date"`
	DueDate            string     `json:"due_date"`
	BusinessCalendarID string     `json:"business_calendar_id"`
}

// createOpenItemFromDocument is the privileged HTTP twin of the port: the document consequence normally arrives from
// AR-04 inside the issuing transaction; this exists for controlled backfill / repair and is idempotent per document.
func (m *Module) createOpenItemFromDocument(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionReceivableManage)
	if !ok {
		return
	}
	issue, err := parseDate("issue_date", req.IssueDate)
	if err != nil {
		m.fail(w, err)
		return
	}
	due, err := parseDate("due_date", req.DueDate)
	if err != nil {
		m.fail(w, err)
		return
	}
	var out *OpenItem
	var replayed bool
	err = m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) (e error) {
		out, replayed, e = m.createOpenItem(r.Context(), tx, c, kit.OpenItemFromDocument{DocumentKind: req.DocumentKind, SourceDocumentID: req.SourceDocumentID,
			DocumentNumber: req.DocumentNumber, CustomerPartyID: req.CustomerPartyID, Currency: req.Currency, OriginalAmount: req.OriginalAmount, IssueDate: issue, DueDate: due},
			req.BusinessCalendarID)
		return
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	kit.WriteJSON(w, status, out)
}

type adjustReq struct {
	LegalEntityID     string     `json:"legal_entity_id"`
	OpenItemID        string     `json:"open_item_id"`
	ExpectedVersion   *int       `json:"expected_version"`
	Kind              string     `json:"kind"`
	Amount            kit.Amount `json:"amount"`
	ReasonCode        string     `json:"reason_code"`
	EvidenceID        string     `json:"evidence_id"`
	OwningDocumentTyp string     `json:"owning_document_type"`
	OwningDocumentID  string     `json:"owning_document_id"`
	EventKey          string     `json:"event_key"`
	Reason            string     `json:"reason"`
}

// applyReceivableAdjustment is deliberately narrow (#20): HTTP accepts only a CORRECTION that names its owning document, a
// reason code and evidence, under a privileged action. Cash, credit, refund and write-off adjustments arrive only through
// the port from their owning module. There is no field that sets a balance; unknown fields are rejected by the decoder.
func (m *Module) applyReceivableAdjustment(w http.ResponseWriter, r *http.Request) {
	var req adjustReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionReceivableAdjust)
	if !ok {
		return
	}
	if req.Kind != kit.AdjCorrection {
		m.fail(w, kit.Unprocessable(kit.CodeValidationFailed, "only kind CORRECTION can be applied over HTTP; cash, credit, refund and write-off adjustments arrive from their owning module"))
		return
	}
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if strings.TrimSpace(req.EvidenceID) == "" {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeEvidenceRequired, "evidence_id is required")
		return
	}
	if strings.TrimSpace(req.OwningDocumentID) == "" || strings.TrimSpace(req.OwningDocumentTyp) == "" {
		m.fail(w, kit.Invalid("a balance adjustment must reference its owning event or document (owning_document_type, owning_document_id)"))
		return
	}
	key := req.EventKey
	if key == "" {
		key = "CORRECTION:" + req.OwningDocumentTyp + ":" + req.OwningDocumentID + ":" + req.OpenItemID
	}
	var out *OpenItem
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) (e error) {
		out, _, e = m.applyAdjustment(r.Context(), tx, c, kit.OpenItemAdjustment{OpenItemID: req.OpenItemID, Kind: kit.AdjCorrection, Amount: req.Amount,
			SourceRefType: req.OwningDocumentTyp, SourceRefID: req.OwningDocumentID, EventKey: key, Reason: req.Reason},
			adjustExtra{ReasonCode: req.ReasonCode, EvidenceID: req.EvidenceID, Expected: req.ExpectedVersion, Command: "ApplyReceivableAdjustment"})
		return
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

type itemCmdReq struct {
	LegalEntityID   string `json:"legal_entity_id"`
	OpenItemID      string `json:"open_item_id"`
	ExpectedVersion *int   `json:"expected_version"`
	ReasonCode      string `json:"reason_code"`
	EvidenceID      string `json:"evidence_id"`
	ResolutionCode  string `json:"resolution_code"`
}

// itemCommand runs the shared shape of the flag commands: decode, guard, mandatory expected_version, ONE tx.
func (m *Module) itemCommand(w http.ResponseWriter, r *http.Request, action string, needReason, needEvidence bool, pre func(*itemCmdReq) error,
	fn func(tx pgx.Tx, c kit.Caller, req *itemCmdReq) (*OpenItem, error)) {
	var req itemCmdReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, action)
	if !ok {
		return
	}
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	if needReason && !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if needEvidence && strings.TrimSpace(req.EvidenceID) == "" {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeEvidenceRequired, "evidence_id is required")
		return
	}
	if err := mustUUID("open_item_id", req.OpenItemID); err != nil {
		m.fail(w, err)
		return
	}
	if pre != nil {
		if err := pre(&req); err != nil {
			m.fail(w, err)
			return
		}
	}
	var out *OpenItem
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) (e error) { out, e = fn(tx, c, &req); return })
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) openDisputeCmd(w http.ResponseWriter, r *http.Request) {
	m.itemCommand(w, r, ActionDisputeManage, true, true, nil, func(tx pgx.Tx, c kit.Caller, q *itemCmdReq) (*OpenItem, error) {
		return m.openDispute(r.Context(), tx, c, q.OpenItemID, q.ExpectedVersion, q.ReasonCode, q.EvidenceID)
	})
}

func (m *Module) resolveDisputeCmd(w http.ResponseWriter, r *http.Request) {
	m.itemCommand(w, r, ActionDisputeManage, false, true, func(q *itemCmdReq) error {
		if q.ResolutionCode != ResolutionUpheld && q.ResolutionCode != ResolutionRejected {
			return kit.Invalid("resolution_code must be UPHELD or REJECTED")
		}
		return nil
	}, func(tx pgx.Tx, c kit.Caller, q *itemCmdReq) (*OpenItem, error) {
		return m.resolveDispute(r.Context(), tx, c, q.OpenItemID, q.ExpectedVersion, q.ResolutionCode, q.EvidenceID)
	})
}

func (m *Module) placeCollectionHold(w http.ResponseWriter, r *http.Request) {
	m.itemCommand(w, r, ActionHoldManage, true, false, nil, func(tx pgx.Tx, c kit.Caller, q *itemCmdReq) (*OpenItem, error) {
		return m.setHold(r.Context(), tx, c, q.OpenItemID, q.ExpectedVersion, true, q.ReasonCode, q.EvidenceID)
	})
}

func (m *Module) releaseCollectionHold(w http.ResponseWriter, r *http.Request) {
	m.itemCommand(w, r, ActionHoldManage, true, false, nil, func(tx pgx.Tx, c kit.Caller, q *itemCmdReq) (*OpenItem, error) {
		return m.setHold(r.Context(), tx, c, q.OpenItemID, q.ExpectedVersion, false, q.ReasonCode, q.EvidenceID)
	})
}

func (m *Module) closeOpenItem(w http.ResponseWriter, r *http.Request) {
	m.itemCommand(w, r, ActionReceivableManage, false, false, nil, func(tx pgx.Tx, c kit.Caller, q *itemCmdReq) (*OpenItem, error) {
		return m.closeItem(r.Context(), tx, c, q.OpenItemID, q.ExpectedVersion, q.ReasonCode, q.EvidenceID)
	})
}
