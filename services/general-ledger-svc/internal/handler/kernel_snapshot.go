package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// WithSigner wires the key snapshots are signed with. Without one the snapshot command refuses: an unsigned snapshot is not what
// the period close is meant to retain.
func (h *KernelHandler) WithSigner(s kernel.Signer) *KernelHandler {
	h.signer = s
	return h
}

func (h *KernelHandler) snapshots(w http.ResponseWriter) (*kernel.SnapshotService, bool) {
	sr, ok1 := h.repo.(kernel.SnapshotRepo)
	se, ok2 := h.repo.(kernel.SealRepo)
	if !ok1 || !ok2 {
		writeError(w, http.StatusNotImplemented, "snapshots_not_configured", "")
		return nil, false
	}
	return &kernel.SnapshotService{Repo: sr, Books: h.repo, Seals: se, Signer: h.signer}, true
}

// CreateBalanceSnapshot takes, signs and retains the balances of a book. A close snapshot is of a sealed period, at its watermark.
func (h *KernelHandler) CreateBalanceSnapshot(w http.ResponseWriter, r *http.Request) {
	var req kernel.SnapshotRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionSnapshotSign)
	if !ok {
		return
	}
	svc, ok := h.snapshots(w)
	if !ok {
		return
	}
	snap, err := svc.Create(r.Context(), tenant, a, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snap)
}

func (h *KernelHandler) GetBalanceSnapshot(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionSnapshotRead)
	if !ok {
		return
	}
	svc, ok := h.snapshots(w)
	if !ok {
		return
	}
	snap, err := svc.Repo.GetSnapshot(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (h *KernelHandler) ListBalanceSnapshots(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionSnapshotRead)
	if !ok {
		return
	}
	svc, ok := h.snapshots(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := svc.Repo.ListSnapshots(r.Context(), tenant, q.Get("legal_entity_id"), q.Get("book_id"), q.Get("fiscal_period"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": list, "total": len(list)})
}

// VerifyBalanceSnapshot recomputes a snapshot from the ledger at its watermark and checks its signature.
func (h *KernelHandler) VerifyBalanceSnapshot(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionSnapshotRead)
	if !ok {
		return
	}
	svc, ok := h.snapshots(w)
	if !ok {
		return
	}
	res, err := svc.Verify(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// GetCloseEvidence is what the kernel can say about whether a period of an entity is ready to close: open reconciliation cases,
// failed postings, foreign items awaiting revaluation and journals still in the review queue, per active book, with the seal and the
// latest signed snapshot. The period close service reads it; closing stays with that service.
func (h *KernelHandler) GetCloseEvidence(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entity := q.Get("legal_entity_id")
	if entity == "" || q.Get("fiscal_period") == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id and fiscal_period are required")
		return
	}
	tenant, _, ok := h.guardFor(w, r, actionCloseEvidence, entity)
	if !ok {
		return
	}
	rec, ok1 := h.repo.(kernel.ReconRepo)
	rev, ok2 := h.repo.(kernel.RevalRepo)
	se, ok3 := h.repo.(kernel.SealRepo)
	sn, ok4 := h.repo.(kernel.SnapshotRepo)
	pe, ok5 := h.repo.(kernel.PendingRepo)
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		writeError(w, http.StatusNotImplemented, "close_evidence_not_configured", "")
		return
	}
	var require []string
	if v := q.Get("require"); v != "" {
		require = strings.Split(v, ",")
	}
	esvc := &kernel.EvidenceService{Books: h.repo, Recon: rec, Reval: rev, Seals: se, Snaps: sn, Pending: pe}
	if cr, ok := h.repo.(kernel.ConsequenceRepo); ok {
		esvc.Orch = &kernel.OrchestrationService{Repo: h.repo, Cons: cr}
	}
	ev, err := esvc.For(r.Context(), tenant, entity, q.Get("fiscal_period"), require)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// GetBalanceSnapshotTrace explains one account of a retained snapshot (standard §31, §32): the ledger entries behind its figure, and
// for each one the journal, accounting event, rule version and evidence it came from, with a check that they add up to the figure.
// Reading it needs both the right to read snapshots and the right to see postings in the snapshot's entity.
func (h *KernelHandler) GetBalanceSnapshotTrace(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionSnapshotRead)
	if !ok {
		return
	}
	svc, ok := h.snapshots(w)
	tr, ok2 := h.repo.(kernel.SnapshotTraceRepo)
	if !ok || !ok2 {
		if ok {
			writeError(w, http.StatusNotImplemented, "snapshot_trace_not_configured", "")
		}
		return
	}
	q := r.URL.Query()
	account := strings.TrimSpace(q.Get("account_code"))
	if account == "" {
		writeError(w, http.StatusBadRequest, "account_code_required", "name the account of the snapshot to trace")
		return
	}
	after, limit := int64(0), 0
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_after", "after is the last ledger entry sequence already seen")
			return
		}
		after = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit is a positive number")
			return
		}
		limit = n
	}
	id := chi.URLParam(r, "id")
	snap, err := svc.Repo.GetSnapshot(r.Context(), tenant, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if _, _, ok := h.guardFor(w, r, actionPostingView, snap.LegalEntityID); !ok {
		return
	}
	res, err := tr.SnapshotTrace(r.Context(), tenant, kernel.SnapshotTraceQuery{SnapshotID: id, AccountCode: account, After: after, Limit: limit})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
