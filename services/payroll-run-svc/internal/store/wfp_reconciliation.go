package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// ReconChange is what a committed Wave 8 command changed; the builder turns it into
// outbox events inside the same transaction.
type ReconChange struct {
	Kind        string
	Run         *domain.ReconRun
	Exception   *domain.ReconException
	Instruction *domain.PaymentInstruction
	Observation *domain.BankingObservation
}

type ReconEventBuilder func(ReconChange) ([]domain.OutboxEvent, error)

func enqueueRecon(ctx context.Context, tx pgx.Tx, mk ReconEventBuilder, ch ReconChange) error {
	if mk == nil {
		return nil
	}
	evs, err := mk(ch)
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
	return nil
}

// ── scanning ─────────────────────────────────────────────────────────────────

const reconRunCols = `recon_run_id::text, tenant_id, legal_entity_id, run_ref_id::text, result_id::text, outcome, population, population_hash,
	assertions, exception_count, executed_by, executed_at, reperformed_from::text, certified_by, certified_at, correlation_id`

func scanReconRun(row pgx.Row) (*domain.ReconRun, error) {
	var r domain.ReconRun
	var pop, as []byte
	err := row.Scan(&r.ReconRunID, &r.TenantID, &r.LegalEntityID, &r.RunRefID, &r.ResultID, &r.Outcome, &pop, &r.PopulationHash,
		&as, &r.ExceptionCount, &r.ExecutedBy, &r.ExecutedAt, &r.ReperformedFrom, &r.CertifiedBy, &r.CertifiedAt, &r.CorrelationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrReconRunNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Population = pop
	if err := json.Unmarshal(as, &r.Assertions); err != nil {
		return nil, fmt.Errorf("decode assertions: %w", err)
	}
	return &r, nil
}

const reconExcCols = `exception_id::text, tenant_id, legal_entity_id, run_ref_id::text, result_id::text, recon_run_id::text, code, subject_key,
	severity, amount::text, detail, state, owner, resolution, resolved_at, waived_by, waiver_reason, waiver_evidence_ref, raised_by,
	created_at, updated_at`

func scanReconException(row pgx.Row) (*domain.ReconException, error) {
	var e domain.ReconException
	err := row.Scan(&e.ExceptionID, &e.TenantID, &e.LegalEntityID, &e.RunRefID, &e.ResultID, &e.ReconRunID, &e.Code, &e.SubjectKey,
		&e.Severity, &e.Amount, &e.Detail, &e.State, &e.Owner, &e.Resolution, &e.ResolvedAt, &e.WaivedBy, &e.WaiverReason, &e.WaiverEvidenceRef,
		&e.RaisedBy, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrReconExceptionNotFound
	}
	return &e, err
}

func queryReconExceptions(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]domain.ReconException, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ReconException{}
	for rows.Next() {
		e, err := scanReconException(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func mapReconGuard(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.ConstraintName {
	case "wfp_rr_sod", "wfp_re_waiver":
		return domain.ErrReconSoD
	case "wfp_rr_certify_matched":
		return domain.ErrReconNotMatched
	}
	if pgErr.Code == "23000" {
		switch {
		case strings.Contains(pgErr.Message, "Banking reported the payment RETURNED"):
			return domain.ErrInstructionNotReturned
		case strings.Contains(pgErr.Message, "final"), strings.Contains(pgErr.Message, "terminal"):
			return domain.ErrReconExceptionClosed
		}
	}
	return err
}

// ── gathering the population ─────────────────────────────────────────────────

type acceptedResult struct {
	Ref *domain.PayrollRunRef
	Res *domain.PayrollResult
}

// loadAcceptedTx loads a run reference and its result and requires both ACCEPTED.
func loadAcceptedTx(ctx context.Context, tx pgx.Tx, tenantID, runRefID string) (*acceptedResult, error) {
	ref, err := scanRunRef(tx.QueryRow(ctx, `SELECT `+runRefCols+` FROM wfp_payroll_run_refs
		WHERE tenant_id = $1 AND run_ref_id = $2::uuid FOR SHARE`, tenantID, runRefID))
	if err != nil {
		return nil, err
	}
	if ref.AcceptanceState != domain.AcceptanceAccepted {
		return nil, domain.ErrReconResultNotAccepted
	}
	res, err := scanResult(tx.QueryRow(ctx, `SELECT `+resultCols+` FROM wfp_payroll_results
		WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, runRefID))
	if err != nil {
		return nil, err
	}
	if res.Outcome != domain.ResultAccepted {
		return nil, domain.ErrReconResultNotAccepted
	}
	return &acceptedResult{Ref: ref, Res: res}, nil
}

// gatherRecon reads everything one reconciliation compares and runs the four assertions.
// Liabilities and instructions of the result are locked FOR SHARE while it reads, so a
// concurrent command cannot change them mid-comparison.
func gatherRecon(ctx context.Context, tx pgx.Tx, tenantID string, a *acceptedResult) (*domain.ReconPopulation, []domain.Assertion, error) {
	res := a.Res
	pop := &domain.ReconPopulation{
		ResultID: res.ResultID, ResultFingerprint: res.ResultFingerprint, TotalGross: res.TotalGross, TotalNetPay: res.TotalNetPay,
		WorkerNet: map[string]string{}, Liabilities: []domain.LiabilityFact{}, Instructions: []domain.InstructionFact{},
	}

	// What the stored result implies should have been posted.
	in := domain.ResultPostingInput{ByKind: map[string]*big.Rat{}}
	rows, err := tx.Query(ctx, `SELECT kind, COALESCE(sum(amount), 0)::text FROM wfp_payroll_result_components
		WHERE tenant_id = $1 AND result_id = $2::uuid GROUP BY kind`, tenantID, res.ResultID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var kind, sum string
		if err := rows.Scan(&kind, &sum); err != nil {
			rows.Close()
			return nil, nil, err
		}
		r, perr := domain.ParseDecimal(sum)
		if perr != nil {
			rows.Close()
			return nil, nil, perr
		}
		in.ByKind[kind] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if in.Gross, err = domain.ParseDecimal(res.TotalGross); err != nil {
		return nil, nil, err
	}
	if in.Net, err = domain.ParseDecimal(res.TotalNetPay); err != nil {
		return nil, nil, err
	}
	expected, err := domain.BuildPostingLines(in)
	if err != nil {
		return nil, nil, err
	}

	// The POSTED payroll journal subject, if any.
	var posted *domain.AccountingBridge
	b, err := scanBridge(tx.QueryRow(ctx, `SELECT `+bridgeCols+` FROM wfp_payroll_accounting_bridges
		WHERE tenant_id = $1 AND result_id = $2::uuid AND posting_purpose = 'PAYROLL_RESULT' AND state = 'POSTED'`, tenantID, res.ResultID))
	switch {
	case errors.Is(err, domain.ErrBridgeNotFound):
	case err != nil:
		return nil, nil, err
	default:
		posted = b
		pop.Posting = &domain.PostingFact{BridgeID: b.BridgeID, State: b.State, Lines: b.Lines}
	}

	// Worker net pay.
	workerNet := map[string]*big.Rat{}
	wrows, err := tx.Query(ctx, `SELECT worker_id::text, net_pay::text FROM wfp_payroll_result_workers
		WHERE tenant_id = $1 AND result_id = $2::uuid ORDER BY worker_id`, tenantID, res.ResultID)
	if err != nil {
		return nil, nil, err
	}
	for wrows.Next() {
		var w, n string
		if err := wrows.Scan(&w, &n); err != nil {
			wrows.Close()
			return nil, nil, err
		}
		r, perr := domain.ParseDecimal(n)
		if perr != nil {
			wrows.Close()
			return nil, nil, perr
		}
		workerNet[w] = r
		pop.WorkerNet[w] = n
	}
	wrows.Close()
	if err := wrows.Err(); err != nil {
		return nil, nil, err
	}

	// Recognized liabilities (not superseded), summed per payable mapping key.
	libs, err := queryLiabilities(ctx, tx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
		WHERE tenant_id = $1 AND result_id = $2::uuid AND state <> 'SUPERSEDED' ORDER BY liability_id FOR SHARE`, tenantID, res.ResultID)
	if err != nil {
		return nil, nil, err
	}
	recognized := map[string]*big.Rat{}
	for _, l := range libs {
		pop.Liabilities = append(pop.Liabilities, domain.LiabilityFact{
			LiabilityID: l.LiabilityID, ClassCode: l.ClassCode, State: l.State, Amount: l.Amount, KindAmounts: l.KindAmounts, Version: l.Version,
		})
		for kind, amt := range l.KindAmounts {
			v, perr := domain.ParseDecimal(amt)
			if perr != nil {
				return nil, nil, perr
			}
			key := domain.PayableKeyOf(kind)
			if recognized[key] == nil {
				recognized[key] = new(big.Rat)
			}
			recognized[key].Add(recognized[key], v)
		}
	}

	// Payment instructions and the newest Banking observation of each.
	irows, err := tx.Query(ctx, `SELECT `+instrCols+` FROM wfp_payment_instructions i
		WHERE i.tenant_id = $1 AND i.result_id = $2::uuid ORDER BY i.instruction_id FOR SHARE OF i`, tenantID, res.ResultID)
	if err != nil {
		return nil, nil, err
	}
	var ins []domain.PaymentInstruction
	for irows.Next() {
		p, serr := scanInstruction(irows)
		if serr != nil {
			irows.Close()
			return nil, nil, serr
		}
		ins = append(ins, *p)
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		return nil, nil, err
	}
	latest := map[string]string{}
	orows, err := tx.Query(ctx, `SELECT DISTINCT ON (o.instruction_id) o.instruction_id::text, o.outcome
		FROM wfp_instruction_banking_observations o JOIN wfp_payment_instructions i ON i.instruction_id = o.instruction_id
		WHERE o.tenant_id = $1 AND i.result_id = $2::uuid ORDER BY o.instruction_id, o.observed_at DESC, o.observation_id DESC`, tenantID, res.ResultID)
	if err != nil {
		return nil, nil, err
	}
	for orows.Next() {
		var id, outcome string
		if err := orows.Scan(&id, &outcome); err != nil {
			orows.Close()
			return nil, nil, err
		}
		latest[id] = outcome
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return nil, nil, err
	}
	for _, p := range ins {
		pop.Instructions = append(pop.Instructions, domain.InstructionFact{
			InstructionID: p.InstructionID, Type: p.InstructionType, WorkerID: p.WorkerID, LiabilityID: p.LiabilityID,
			State: p.State, Amount: p.Amount, Version: p.Version, LatestOutcome: latest[p.InstructionID],
		})
	}

	assertions := []domain.Assertion{
		domain.ReconcileResultToGL(expected, posted),
		domain.ReconcileResultToLiabilities(expected, recognized),
		domain.ReconcileNetPay(workerNet, pop.Instructions),
		domain.ReconcileBankingOutcomes(pop.Instructions),
	}
	return pop, assertions, nil
}

// ── run ──────────────────────────────────────────────────────────────────────

type ReconOutcome struct {
	Run      *domain.ReconRun        `json:"run"`
	Created  []domain.ReconException `json:"exceptions_raised"`
	Resolved []domain.ReconException `json:"exceptions_resolved"`
	Open     []domain.ReconException `json:"open_exceptions"`
}

// ExecuteReconciliation compares the accepted result's frozen population across WFP-04,
// WFP-05, WFP-06 and WFP-07/Banking and records the run. Exceptions are synchronized:
// a difference that is still there stays one open exception (it is never duplicated);
// a new one is raised; one whose cause has cleared is RESOLVED as CAUSE_CLEARED. Nothing
// is dismissed by hand here.
func (s *PgStore) ExecuteReconciliation(ctx context.Context, runRefID, actor, correlationID string, reperform bool, mk ReconEventBuilder) (*ReconOutcome, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := &ReconOutcome{Created: []domain.ReconException{}, Resolved: []domain.ReconException{}, Open: []domain.ReconException{}}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// One reconciliation of a result at a time. Without it two concurrent runs (two operators, two
		// replicas) can both decide to raise the same exception and one fails on the unique index. It is
		// FOR NO KEY UPDATE, taken BEFORE the shared read in loadAcceptedTx (a share-to-update upgrade
		// between two runs would deadlock), and it does not block foreign-key checks, so instruction and
		// journal inserts that reference this run reference are never held up by it.
		var locked int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM wfp_payroll_run_refs WHERE tenant_id = $1 AND run_ref_id = $2::uuid FOR NO KEY UPDATE`,
			tenantID, runRefID).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRunRefNotFound
			}
			return err
		}
		acc, err := loadAcceptedTx(ctx, tx, tenantID, runRefID)
		if err != nil {
			return err
		}
		pop, assertions, err := gatherRecon(ctx, tx, tenantID, acc)
		if err != nil {
			return err
		}
		hash, popJSON, err := pop.Hash()
		if err != nil {
			return err
		}
		drafts := domain.CollectExceptions(assertions)
		asJSON, err := json.Marshal(assertions)
		if err != nil {
			return err
		}
		var prior *string
		if reperform {
			var id string
			if e := tx.QueryRow(ctx, `SELECT recon_run_id::text FROM wfp_reconciliation_runs WHERE tenant_id = $1 AND result_id = $2::uuid
				ORDER BY executed_at DESC, recon_run_id DESC LIMIT 1`, tenantID, acc.Res.ResultID).Scan(&id); e == nil {
				prior = &id
			} else if !errors.Is(e, pgx.ErrNoRows) {
				return e
			}
		}
		run, err := scanReconRun(tx.QueryRow(ctx, `INSERT INTO wfp_reconciliation_runs
			(recon_run_id, tenant_id, legal_entity_id, run_ref_id, result_id, outcome, population, population_hash, assertions,
			 exception_count, executed_by, reperformed_from, correlation_id)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6, $7::jsonb, $8, $9::jsonb, $10, $11, $12::uuid, $13)
			RETURNING `+reconRunCols,
			uuid.NewString(), tenantID, acc.Ref.LegalEntityID, acc.Ref.RunRefID, acc.Res.ResultID, domain.RunOutcome(assertions),
			string(popJSON), hash, string(asJSON), len(drafts), actor, prior, correlationID))
		if err != nil {
			return err
		}
		out.Run = run

		open, err := queryReconExceptions(ctx, tx, `SELECT `+reconExcCols+` FROM wfp_reconciliation_exceptions
			WHERE tenant_id = $1 AND result_id = $2::uuid AND state IN ('OPEN', 'ASSIGNED') ORDER BY created_at FOR UPDATE`, tenantID, acc.Res.ResultID)
		if err != nil {
			return err
		}
		existing := map[string]domain.ReconException{}
		for _, e := range open {
			existing[e.Code+"\x00"+e.SubjectKey] = e
		}
		present := map[string]bool{}
		for _, d := range drafts {
			k := d.Code + "\x00" + d.SubjectKey
			present[k] = true
			if cur, ok := existing[k]; ok {
				out.Open = append(out.Open, cur)
				continue
			}
			// A difference someone with authority already waived, with evidence, is accepted for the close:
			// it is not raised again. The run still reports BREAK and cannot be certified.
			var waived int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM wfp_reconciliation_exceptions WHERE tenant_id = $1 AND result_id = $2::uuid
				AND code = $3 AND subject_key = $4 AND state = 'WAIVED'`, tenantID, acc.Res.ResultID, d.Code, d.SubjectKey).Scan(&waived); err != nil {
				return err
			}
			if waived > 0 {
				continue
			}
			e, err := scanReconException(tx.QueryRow(ctx, `INSERT INTO wfp_reconciliation_exceptions
				(exception_id, tenant_id, legal_entity_id, run_ref_id, result_id, recon_run_id, code, subject_key, severity, amount, detail, raised_by)
				VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6::uuid, $7, $8, $9, $10::numeric, $11, $12)
				RETURNING `+reconExcCols,
				uuid.NewString(), tenantID, acc.Ref.LegalEntityID, acc.Ref.RunRefID, acc.Res.ResultID, run.ReconRunID, d.Code, d.SubjectKey,
				d.Severity, d.Amount, d.Detail, actor))
			if err != nil {
				return err
			}
			out.Created = append(out.Created, *e)
			out.Open = append(out.Open, *e)
			if err := enqueueRecon(ctx, tx, mk, ReconChange{Kind: "EXCEPTION_CREATED", Exception: e}); err != nil {
				return err
			}
		}
		for k, cur := range existing {
			if present[k] {
				continue
			}
			e, err := scanReconException(tx.QueryRow(ctx, `UPDATE wfp_reconciliation_exceptions
				SET state = 'RESOLVED', resolution = $3, resolved_at = now(), updated_at = now()
				WHERE tenant_id = $1 AND exception_id = $2::uuid RETURNING `+reconExcCols, tenantID, cur.ExceptionID, domain.ResolutionCauseCleared))
			if err != nil {
				return mapReconGuard(err)
			}
			out.Resolved = append(out.Resolved, *e)
			if err := enqueueRecon(ctx, tx, mk, ReconChange{Kind: "EXCEPTION_RESOLVED", Exception: e}); err != nil {
				return err
			}
		}
		return enqueueRecon(ctx, tx, mk, ReconChange{Kind: "RUN_COMPLETED", Run: run})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CertifyReconRun certifies a MATCHED run. It must be the newest run of its result, by a
// principal other than the one who executed it, and the population must still be exactly
// what the run compared (any change since means: reperform).
func (s *PgStore) CertifyReconRun(ctx context.Context, id, actor string, mk ReconEventBuilder) (*domain.ReconRun, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.ReconRun
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanReconRun(tx.QueryRow(ctx, `SELECT `+reconRunCols+` FROM wfp_reconciliation_runs
			WHERE tenant_id = $1 AND recon_run_id = $2::uuid FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.CertifiedBy != nil {
			return domain.ErrReconAlreadyCertified
		}
		if cur.Outcome != domain.ReconOutcomeMatched {
			return domain.ErrReconNotMatched
		}
		if cur.ExecutedBy == actor {
			return domain.ErrReconSoD
		}
		var newest string
		if err := tx.QueryRow(ctx, `SELECT recon_run_id::text FROM wfp_reconciliation_runs WHERE tenant_id = $1 AND result_id = $2::uuid
			ORDER BY executed_at DESC, recon_run_id DESC LIMIT 1`, tenantID, cur.ResultID).Scan(&newest); err != nil {
			return err
		}
		if newest != cur.ReconRunID {
			return domain.ErrReconRunStale
		}
		acc, err := loadAcceptedTx(ctx, tx, tenantID, cur.RunRefID)
		if err != nil {
			return err
		}
		pop, _, err := gatherRecon(ctx, tx, tenantID, acc)
		if err != nil {
			return err
		}
		hash, _, err := pop.Hash()
		if err != nil {
			return err
		}
		if hash != cur.PopulationHash {
			return domain.ErrReconRunStale
		}
		out, err = scanReconRun(tx.QueryRow(ctx, `UPDATE wfp_reconciliation_runs SET certified_by = $3, certified_at = now()
			WHERE tenant_id = $1 AND recon_run_id = $2::uuid RETURNING `+reconRunCols, tenantID, id, actor))
		if err != nil {
			return mapReconGuard(err)
		}
		return enqueueRecon(ctx, tx, mk, ReconChange{Kind: "RUN_CERTIFIED", Run: out})
	})
	return out, err
}

func (s *PgStore) GetReconRun(ctx context.Context, id string) (*domain.ReconRun, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.ReconRun
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, e = scanReconRun(tx.QueryRow(ctx, `SELECT `+reconRunCols+` FROM wfp_reconciliation_runs WHERE tenant_id = $1 AND recon_run_id = $2::uuid`, tenantID, id))
		return e
	})
	return out, err
}

func (s *PgStore) ListReconRuns(ctx context.Context, runRefID string) ([]domain.ReconRun, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.ReconRun{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT `+reconRunCols+` FROM wfp_reconciliation_runs WHERE tenant_id = $1 AND run_ref_id = $2::uuid
			ORDER BY executed_at DESC, recon_run_id DESC LIMIT 100`, tenantID, runRefID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			r, e := scanReconRun(rows)
			if e != nil {
				return e
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// ── exceptions ───────────────────────────────────────────────────────────────

func (s *PgStore) ListReconExceptions(ctx context.Context, runRefID, legalEntityID, state string) ([]domain.ReconException, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.ReconException
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q := `SELECT ` + reconExcCols + ` FROM wfp_reconciliation_exceptions WHERE tenant_id = $1`
		args := []any{tenantID}
		if runRefID != "" {
			args = append(args, runRefID)
			q += fmt.Sprintf(" AND run_ref_id = $%d::uuid", len(args))
		}
		if legalEntityID != "" {
			args = append(args, legalEntityID)
			q += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if state != "" {
			args = append(args, state)
			q += fmt.Sprintf(" AND state = $%d", len(args))
		}
		var e error
		out, e = queryReconExceptions(ctx, tx, q+` ORDER BY created_at DESC LIMIT 500`, args...)
		return e
	})
	return out, err
}

func (s *PgStore) GetReconException(ctx context.Context, id string) (*domain.ReconException, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.ReconException
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, e = scanReconException(tx.QueryRow(ctx, `SELECT `+reconExcCols+` FROM wfp_reconciliation_exceptions
			WHERE tenant_id = $1 AND exception_id = $2::uuid`, tenantID, id))
		return e
	})
	return out, err
}

// AssignReconException gives an open exception an owner (spec: explicit exception ownership).
func (s *PgStore) AssignReconException(ctx context.Context, id, owner string) (*domain.ReconException, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.ReconException
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, e = scanReconException(tx.QueryRow(ctx, `UPDATE wfp_reconciliation_exceptions SET state = 'ASSIGNED', owner = $3, updated_at = now()
			WHERE tenant_id = $1 AND exception_id = $2::uuid AND state IN ('OPEN', 'ASSIGNED') RETURNING `+reconExcCols, tenantID, id, owner))
		if errors.Is(e, domain.ErrReconExceptionNotFound) {
			return domain.ErrReconExceptionClosed
		}
		return e
	})
	return out, err
}

// WaiveReconException closes an exception with evidence. It is never done by the
// principal who raised it (database CHECK as well as code).
func (s *PgStore) WaiveReconException(ctx context.Context, id, actor, reason, evidenceRef string, mk ReconEventBuilder) (*domain.ReconException, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.ReconException
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanReconException(tx.QueryRow(ctx, `SELECT `+reconExcCols+` FROM wfp_reconciliation_exceptions
			WHERE tenant_id = $1 AND exception_id = $2::uuid FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.State != domain.ReconStateOpen && cur.State != domain.ReconStateAssigned {
			return domain.ErrReconExceptionClosed
		}
		if cur.RaisedBy == actor {
			return domain.ErrReconSoD
		}
		out, err = scanReconException(tx.QueryRow(ctx, `UPDATE wfp_reconciliation_exceptions
			SET state = 'WAIVED', resolution = 'WAIVED', resolved_at = now(), waived_by = $3, waiver_reason = $4, waiver_evidence_ref = $5, updated_at = now()
			WHERE tenant_id = $1 AND exception_id = $2::uuid RETURNING `+reconExcCols, tenantID, id, actor, reason, evidenceRef))
		if err != nil {
			return mapReconGuard(err)
		}
		return enqueueRecon(ctx, tx, mk, ReconChange{Kind: "EXCEPTION_WAIVED", Exception: out})
	})
	return out, err
}

// ResolveExceptionsForSupersededResult closes a superseded result's open exceptions: the
// subject they were about no longer stands, and the replacement result is reconciled on its own.
func (s *PgStore) ResolveExceptionsForSupersededResult(ctx context.Context, runRefID string, mk ReconEventBuilder) ([]domain.ReconException, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.ReconException{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT acceptance_state FROM wfp_payroll_run_refs WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, runRefID).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRunRefNotFound
			}
			return err
		}
		if state != domain.AcceptanceSuperseded {
			return domain.ErrResultNotSuperseded
		}
		rows, err := queryReconExceptions(ctx, tx, `UPDATE wfp_reconciliation_exceptions
			SET state = 'RESOLVED', resolution = $3, resolved_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND run_ref_id = $2::uuid AND state IN ('OPEN', 'ASSIGNED') RETURNING `+reconExcCols, tenantID, runRefID, domain.ResolutionResultSuperseded)
		if err != nil {
			return err
		}
		for i := range rows {
			out = append(out, rows[i])
			if err := enqueueRecon(ctx, tx, mk, ReconChange{Kind: "EXCEPTION_RESOLVED", Exception: &rows[i]}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ── Banking observations ─────────────────────────────────────────────────────

func (s *PgStore) ListBankingObservations(ctx context.Context, instructionID string) ([]domain.BankingObservation, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.BankingObservation{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT observation_id::text, tenant_id, legal_entity_id, instruction_id::text, source, raw_status, outcome,
			attempt_id, payment_id, detail, observed_at FROM wfp_instruction_banking_observations
			WHERE tenant_id = $1 AND instruction_id = $2::uuid ORDER BY observed_at DESC, observation_id DESC LIMIT 200`, tenantID, instructionID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var o domain.BankingObservation
			if e := rows.Scan(&o.ObservationID, &o.TenantID, &o.LegalEntityID, &o.InstructionID, &o.Source, &o.RawStatus, &o.Outcome,
				&o.AttemptID, &o.PaymentID, &o.Detail, &o.ObservedAt); e != nil {
				return e
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, err
}

// ObserveFunc asks Banking about one instruction. A nil observation means "nothing to
// record" (Banking unreachable, for example): the instruction is not changed and is looked
// at again later. It must never invent an outcome.
type ObserveFunc func(ctx context.Context, in domain.PaymentInstruction) *domain.BankingObservation

func insertObservationTx(ctx context.Context, tx pgx.Tx, in *domain.PaymentInstruction, o *domain.BankingObservation) (*domain.BankingObservation, bool, error) {
	var lastSource, lastRaw, lastOutcome string
	err := tx.QueryRow(ctx, `SELECT source, raw_status, outcome FROM wfp_instruction_banking_observations
		WHERE instruction_id = $1::uuid ORDER BY observed_at DESC, observation_id DESC LIMIT 1`, in.InstructionID).Scan(&lastSource, &lastRaw, &lastOutcome)
	if err == nil && lastSource == o.Source && lastRaw == o.RawStatus && lastOutcome == o.Outcome {
		return nil, false, nil // nothing new; evidence is not repeated
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	var out domain.BankingObservation
	err = tx.QueryRow(ctx, `INSERT INTO wfp_instruction_banking_observations
		(observation_id, tenant_id, legal_entity_id, instruction_id, source, raw_status, outcome, attempt_id, payment_id, detail)
		VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10)
		RETURNING observation_id::text, tenant_id, legal_entity_id, instruction_id::text, source, raw_status, outcome, attempt_id, payment_id, detail, observed_at`,
		uuid.NewString(), in.TenantID, in.LegalEntityID, in.InstructionID, o.Source, o.RawStatus, o.Outcome, o.AttemptID, o.PaymentID, o.Detail).
		Scan(&out.ObservationID, &out.TenantID, &out.LegalEntityID, &out.InstructionID, &out.Source, &out.RawStatus, &out.Outcome,
			&out.AttemptID, &out.PaymentID, &out.Detail, &out.ObservedAt)
	if err != nil {
		return nil, false, err
	}
	return &out, true, nil
}

// RecordBankingObservation records one observation for an instruction on a request path
// (an operator asking for a refresh). It returns whether anything new was recorded.
func (s *PgStore) RecordBankingObservation(ctx context.Context, instructionID string, o *domain.BankingObservation, actor string, mk ReconEventBuilder) (*domain.BankingObservation, bool, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, false, err
	}
	var rec *domain.BankingObservation
	var added bool
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		in, err := lockInstructionTx(ctx, tx, tenantID, instructionID)
		if err != nil {
			return err
		}
		if in.State != domain.PaySubmitted && in.State != domain.PayHandedOff {
			return domain.ErrInstructionState
		}
		if rec, added, err = insertObservationTx(ctx, tx, in, o); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE wfp_payment_instructions SET banking_checked_at = now() WHERE tenant_id = $1 AND instruction_id = $2::uuid`, tenantID, instructionID); err != nil {
			return err
		}
		if added {
			return enqueueRecon(ctx, tx, mk, ReconChange{Kind: "BANKING_OBSERVED", Instruction: in, Observation: rec})
		}
		return nil
	})
	return rec, added, err
}

// RefreshBankingOutcomes is the background refresh: it takes instructions that reached
// Banking and whose outcome is not final (no observation yet, in flight, or unresolved),
// least recently checked first, and records what Banking says. Like the posting dispatcher
// it finds work across tenants under the relay context and then switches to each
// instruction's own tenant before reading or writing anything of it. Replicas never share
// a row (FOR UPDATE SKIP LOCKED). It changes no instruction state and reopens nothing.
func (s *PgStore) RefreshBankingOutcomes(ctx context.Context, limit int, observe ObserveFunc, mk ReconEventBuilder) (int, error) {
	recorded := 0
	err := s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+instrCols+` FROM wfp_payment_instructions i
			WHERE i.state IN ('SUBMITTED_TO_BANKING', 'HANDED_OFF')
			  AND COALESCE((SELECT o.outcome FROM wfp_instruction_banking_observations o WHERE o.instruction_id = i.instruction_id
			                ORDER BY o.observed_at DESC, o.observation_id DESC LIMIT 1), 'NONE') IN ('NONE', 'IN_FLIGHT', 'UNRESOLVED')
			ORDER BY i.banking_checked_at NULLS FIRST, i.instruction_id LIMIT $1 FOR UPDATE OF i SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("claim instructions: %w", err)
		}
		var claimed []domain.PaymentInstruction
		for rows.Next() {
			p, serr := scanInstruction(rows)
			if serr != nil {
				rows.Close()
				return serr
			}
			claimed = append(claimed, *p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range claimed {
			in := &claimed[i]
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", in.TenantID); err != nil {
				return fmt.Errorf("set tenant context: %w", err)
			}
			if o := observe(ctx, *in); o != nil {
				rec, added, err := insertObservationTx(ctx, tx, in, o)
				if err != nil {
					return err
				}
				if added {
					recorded++
					if err := enqueueRecon(ctx, tx, mk, ReconChange{Kind: "BANKING_OBSERVED", Instruction: in, Observation: rec}); err != nil {
						return err
					}
				}
			}
			// Checked now either way, so one unanswerable instruction cannot starve the rest.
			if _, err := tx.Exec(ctx, `UPDATE wfp_payment_instructions SET banking_checked_at = now() WHERE tenant_id = $1 AND instruction_id = $2::uuid`,
				in.TenantID, in.InstructionID); err != nil {
				return err
			}
		}
		return nil
	})
	return recorded, err
}

// LinkInstructionPayment records which Banking payment an instruction produced. The
// caller has already verified with payment-status-svc that the payment carries this
// instruction as its source reference and belongs to the same legal entity. Set once.
func (s *PgStore) LinkInstructionPayment(ctx context.Context, id, paymentID string) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.State != domain.PaySubmitted && cur.State != domain.PayHandedOff {
			return domain.ErrInstructionState
		}
		if cur.PaymentID != nil {
			if *cur.PaymentID == paymentID {
				out = cur
				return nil
			}
			return domain.ErrPaymentLinkMismatch
		}
		out, err = updateInstructionTx(ctx, tx, tenantID, id, `payment_id = $3`, paymentID)
		return err
	})
	return out, err
}

// MarkInstructionReturned is the explicit, authorised acknowledgement that Banking returned
// a payment. The database allows it only when the newest Banking observation says RETURNED.
// The instruction leaves the live population, so its amount can be instructed again; nothing
// else is reopened automatically. A returned payment that already settled a liability is
// refused: reversing a settlement is not modelled and must not be faked.
func (s *PgStore) MarkInstructionReturned(ctx context.Context, id string, expectedVersion int, reason string, mk ReconEventBuilder) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrInstructionVersionConflict
		}
		if cur.State != domain.PayHandedOff {
			return domain.ErrInstructionState
		}
		var settled int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM wfp_liability_settlements WHERE tenant_id = $1 AND instruction_id = $2::uuid`, tenantID, id).Scan(&settled); err != nil {
			return err
		}
		if settled > 0 {
			return domain.ErrReturnedAfterSettlement
		}
		out, err = updateInstructionTx(ctx, tx, tenantID, id, `state = 'RETURNED', returned_at = now(), return_reason = $3`, reason)
		if err != nil {
			return mapReconGuard(err)
		}
		return enqueueRecon(ctx, tx, mk, ReconChange{Kind: "INSTRUCTION_RETURNED", Instruction: out})
	})
	return out, err
}

// ── close gate (WFP-CTRL-030) ────────────────────────────────────────────────

// CloseGateFor reports whether payroll allows the period to close for the entity. It is fail-closed.
// For each accepted result it is shut while any of these holds: no reconciliation run exists; the
// latest run is stale (the population changed since it ran, which includes a changed Banking
// outcome); an open BLOCKING reconciliation exception; an open Wave 6 liability exception. A missing
// POSTED journal blocks directly only when there is no current run to say so: once a current run
// exists, it speaks through its RESULT_NOT_POSTED exception, which an evidenced waiver can close.
// Warnings (not certified, a payment not yet settled) are listed and do not shut it. A period with no
// accepted payroll has nothing to gate and says so. The period is matched on the run's payroll
// period id or the pay date's YYYY-MM.
func (s *PgStore) CloseGateFor(ctx context.Context, legalEntityID, period string) (*domain.CloseGate, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	g := &domain.CloseGate{LegalEntityID: legalEntityID, Period: period, Blockers: []domain.GateBlocker{}, Warnings: []domain.GateBlocker{}, Control: domain.CtrlCloseGate}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r.run_ref_id::text FROM wfp_payroll_run_refs r
			JOIN wfp_payroll_results res ON res.run_ref_id = r.run_ref_id AND res.outcome = 'ACCEPTED'
			WHERE r.tenant_id = $1 AND r.legal_entity_id = $2 AND r.acceptance_state = 'ACCEPTED'
			  AND (r.payroll_period_id = $3 OR to_char(r.pay_date, 'YYYY-MM') = $3) ORDER BY r.pay_date, r.run_ref_id`, tenantID, legalEntityID, period)
		if err != nil {
			return err
		}
		var refs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		g.Results = len(refs)
		for _, runRefID := range refs {
			acc, err := loadAcceptedTx(ctx, tx, tenantID, runRefID)
			if err != nil {
				return err
			}
			resultID := acc.Res.ResultID
			add := func(code, subject, detail string) {
				g.Blockers = append(g.Blockers, domain.GateBlocker{Code: code, ResultID: resultID, RunRefID: runRefID, Subject: subject, Detail: detail})
			}
			warn := func(code, subject, detail string) {
				g.Warnings = append(g.Warnings, domain.GateBlocker{Code: code, ResultID: resultID, RunRefID: runRefID, Subject: subject, Detail: detail})
			}
			pop, _, err := gatherRecon(ctx, tx, tenantID, acc)
			if err != nil {
				return err
			}
			run, err := scanReconRun(tx.QueryRow(ctx, `SELECT `+reconRunCols+` FROM wfp_reconciliation_runs WHERE tenant_id = $1 AND result_id = $2::uuid
				ORDER BY executed_at DESC, recon_run_id DESC LIMIT 1`, tenantID, resultID))
			switch {
			case errors.Is(err, domain.ErrReconRunNotFound):
				add(domain.GateNoReconRun, "result", "payroll reconciliation has not been run for this result")
				if pop.Posting == nil {
					add(domain.GateNotPosted, "result", "the accepted payroll result has no POSTED journal subject")
				}
			case err != nil:
				return err
			default:
				hash, _, herr := pop.Hash()
				if herr != nil {
					return herr
				}
				if hash != run.PopulationHash {
					add(domain.GateStaleReconciliation, run.ReconRunID, "the population changed since the last reconciliation run; reperform it")
					if pop.Posting == nil {
						add(domain.GateNotPosted, "result", "the accepted payroll result has no POSTED journal subject")
					}
				}
				if run.CertifiedBy == nil {
					warn(domain.GateNotCertified, run.ReconRunID, "the latest reconciliation run is not certified")
				}
			}
			excs, err := queryReconExceptions(ctx, tx, `SELECT `+reconExcCols+` FROM wfp_reconciliation_exceptions
				WHERE tenant_id = $1 AND result_id = $2::uuid AND state IN ('OPEN', 'ASSIGNED') ORDER BY created_at`, tenantID, resultID)
			if err != nil {
				return err
			}
			for _, e := range excs {
				if e.Severity == domain.SeverityBlocking {
					add(domain.GateOpenException, e.ExceptionID, e.Code+": "+e.Detail)
				} else {
					warn(domain.GateOpenException, e.ExceptionID, e.Code+": "+e.Detail)
				}
			}
			lrows, err := tx.Query(ctx, `SELECT exception_id::text, code, detail FROM wfp_liability_exceptions
				WHERE tenant_id = $1 AND result_id = $2::uuid AND state = 'OPEN' ORDER BY created_at`, tenantID, resultID)
			if err != nil {
				return err
			}
			for lrows.Next() {
				var id, code, detail string
				if err := lrows.Scan(&id, &code, &detail); err != nil {
					lrows.Close()
					return err
				}
				add(domain.GateOpenLiabException, id, code+": "+detail)
			}
			lrows.Close()
			if err := lrows.Err(); err != nil {
				return err
			}
			for _, in := range pop.Instructions {
				if in.State != domain.PaySubmitted && in.State != domain.PayHandedOff {
					continue
				}
				// A changed Banking outcome makes the last run stale (the population hash includes it), and the
				// re-run raises the exception that blocks; a waiver of that exception is therefore effective.
				if in.LatestOutcome != domain.OutcomeSettled {
					warn(domain.GateUnresolvedPayment, in.InstructionID, "payment with Banking is not settled (outcome "+in.LatestOutcome+")")
				}
			}
		}
		g.Open = len(g.Blockers) == 0
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// ── correction completeness (WFP-CTRL-024) ───────────────────────────────────

// CorrectionCompletenessFor reports, for a SUPERSEDED result, whether its accounting,
// liabilities and payments have all been dealt with. Anything left is listed as pending
// with a count; nothing here changes state.
func (s *PgStore) CorrectionCompletenessFor(ctx context.Context, runRefID string) (*domain.CorrectionCompleteness, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := &domain.CorrectionCompleteness{RunRefID: runRefID, Effects: []domain.CorrectionEffect{}}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var state, resultID string
		if err := tx.QueryRow(ctx, `SELECT r.acceptance_state, res.result_id::text FROM wfp_payroll_run_refs r
			JOIN wfp_payroll_results res ON res.run_ref_id = r.run_ref_id WHERE r.tenant_id = $1 AND r.run_ref_id = $2::uuid`, tenantID, runRefID).Scan(&state, &resultID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRunRefNotFound
			}
			return err
		}
		if state != domain.AcceptanceSuperseded {
			return domain.ErrResultNotSuperseded
		}
		count := func(sql string) (int, error) {
			var n int
			err := tx.QueryRow(ctx, sql, tenantID, resultID).Scan(&n)
			return n, err
		}
		// Accounting: every subject is either corrected (withdrawn/reversed) or still owes that.
		acctPending, err := count(`SELECT count(*) FROM wfp_payroll_accounting_bridges
			WHERE tenant_id = $1 AND result_id = $2::uuid AND posting_purpose = 'PAYROLL_RESULT' AND state NOT IN ('CORRECTED')`)
		if err != nil {
			return err
		}
		revPending, err := count(`SELECT count(*) FROM wfp_payroll_accounting_bridges rv
			WHERE rv.tenant_id = $1 AND rv.result_id = $2::uuid AND rv.posting_purpose = 'PAYROLL_REVERSAL' AND rv.state NOT IN ('POSTED', 'REJECTED', 'CORRECTED')`)
		if err != nil {
			return err
		}
		out.Effects = append(out.Effects, domain.CorrectionEffect{Effect: "ACCOUNTING", Pending: acctPending + revPending, Complete: acctPending+revPending == 0,
			Detail: "every journal subject of the superseded result is withdrawn (unposted) or reversed (posted), and every reversal has posted"})
		libPending, err := count(`SELECT count(*) FROM wfp_payroll_liabilities WHERE tenant_id = $1 AND result_id = $2::uuid AND state <> 'SUPERSEDED'`)
		if err != nil {
			return err
		}
		out.Effects = append(out.Effects, domain.CorrectionEffect{Effect: "LIABILITIES", Pending: libPending, Complete: libPending == 0,
			Detail: "every liability is superseded; one with settlements needs an explicit adjustment"})
		payPending, err := count(`SELECT count(*) FROM wfp_payment_instructions WHERE tenant_id = $1 AND result_id = $2::uuid
			AND state NOT IN ('CANCELLED', 'SUPERSEDED', 'RETURNED')`)
		if err != nil {
			return err
		}
		out.Effects = append(out.Effects, domain.CorrectionEffect{Effect: "PAYMENTS", Pending: payPending, Complete: payPending == 0,
			Detail: "every instruction is cancelled, superseded or returned; one already with Banking is resolved with Banking"})
		out.Complete = true
		for _, e := range out.Effects {
			if !e.Complete {
				out.Complete = false
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
