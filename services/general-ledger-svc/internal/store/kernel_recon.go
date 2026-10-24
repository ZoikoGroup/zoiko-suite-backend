package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

var _ kernel.ReconRepo = (*KernelRepo)(nil)

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EventPosting implements kernel.ReconRepo: how many accounting events were registered up to a moment and how many of them have a
// posting in the book as of that moment. The events that do not are listed (the first 200).
func (r *KernelRepo) EventPosting(ctx context.Context, tenant, entity, bookID string, asOf time.Time, at time.Time) (kernel.EventPosting, error) {
	ep := kernel.EventPosting{At: at}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return t.QueryRow(ctx, `
			SELECT count(*), count(l.accounting_event_id),
			       COALESCE((array_agg(e.accounting_event_id::text ORDER BY e.registered_at) FILTER (WHERE l.accounting_event_id IS NULL))[1:200], '{}')
			FROM accounting_events e
			LEFT JOIN event_posting_links l ON l.tenant_id = e.tenant_id AND l.accounting_event_id = e.accounting_event_id
			     AND l.accounting_book_id = $3::uuid AND l.created_at <= $5
			WHERE e.tenant_id = $1::uuid AND e.legal_entity_id = $2::uuid AND e.occurred_at::date <= $4::date AND e.registered_at <= $5`,
			tenant, entity, bookID, asOf, at).Scan(&ep.Registered, &ep.Posted, &ep.Missing)
	})
	return ep, kernelErr(err)
}

const tolCols = `tolerance_rule_id::text, tenant_id::text, reconciliation_type, COALESCE(legal_entity_id::text, ''), match_amount::text, warning_amount::text, version, effective_from, created_by, created_at`

func scanTolerance(row pgx.Row, t *kernel.Tolerance) error {
	return row.Scan(&t.ToleranceRuleID, &t.TenantID, &t.ReconciliationType, &t.LegalEntityID, &t.MatchAmount, &t.WarningAmount, &t.Version, &t.EffectiveFrom, &t.CreatedBy, &t.CreatedAt)
}

// PickTolerance implements kernel.ReconRepo: the rule in force on the date, an entity's own before the tenant-wide one.
func (r *KernelRepo) PickTolerance(ctx context.Context, tenant, reconType, entity string, asOf time.Time) (*kernel.Tolerance, error) {
	var t kernel.Tolerance
	err := r.tx(ctx, tenant, func(tx pgx.Tx) error {
		return scanTolerance(tx.QueryRow(ctx, `SELECT `+tolCols+` FROM reconciliation_tolerances
			WHERE tenant_id = $1::uuid AND reconciliation_type = $2 AND (legal_entity_id IS NULL OR legal_entity_id = $3::uuid) AND effective_from <= $4::date
			ORDER BY (legal_entity_id IS NOT NULL) DESC, effective_from DESC, version DESC LIMIT 1`, tenant, reconType, entity, asOf), &t)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &t, nil
}

// InsertTolerance implements kernel.ReconRepo: the next version of the rule for its scope.
func (r *KernelRepo) InsertTolerance(ctx context.Context, t *kernel.Tolerance, out kernel.OutEvent) error {
	return r.tx(ctx, t.TenantID, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "tolerance:"+t.TenantID+":"+t.ReconciliationType+":"+t.LegalEntityID); e != nil {
			return e
		}
		if e := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM reconciliation_tolerances
			WHERE tenant_id = $1::uuid AND reconciliation_type = $2 AND legal_entity_id IS NOT DISTINCT FROM $3::uuid`, t.TenantID, t.ReconciliationType, nilIfEmpty(t.LegalEntityID)).Scan(&t.Version); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `INSERT INTO reconciliation_tolerances (tolerance_rule_id, tenant_id, reconciliation_type, legal_entity_id, match_amount, warning_amount, version, effective_from, created_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5::numeric, $6::numeric, $7, $8::date, $9, $10)`,
			t.ToleranceRuleID, t.TenantID, t.ReconciliationType, nilIfEmpty(t.LegalEntityID), t.MatchAmount, t.WarningAmount, t.Version, t.EffectiveFrom, t.CreatedBy, t.CreatedAt); e != nil {
			return e
		}
		out.Data["version"] = t.Version
		return r.enqueue(ctx, tx, t.TenantID, out)
	})
}

// ListTolerances implements kernel.ReconRepo.
func (r *KernelRepo) ListTolerances(ctx context.Context, tenant, reconType string) ([]kernel.Tolerance, error) {
	out := []kernel.Tolerance{}
	err := r.tx(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+tolCols+` FROM reconciliation_tolerances WHERE tenant_id = $1::uuid AND ($2 = '' OR reconciliation_type = $2)
			ORDER BY reconciliation_type, legal_entity_id NULLS FIRST, version DESC LIMIT 500`, tenant, reconType)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t kernel.Tolerance
			if err := scanTolerance(rows, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

const runCols = `reconciliation_run_id::text, tenant_id::text, reconciliation_type, legal_entity_id::text, accounting_book_id::text, fiscal_period, as_of::text, COALESCE(currency, ''),
	population_a, population_b, expected_total::text, actual_total::text, difference::text, COALESCE(tolerance_rule_id::text, ''), COALESCE(tolerance_version, 0), status,
	COALESCE(owner, ''), sla_due_at, COALESCE(resolution_type, ''), COALESCE(resolution_note, ''), evidence, COALESCE(resolved_by, ''), resolved_at, COALESCE(approved_by, ''), approved_at,
	created_by, created_at`

func scanRun(row pgx.Row, r *kernel.ReconRun) error {
	var a, b, ev []byte
	if err := row.Scan(&r.RunID, &r.TenantID, &r.Type, &r.LegalEntityID, &r.BookID, &r.FiscalPeriod, &r.AsOf, &r.Currency, &a, &b, &r.ExpectedTotal, &r.ActualTotal, &r.Difference,
		&r.ToleranceRuleID, &r.ToleranceVersion, &r.Status, &r.Owner, &r.SLADueAt, &r.ResolutionType, &r.ResolutionNote, &ev, &r.ResolvedBy, &r.ResolvedAt, &r.ApprovedBy, &r.ApprovedAt,
		&r.CreatedBy, &r.CreatedAt); err != nil {
		return err
	}
	_ = json.Unmarshal(a, &r.PopulationA)
	_ = json.Unmarshal(b, &r.PopulationB)
	r.Evidence = []string{}
	_ = json.Unmarshal(ev, &r.Evidence)
	return nil
}

func addCaseEvent(ctx context.Context, t pgx.Tx, tenant, runID, kind, from, to, actor string, detail map[string]any, at time.Time) error {
	d, _ := json.Marshal(detail)
	_, err := t.Exec(ctx, `INSERT INTO reconciliation_case_events (event_id, tenant_id, reconciliation_run_id, kind, from_status, to_status, actor, detail, at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8::jsonb, $9)`, uuid.NewString(), tenant, runID, kind, from, to, actor, string(d), at)
	return err
}

// InsertRun implements kernel.ReconRepo: the run, the first entry of its history and, for a case, the event that raises it.
func (r *KernelRepo) InsertRun(ctx context.Context, run *kernel.ReconRun, out *kernel.OutEvent) error {
	return r.tx(ctx, run.TenantID, func(t pgx.Tx) error {
		a, _ := json.Marshal(run.PopulationA)
		b, _ := json.Marshal(run.PopulationB)
		ev, _ := json.Marshal(run.Evidence)
		if _, e := t.Exec(ctx, `
			INSERT INTO reconciliation_runs (reconciliation_run_id, tenant_id, reconciliation_type, legal_entity_id, accounting_book_id, fiscal_period, as_of, currency, population_a, population_b,
				expected_total, actual_total, difference, tolerance_rule_id, tolerance_version, status, owner, sla_due_at, evidence, created_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5::uuid, $6, $7::date, NULLIF($8, ''), $9::jsonb, $10::jsonb, $11::numeric, $12::numeric, $13::numeric, NULLIF($14, '')::uuid, NULLIF($15, 0),
			        $16, NULLIF($17, ''), $18, $19::jsonb, $20, $21)`,
			run.RunID, run.TenantID, run.Type, run.LegalEntityID, run.BookID, run.FiscalPeriod, run.AsOf, run.Currency, string(a), string(b),
			run.ExpectedTotal, run.ActualTotal, run.Difference, run.ToleranceRuleID, run.ToleranceVersion, run.Status, run.Owner, run.SLADueAt, string(ev), run.CreatedBy, run.CreatedAt); e != nil {
			return e
		}
		if e := addCaseEvent(ctx, t, run.TenantID, run.RunID, "RUN", "", run.Status, run.CreatedBy, map[string]any{"difference": run.Difference, "expected_total": run.ExpectedTotal, "actual_total": run.ActualTotal}, run.CreatedAt); e != nil {
			return e
		}
		if out != nil {
			return r.enqueue(ctx, t, run.TenantID, *out)
		}
		return nil
	})
}

// GetRun implements kernel.ReconRepo.
func (r *KernelRepo) GetRun(ctx context.Context, tenant, id string) (*kernel.ReconRun, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var run kernel.ReconRun
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanRun(t.QueryRow(ctx, `SELECT `+runCols+` FROM reconciliation_runs WHERE tenant_id = $1::uuid AND reconciliation_run_id = $2::uuid`, tenant, id), &run)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &run, nil
}

// ListRuns implements kernel.ReconRepo.
func (r *KernelRepo) ListRuns(ctx context.Context, tenant string, f kernel.ReconFilter) ([]kernel.ReconRun, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	for _, id := range []string{f.LegalEntityID, f.BookID} {
		if id != "" {
			if _, err := uuid.Parse(id); err != nil {
				return []kernel.ReconRun{}, nil
			}
		}
	}
	out := []kernel.ReconRun{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT `+runCols+` FROM reconciliation_runs WHERE tenant_id = $1::uuid
			AND ($2 = '' OR reconciliation_type = $2) AND ($3 = '' OR status = $3) AND ($4 = '' OR legal_entity_id::text = $4) AND ($5 = '' OR accounting_book_id::text = $5)
			AND ($6 = '' OR fiscal_period = $6) ORDER BY created_at DESC, reconciliation_run_id LIMIT $7`, tenant, f.Type, f.Status, f.LegalEntityID, f.BookID, f.FiscalPeriod, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var run kernel.ReconRun
			if err := scanRun(rows, &run); err != nil {
				return err
			}
			out = append(out, run)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// RunEvents implements kernel.ReconRepo.
func (r *KernelRepo) RunEvents(ctx context.Context, tenant, id string) ([]kernel.ReconEvent, error) {
	out := []kernel.ReconEvent{}
	if _, err := uuid.Parse(id); err != nil {
		return out, nil
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT kind, COALESCE(from_status, ''), COALESCE(to_status, ''), actor, detail, at FROM reconciliation_case_events
			WHERE tenant_id = $1::uuid AND reconciliation_run_id = $2::uuid ORDER BY at, event_id`, tenant, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e kernel.ReconEvent
			var d []byte
			if err := rows.Scan(&e.Kind, &e.FromStatus, &e.ToStatus, &e.Actor, &d, &e.At); err != nil {
				return err
			}
			_ = json.Unmarshal(d, &e.Detail)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

func lockRun(ctx context.Context, t pgx.Tx, tenant, id string) (*kernel.ReconRun, error) {
	var run kernel.ReconRun
	if err := scanRun(t.QueryRow(ctx, `SELECT `+runCols+` FROM reconciliation_runs WHERE tenant_id = $1::uuid AND reconciliation_run_id = $2::uuid FOR UPDATE`, tenant, id), &run); err != nil {
		return nil, kernelErr(err)
	}
	return &run, nil
}

func (r *KernelRepo) mutateRun(ctx context.Context, tenant, id string, fn func(t pgx.Tx, run *kernel.ReconRun) error) (*kernel.ReconRun, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var out kernel.ReconRun
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		run, e := lockRun(ctx, t, tenant, id)
		if e != nil {
			return e
		}
		if e := fn(t, run); e != nil {
			return e
		}
		return scanRun(t.QueryRow(ctx, `SELECT `+runCols+` FROM reconciliation_runs WHERE tenant_id = $1::uuid AND reconciliation_run_id = $2::uuid`, tenant, id), &out)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &out, nil
}

// AssignRun implements kernel.ReconRepo.
func (r *KernelRepo) AssignRun(ctx context.Context, tenant, id, owner string, sla time.Time, actor string) (*kernel.ReconRun, error) {
	return r.mutateRun(ctx, tenant, id, func(t pgx.Tx, run *kernel.ReconRun) error {
		if run.Status != kernel.ReconWarning && run.Status != kernel.ReconBreak {
			return kernel.ErrState
		}
		if _, e := t.Exec(ctx, `UPDATE reconciliation_runs SET owner = $3, sla_due_at = $4 WHERE tenant_id = $1::uuid AND reconciliation_run_id = $2::uuid`, tenant, id, owner, sla); e != nil {
			return e
		}
		return addCaseEvent(ctx, t, tenant, id, "ASSIGNED", run.Status, run.Status, actor, map[string]any{"owner": owner, "sla_due_at": sla}, time.Now().UTC())
	})
}

// AcceptRun implements kernel.ReconRepo.
func (r *KernelRepo) AcceptRun(ctx context.Context, tenant, id, approver, note string, evidence []string, at time.Time) (*kernel.ReconRun, error) {
	return r.mutateRun(ctx, tenant, id, func(t pgx.Tx, run *kernel.ReconRun) error {
		if run.Status != kernel.ReconWarning && run.Status != kernel.ReconBreak {
			return kernel.ErrState
		}
		if run.CreatedBy == approver {
			return kernel.ErrSelfApproval
		}
		ev, _ := json.Marshal(evidence)
		if _, e := t.Exec(ctx, `UPDATE reconciliation_runs SET status = 'ACCEPTED_EXCEPTION', approved_by = $3, approved_at = $4, resolution_note = $5, evidence = evidence || $6::jsonb
			WHERE tenant_id = $1::uuid AND reconciliation_run_id = $2::uuid`, tenant, id, approver, at, note, string(ev)); e != nil {
			return e
		}
		if e := addCaseEvent(ctx, t, tenant, id, "ACCEPTED", run.Status, kernel.ReconAcceptedException, approver, map[string]any{"note": note, "evidence": evidence, "difference": run.Difference}, at); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, kernel.OutEvent{Fact: "reconciliation.exception-accepted", AggregateType: "reconciliation-run", AggregateID: id, Actor: approver, LegalEntityID: run.LegalEntityID,
			Data: map[string]any{"reconciliation_run_id": id, "reconciliation_type": run.Type, "difference": run.Difference, "approved_by": approver}})
	})
}

// ResolveRun implements kernel.ReconRepo.
func (r *KernelRepo) ResolveRun(ctx context.Context, tenant, id, resolver, resolutionType, note string, evidence []string, journalID string, at time.Time) (*kernel.ReconRun, error) {
	return r.mutateRun(ctx, tenant, id, func(t pgx.Tx, run *kernel.ReconRun) error {
		if run.Status != kernel.ReconWarning && run.Status != kernel.ReconBreak && run.Status != kernel.ReconAcceptedException {
			return kernel.ErrState
		}
		if resolutionType == "MANUAL_CORRECTION" {
			var st string
			e := t.QueryRow(ctx, `SELECT status FROM journal_headers WHERE tenant_id = $1::uuid AND journal_id = $2::uuid AND reconciliation_case_id = $3::uuid`, tenant, journalID, id).Scan(&st)
			if errors.Is(e, pgx.ErrNoRows) {
				return &kernel.Exception{Family: kernel.FamData, Code: "RESOLUTION_JOURNAL_INVALID", Detail: "the journal does not exist or does not name this reconciliation case"}
			}
			if e != nil {
				return e
			}
			if st != "FINALIZED" {
				return &kernel.Exception{Family: kernel.FamData, Code: "RESOLUTION_JOURNAL_INVALID", Detail: fmt.Sprintf("the correcting journal is %s: it must be posted", st)}
			}
		}
		ev, _ := json.Marshal(evidence)
		if _, e := t.Exec(ctx, `UPDATE reconciliation_runs SET status = 'RESOLVED', resolution_type = $3, resolution_note = NULLIF($4, ''), evidence = evidence || $5::jsonb, resolved_by = $6, resolved_at = $7
			WHERE tenant_id = $1::uuid AND reconciliation_run_id = $2::uuid`, tenant, id, resolutionType, note, string(ev), resolver, at); e != nil {
			return e
		}
		detail := map[string]any{"resolution_type": resolutionType, "note": note, "evidence": evidence}
		if journalID != "" {
			detail["journal_id"] = journalID
		}
		if e := addCaseEvent(ctx, t, tenant, id, "RESOLVED", run.Status, kernel.ReconResolved, resolver, detail, at); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, kernel.OutEvent{Fact: "reconciliation.resolved", AggregateType: "reconciliation-run", AggregateID: id, Actor: resolver, LegalEntityID: run.LegalEntityID,
			Data: map[string]any{"reconciliation_run_id": id, "reconciliation_type": run.Type, "resolution_type": resolutionType, "difference": run.Difference}})
	})
}

// CaseStatus implements kernel.ReconRepo: the reconciliation picture of one scope for the period-close gate.
func (r *KernelRepo) CaseStatus(ctx context.Context, tenant, entity, bookID, period string) (*kernel.CaseStatus, error) {
	cs := &kernel.CaseStatus{LatestByType: map[string]string{}, OpenCaseIDs: []string{}, MissingRequired: []string{}}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT reconciliation_run_id::text, status FROM reconciliation_runs
			WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND accounting_book_id = $3::uuid AND fiscal_period = $4 AND status IN ('WARNING', 'BREAK') ORDER BY created_at`, tenant, entity, bookID, period)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, st string
			if err := rows.Scan(&id, &st); err != nil {
				rows.Close()
				return err
			}
			cs.OpenCaseIDs = append(cs.OpenCaseIDs, id)
			if st == kernel.ReconBreak {
				cs.OpenBreaks++
			} else {
				cs.OpenWarnings++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = t.Query(ctx, `SELECT DISTINCT ON (reconciliation_type) reconciliation_type, status FROM reconciliation_runs
			WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND accounting_book_id = $3::uuid AND fiscal_period = $4 ORDER BY reconciliation_type, created_at DESC`, tenant, entity, bookID, period)
		if err != nil {
			return err
		}
		for rows.Next() {
			var typ, st string
			if err := rows.Scan(&typ, &st); err != nil {
				rows.Close()
				return err
			}
			cs.LatestByType[typ] = st
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT count(*) FROM posting_executions WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND status IN ('FAILED', 'QUARANTINED')`, tenant, entity).Scan(&cs.FailedPostings)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	for _, typ := range []string{kernel.ReconAR, kernel.ReconAP, kernel.ReconBank, kernel.ReconEventPosting} {
		if _, ok := cs.LatestByType[typ]; !ok {
			cs.MissingRequired = append(cs.MissingRequired, typ)
		}
	}
	cs.Clear = cs.OpenBreaks == 0 && cs.OpenWarnings == 0 && cs.FailedPostings == 0
	return cs, nil
}

// CounterpartyBalances implements kernel.ReconRepo: the position of each counterparty on a control type, so a subledger can be
// compared customer by customer or supplier by supplier.
func (r *KernelRepo) CounterpartyBalances(ctx context.Context, tenant, entity, bookCode, control string, asOf time.Time) ([]kernel.CounterpartyBalance, error) {
	out := []kernel.CounterpartyBalance{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT COALESCE(counterparty_id, ''), count(*), sum(debit_amount)::text, sum(credit_amount)::text FROM ledger_entries
			WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = $3 AND control_type = $4 AND posting_date <= $5::date
			GROUP BY counterparty_id ORDER BY counterparty_id LIMIT 5000`, tenant, entity, bookCode, control, asOf)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b kernel.CounterpartyBalance
			if err := rows.Scan(&b.CounterpartyID, &b.Entries, &b.Debit, &b.Credit); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}
