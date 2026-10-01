package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/events"
	"zoiko.io/financial-control-svc/internal/outbox"
)

// ─── Command idempotency + start ─────────────────────────────────────────────

// BeginExecution claims the right to execute a run exactly once and moves it
// SCHEDULED -> PREPARING in the same transaction. A repeat of the same
// (tenant, Idempotency-Key, command, run) returns replay=true with the run as it
// now stands and does NOT start a second execution; the same key used for a
// different run or command is a conflict.
func (s *PgStore) BeginExecution(ctx context.Context, tenantID, runID, idempotencyKey, actor, correlationID string, expectedVersion int) (*domain.ControlRun, bool, error) {
	var run *domain.ControlRun
	replay := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var existingRun, existingCmd string
		err := tx.QueryRow(ctx, `SELECT run_id::text, command FROM command_idempotency
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, idempotencyKey).Scan(&existingRun, &existingCmd)
		switch {
		case err == nil:
			if existingRun != runID || existingCmd != "EXECUTE" {
				return fmt.Errorf("%w: idempotency key already used for a different command", domain.ErrConflict)
			}
			replay = true
			var r domain.ControlRun
			if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
				WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID), &r); err != nil {
				return err
			}
			run = &r
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		// Advance first: it takes the row lock and validates the edge, so a run that is
		// not SCHEDULED never consumes the key.
		r, err := advanceTx(ctx, tx, tenantID, runID, AdvanceParams{
			ExpectedVersion: expectedVersion, ToLifecycle: domain.LifecyclePreparing,
			Reason: "execution started", Actor: actor, CorrelationID: correlationID})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO command_idempotency (tenant_id, idempotency_key, command, run_id)
			VALUES ($1,$2,'EXECUTE',$3)`, tenantID, idempotencyKey, runID); err != nil {
			return err
		}
		run = r
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return run, replay, nil
}

// ExecutionContext is everything an execution needs, read consistently.
type ExecutionContext struct {
	Run domain.ControlRun
	// RuleLogic is the executable logic of the rule version the run PINNED. It is read
	// from that exact version — never "the latest" — so approved logic cannot change
	// under a run.
	RuleLogic   domain.RuleLogic
	Definition  domain.ControlDefinition
	Tolerance   *domain.TolerancePolicy
	Materiality *domain.MaterialityPolicy
}

// GetExecutionContext loads the run with the tolerance/materiality it PINNED at
// creation and its definition. The engine reads pinned values only; it never
// resolves "the latest" policy at execution time (Invariant 4).
func (s *PgStore) GetExecutionContext(ctx context.Context, tenantID, runID string) (*ExecutionContext, error) {
	var ec ExecutionContext
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID), &ec.Run); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if err := scanDef(tx.QueryRow(ctx, `SELECT `+defColumns+` FROM control_definitions
			WHERE tenant_id = $1 AND control_definition_id = $2`, tenantID, ec.Run.ControlDefinitionID), &ec.Definition); err != nil {
			return err
		}
		var rawLogic []byte
		if err := tx.QueryRow(ctx, `SELECT logic FROM control_rule_versions
			WHERE tenant_id = $1 AND control_definition_id = $2 AND rule_version = $3`,
			tenantID, ec.Run.ControlDefinitionID, ec.Run.RuleVersion).Scan(&rawLogic); err != nil {
			return err
		}
		logic, err := domain.ParseRuleLogic(rawLogic)
		if err != nil {
			return err
		}
		ec.RuleLogic = logic
		if ec.Run.ToleranceID != nil {
			var t domain.TolerancePolicy
			if err := scanTol(tx.QueryRow(ctx, `SELECT `+tolColumns+` FROM tolerance_policies
				WHERE tenant_id = $1 AND tolerance_id = $2`, tenantID, *ec.Run.ToleranceID), &t); err != nil {
				return err
			}
			ec.Tolerance = &t
		}
		if ec.Run.MaterialityID != nil {
			var m domain.MaterialityPolicy
			if err := scanMat(tx.QueryRow(ctx, `SELECT `+matColumns+` FROM materiality_policies
				WHERE tenant_id = $1 AND materiality_id = $2`, tenantID, *ec.Run.MaterialityID), &m); err != nil {
				return err
			}
			ec.Materiality = &m
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ec, nil
}

// ─── Population freeze ───────────────────────────────────────────────────────

// PopulationBundle is one side's snapshot plus its full record identity set.
type PopulationBundle struct {
	Snapshot domain.PopulationSnapshot
	Records  []domain.PopulationRecord
}

const insertBatch = 500

// FreezePopulations persists both sides' snapshots and records and moves the
// run PREPARING -> POPULATION_FROZEN in ONE transaction: a run is never
// "frozen" without its population, and a population never exists for a run
// that did not freeze. Rows are append-only from here on, so later data cannot
// mutate what was tested (scenario 05).
func (s *PgStore) FreezePopulations(ctx context.Context, tenantID, runID, actor, correlationID string, bundles []PopulationBundle) (*domain.ControlRun, error) {
	var run *domain.ControlRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		for _, b := range bundles {
			totals, err := json.Marshal(b.Snapshot.Totals)
			if err != nil {
				return err
			}
			excl, err := json.Marshal(nonNilExclusions(b.Snapshot.Exclusions))
			if err != nil {
				return err
			}
			var popID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO population_snapshots (tenant_id, run_id, side, source_system, spec_ref, row_count,
					control_totals, population_hash, source_watermark, exclusions)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING population_id::text`,
				tenantID, runID, string(b.Snapshot.Side), b.Snapshot.SourceSystem, b.Snapshot.SpecRef,
				b.Snapshot.RowCount, totals, b.Snapshot.PopulationHash, b.Snapshot.Watermark, excl).Scan(&popID); err != nil {
				return err
			}
			for i := 0; i < len(b.Records); i += insertBatch {
				end := i + insertBatch
				if end > len(b.Records) {
					end = len(b.Records)
				}
				batch := &pgx.Batch{}
				for _, r := range b.Records[i:end] {
					attrs, _ := json.Marshal(nonNilAttrs(r.Attributes))
					batch.Queue(`INSERT INTO population_records (tenant_id, population_id, record_id, reference, amount, currency, record_date, attributes)
						VALUES ($1,$2,$3,$4,$5::numeric,$6,$7::date,$8)`,
						tenantID, popID, r.RecordID, r.Reference, r.Amount, r.Currency, r.Date, attrs)
				}
				br := tx.SendBatch(ctx, batch)
				for range b.Records[i:end] {
					if _, err := br.Exec(); err != nil {
						br.Close()
						return err
					}
				}
				if err := br.Close(); err != nil {
					return err
				}
			}
		}
		r, err := advanceTx(ctx, tx, tenantID, runID, AdvanceParams{
			ExpectedVersion: SkipVersionCheck, ToLifecycle: domain.LifecyclePopulationFroze,
			Reason: "population frozen", Actor: actor, CorrelationID: correlationID})
		if err != nil {
			return err
		}
		run = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func nonNilExclusions(e []domain.Exclusion) []domain.Exclusion {
	if e == nil {
		return []domain.Exclusion{}
	}
	return e
}

func nonNilAttrs(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// ─── Execution result ────────────────────────────────────────────────────────

// ExecutionRecord is the complete outcome of one deterministic execution.
type ExecutionRecord struct {
	Matches         []domain.MatchResult
	Exceptions      []domain.ControlException
	EvidenceContent json.RawMessage
	EvidenceDigest  string
	Result          domain.ResultState
}

// RecordExecution commits matches, exceptions, the sealed evidence package and
// the state movement together. Either the whole outcome is durable — with its
// events — or none of it is.
//
//	PASS  -> READY_TO_CERTIFY   (no exceptions at all)
//	FAIL  -> EXCEPTION_REVIEW   (item-level exceptions await remediation)
func (s *PgStore) RecordExecution(ctx context.Context, tenantID, runID, actor, correlationID string, rec ExecutionRecord) (*domain.ControlRun, error) {
	if rec.Result != domain.ResultPass && rec.Result != domain.ResultFail {
		return nil, fmt.Errorf("%w: an execution records Pass or Fail; technical failures use FailRun", domain.ErrInvalidArgument)
	}
	if rec.Result == domain.ResultPass && len(rec.Exceptions) > 0 {
		return nil, fmt.Errorf("%w: a run with exceptions cannot pass", domain.ErrInvalidTransition)
	}
	if rec.Result == domain.ResultFail && len(rec.Exceptions) == 0 {
		return nil, fmt.Errorf("%w: a failing result requires at least one exception", domain.ErrInvalidTransition)
	}
	var run *domain.ControlRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := advanceTx(ctx, tx, tenantID, runID, AdvanceParams{
			ExpectedVersion: SkipVersionCheck, ToLifecycle: domain.LifecycleExecuting,
			Reason: "comparison executing", Actor: actor, CorrelationID: correlationID}); err != nil {
			return err
		}

		for i := 0; i < len(rec.Matches); i += insertBatch {
			end := i + insertBatch
			if end > len(rec.Matches) {
				end = len(rec.Matches)
			}
			batch := &pgx.Batch{}
			for _, m := range rec.Matches[i:end] {
				kind := m.Kind
				if kind == "" {
					kind = domain.MatchOneToOne
				}
				batch.Queue(`INSERT INTO match_results (tenant_id, run_id, side_a_record, side_b_record, match_rule, outcome, difference, currency,
						kind, group_reference, side_a_records, side_b_records)
					VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,$11,$12)`,
					tenantID, runID, m.SideA, m.SideB, m.Rule, string(m.Outcome), m.Difference, m.Currency,
					kind, m.Reference, m.SideARecords, m.SideBRecords)
			}
			br := tx.SendBatch(ctx, batch)
			for range rec.Matches[i:end] {
				if _, err := br.Exec(); err != nil {
					br.Close()
					return err
				}
			}
			if err := br.Close(); err != nil {
				return err
			}
		}

		var cur domain.ControlRun
		if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID), &cur); err != nil {
			return err
		}
		for _, e := range rec.Exceptions {
			if _, err := tx.Exec(ctx, `
				INSERT INTO control_exceptions (exception_id, tenant_id, run_id, legal_entity_id, category, reason_code,
					assertion, severity, side, record_ids, exposure, currency, detail, owner_role, due_at, state, expected_clearing)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::numeric,$12,$13,$14,$15,'OPEN',$16::date)`,
				e.ExceptionID, tenantID, runID, cur.LegalEntityID, e.Category, e.ReasonCode, e.Assertion,
				string(e.Severity), string(e.Side), e.RecordIDs, e.Exposure, e.Currency, e.Detail, e.OwnerRole, e.DueAt, e.ExpectedClearing); err != nil {
				return err
			}
			if err := insertExceptionTransition(ctx, tx, tenantID, e.ExceptionID, "", string(domain.ExOpen),
				"opened by control execution", actor, correlationID); err != nil {
				return err
			}
			if err := emitException(ctx, tx, cur, e.ExceptionID, events.ExceptionOpened, actor, correlationID, map[string]any{
				"exception_id": e.ExceptionID, "run_id": runID, "category": e.Category, "reason_code": e.ReasonCode,
				"assertion": e.Assertion, "severity": e.Severity, "exposure": e.Exposure, "currency": e.Currency,
				"owner_role": e.OwnerRole, "due_at": e.DueAt.UTC().Format(time.RFC3339),
			}); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `INSERT INTO evidence_packages (tenant_id, run_id, content, digest, algorithm, sealed_by)
			VALUES ($1,$2,$3,$4,$5,$6)`, tenantID, runID, rec.EvidenceContent, rec.EvidenceDigest,
			domain.EvidenceAlgorithm, actor); err != nil {
			return err
		}

		next := domain.LifecycleReadyToCertify
		if rec.Result == domain.ResultFail {
			next = domain.LifecycleExceptionReview
		}
		res := rec.Result
		r, err := advanceTx(ctx, tx, tenantID, runID, AdvanceParams{
			ExpectedVersion: SkipVersionCheck, ToLifecycle: next, ToResult: &res,
			Reason: fmt.Sprintf("execution complete: %d matched, %d exceptions", len(rec.Matches), len(rec.Exceptions)),
			Actor:  actor, CorrelationID: correlationID})
		if err != nil {
			return err
		}
		run = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// FailRun records that the control could not determine a valid result. The
// result is Indeterminate (or Fail), never Pass, and the run is terminal.
func (s *PgStore) FailRun(ctx context.Context, tenantID, runID, actor, correlationID, reason string) (*domain.ControlRun, error) {
	res := domain.ResultIndeterminate
	return s.AdvanceRun(ctx, tenantID, runID, AdvanceParams{
		ExpectedVersion: SkipVersionCheck, ToLifecycle: domain.LifecycleFailed, ToResult: &res,
		Reason: reason, Actor: actor, CorrelationID: correlationID})
}

// ─── Population reads ────────────────────────────────────────────────────────

func (s *PgStore) ListPopulationSnapshots(ctx context.Context, tenantID, runID string) ([]domain.PopulationSnapshot, error) {
	var out []domain.PopulationSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT population_id::text, tenant_id::text, run_id::text, side, source_system, spec_ref,
			row_count, control_totals, population_hash, source_watermark, exclusions, frozen_at
			FROM population_snapshots WHERE tenant_id = $1 AND run_id = $2 ORDER BY side`, tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p domain.PopulationSnapshot
			var side string
			var totals, excl []byte
			if err := rows.Scan(&p.PopulationID, &p.TenantID, &p.RunID, &side, &p.SourceSystem, &p.SpecRef,
				&p.RowCount, &totals, &p.PopulationHash, &p.Watermark, &excl, &p.FrozenAt); err != nil {
				return err
			}
			p.Side = domain.Side(side)
			if err := json.Unmarshal(totals, &p.Totals); err != nil {
				return err
			}
			if err := json.Unmarshal(excl, &p.Exclusions); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// PopulationRecordRow carries the paging cursor with each record.
type PopulationRecordRow struct {
	Seq    int64
	Record domain.PopulationRecord
}

// ListPopulationRecords pages one side's identity set by insertion sequence
// (keyset, never OFFSET).
func (s *PgStore) ListPopulationRecords(ctx context.Context, tenantID, runID string, side domain.Side, afterSeq int64, limit int) ([]PopulationRecordRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []PopulationRecordRow
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r.seq, r.record_id, r.reference, r.amount::text, r.currency,
				to_char(r.record_date,'YYYY-MM-DD'), r.attributes
			FROM population_records r
			JOIN population_snapshots p ON p.population_id = r.population_id AND p.tenant_id = r.tenant_id
			WHERE r.tenant_id = $1 AND p.run_id = $2 AND p.side = $3 AND r.seq > $4
			ORDER BY r.seq LIMIT $5`, tenantID, runID, string(side), afterSeq, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row PopulationRecordRow
			var attrs []byte
			if err := rows.Scan(&row.Seq, &row.Record.RecordID, &row.Record.Reference, &row.Record.Amount,
				&row.Record.Currency, &row.Record.Date, &attrs); err != nil {
				return err
			}
			_ = json.Unmarshal(attrs, &row.Record.Attributes)
			row.Record.Amount = domain.NormalizeDecimalText(row.Record.Amount)
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}

// ─── Exceptions ──────────────────────────────────────────────────────────────

const exColumns = `exception_id::text, tenant_id::text, run_id::text, legal_entity_id::text, category, reason_code, assertion,
	severity, side, record_ids, exposure::text, currency, detail, owner_role, owner_principal_id, due_at, state, version,
	created_at, updated_at, to_char(expected_clearing, 'YYYY-MM-DD')`

func scanException(row pgx.Row, e *domain.ControlException) error {
	var sev, side, state string
	if err := row.Scan(&e.ExceptionID, &e.TenantID, &e.RunID, &e.LegalEntityID, &e.Category, &e.ReasonCode, &e.Assertion,
		&sev, &side, &e.RecordIDs, &e.Exposure, &e.Currency, &e.Detail, &e.OwnerRole, &e.OwnerPrincipal, &e.DueAt,
		&state, &e.Version, &e.CreatedAt, &e.UpdatedAt, &e.ExpectedClearing); err != nil {
		return err
	}
	e.Severity, e.Side, e.State = domain.Severity(sev), domain.Side(side), domain.ExceptionState(state)
	e.Exposure = domain.NormalizeDecimalText(e.Exposure)
	return nil
}

type ListExceptionsFilter struct {
	State    string
	Severity string
	Limit    int
	AfterID  string
}

func (s *PgStore) ListExceptions(ctx context.Context, tenantID, runID string, f ListExceptionsFilter) ([]domain.ControlException, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	var out []domain.ControlException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+exColumns+` FROM control_exceptions
			WHERE tenant_id = $1 AND run_id = $2 AND ($3 = '' OR state = $3) AND ($4 = '' OR severity = $4)
			  AND ($5 = '' OR exception_id > NULLIF($5,'')::uuid)
			ORDER BY exception_id LIMIT $6`, tenantID, runID, f.State, f.Severity, f.AfterID, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ControlException
			if err := scanException(rows, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetException(ctx context.Context, tenantID, exceptionID string) (*domain.ControlException, error) {
	var e domain.ControlException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanException(tx.QueryRow(ctx, `SELECT `+exColumns+` FROM control_exceptions
			WHERE tenant_id = $1 AND exception_id = $2`, tenantID, exceptionID), &e); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// AssignException establishes (or changes) a named owner. It uses expected
// version semantics, validates the exception state edge, may tighten but never
// extend the SLA, and appends to the exception's transition history.
func (s *PgStore) AssignException(ctx context.Context, tenantID, exceptionID, actor, correlationID string, expectedVersion int, req domain.AssignExceptionRequest) (*domain.ControlException, error) {
	var out domain.ControlException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var cur domain.ControlException
		if err := scanException(tx.QueryRow(ctx, `SELECT `+exColumns+` FROM control_exceptions
			WHERE tenant_id = $1 AND exception_id = $2 FOR UPDATE`, tenantID, exceptionID), &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if cur.Version != expectedVersion {
			return fmt.Errorf("%w: exception is at version %d, caller expected %d", domain.ErrConflict, cur.Version, expectedVersion)
		}
		if err := domain.ValidateExceptionTransition(cur.State, domain.ExAssigned); err != nil {
			return err
		}
		due := cur.DueAt
		if req.DueDate != "" {
			d, _ := time.Parse("2006-01-02", req.DueDate)
			d = d.Add(24*time.Hour - time.Second) // end of the named day, UTC
			if d.After(cur.DueAt) {
				return fmt.Errorf("%w: assignment may tighten the SLA but not extend it; extension is a governed waiver", domain.ErrInvalidArgument)
			}
			due = d
		}
		if err := scanException(tx.QueryRow(ctx, `
			UPDATE control_exceptions SET state = 'ASSIGNED', owner_principal_id = $3, due_at = $4,
				version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND exception_id = $2 RETURNING `+exColumns,
			tenantID, exceptionID, req.OwnerPrincipalID, due), &out); err != nil {
			return err
		}
		if err := insertExceptionTransition(ctx, tx, tenantID, exceptionID, string(cur.State), string(domain.ExAssigned),
			req.Reason, actor, correlationID); err != nil {
			return err
		}
		var run domain.ControlRun
		if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2`, tenantID, out.RunID), &run); err != nil {
			return err
		}
		return emitException(ctx, tx, run, exceptionID, events.ExceptionAssigned, actor, correlationID, map[string]any{
			"exception_id": exceptionID, "run_id": out.RunID, "owner_principal_id": req.OwnerPrincipalID,
			"due_at": due.UTC().Format(time.RFC3339), "reason": req.Reason,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func insertExceptionTransition(ctx context.Context, tx pgx.Tx, tenantID, exceptionID, from, to, reason, actor, correlationID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO exception_transitions (tenant_id, exception_id, from_state, to_state, reason, actor_id, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenantID, exceptionID, from, to, reason, actor, correlationID)
	return err
}

func emitException(ctx context.Context, tx pgx.Tx, r domain.ControlRun, exceptionID, eventType, actor, correlationID string, payload map[string]any) error {
	payload["occurred_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	a, c := actor, correlationID
	return outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "control_exception", AggregateID: exceptionID, EventType: eventType,
		TenantID: r.TenantID, LegalEntityID: r.LegalEntityID, ActorID: &a, CorrelationID: &c, Payload: payload,
	})
}

// ─── Evidence ────────────────────────────────────────────────────────────────

// GetLatestEvidence returns the run's most recent sealed package with its
// integrity re-verified from the stored content at read time.
func (s *PgStore) GetLatestEvidence(ctx context.Context, tenantID, runID string) (*domain.EvidencePackage, error) {
	var p domain.EvidencePackage
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT package_id::text, run_id::text, content, digest, algorithm, sealed_at, sealed_by
			FROM evidence_packages WHERE tenant_id = $1 AND run_id = $2 ORDER BY sealed_at DESC LIMIT 1`,
			tenantID, runID).Scan(&p.PackageID, &p.RunID, &p.Content, &p.Digest, &p.Algorithm, &p.SealedAt, &p.SealedBy); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ok := domain.VerifyEvidence(p.Content, p.Digest)
	p.Verified = &ok
	return &p, nil
}

// ListMatchResults returns the run's pairings (for evidence review and tests).
func (s *PgStore) ListMatchResults(ctx context.Context, tenantID, runID string) ([]domain.MatchResult, error) {
	var out []domain.MatchResult
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT side_a_record, side_b_record, match_rule, outcome, difference::text, currency,
				kind, group_reference, side_a_records, side_b_records
			FROM match_results WHERE tenant_id = $1 AND run_id = $2 ORDER BY side_a_record, side_b_record`, tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.MatchResult
			var oc string
			if err := rows.Scan(&m.SideA, &m.SideB, &m.Rule, &oc, &m.Difference, &m.Currency,
				&m.Kind, &m.Reference, &m.SideARecords, &m.SideBRecords); err != nil {
				return err
			}
			m.Outcome = domain.MatchOutcome(oc)
			m.Difference = domain.NormalizeDecimalText(m.Difference)
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}
