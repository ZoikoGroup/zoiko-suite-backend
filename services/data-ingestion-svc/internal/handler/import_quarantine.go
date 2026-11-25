package handler

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zoiko.io/data-ingestion-svc/internal/imports"
)

// Governed release from quarantine (ZS-SVC-Z-001 DQC-02, INV-11/INV-12, NP-16). Asking for a
// release needs the permission to work items; deciding one needs its own permission, and the
// decider must be someone other than the requester and the person who quarantined the item.

func itemRow(w http.ResponseWriter, r *http.Request, h *ImportHandler) (int, bool) {
	row, err := strconv.Atoi(chi.URLParam(r, "row"))
	if err != nil || row < 1 {
		h.problem(w, r, imports.ErrValidation)
		return 0, false
	}
	return row, true
}

func (h *ImportHandler) requestRelease(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportItemManage)
	if !ok {
		return
	}
	var req struct {
		Reason        string `json:"reason"`
		CorrectionRef string `json:"correction_ref"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	row, ok := itemRow(w, r, h)
	if !ok {
		return
	}
	if err := h.svc.RequestRelease(r.Context(), a, b.BatchID, row, req.Reason, req.CorrectionRef); err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusAccepted, map[string]any{"batch_id": b.BatchID, "row_no": row, "status": "QUARANTINED", "release": "REQUESTED"})
}

func (h *ImportHandler) decide(w http.ResponseWriter, r *http.Request, release bool) {
	a, b, ok := h.load(w, r, ActionImportQuarantineRelease)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	row, ok := itemRow(w, r, h)
	if !ok {
		return
	}
	var err error
	status, state := "QUARANTINED", "REFUSED"
	if release {
		err = h.svc.ApproveRelease(r.Context(), a, b.BatchID, row, req.Reason)
		status, state = "PENDING", "RELEASED"
	} else {
		err = h.svc.RefuseRelease(r.Context(), a, b.BatchID, row, req.Reason)
	}
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "row_no": row, "status": status, "release": state})
}

func (h *ImportHandler) approveRelease(w http.ResponseWriter, r *http.Request) { h.decide(w, r, true) }
func (h *ImportHandler) refuseRelease(w http.ResponseWriter, r *http.Request)  { h.decide(w, r, false) }

// quarantineView is the batch's quarantine state (NONE / PARTIAL / FULL / RELEASE_PENDING) and the
// ledger behind it.
func (h *ImportHandler) quarantineView(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportRead)
	if !ok {
		return
	}
	v, err := h.svc.QuarantineView(r.Context(), a.TenantID, b.BatchID, 0)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, v)
}

func (h *ImportHandler) itemQuarantineView(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportRead)
	if !ok {
		return
	}
	row, ok := itemRow(w, r, h)
	if !ok {
		return
	}
	v, err := h.svc.QuarantineView(r.Context(), a.TenantID, b.BatchID, row)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, v)
}

// dqcQuarantineRelease is the specification's POST /dqc/quarantine/release (ZS-SVC-Z-001 section 11):
// one release decision, with "optimistic concurrency + authority check + reason required". The decision
// names the release request it reviewed (request_ledger_id, from the quarantine view); if the item's open
// request has since changed it is refused with 409 rather than applied to something the decider did not see.
// Authority and independence are exactly those of :approve-release / :refuse-release.
func (h *ImportHandler) dqcQuarantineRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BatchID         string `json:"batch_id"`
		RowNo           int    `json:"row_no"`
		Decision        string `json:"decision"` // RELEASE or REFUSE
		Reason          string `json:"reason"`
		RequestLedgerID int64  `json:"request_ledger_id"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	var release bool
	switch req.Decision {
	case "RELEASE":
		release = true
	case "REFUSE":
	default:
		h.problem(w, r, imports.ErrValidation)
		return
	}
	if req.BatchID == "" || req.RowNo < 1 {
		h.problem(w, r, imports.ErrValidation)
		return
	}
	a, b, ok := h.loadBatch(w, r, ActionImportQuarantineRelease, req.BatchID)
	if !ok {
		return
	}
	if err := h.svc.DecideReleaseOfRequest(r.Context(), a, b.BatchID, req.RowNo, req.Reason, release, req.RequestLedgerID); err != nil {
		h.problem(w, r, err)
		return
	}
	state, status := "REFUSED", "QUARANTINED"
	if release {
		state, status = "RELEASED", "PENDING"
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "row_no": req.RowNo, "status": status, "release": state,
		"request_ledger_id": req.RequestLedgerID})
}
