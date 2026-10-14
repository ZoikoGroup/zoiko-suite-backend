package arctl

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

type runRequest struct {
	LegalEntityID     string `json:"legal_entity_id"`
	AsOfDate          string `json:"as_of_date"`
	TolerancePolicyID string `json:"tolerance_policy_id"`
	ToleranceVersion  string `json:"tolerance_policy_version"`
	SupersedesRunID   string `json:"supersedes_run_id"`
	ReasonCode        string `json:"reason_code"`
}

// runARGLReconciliation computes and RECORDS one control run. The tolerance fields are a reference only: the caller
// (financial-control-svc) owns the tolerance policy, AR compares exactly and never evaluates it.
func (m *Module) runARGLReconciliation(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionRunReconciliation)
	if !ok {
		return
	}
	asOf, err := time.Parse("2006-01-02", req.AsOfDate)
	if err != nil {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "as_of_date must be YYYY-MM-DD")
		return
	}
	if asOf.After(m.deps.Now()) {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "as_of_date must not be in the future")
		return
	}
	if req.ToleranceVersion != "" && req.TolerancePolicyID == "" {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "tolerance_policy_version requires tolerance_policy_id")
		return
	}
	if req.SupersedesRunID != "" {
		if _, err := uuid.Parse(req.SupersedesRunID); err != nil {
			kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "supersedes_run_id must be a UUID")
			return
		}
	}
	in, err := m.collect(r.Context(), c, asOf, ToleranceRef{PolicyID: req.TolerancePolicyID, Version: req.ToleranceVersion})
	if err != nil {
		m.fail(w, kit.Unavailable("a source of the control run could not be read: "+err.Error()))
		return
	}
	findings := Evaluate(in)
	view, err := m.save(r.Context(), c, SaveRequest{Inputs: in, Findings: findings, Result: ResultOf(findings), Summary: SummaryOf(in, findings),
		SupersedesRunID: req.SupersedesRunID, ReasonCode: req.ReasonCode}, asOf)
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, view)
}

type ackRequest struct {
	FindingID  string `json:"finding_id"`
	ReasonCode string `json:"reason_code"`
	EvidenceID string `json:"evidence_id"`
	Note       string `json:"note"`
}

// acknowledgeFinding annotates a finding. It never clears, edits or hides it. The acknowledger must not be the principal
// that ran the reconciliation (maker != acknowledger).
func (m *Module) acknowledgeFinding(w http.ResponseWriter, r *http.Request) {
	var req ackRequest
	if !kit.Decode(w, r, &req) {
		return
	}
	if _, err := uuid.Parse(req.FindingID); err != nil {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "finding_id must be a UUID")
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if strings.TrimSpace(req.EvidenceID) == "" {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeEvidenceRequired, "evidence_id is required")
		return
	}
	id, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	var entityID, runID, runBy string
	err := m.deps.DB.WithTx(r.Context(), id.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT f.legal_entity_id::text, f.run_id::text, r.run_by FROM arctl_findings f JOIN arctl_runs r ON r.run_id = f.run_id AND r.tenant_id = f.tenant_id
			 WHERE f.tenant_id = $1 AND f.finding_id::text = $2`, id.TenantID, req.FindingID).Scan(&entityID, &runID, &runBy)
	})
	if err == pgx.ErrNoRows {
		m.fail(w, kit.NotFound("finding not found"))
		return
	}
	if err != nil {
		m.fail(w, err)
		return
	}
	c, ok := m.deps.Guard(w, r, entityID, ActionAcknowledgeFinding)
	if !ok {
		return
	}
	if runBy == c.PrincipalID {
		m.fail(w, kit.Forbidden(kit.CodeSegregationOfDuties, "the principal that ran the reconciliation cannot acknowledge its findings"))
		return
	}
	ackID := uuid.NewString()
	err = m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO arctl_acknowledgements (ack_id, finding_id, run_id, tenant_id, legal_entity_id, reason_code, evidence_id, note, acknowledged_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, ackID, req.FindingID, runID, c.TenantID, entityID, req.ReasonCode, req.EvidenceID, req.Note, c.PrincipalID); err != nil {
			return err
		}
		if err := kit.Emit(r.Context(), tx, c, kit.Event{AggregateType: aggregateFinding, AggregateID: req.FindingID, EventType: EventFindingAcked,
			Payload: map[string]any{"finding_id": req.FindingID, "run_id": runID, "ack_id": ackID, "reason_code": req.ReasonCode, "correlation_id": c.CorrelationID}}); err != nil {
			return err
		}
		return kit.RecordAudit(r.Context(), tx, c, kit.Audit{AggregateType: aggregateFinding, AggregateID: req.FindingID, Command: "acknowledgeFinding",
			ReasonCode: req.ReasonCode, EvidenceID: req.EvidenceID, After: map[string]any{"ack_id": ackID, "note_present": req.Note != ""}})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, map[string]any{"ack_id": ackID, "finding_id": req.FindingID, "run_id": runID, "finding_changed": false})
}

// ---- queries (tenant-scoped; absent and cross-tenant look identical) ----

func qv(r *http.Request, name string) string { return strings.TrimSpace(r.URL.Query().Get(name)) }

func (m *Module) getReconciliationRun(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	runID := qv(r, "run_id")
	if _, err := uuid.Parse(runID); err != nil {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "run_id must be a UUID")
		return
	}
	run, err := m.getRun(r.Context(), c.TenantID, runID)
	if err != nil {
		m.fail(w, err)
		return
	}
	withInputs := qv(r, "include_inputs") == "true"
	ev, err := m.evidence(r.Context(), c.TenantID, runID, withInputs || qv(r, "verify") == "true")
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"run": run}
	if qv(r, "verify") == "true" {
		ok, why := m.verifyReproducible(r, c.TenantID, run, ev)
		resp["reproducible"], resp["reproducibility_detail"] = ok, why
	}
	if !withInputs {
		for i := range ev {
			ev[i].Content = nil
		}
	}
	resp["evidence"] = ev
	kit.WriteJSON(w, http.StatusOK, resp)
}

// verifyReproducible re-evaluates the stored inputs and compares the result with the stored findings and inputs hash.
func (m *Module) verifyReproducible(r *http.Request, tenantID string, run *RunView, ev []EvidenceView) (bool, string) {
	raw := map[string][]byte{}
	for _, e := range ev {
		raw[e.Kind] = e.Content
	}
	in, err := joinEvidence(raw)
	if err != nil {
		return false, err.Error()
	}
	if h, _, err := hashJSON(splitEvidence(in)); err != nil || h != run.InputsHash {
		return false, "stored evidence does not hash to the run's inputs_hash"
	}
	stored, err := m.findings(r.Context(), tenantID, run.RunID, "", "", 0, 100000)
	if err != nil {
		return false, err.Error()
	}
	again := Evaluate(in)
	if len(again) != len(stored) {
		return false, "finding count differs"
	}
	for i, f := range again {
		s := stored[i]
		if f.Kind != s.Kind || f.SubjectID != s.SubjectID || f.Severity != s.Severity || f.Category != s.Category || f.Currency != s.Currency ||
			ptrStr(amountArg(f.ARAmount)) != ptrStr(s.ARAmount) || ptrStr(amountArg(f.GLAmount)) != ptrStr(s.GLAmount) || ptrStr(amountArg(f.Difference)) != ptrStr(s.Difference) {
			return false, "finding " + strconv.Itoa(f.Seq) + " differs"
		}
	}
	return ResultOf(again) == run.Result, "re-evaluation of the stored inputs reproduces the stored findings and result"
}

func ptrStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case string:
		return x
	case *string:
		if x == nil {
			return "<nil>"
		}
		return *x
	}
	return ""
}

func (m *Module) listReconciliationRuns(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	limit, offset := 50, 0
	if s := qv(r, "limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 200 {
			kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "limit must be 1..200")
			return
		}
		limit = n
	}
	if s := qv(r, "offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 100000 {
			kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "offset must be 0..100000")
			return
		}
		offset = n
	}
	entity, asOf := qv(r, "legal_entity_id"), qv(r, "as_of_date")
	if entity != "" {
		if _, err := uuid.Parse(entity); err != nil {
			kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "legal_entity_id must be a UUID")
			return
		}
	}
	var asOfT any
	if asOf != "" {
		t, err := time.Parse("2006-01-02", asOf)
		if err != nil {
			kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "as_of_date must be YYYY-MM-DD")
			return
		}
		asOfT = t
	}
	runs := []*RunView{}
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT `+runCols+` FROM arctl_runs r
			WHERE r.tenant_id = $1 AND ($2 = '' OR r.legal_entity_id::text = $2) AND ($3::date IS NULL OR r.as_of_date = $3::date)
			ORDER BY r.created_at DESC, r.run_id LIMIT $4 OFFSET $5`, c.TenantID, entity, asOfT, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanRun(rows)
			if err != nil {
				return err
			}
			runs = append(runs, v)
		}
		return rows.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs, "limit": limit, "offset": offset})
}

func (m *Module) getFindings(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	runID := qv(r, "run_id")
	if _, err := uuid.Parse(runID); err != nil {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "run_id must be a UUID")
		return
	}
	limit, after := 200, 0
	if s := qv(r, "limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 200 {
			kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "limit must be 1..200")
			return
		}
		limit = n
	}
	if s := qv(r, "after_seq"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "after_seq must be a non-negative integer")
			return
		}
		after = n
	}
	if _, err := m.getRun(r.Context(), c.TenantID, runID); err != nil {
		m.fail(w, err)
		return
	}
	fs, err := m.findings(r.Context(), c.TenantID, runID, qv(r, "category"), qv(r, "severity"), after, limit)
	if err != nil {
		m.fail(w, err)
		return
	}
	next := 0
	if len(fs) == limit {
		next = fs[len(fs)-1].Seq
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"run_id": runID, "findings": nonNilFindings(fs), "next_after_seq": next})
}

func nonNilFindings(f []FindingView) []FindingView {
	if f == nil {
		return []FindingView{}
	}
	return f
}

// getControlExceptions is the live roll-up of the owning modules' visible exceptions for a legal entity, with a reference
// to the latest recorded run.
func (m *Module) getControlExceptions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	entity := qv(r, "legal_entity_id")
	if _, err := uuid.Parse(entity); err != nil {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "legal_entity_id is required and must be a UUID")
		return
	}
	items, err := m.rollup(r.Context(), c.TenantID, entity)
	if err != nil {
		m.fail(w, kit.Unavailable("an exception source could not be read: "+err.Error()))
		return
	}
	bySource := map[string]int{}
	for _, it := range items {
		bySource[it.Source]++
	}
	resp := map[string]any{"legal_entity_id": entity, "exceptions": items, "count_by_source": bySource, "total": len(items)}
	var latest *RunView
	err = m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		v, err := scanRun(tx.QueryRow(r.Context(), `SELECT `+runCols+` FROM arctl_runs r WHERE r.tenant_id = $1 AND r.legal_entity_id::text = $2
			ORDER BY r.created_at DESC, r.run_id LIMIT 1`, c.TenantID, entity))
		if err == pgx.ErrNoRows {
			return nil
		}
		latest = v
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	if latest != nil {
		resp["latest_run"] = map[string]any{"run_id": latest.RunID, "as_of_date": latest.AsOfDate, "result": latest.Result, "finding_count": latest.FindingCount}
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}

// getARControlLink is the spec ARControlLink: AR population watermark, accounting-event / posting refs and the GL
// control-account reconciliation evidence of a run (given run_id, or the latest for legal_entity_id + as_of_date).
func (m *Module) getARControlLink(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	runID := qv(r, "run_id")
	if runID == "" {
		entity, asOf := qv(r, "legal_entity_id"), qv(r, "as_of_date")
		t, err := time.Parse("2006-01-02", asOf)
		if _, uerr := uuid.Parse(entity); uerr != nil || err != nil {
			kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "run_id, or legal_entity_id and as_of_date, is required")
			return
		}
		err = m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(), `SELECT r.run_id::text FROM arctl_runs r WHERE r.tenant_id = $1 AND r.legal_entity_id::text = $2 AND r.as_of_date = $3
				AND NOT EXISTS (SELECT 1 FROM arctl_runs s WHERE s.supersedes_run_id = r.run_id)`, c.TenantID, entity, t).Scan(&runID)
		})
		if err == pgx.ErrNoRows {
			m.fail(w, kit.NotFound("no reconciliation run for that entity and date"))
			return
		}
		if err != nil {
			m.fail(w, err)
			return
		}
	}
	run, err := m.getRun(r.Context(), c.TenantID, runID)
	if err != nil {
		m.fail(w, err)
		return
	}
	ev, err := m.evidence(r.Context(), c.TenantID, runID, true)
	if err != nil {
		m.fail(w, err)
		return
	}
	raw := map[string][]byte{}
	for _, e := range ev {
		raw[e.Kind] = e.Content
	}
	in, err := joinEvidence(raw)
	if err != nil {
		m.fail(w, err)
		return
	}
	fs, err := m.findings(r.Context(), c.TenantID, runID, "", "", 0, 100000)
	if err != nil {
		m.fail(w, err)
		return
	}
	q := map[string]QueueRow{}
	for _, row := range in.Queue {
		q[row.SourceEventID] = row
	}
	type ref struct {
		SourceType    string `json:"source_type"`
		SourceID      string `json:"source_id"`
		SourceEventID string `json:"source_event_id"`
		Status        string `json:"status"`
		JournalID     string `json:"journal_id,omitempty"`
	}
	posted, unposted := 0, []ref{}
	for _, f := range in.Facts {
		rw, found := q[f.SourceEventID]
		switch {
		case f.Legacy && f.JournalID != "":
			posted++
		case f.Legacy:
			if len(unposted) < 200 {
				unposted = append(unposted, ref{f.SourceType, f.SourceID, f.SourceEventID, "MISSING", ""})
			}
		case found && rw.Status == kit.PostingPosted:
			posted++
		default:
			if len(unposted) < 200 {
				st := "MISSING"
				if found {
					st = rw.Status
				}
				unposted = append(unposted, ref{f.SourceType, f.SourceID, f.SourceEventID, st, rw.JournalID})
			}
		}
	}
	open, ack := 0, 0
	for _, f := range fs {
		if f.Severity != SevInfo {
			open++
			if f.Acknowledged {
				ack++
			}
		}
	}
	var sum json.RawMessage = run.Summary
	kit.WriteJSON(w, http.StatusOK, map[string]any{
		"run_id": run.RunID, "legal_entity_id": run.LegalEntityID, "as_of_date": run.AsOfDate, "cutoff_date": run.CutoffDate, "result": run.Result,
		"population": map[string]any{"population": "open-invoices", "watermark": in.Subledger.Watermark, "row_count": in.Subledger.Count, "totals": in.Subledger.Totals,
			"expected_documents": len(in.Expected), "open_item_rows": openRows(in), "completeness_balanced": in.Completeness != nil && in.Completeness.Balanced},
		"accounting_events": map[string]any{"facts_checked": len(in.Facts), "posted": posted, "not_posted_refs": unposted},
		"gl_reconciliation": map[string]any{"gl_read": in.GL, "tolerance_policy": in.Tolerance, "exact_comparison": true,
			"differences": filterKind(fs, KindGLDifference), "unverified": !in.GL.Available || !in.Subledger.Available},
		"findings": map[string]any{"blocking": open, "acknowledged": ack}, "summary": sum, "inputs_hash": run.InputsHash,
	})
}

func openRows(in *Inputs) int {
	if in.Completeness == nil {
		return 0
	}
	return in.Completeness.OpenItemRows
}

func filterKind(fs []FindingView, kind string) []FindingView {
	out := []FindingView{}
	for _, f := range fs {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}
