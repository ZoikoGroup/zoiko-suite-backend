package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// ResultChange is what a committed ReceivePayrollResult changed; the builder turns
// it into outbox events INSIDE the transaction.
type ResultChange struct {
	Ref        *domain.PayrollRunRef
	Result     *domain.PayrollResult
	Registered bool                  // the run reference was created by this command
	Superseded *domain.PayrollRunRef // the previous version, now SUPERSEDED
}

type ResultEventBuilder func(ResultChange) ([]domain.OutboxEvent, error)

// ReceiveResultInput carries a prepared command: the run reference to register, the
// request, and the pure evaluation of every control.
type ReceiveResultInput struct {
	Ref             domain.PayrollRunRef
	Request         domain.ReceivePayrollResultRequest
	Evaluation      domain.ResultEvaluation
	ReceivedBy      string
	CorrelationID   string
	SupersedeReason string
}

type ReceiveResultOutput struct {
	Ref        *domain.PayrollRunRef
	Result     *domain.PayrollResult
	Created    bool // false: the identical result was already on record
	Superseded *domain.PayrollRunRef
}

const resultCols = `result_id, tenant_id, run_ref_id, legal_entity_id, outcome, rejection_reasons, currency, source_run_status,
	source_approval_ref, calculation_ref, input_batch_id::text, evidence_flags, worker_count, total_gross::text,
	total_taxable_pay::text, total_employee_deductions::text, total_employer_liabilities::text, total_net_pay::text,
	declared_totals, result_fingerprint, received_by, correlation_id, created_at`

func scanResult(row pgx.Row) (*domain.PayrollResult, error) {
	var r domain.PayrollResult
	var reasons, declared []byte
	err := row.Scan(&r.ResultID, &r.TenantID, &r.RunRefID, &r.LegalEntityID, &r.Outcome, &reasons, &r.Currency, &r.SourceRunStatus,
		&r.SourceApprovalRef, &r.CalculationRef, &r.InputBatchID, &r.EvidenceFlags, &r.WorkerCount, &r.TotalGross,
		&r.TotalTaxablePay, &r.TotalEmployeeDeductions, &r.TotalEmployerLiabilities, &r.TotalNetPay,
		&declared, &r.ResultFingerprint, &r.ReceivedBy, &r.CorrelationID, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrResultNotFound
	}
	if err != nil {
		return nil, err
	}
	r.RejectionReasons = []domain.ResultReason{}
	if len(reasons) > 0 {
		if err := json.Unmarshal(reasons, &r.RejectionReasons); err != nil {
			return nil, fmt.Errorf("decode rejection reasons: %w", err)
		}
	}
	if len(declared) > 0 {
		if err := json.Unmarshal(declared, &r.DeclaredTotals); err != nil {
			return nil, fmt.Errorf("decode declared totals: %w", err)
		}
	}
	if r.EvidenceFlags == nil {
		r.EvidenceFlags = []string{}
	}
	return &r, nil
}

func lockRunRefTx(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.PayrollRunRef, error) {
	return scanRunRef(tx.QueryRow(ctx, `SELECT `+runRefCols+` FROM wfp_payroll_run_refs
		WHERE tenant_id = $1 AND run_ref_id = $2::uuid FOR UPDATE`, tenantID, id))
}

func setRunRefStateTx(ctx context.Context, tx pgx.Tx, tenantID string, ref *domain.PayrollRunRef, to, fingerprint string) (*domain.PayrollRunRef, error) {
	var fp *string
	if fingerprint != "" {
		fp = &fingerprint
	}
	return scanRunRef(tx.QueryRow(ctx, `
		UPDATE wfp_payroll_run_refs
		SET acceptance_state = $3, result_fingerprint = COALESCE($4, result_fingerprint), version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND run_ref_id = $2::uuid AND version = $5 RETURNING `+runRefCols,
		tenantID, ref.RunRefID, to, fp, ref.Version))
}

// ReceivePayrollResult registers the run version (or finds it), validates it, and
// stores the outcome in ONE transaction.
//
//   - The identical result again is a replay and returns the stored one.
//   - The same run version with different content is ErrResultFingerprintConflict
//     (a changed result must be a new source_run_version, WFP-NP-015).
//   - A failed control stores a REJECTED result (reasons and totals as evidence, no
//     worker detail) and the run version ends REJECTED.
//   - An accepted result stores worker and component detail, and if the previous
//     version of the same source run was ACCEPTED it is SUPERSEDED through a
//     correction chain; the previous result is never edited.
func (s *PgStore) ReceivePayrollResult(ctx context.Context, in ReceiveResultInput, mk ResultEventBuilder) (*ReceiveResultOutput, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *ReceiveResultOutput
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			tenantID+"|result|"+in.Ref.LegalEntityID+"|"+in.Ref.SourceSystemID+"|"+in.Ref.SourceRunID); err != nil {
			return err
		}

		ref := in.Ref
		registered, err := registerRunRefTx(ctx, tx, tenantID, &ref)
		if err != nil {
			return err
		}
		if !registered {
			existing, err := scanResult(tx.QueryRow(ctx, `SELECT `+resultCols+` FROM wfp_payroll_results
				WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, ref.RunRefID))
			switch {
			case err == nil:
				if existing.ResultFingerprint != in.Evaluation.Fingerprint {
					return domain.ErrResultFingerprintConflict
				}
				out = &ReceiveResultOutput{Ref: &ref, Result: existing}
				return nil
			case !errors.Is(err, domain.ErrResultNotFound):
				return err
			}
			// A reference pre-registered without a result is still RECEIVED; anything
			// else here would be a run version with no result to compare against.
			if ref.AcceptanceState != domain.AcceptanceReceived {
				return domain.ErrInvalidAcceptanceMove
			}
		}

		locked, err := lockRunRefTx(ctx, tx, tenantID, ref.RunRefID)
		if err != nil {
			return err
		}
		validating, err := setRunRefStateTx(ctx, tx, tenantID, locked, domain.AcceptanceValidating, "")
		if err != nil {
			return err
		}

		accepted := in.Evaluation.Accepted()
		result, err := insertResultTx(ctx, tx, tenantID, validating, in, accepted)
		if err != nil {
			return err
		}

		var superseded *domain.PayrollRunRef
		final := validating
		if accepted {
			if err := insertResultDetailTx(ctx, tx, tenantID, result.ResultID, in.Evaluation.Workers); err != nil {
				return err
			}
			if validating.SupersedesRunRefID != nil {
				prior, err := lockRunRefTx(ctx, tx, tenantID, *validating.SupersedesRunRefID)
				if err != nil {
					return err
				}
				if prior.AcceptanceState == domain.AcceptanceAccepted {
					priorResult, err := scanResult(tx.QueryRow(ctx, `SELECT `+resultCols+` FROM wfp_payroll_results
						WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, prior.RunRefID))
					if err != nil {
						return fmt.Errorf("previous accepted version has no result: %w", err)
					}
					reason := in.SupersedeReason
					if reason == "" {
						reason = fmt.Sprintf("superseded by source run version %d", validating.SourceRunVersion)
					}
					if _, err := tx.Exec(ctx, `
						INSERT INTO wfp_payroll_correction_chains
							(chain_id, tenant_id, original_run_ref_id, superseding_run_ref_id, original_result_id, superseding_result_id, reason, created_by)
						VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5::uuid, $6::uuid, $7, $8)`,
						uuid.NewString(), tenantID, prior.RunRefID, validating.RunRefID, priorResult.ResultID, result.ResultID, reason, in.ReceivedBy); err != nil {
						return fmt.Errorf("correction chain: %w", err)
					}
					superseded, err = setRunRefStateTx(ctx, tx, tenantID, prior, domain.AcceptanceSuperseded, "")
					if err != nil {
						return err
					}
				}
			}
			final, err = setRunRefStateTx(ctx, tx, tenantID, validating, domain.AcceptanceAccepted, in.Evaluation.Fingerprint)
		} else {
			final, err = setRunRefStateTx(ctx, tx, tenantID, validating, domain.AcceptanceRejected, "")
		}
		if err != nil {
			return err
		}

		if mk != nil {
			evs, err := mk(ResultChange{Ref: final, Result: result, Registered: registered, Superseded: superseded})
			if err != nil {
				return fmt.Errorf("build events: %w", err)
			}
			for _, ev := range evs {
				if !domain.EventCatalogue[ev.EventType] {
					return fmt.Errorf("event type %q is not in the WFP catalogue", ev.EventType)
				}
				if _, err := tx.Exec(ctx, `INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
					ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload); err != nil {
					return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
				}
			}
		}
		out = &ReceiveResultOutput{Ref: final, Result: result, Created: true, Superseded: superseded}
		return nil
	})
	return out, err
}

func insertResultTx(ctx context.Context, tx pgx.Tx, tenantID string, ref *domain.PayrollRunRef, in ReceiveResultInput, accepted bool) (*domain.PayrollResult, error) {
	outcome := domain.ResultRejected
	if accepted {
		outcome = domain.ResultAccepted
	}
	reasons := in.Evaluation.Reasons
	if reasons == nil {
		reasons = []domain.ResultReason{}
	}
	reasonsJSON, err := json.Marshal(reasons)
	if err != nil {
		return nil, err
	}
	t := in.Request.RunTotals
	declared, err := json.Marshal(domain.ResultTotalsInput{
		WorkerCount: t.WorkerCount, Gross: normalize(t.Gross), TaxablePay: normalize(t.TaxablePay),
		EmployeeDeductions: normalize(t.EmployeeDeductions), EmployerLiabilities: normalize(t.EmployerLiabilities), NetPay: normalize(t.NetPay),
	})
	if err != nil {
		return nil, err
	}
	flags := in.Request.EvidenceFlags
	if flags == nil {
		flags = []string{}
	}
	ev := in.Evaluation.Totals
	return scanResult(tx.QueryRow(ctx, `
		INSERT INTO wfp_payroll_results
			(result_id, tenant_id, run_ref_id, legal_entity_id, outcome, rejection_reasons, currency, source_run_status,
			 source_approval_ref, calculation_ref, input_batch_id, evidence_flags, worker_count, total_gross, total_taxable_pay,
			 total_employee_deductions, total_employer_liabilities, total_net_pay, declared_totals, result_fingerprint,
			 received_by, correlation_id)
		VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6::jsonb, $7, $8, $9, $10, $11::uuid, $12, $13, $14::numeric, $15::numeric,
		        $16::numeric, $17::numeric, $18::numeric, $19::jsonb, $20, $21, $22)
		RETURNING `+resultCols,
		uuid.NewString(), tenantID, ref.RunRefID, in.Request.LegalEntityID, outcome, string(reasonsJSON), in.Request.Currency,
		in.Request.SourceRunStatus, nilIfBlank(in.Request.SourceApprovalRef), nilIfBlank(in.Request.CalculationRef),
		in.Request.InputBatchID, flags, ev.WorkerCount, ev.Gross, ev.TaxablePay, ev.EmployeeDeductions, ev.EmployerLiabilities,
		ev.NetPay, string(declared), in.Evaluation.Fingerprint, in.ReceivedBy, in.CorrelationID))
}

func normalize(s string) string {
	r, err := domain.ParseDecimal(s)
	if err != nil {
		return s
	}
	return domain.FormatDecimal(r)
}

func nilIfBlank(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func insertResultDetailTx(ctx context.Context, tx pgx.Tx, tenantID, resultID string, workers []domain.NormalizedWorker) error {
	for _, w := range workers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO wfp_payroll_result_workers
				(tenant_id, result_id, worker_id, gross, taxable_pay, employee_deductions, employer_liabilities, net_pay, currency)
			VALUES ($1, $2::uuid, $3::uuid, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $8::numeric, $9)`,
			tenantID, resultID, w.WorkerID, w.Gross, w.TaxablePay, w.EmployeeDeductions, w.EmployerLiabilities, w.NetPay, w.Currency); err != nil {
			return fmt.Errorf("insert worker %s: %w", w.WorkerID, err)
		}
		for i, c := range w.Components {
			if _, err := tx.Exec(ctx, `
				INSERT INTO wfp_payroll_result_components (component_id, tenant_id, result_id, worker_id, sequence, code, kind, amount, authority_ref)
				VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6, $7, $8::numeric, $9)`,
				uuid.NewString(), tenantID, resultID, w.WorkerID, i+1, c.Code, c.Kind, c.Amount, c.AuthorityRef); err != nil {
				return fmt.Errorf("insert component %s/%s: %w", w.WorkerID, c.Code, err)
			}
		}
	}
	return nil
}

// ── queries ──────────────────────────────────────────────────────────────────

func (s *PgStore) GetPayrollResultByRef(ctx context.Context, runRefID string) (*domain.PayrollResult, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var r *domain.PayrollResult
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		r, e = scanResult(tx.QueryRow(ctx, `SELECT `+resultCols+` FROM wfp_payroll_results
			WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, runRefID))
		return e
	})
	return r, err
}

func (s *PgStore) ListResultWorkers(ctx context.Context, runRefID string) ([]domain.PayrollResultWorker, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.PayrollResultWorker{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT w.worker_id::text, w.gross::text, w.taxable_pay::text, w.employee_deductions::text,
			       w.employer_liabilities::text, w.net_pay::text, w.currency
			FROM wfp_payroll_result_workers w JOIN wfp_payroll_results r ON r.result_id = w.result_id AND r.tenant_id = w.tenant_id
			WHERE w.tenant_id = $1 AND r.run_ref_id = $2::uuid ORDER BY w.worker_id`, tenantID, runRefID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w domain.PayrollResultWorker
			if err := rows.Scan(&w.WorkerID, &w.Gross, &w.TaxablePay, &w.EmployeeDeductions, &w.EmployerLiabilities, &w.NetPay, &w.Currency); err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ListResultComponents(ctx context.Context, runRefID, workerID string) ([]domain.PayrollResultComponent, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.PayrollResultComponent{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.component_id::text, c.worker_id::text, c.sequence, c.code, c.kind, c.amount::text, c.authority_ref
			FROM wfp_payroll_result_components c JOIN wfp_payroll_results r ON r.result_id = c.result_id AND r.tenant_id = c.tenant_id
			WHERE c.tenant_id = $1 AND r.run_ref_id = $2::uuid AND ($3 = '' OR c.worker_id::text = $3)
			ORDER BY c.worker_id, c.sequence`, tenantID, runRefID, workerID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.PayrollResultComponent
			if err := rows.Scan(&c.ComponentID, &c.WorkerID, &c.Sequence, &c.Code, &c.Kind, &c.Amount, &c.AuthorityRef); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// CorrectionChains returns the chain links in which the run version is the original
// (it was superseded) and the one in which it is the superseding version.
func (s *PgStore) CorrectionChains(ctx context.Context, runRefID string) (asOriginal, asSuperseding []domain.PayrollCorrectionChain, err error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, nil, err
	}
	asOriginal, asSuperseding = []domain.PayrollCorrectionChain{}, []domain.PayrollCorrectionChain{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT chain_id::text, original_run_ref_id::text, superseding_run_ref_id::text, original_result_id::text,
			       superseding_result_id::text, reason, created_by, created_at
			FROM wfp_payroll_correction_chains
			WHERE tenant_id = $1 AND (original_run_ref_id = $2::uuid OR superseding_run_ref_id = $2::uuid)
			ORDER BY created_at`, tenantID, runRefID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.PayrollCorrectionChain
			if err := rows.Scan(&c.ChainID, &c.OriginalRunRefID, &c.SupersedingRunRefID, &c.OriginalResultID,
				&c.SupersedingResultID, &c.Reason, &c.CreatedBy, &c.CreatedAt); err != nil {
				return err
			}
			if c.OriginalRunRefID == runRefID {
				asOriginal = append(asOriginal, c)
			} else {
				asSuperseding = append(asSuperseding, c)
			}
		}
		return rows.Err()
	})
	return asOriginal, asSuperseding, err
}

// LatestRunRefVersion returns the highest registered version of a source run (0 if none).
func (s *PgStore) LatestRunRefVersion(ctx context.Context, legalEntityID, sourceSystemID, sourceRunID string) (int, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return 0, err
	}
	var v int
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(max(source_run_version), 0) FROM wfp_payroll_run_refs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND source_system_id = $3 AND source_run_id = $4`,
			tenantID, legalEntityID, sourceSystemID, sourceRunID).Scan(&v)
	})
	return v, err
}

// RecomputeResultTotals re-derives every total of an ACCEPTED result from its stored
// worker and component rows and reports each difference from what is recorded. An
// empty list means the stored result still reconciles exactly.
func (s *PgStore) RecomputeResultTotals(ctx context.Context, runRefID string) ([]domain.ResultReason, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.ResultReason{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var resultID, outcome string
		var gross, taxable, ded, emp, net string
		var workers int
		var declared []byte
		err := tx.QueryRow(ctx, `SELECT result_id::text, outcome, worker_count, total_gross::text, total_taxable_pay::text,
				total_employee_deductions::text, total_employer_liabilities::text, total_net_pay::text, declared_totals
			FROM wfp_payroll_results WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, runRefID).
			Scan(&resultID, &outcome, &workers, &gross, &taxable, &ded, &emp, &net, &declared)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrResultNotFound
		}
		if err != nil {
			return err
		}
		if outcome != domain.ResultAccepted {
			return nil // a rejected result has no detail to reconcile
		}
		add := func(code, worker, detail string) {
			out = append(out, domain.ResultReason{Code: code, WorkerID: worker, Detail: detail})
		}

		var sg, st, sd, se, sn string
		var n int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(gross),0)::text, COALESCE(sum(taxable_pay),0)::text, COALESCE(sum(employee_deductions),0)::text,
				COALESCE(sum(employer_liabilities),0)::text, COALESCE(sum(net_pay),0)::text, count(*)::int
			FROM wfp_payroll_result_workers WHERE tenant_id = $1 AND result_id = $2::uuid`, tenantID, resultID).Scan(&sg, &st, &sd, &se, &sn, &n); err != nil {
			return err
		}
		for _, c := range []struct{ name, stored, sum string }{
			{"gross", gross, sg}, {"taxable_pay", taxable, st}, {"employee_deductions", ded, sd}, {"employer_liabilities", emp, se}, {"net_pay", net, sn},
		} {
			if c.stored != c.sum {
				add(domain.RsRunTotalMismatch, "", fmt.Sprintf("%s recorded %s, workers sum %s", c.name, c.stored, c.sum))
			}
		}
		if n != workers {
			add(domain.RsRunTotalMismatch, "", fmt.Sprintf("worker_count recorded %d, rows %d", workers, n))
		}

		rows, err := tx.Query(ctx, `
			SELECT w.worker_id::text, w.gross::text, w.employee_deductions::text, w.employer_liabilities::text,
			       COALESCE(sum(c.amount) FILTER (WHERE c.kind = 'EARNING'), 0)::text,
			       COALESCE(sum(c.amount) FILTER (WHERE c.kind LIKE 'EMPLOYEE_%'), 0)::text,
			       COALESCE(sum(c.amount) FILTER (WHERE c.kind LIKE 'EMPLOYER_%'), 0)::text
			FROM wfp_payroll_result_workers w
			LEFT JOIN wfp_payroll_result_components c ON c.result_id = w.result_id AND c.worker_id = w.worker_id
			WHERE w.tenant_id = $1 AND w.result_id = $2::uuid
			GROUP BY w.worker_id, w.gross, w.employee_deductions, w.employer_liabilities ORDER BY w.worker_id`, tenantID, resultID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, g, d, e, ce, cd, cr string
			if err := rows.Scan(&id, &g, &d, &e, &ce, &cd, &cr); err != nil {
				return err
			}
			if ce != g {
				add(domain.RsGrossNotEarnings, id, fmt.Sprintf("earnings %s, gross %s", ce, g))
			}
			if cd != d {
				add(domain.RsDeductionsMismatch, id, fmt.Sprintf("components %s, recorded %s", cd, d))
			}
			if cr != e {
				add(domain.RsEmployerMismatch, id, fmt.Sprintf("components %s, recorded %s", cr, e))
			}
		}
		return rows.Err()
	})
	return out, err
}

// RunRefVersionForPayrollRun returns the source_run_version under which a given
// payroll run was already registered for a source run, so promoting the same
// correction run twice is a replay, not a second version.
func (s *PgStore) RunRefVersionForPayrollRun(ctx context.Context, legalEntityID, sourceSystemID, sourceRunID, payrollRunID string) (int, bool, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return 0, false, err
	}
	var v int
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT source_run_version FROM wfp_payroll_run_refs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND source_system_id = $3 AND source_run_id = $4
			  AND payroll_run_id = $5::uuid ORDER BY source_run_version DESC LIMIT 1`,
			tenantID, legalEntityID, sourceSystemID, sourceRunID, payrollRunID).Scan(&v)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return v, err == nil, err
}
