package arctl

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// RunView is a stored run as returned to callers: exactly what was computed then.
type RunView struct {
	RunID               string          `json:"run_id"`
	LegalEntityID       string          `json:"legal_entity_id"`
	AsOfDate            string          `json:"as_of_date"`
	CutoffDate          string          `json:"cutoff_date"`
	Result              string          `json:"result"`
	SupersedesRunID     string          `json:"supersedes_run_id,omitempty"`
	SupersededByRunID   string          `json:"superseded_by_run_id,omitempty"`
	ToleranceRef        ToleranceRef    `json:"tolerance_policy"`
	ReasonCode          string          `json:"reason_code,omitempty"`
	PopulationWatermark string          `json:"population_watermark"`
	GLWatermark         string          `json:"gl_watermark"`
	InputsHash          string          `json:"inputs_hash"`
	Summary             json.RawMessage `json:"summary"`
	RunBy               string          `json:"run_by"`
	ComputedAt          time.Time       `json:"computed_at"`
	CreatedAt           time.Time       `json:"created_at"`
	FindingCount        int             `json:"finding_count"`
}

// FindingView is a stored finding plus its acknowledgements (annotations; the finding itself is never changed).
type FindingView struct {
	FindingID        string          `json:"finding_id"`
	RunID            string          `json:"run_id"`
	Seq              int             `json:"seq"`
	Category         string          `json:"category"`
	Kind             string          `json:"kind"`
	Code             string          `json:"code"`
	Severity         string          `json:"severity"`
	SubjectType      string          `json:"subject_type"`
	SubjectID        string          `json:"subject_id"`
	Currency         string          `json:"currency,omitempty"`
	ARAmount         *string         `json:"ar_amount"`
	GLAmount         *string         `json:"gl_amount"`
	Difference       *string         `json:"difference"`
	Detail           json.RawMessage `json:"detail"`
	Acknowledged     bool            `json:"acknowledged"`
	Acknowledgements []AckView       `json:"acknowledgements"`
}

// AckView is one acknowledgement annotation.
type AckView struct {
	AckID          string    `json:"ack_id"`
	ReasonCode     string    `json:"reason_code"`
	EvidenceID     string    `json:"evidence_id"`
	Note           string    `json:"note,omitempty"`
	AcknowledgedBy string    `json:"acknowledged_by"`
	CreatedAt      time.Time `json:"created_at"`
}

// EvidenceView indexes (and optionally carries) a run's evidence.
type EvidenceView struct {
	EvidenceID  string          `json:"evidence_id"`
	Kind        string          `json:"kind"`
	ContentHash string          `json:"content_hash"`
	Content     json.RawMessage `json:"content,omitempty"`
}

const runCols = `r.run_id::text, r.legal_entity_id::text, to_char(r.as_of_date,'YYYY-MM-DD'), to_char(r.cutoff_date,'YYYY-MM-DD'), r.result,
	COALESCE(r.supersedes_run_id::text,''), COALESCE((SELECT s.run_id::text FROM arctl_runs s WHERE s.supersedes_run_id = r.run_id),''),
	r.tolerance_policy_id, r.tolerance_policy_version, r.reason_code, r.population_watermark, r.gl_watermark, r.inputs_hash, r.summary,
	r.run_by, r.computed_at, r.created_at, (SELECT count(*) FROM arctl_findings f WHERE f.run_id = r.run_id)`

func scanRun(row pgx.Row) (*RunView, error) {
	var v RunView
	if err := row.Scan(&v.RunID, &v.LegalEntityID, &v.AsOfDate, &v.CutoffDate, &v.Result, &v.SupersedesRunID, &v.SupersededByRunID,
		&v.ToleranceRef.PolicyID, &v.ToleranceRef.Version, &v.ReasonCode, &v.PopulationWatermark, &v.GLWatermark, &v.InputsHash, &v.Summary,
		&v.RunBy, &v.ComputedAt, &v.CreatedAt, &v.FindingCount); err != nil {
		return nil, err
	}
	return &v, nil
}

func amountArg(a *kit.Amount) any {
	if a == nil {
		return nil
	}
	return a.String()
}

// SaveRequest is one run to persist.
type SaveRequest struct {
	Inputs          *Inputs
	Findings        []Finding
	Result          string
	Summary         Summary
	SupersedesRunID string // optional guard: must be the current head of the chain
	ReasonCode      string
}

// save persists the run, findings and evidence in ONE transaction (with the audit row and outbox events), serialised per
// (tenant, entity, as-of). The previous head of the chain, if any, becomes `supersedes_run_id`.
func (m *Module) save(ctx context.Context, c kit.Caller, req SaveRequest, asOf time.Time) (*RunView, error) {
	in := req.Inputs
	inputsHash, _, err := hashJSON(splitEvidence(in))
	if err != nil {
		return nil, err
	}
	asOfT := asOf
	cutoffT := CutoffOf(asOf)
	runID := uuid.NewString()
	var out *RunView
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, c.TenantID+"|"+c.LegalEntityID+"|"+in.AsOfDate); err != nil {
			return err
		}
		var head string
		err := tx.QueryRow(ctx, `
			SELECT r.run_id::text FROM arctl_runs r
			 WHERE r.tenant_id = $1 AND r.legal_entity_id = $2 AND r.as_of_date = $3
			   AND NOT EXISTS (SELECT 1 FROM arctl_runs s WHERE s.supersedes_run_id = r.run_id)`, c.TenantID, c.LegalEntityID, asOf).Scan(&head)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if req.SupersedesRunID != "" && req.SupersedesRunID != head {
			return kit.Err(http.StatusConflict, kit.CodeStaleVersion, "supersedes_run_id is not the current run for this entity and as-of date")
		}
		var supersedes any
		if head != "" {
			supersedes = head
		}
		summaryJSON, err := json.Marshal(req.Summary)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO arctl_runs (run_id, tenant_id, legal_entity_id, as_of_date, cutoff_date, result, supersedes_run_id,
				tolerance_policy_id, tolerance_policy_version, reason_code, population_watermark, gl_watermark, inputs_hash, summary,
				run_by, correlation_id, computed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			runID, c.TenantID, c.LegalEntityID, asOfT, cutoffT, req.Result, supersedes,
			in.Tolerance.PolicyID, in.Tolerance.Version, req.ReasonCode, in.Subledger.Watermark, in.GL.Watermark, inputsHash, summaryJSON,
			c.PrincipalID, c.CorrelationID, in.ComputedAt); err != nil {
			return err
		}
		for _, f := range req.Findings {
			detail, err := json.Marshal(f.Detail)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO arctl_findings (finding_id, run_id, tenant_id, legal_entity_id, seq, category, kind, code, severity, subject_type, subject_id,
					currency, ar_amount, gl_amount, difference, detail)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
				uuid.NewString(), runID, c.TenantID, c.LegalEntityID, f.Seq, f.Category, f.Kind, f.Code, f.Severity, f.SubjectType, f.SubjectID,
				f.Currency, amountArg(f.ARAmount), amountArg(f.GLAmount), amountArg(f.Difference), detail); err != nil {
				return err
			}
		}
		for kind, content := range splitEvidence(in) {
			hash, b, err := hashJSON(content)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO arctl_evidence (evidence_id, run_id, tenant_id, legal_entity_id, kind, content, content_hash)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.NewString(), runID, c.TenantID, c.LegalEntityID, kind, b, hash); err != nil {
				return err
			}
		}
		payload := map[string]any{"run_id": runID, "legal_entity_id": c.LegalEntityID, "as_of_date": in.AsOfDate, "result": req.Result,
			"finding_count": len(req.Findings), "supersedes_run_id": supersedes, "tolerance_policy_id": in.Tolerance.PolicyID,
			"tolerance_policy_version": in.Tolerance.Version, "correlation_id": c.CorrelationID}
		events := []kit.Event{{AggregateType: aggregateRun, AggregateID: runID, EventType: EventRunCompleted, Payload: payload}}
		if req.Result == ResultBreak {
			events = append(events, kit.Event{AggregateType: aggregateRun, AggregateID: runID, EventType: EventBreakDetected, Payload: payload})
		}
		if err := kit.Emit(ctx, tx, c, events...); err != nil {
			return err
		}
		if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: aggregateRun, AggregateID: runID, Command: "runARGLReconciliation",
			ReasonCode: req.ReasonCode, After: map[string]any{"result": req.Result, "inputs_hash": inputsHash}}); err != nil {
			return err
		}
		out, err = scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM arctl_runs r WHERE r.tenant_id = $1 AND r.run_id = $2`, c.TenantID, runID))
		return err
	})
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	return out, nil
}

func (m *Module) getRun(ctx context.Context, tenantID, runID string) (*RunView, error) {
	var v *RunView
	err := m.deps.DB.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		v, err = scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM arctl_runs r WHERE r.tenant_id = $1 AND r.run_id::text = $2`, tenantID, runID))
		return err
	})
	if err == pgx.ErrNoRows {
		return nil, kit.NotFound("reconciliation run not found")
	}
	return v, kit.MapPgErr(err)
}

func (m *Module) evidence(ctx context.Context, tenantID, runID string, withContent bool) ([]EvidenceView, error) {
	var out []EvidenceView
	err := m.deps.DB.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT evidence_id::text, kind, content_hash, content FROM arctl_evidence WHERE tenant_id = $1 AND run_id::text = $2 ORDER BY kind`, tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e EvidenceView
			var content json.RawMessage
			if err := rows.Scan(&e.EvidenceID, &e.Kind, &e.ContentHash, &content); err != nil {
				return err
			}
			if withContent {
				e.Content = content
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, kit.MapPgErr(err)
}

func (m *Module) findings(ctx context.Context, tenantID, runID, category, severity string, afterSeq, limit int) ([]FindingView, error) {
	var out []FindingView
	err := m.deps.DB.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT finding_id::text, run_id::text, seq, category, kind, code, severity, subject_type, subject_id, currency,
			       ar_amount::text, gl_amount::text, difference::text, detail
			  FROM arctl_findings WHERE tenant_id = $1 AND run_id::text = $2 AND seq > $3
			   AND ($4 = '' OR category = $4) AND ($5 = '' OR severity = $5)
			 ORDER BY seq LIMIT $6`, tenantID, runID, afterSeq, category, severity, limit)
		if err != nil {
			return err
		}
		ids := map[string]int{}
		for rows.Next() {
			var f FindingView
			if err := rows.Scan(&f.FindingID, &f.RunID, &f.Seq, &f.Category, &f.Kind, &f.Code, &f.Severity, &f.SubjectType, &f.SubjectID, &f.Currency,
				&f.ARAmount, &f.GLAmount, &f.Difference, &f.Detail); err != nil {
				rows.Close()
				return err
			}
			f.Acknowledgements = []AckView{}
			ids[f.FindingID] = len(out)
			out = append(out, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		ar, err := tx.Query(ctx, `
			SELECT finding_id::text, ack_id::text, reason_code, evidence_id, note, acknowledged_by, created_at
			  FROM arctl_acknowledgements WHERE tenant_id = $1 AND run_id::text = $2 ORDER BY created_at, ack_id`, tenantID, runID)
		if err != nil {
			return err
		}
		defer ar.Close()
		for ar.Next() {
			var fid string
			var a AckView
			if err := ar.Scan(&fid, &a.AckID, &a.ReasonCode, &a.EvidenceID, &a.Note, &a.AcknowledgedBy, &a.CreatedAt); err != nil {
				return err
			}
			if i, ok := ids[fid]; ok {
				out[i].Acknowledgements = append(out[i].Acknowledgements, a)
				out[i].Acknowledged = true
			}
		}
		return ar.Err()
	})
	return out, kit.MapPgErr(err)
}
