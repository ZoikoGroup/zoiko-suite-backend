package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// LiabilityChange is what a committed WFP-06 command changed; the builder turns it
// into outbox events inside the same transaction. Kind is one of RECOGNIZED, DUE,
// SCHEDULED, SETTLED, ADJUSTED, SUPERSEDED, DISPUTED, DISPUTE_CLEARED,
// RECONCILIATION_FAILED, EXCEPTION.
type LiabilityChange struct {
	Kind      string
	Liability *domain.PayrollLiability
	Exception *domain.LiabilityException
	Detail    map[string]any
}

type LiabilityEventBuilder func(LiabilityChange) ([]domain.OutboxEvent, error)

func enqueueLiability(ctx context.Context, tx pgx.Tx, mk LiabilityEventBuilder, ch LiabilityChange) error {
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

// mapLiabilityGuard turns the database guards' refusals into domain errors.
func mapLiabilityGuard(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23000":
		switch {
		case strings.Contains(pgErr.Message, "ACCEPTED result"):
			return domain.ErrLiabilityResultNotActive
		case strings.Contains(pgErr.Message, "cannot be scheduled"):
			return domain.ErrLiabilityNeedsReview
		case strings.Contains(pgErr.Message, "illegal liability transition"):
			return domain.ErrLiabilityState
		case strings.Contains(pgErr.Message, "is not superseded"):
			return domain.ErrLiabilityHasSettlements
		}
	case "23514":
		switch pgErr.ConstraintName {
		case "wfp_liab_settled_bound":
			return domain.ErrOverSettlement
		case "wfp_liab_adjust_bound":
			return domain.ErrAdjustBelowSettled
		}
	}
	return err
}

// ── registry ─────────────────────────────────────────────────────────────────

const classCols = `class_row_id::text, tenant_id, legal_entity_id, component_kind, component_code, class_version, class_code,
	liability_type, authority_counterparty_id, jurisdiction_id, due_rule_kind, due_rule_value, required, active, rule_ref,
	effective_from::text, registered_by, created_at`

func scanClass(row pgx.Row) (*domain.LiabilityClass, error) {
	var c domain.LiabilityClass
	err := row.Scan(&c.ClassRowID, &c.TenantID, &c.LegalEntityID, &c.ComponentKind, &c.ComponentCode, &c.ClassVersion, &c.ClassCode,
		&c.LiabilityType, &c.AuthorityCounterpartyID, &c.JurisdictionID, &c.DueRuleKind, &c.DueRuleValue, &c.Required, &c.Active,
		&c.RuleRef, &c.EffectiveFrom, &c.RegisteredBy, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrLiabilityClassNotFound
	}
	return &c, err
}

// RegisterLiabilityClass appends a registry row. A change is a NEW row with a later
// effective_from; nothing is updated in place (the table is append-only).
func (s *PgStore) RegisterLiabilityClass(ctx context.Context, req domain.RegisterLiabilityClassRequest, actor string) (*domain.LiabilityClass, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if req.ComponentCode == "" {
		req.ComponentCode = "*" // the store does not rely on the handler having validated
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	var out *domain.LiabilityClass
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		lockKey := strings.Join([]string{tenantID, req.LegalEntityID, req.ComponentKind, req.ComponentCode}, "|")
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
			return err
		}
		var maxVersion int
		var maxEffective *string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(class_version), 0), max(effective_from)::text FROM wfp_liability_classes
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND component_kind = $3 AND component_code = $4`,
			tenantID, req.LegalEntityID, req.ComponentKind, req.ComponentCode).Scan(&maxVersion, &maxEffective); err != nil {
			return err
		}
		if maxEffective != nil && req.EffectiveFrom <= *maxEffective {
			return domain.ErrLiabilityClassOrder
		}
		out, err = scanClass(tx.QueryRow(ctx, `
			INSERT INTO wfp_liability_classes
				(class_row_id, tenant_id, legal_entity_id, component_kind, component_code, class_version, class_code, liability_type,
				 authority_counterparty_id, jurisdiction_id, due_rule_kind, due_rule_value, required, active, rule_ref, effective_from, registered_by)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::date, $17)
			RETURNING `+classCols,
			newUUID(), tenantID, req.LegalEntityID, req.ComponentKind, req.ComponentCode, maxVersion+1, req.ClassCode, req.LiabilityType,
			req.AuthorityCounterpartyID, req.JurisdictionID, req.DueRuleKind, req.DueRuleValue, req.Required, active, req.RuleRef,
			req.EffectiveFrom, actor))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrLiabilityClassConflict
		}
		return err
	})
	return out, err
}

// ListLiabilityClasses returns every registry row of a legal entity (all versions).
func (s *PgStore) ListLiabilityClasses(ctx context.Context, legalEntityID string) ([]domain.LiabilityClass, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.LiabilityClass{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+classCols+` FROM wfp_liability_classes
			WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY component_kind, component_code, class_version`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanClass(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

// ── liabilities ──────────────────────────────────────────────────────────────

const liabCols = `liability_id::text, tenant_id, legal_entity_id, run_ref_id::text, result_id::text, payroll_period_id, class_code,
	class_row_id::text, liability_type, party_side, authority_counterparty_id, jurisdiction_id, rule_ref, component_kinds,
	source_component_count, authority_labels, kind_amounts, amount::text, currency, due_date::text, due_rule_kind, due_rule_value,
	state, exception_state, settled_amount::text, adjustment_total::text, scheduled_for::text, scheduled_by, dispute_reason,
	reconciliation_state, reconciled_at, reconciliation_detail, version, correlation_id, created_by, created_at, updated_at`

func scanLiability(row pgx.Row) (*domain.PayrollLiability, error) {
	var l domain.PayrollLiability
	var kindAmounts, detail []byte
	err := row.Scan(&l.LiabilityID, &l.TenantID, &l.LegalEntityID, &l.RunRefID, &l.ResultID, &l.PayrollPeriodID, &l.ClassCode,
		&l.ClassRowID, &l.LiabilityType, &l.PartySide, &l.AuthorityCounterpartyID, &l.JurisdictionID, &l.RuleRef, &l.ComponentKinds,
		&l.SourceComponentCount, &l.AuthorityLabels, &kindAmounts, &l.Amount, &l.Currency, &l.DueDate, &l.DueRuleKind, &l.DueRuleValue,
		&l.State, &l.ExceptionState, &l.SettledAmount, &l.AdjustmentTotal, &l.ScheduledFor, &l.ScheduledBy, &l.DisputeReason,
		&l.ReconciliationState, &l.ReconciledAt, &detail, &l.Version, &l.CorrelationID, &l.CreatedBy, &l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrLiabilityNotFound
	}
	if err != nil {
		return nil, err
	}
	l.KindAmounts = map[string]string{}
	if len(kindAmounts) > 0 {
		if err := json.Unmarshal(kindAmounts, &l.KindAmounts); err != nil {
			return nil, fmt.Errorf("decode kind amounts: %w", err)
		}
	}
	if len(detail) > 0 {
		l.ReconciliationDetail = json.RawMessage(detail)
	}
	if l.AuthorityLabels == nil {
		l.AuthorityLabels = []string{}
	}
	if l.OutstandingAmount, err = domain.Outstanding(l.Amount, l.AdjustmentTotal, l.SettledAmount); err != nil {
		return nil, err
	}
	return &l, nil
}

func lockLiabilityTx(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.PayrollLiability, error) {
	return scanLiability(tx.QueryRow(ctx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
		WHERE tenant_id = $1 AND liability_id = $2::uuid FOR UPDATE`, tenantID, id))
}

func queryLiabilities(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]domain.PayrollLiability, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PayrollLiability{}
	for rows.Next() {
		l, err := scanLiability(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

type RecognitionOutcome struct {
	Liabilities []domain.PayrollLiability   `json:"liabilities"`
	Exceptions  []domain.LiabilityException `json:"exceptions"`
	NewCount    int                         `json:"newly_recognized"`
}

const excCols = `exception_id::text, tenant_id, legal_entity_id, result_id::text, liability_id::text, code, subject_key, amount::text,
	detail, state, resolution, resolved_at, created_at`

func scanException(row pgx.Row) (*domain.LiabilityException, error) {
	var e domain.LiabilityException
	err := row.Scan(&e.ExceptionID, &e.TenantID, &e.LegalEntityID, &e.ResultID, &e.LiabilityID, &e.Code, &e.SubjectKey, &e.Amount,
		&e.Detail, &e.State, &e.Resolution, &e.ResolvedAt, &e.CreatedAt)
	return &e, err
}

func listExceptionsTx(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]domain.LiabilityException, error) {
	rows, err := tx.Query(ctx, `SELECT `+excCols+` FROM wfp_liability_exceptions WHERE `+where+` ORDER BY created_at, code, subject_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.LiabilityException{}
	for rows.Next() {
		e, err := scanException(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// managedExceptionCodes are the exceptions recognition owns: it opens them when the
// cause exists and resolves them when the cause is gone. The GL mismatch belongs to
// reconciliation, the employer-not-computed one to the result's flag.
var managedExceptionCodes = []string{domain.ExcUnmappedClass, domain.ExcClassConflict, domain.ExcMissingExpected, domain.ExcAuthorityInvalid}

func raiseExceptionTx(ctx context.Context, tx pgx.Tx, tenantID, entityID, resultID string, liabilityID *string, d domain.ExceptionDraft, mk LiabilityEventBuilder) error {
	var prior *string
	err := tx.QueryRow(ctx, `SELECT state FROM wfp_liability_exceptions WHERE tenant_id = $1 AND result_id = $2::uuid AND code = $3 AND subject_key = $4`,
		tenantID, resultID, d.Code, d.SubjectKey).Scan(&prior)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if prior != nil && *prior == domain.ExcOpen {
		return nil // already open: nothing new to report
	}
	var e *domain.LiabilityException
	if prior == nil {
		e, err = scanException(tx.QueryRow(ctx, `
			INSERT INTO wfp_liability_exceptions (exception_id, tenant_id, legal_entity_id, result_id, liability_id, code, subject_key, amount, detail)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6, $7, $8::numeric, $9) RETURNING `+excCols,
			newUUID(), tenantID, entityID, resultID, liabilityID, d.Code, d.SubjectKey, d.Amount, d.Detail))
	} else { // the cause came back after it was resolved
		e, err = scanException(tx.QueryRow(ctx, `
			UPDATE wfp_liability_exceptions SET state = 'OPEN', resolution = NULL, resolved_at = NULL, amount = $5::numeric
			WHERE tenant_id = $1 AND result_id = $2::uuid AND code = $3 AND subject_key = $4 RETURNING `+excCols,
			tenantID, resultID, d.Code, d.SubjectKey, d.Amount))
	}
	if err != nil {
		return err
	}
	return enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "EXCEPTION", Exception: e})
}

// ApplyRecognition writes the plan for one ACCEPTED result in a single transaction:
// new liabilities (existing ones are left exactly as they are), the exceptions the
// plan raises, the resolution of exceptions whose cause is gone, and the DUE
// transition for anything already past its due date. review maps an authority id to
// the reason it failed validation (empty = valid): such a liability is recognized
// with exception_state REVIEW_REQUIRED and cannot be scheduled.
func (s *PgStore) ApplyRecognition(ctx context.Context, ref *domain.PayrollRunRef, res *domain.PayrollResult, plan domain.RecognitionPlan,
	review map[string]string, actor, correlationID, asOf string, mk LiabilityEventBuilder) (*RecognitionOutcome, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := &RecognitionOutcome{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		openKeys := []string{} // never nil: a NULL array would make the resolve below match nothing
		for _, d := range plan.Liabilities {
			kindAmounts := map[string]string{}
			for k, v := range d.KindAmounts {
				kindAmounts[k] = domain.FormatDecimal(v)
			}
			kaJSON, err := json.Marshal(kindAmounts)
			if err != nil {
				return err
			}
			reason := review[d.Class.AuthorityCounterpartyID]
			exc := domain.LiabExcNone
			if reason != "" {
				exc = domain.LiabExcReviewRequired
			}
			l, err := scanLiability(tx.QueryRow(ctx, `
				INSERT INTO wfp_payroll_liabilities
					(liability_id, tenant_id, legal_entity_id, run_ref_id, result_id, payroll_period_id, class_code, class_row_id, liability_type,
					 party_side, authority_counterparty_id, jurisdiction_id, rule_ref, component_kinds, source_component_count, authority_labels,
					 kind_amounts, amount, currency, due_date, due_rule_kind, due_rule_value, state, exception_state, correlation_id, created_by)
				VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6, $7, $8::uuid, $9, $10, $11, $12, $13, $14, $15, $16, $17::jsonb,
				        $18::numeric, $19, $20::date, $21, $22, 'RECOGNIZED', $23, $24, $25)
				ON CONFLICT (tenant_id, result_id, class_code, authority_counterparty_id, liability_type) DO NOTHING
				RETURNING `+liabCols,
				newUUID(), tenantID, ref.LegalEntityID, ref.RunRefID, res.ResultID, ref.PayrollPeriodID, d.Class.ClassCode, d.Class.ClassRowID,
				d.Class.LiabilityType, d.PartySide, d.Class.AuthorityCounterpartyID, d.Class.JurisdictionID, d.Class.RuleRef, d.ComponentKinds,
				d.SourceCount, d.AuthorityLabels, string(kaJSON), domain.FormatDecimal(d.Amount), res.Currency, d.DueDate, d.Class.DueRuleKind,
				d.Class.DueRuleValue, exc, correlationID, actor))
			created := true
			if errors.Is(err, domain.ErrLiabilityNotFound) { // DO NOTHING: the liability is already recognized
				created = false
				l, err = scanLiability(tx.QueryRow(ctx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
					WHERE tenant_id = $1 AND result_id = $2::uuid AND class_code = $3 AND authority_counterparty_id = $4 AND liability_type = $5 FOR UPDATE`,
					tenantID, res.ResultID, d.Class.ClassCode, d.Class.AuthorityCounterpartyID, d.Class.LiabilityType))
			}
			if err != nil {
				return mapLiabilityGuard(err)
			}
			if created {
				out.NewCount++
				if err := enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "RECOGNIZED", Liability: l}); err != nil {
					return err
				}
			} else if l.ExceptionState != domain.LiabExcDisputed && l.State != domain.LiabSuperseded && l.State != domain.LiabReconciled && l.ExceptionState != exc {
				// The authority's validity changed since it was recognized.
				if _, err := tx.Exec(ctx, `UPDATE wfp_payroll_liabilities SET exception_state = $3, version = version + 1, updated_at = now()
					WHERE tenant_id = $1 AND liability_id = $2::uuid`, tenantID, l.LiabilityID, exc); err != nil {
					return mapLiabilityGuard(err)
				}
			}
			if reason != "" {
				lid := l.LiabilityID
				ed := domain.ExceptionDraft{Code: domain.ExcAuthorityInvalid, SubjectKey: d.Class.AuthorityCounterpartyID, Detail: reason}
				if err := raiseExceptionTx(ctx, tx, tenantID, ref.LegalEntityID, res.ResultID, &lid, ed, mk); err != nil {
					return err
				}
				openKeys = append(openKeys, ed.Code+"|"+ed.SubjectKey)
			}
		}
		for _, d := range plan.Exceptions {
			if err := raiseExceptionTx(ctx, tx, tenantID, ref.LegalEntityID, res.ResultID, nil, d, mk); err != nil {
				return err
			}
			openKeys = append(openKeys, d.Code+"|"+d.SubjectKey)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE wfp_liability_exceptions SET state = 'RESOLVED', resolution = 'CAUSE_CLEARED', resolved_at = now()
			WHERE tenant_id = $1 AND result_id = $2::uuid AND state = 'OPEN' AND code = ANY($3)
			  AND (code || '|' || subject_key) <> ALL($4)`, tenantID, res.ResultID, managedExceptionCodes, openKeys); err != nil {
			return err
		}
		if err := sweepDueTx(ctx, tx, tenantID, "", res.ResultID, asOf, mk); err != nil {
			return err
		}
		if out.Liabilities, err = queryLiabilities(ctx, tx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
			WHERE tenant_id = $1 AND result_id = $2::uuid ORDER BY class_code, authority_counterparty_id`, tenantID, res.ResultID); err != nil {
			return err
		}
		out.Exceptions, err = listExceptionsTx(ctx, tx, `tenant_id = $1 AND result_id = $2::uuid`, tenantID, res.ResultID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sweepDueTx moves RECOGNIZED liabilities whose due date has arrived to DUE, for
// one result, one legal entity or (both empty) the tenant.
func sweepDueTx(ctx context.Context, tx pgx.Tx, tenantID, entityID, resultID, asOf string, mk LiabilityEventBuilder) error {
	moved, err := queryLiabilities(ctx, tx, `
		UPDATE wfp_payroll_liabilities SET state = 'DUE', version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND state = 'RECOGNIZED' AND due_date <= $2::date
		  AND ($3 = '' OR legal_entity_id = $3) AND ($4 = '' OR result_id::text = $4)
		RETURNING `+liabCols, tenantID, asOf, entityID, resultID)
	if err != nil {
		return err
	}
	for i := range moved {
		if err := enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "DUE", Liability: &moved[i]}); err != nil {
			return err
		}
	}
	return nil
}

// DueSweep marks every RECOGNIZED liability of an entity whose due date is on or
// before asOf as DUE and returns them.
func (s *PgStore) DueSweep(ctx context.Context, legalEntityID, asOf string, mk LiabilityEventBuilder) ([]domain.PayrollLiability, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.PayrollLiability
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := sweepDueTx(ctx, tx, tenantID, legalEntityID, "", asOf, mk); err != nil {
			return err
		}
		var e error
		out, e = queryLiabilities(ctx, tx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND state = 'DUE' ORDER BY due_date, liability_id`, tenantID, legalEntityID)
		return e
	})
	return out, err
}

type LiabilityFilter struct {
	RunRefID      string
	LegalEntityID string
	State         string
	AuthorityID   string
	Limit         int
}

func (s *PgStore) ListLiabilities(ctx context.Context, f LiabilityFilter) ([]domain.PayrollLiability, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	var out []domain.PayrollLiability
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, e = queryLiabilities(ctx, tx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
			WHERE tenant_id = $1 AND ($2 = '' OR run_ref_id::text = $2) AND ($3 = '' OR legal_entity_id = $3)
			  AND ($4 = '' OR state = $4) AND ($5 = '' OR authority_counterparty_id = $5)
			ORDER BY due_date, class_code, liability_id LIMIT $6`,
			tenantID, f.RunRefID, f.LegalEntityID, f.State, f.AuthorityID, f.Limit)
		return e
	})
	return out, err
}

// GetLiability returns one liability with its settlements and adjustments.
func (s *PgStore) GetLiability(ctx context.Context, id string) (*domain.PayrollLiability, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var l *domain.PayrollLiability
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		if l, e = scanLiability(tx.QueryRow(ctx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
			WHERE tenant_id = $1 AND liability_id = $2::uuid`, tenantID, id)); e != nil {
			return e
		}
		srows, e := tx.Query(ctx, `SELECT settlement_id::text, liability_id::text, payment_id, amount::text, currency, declared_by,
				banking_status, finality_source, amount_evidence, created_at, instruction_id::text
			FROM wfp_liability_settlements WHERE tenant_id = $1 AND liability_id = $2::uuid ORDER BY created_at`, tenantID, id)
		if e != nil {
			return e
		}
		defer srows.Close()
		l.Settlements = []domain.LiabilitySettlement{}
		for srows.Next() {
			var st domain.LiabilitySettlement
			if e := srows.Scan(&st.SettlementID, &st.LiabilityID, &st.PaymentID, &st.Amount, &st.Currency, &st.DeclaredBy,
				&st.BankingStatus, &st.FinalitySource, &st.AmountEvidence, &st.CreatedAt, &st.InstructionID); e != nil {
				return e
			}
			l.Settlements = append(l.Settlements, st)
		}
		if e := srows.Err(); e != nil {
			return e
		}
		arows, e := tx.Query(ctx, `SELECT adjustment_id::text, liability_id::text, delta::text, reason, evidence_ref, created_by, created_at
			FROM wfp_liability_adjustments WHERE tenant_id = $1 AND liability_id = $2::uuid ORDER BY created_at`, tenantID, id)
		if e != nil {
			return e
		}
		defer arows.Close()
		l.Adjustments = []domain.LiabilityAdjustment{}
		for arows.Next() {
			var a domain.LiabilityAdjustment
			if e := arows.Scan(&a.AdjustmentID, &a.LiabilityID, &a.Delta, &a.Reason, &a.EvidenceRef, &a.CreatedBy, &a.CreatedAt); e != nil {
				return e
			}
			l.Adjustments = append(l.Adjustments, a)
		}
		return arows.Err()
	})
	return l, err
}

// ListLiabilityExceptions returns exceptions of a result or, with no run ref, of a
// legal entity; state "" returns every state.
func (s *PgStore) ListLiabilityExceptions(ctx context.Context, runRefID, legalEntityID, state string) ([]domain.LiabilityException, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.LiabilityException
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, e = listExceptionsTx(ctx, tx, `tenant_id = $1 AND ($2 = '' OR result_id IN (SELECT result_id FROM wfp_payroll_results WHERE run_ref_id::text = $2))
			AND ($3 = '' OR legal_entity_id = $3) AND ($4 = '' OR state = $4)`, tenantID, runRefID, legalEntityID, state)
		return e
	})
	return out, err
}

func checkVersion(cur *domain.PayrollLiability, expected int) error {
	if cur.Version != expected {
		return domain.ErrLiabilityVersionConflict
	}
	return nil
}

func updateLiabilityTx(ctx context.Context, tx pgx.Tx, tenantID, id, setSQL string, args ...any) (*domain.PayrollLiability, error) {
	all := append([]any{tenantID, id}, args...)
	l, err := scanLiability(tx.QueryRow(ctx, `UPDATE wfp_payroll_liabilities SET `+setSQL+`, version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND liability_id = $2::uuid RETURNING `+liabCols, all...))
	return l, mapLiabilityGuard(err)
}

// ScheduleLiability records the intention to pay by a date. It is separate from paying:
// the liability stays unsettled until Banking evidence arrives. A liability under
// review or dispute, or whose result is no longer ACCEPTED, cannot be scheduled.
func (s *PgStore) ScheduleLiability(ctx context.Context, id string, expectedVersion int, scheduledFor, actor string, mk LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PayrollLiability
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockLiabilityTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if err := checkVersion(cur, expectedVersion); err != nil {
			return err
		}
		if cur.State != domain.LiabRecognized && cur.State != domain.LiabDue {
			return domain.ErrLiabilityState
		}
		if cur.ExceptionState != domain.LiabExcNone {
			return domain.ErrLiabilityNeedsReview
		}
		out, err = updateLiabilityTx(ctx, tx, tenantID, id, `state = 'SCHEDULED', scheduled_for = $3::date, scheduled_by = $4`, scheduledFor, actor)
		if err != nil {
			return err
		}
		return enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "SCHEDULED", Liability: out, Detail: map[string]any{"scheduled_for": scheduledFor}})
	})
	return out, err
}

// SettlementEvidence is what Banking showed when the settlement was verified.
type SettlementEvidence struct {
	BankingStatus  string
	FinalitySource *string
}

type SettleOutcome struct {
	Liability  *domain.PayrollLiability
	Settlement *domain.LiabilitySettlement
	Replay     bool
}

// SettleLiability applies a verified Banking settlement. One payment applies to one
// liability once (a replay is the same settlement, never counted twice), partial
// settlements keep the residual, and the settled total can never exceed what is owed.
func (s *PgStore) SettleLiability(ctx context.Context, id string, req domain.SettleLiabilityRequest, ev SettlementEvidence, actor string, mk LiabilityEventBuilder) (*SettleOutcome, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	amount, err := domain.ParseDecimal(req.Amount)
	if err != nil {
		return nil, err
	}
	out := &SettleOutcome{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockLiabilityTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		var existing domain.LiabilitySettlement
		err = tx.QueryRow(ctx, `SELECT settlement_id::text, liability_id::text, payment_id, amount::text, currency, declared_by, banking_status,
				finality_source, amount_evidence, created_at, instruction_id::text
			FROM wfp_liability_settlements WHERE tenant_id = $1 AND liability_id = $2::uuid AND payment_id = $3`, tenantID, id, req.PaymentID).
			Scan(&existing.SettlementID, &existing.LiabilityID, &existing.PaymentID, &existing.Amount, &existing.Currency, &existing.DeclaredBy,
				&existing.BankingStatus, &existing.FinalitySource, &existing.AmountEvidence, &existing.CreatedAt, &existing.InstructionID)
		if err == nil {
			prev, perr := domain.ParseDecimal(existing.Amount)
			if perr != nil || prev.Cmp(amount) != 0 {
				return domain.ErrSettlementConflict
			}
			out.Liability, out.Settlement, out.Replay = cur, &existing, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if req.Currency != cur.Currency {
			return domain.ErrSettlementCurrency
		}
		switch cur.State {
		case domain.LiabRecognized, domain.LiabDue, domain.LiabScheduled, domain.LiabPartiallySettled:
		default:
			return domain.ErrLiabilityState
		}
		owed, err := owedOf(cur)
		if err != nil {
			return err
		}
		settledBefore, err := domain.ParseDecimal(cur.SettledAmount)
		if err != nil {
			return err
		}
		settled, state, err := domain.ApplySettlement(owed, settledBefore, amount)
		if err != nil {
			return err
		}
		evidence, instrID := domain.AmountDeclared, (*string)(nil)
		if req.InstructionID != "" {
			var itype, istate, icur, iamt string
			var iliab *string
			err := tx.QueryRow(ctx, `SELECT instruction_type, state, currency, amount::text, liability_id::text FROM wfp_payment_instructions
				WHERE tenant_id = $1 AND instruction_id = $2::uuid`, tenantID, req.InstructionID).Scan(&itype, &istate, &icur, &iamt, &iliab)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrInstructionMismatch
			}
			if err != nil {
				return err
			}
			ia, perr := domain.ParseDecimal(iamt)
			if perr != nil {
				return perr
			}
			if itype != domain.PayTypeLiab || iliab == nil || *iliab != id || icur != req.Currency || ia.Cmp(amount) != 0 ||
				(istate != domain.PaySubmitted && istate != domain.PayHandedOff) {
				return domain.ErrInstructionMismatch
			}
			evidence, instrID = domain.AmountInstructionBound, &req.InstructionID
		}
		st := &domain.LiabilitySettlement{}
		if err := tx.QueryRow(ctx, `
			INSERT INTO wfp_liability_settlements (settlement_id, tenant_id, liability_id, payment_id, amount, currency, declared_by, banking_status, finality_source, amount_evidence, instruction_id)
			VALUES ($1::uuid, $2, $3::uuid, $4, $5::numeric, $6, $7, $8, $9, $10, $11::uuid)
			RETURNING settlement_id::text, liability_id::text, payment_id, amount::text, currency, declared_by, banking_status, finality_source, amount_evidence, created_at, instruction_id::text`,
			newUUID(), tenantID, id, req.PaymentID, req.Amount, req.Currency, actor, ev.BankingStatus, ev.FinalitySource, evidence, instrID).
			Scan(&st.SettlementID, &st.LiabilityID, &st.PaymentID, &st.Amount, &st.Currency, &st.DeclaredBy, &st.BankingStatus, &st.FinalitySource, &st.AmountEvidence, &st.CreatedAt, &st.InstructionID); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.ErrSettlementConflict // this instruction already settled a payment
			}
			return err
		}
		upd, err := updateLiabilityTx(ctx, tx, tenantID, id, `state = $3, settled_amount = $4::numeric`, state, domain.FormatDecimal(settled))
		if err != nil {
			return err
		}
		out.Liability, out.Settlement = upd, st
		return enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "SETTLED", Liability: upd, Detail: map[string]any{
			"payment_id": req.PaymentID, "settlement_amount": req.Amount, "outstanding_amount": upd.OutstandingAmount, "amount_evidence": evidence,
		}})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func owedOf(l *domain.PayrollLiability) (*big.Rat, error) {
	a, err := domain.ParseDecimal(l.Amount)
	if err != nil {
		return nil, err
	}
	adj, err := domain.ParseDecimal(l.AdjustmentTotal)
	if err != nil {
		return nil, err
	}
	return new(big.Rat).Add(a, adj), nil
}

// AdjustLiability applies an explicit, evidenced change to what is owed. The
// original amount is never edited; the adjustment is its own record.
func (s *PgStore) AdjustLiability(ctx context.Context, id string, req domain.AdjustLiabilityRequest, actor string, mk LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	delta, err := domain.ParseDecimal(req.Delta)
	if err != nil {
		return nil, err
	}
	var out *domain.PayrollLiability
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockLiabilityTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if err := checkVersion(cur, req.ExpectedVersion); err != nil {
			return err
		}
		if cur.State == domain.LiabReconciled || cur.State == domain.LiabSuperseded {
			return domain.ErrLiabilityState
		}
		owed, err := owedOf(cur)
		if err != nil {
			return err
		}
		settled, err := domain.ParseDecimal(cur.SettledAmount)
		if err != nil {
			return err
		}
		newOwed := new(big.Rat).Add(owed, delta)
		if newOwed.Cmp(settled) < 0 {
			return domain.ErrAdjustBelowSettled
		}
		adj, err := domain.ParseDecimal(cur.AdjustmentTotal)
		if err != nil {
			return err
		}
		newAdj := new(big.Rat).Add(adj, delta)
		state := cur.State
		switch {
		case newOwed.Sign() == 0 && settled.Sign() == 0:
			state = domain.LiabSuperseded // nothing is owed any more
		case settled.Sign() > 0 && settled.Cmp(newOwed) == 0:
			state = domain.LiabSettled
		case cur.State == domain.LiabSettled && settled.Cmp(newOwed) < 0:
			state = domain.LiabPartiallySettled
		}
		if _, err := tx.Exec(ctx, `INSERT INTO wfp_liability_adjustments (adjustment_id, tenant_id, liability_id, delta, reason, evidence_ref, created_by)
			VALUES ($1::uuid, $2, $3::uuid, $4::numeric, $5, $6, $7)`, newUUID(), tenantID, id, req.Delta, req.Reason, req.EvidenceRef, actor); err != nil {
			return err
		}
		out, err = updateLiabilityTx(ctx, tx, tenantID, id, `state = $3, adjustment_total = $4::numeric`, state, domain.FormatDecimal(newAdj))
		if err != nil {
			return err
		}
		return enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "ADJUSTED", Liability: out, Detail: map[string]any{
			"delta": req.Delta, "reason": req.Reason, "evidence_ref": req.EvidenceRef, "outstanding_amount": out.OutstandingAmount,
		}})
	})
	return out, err
}

// DisputeLiability marks a liability DISPUTED (orthogonal to its lifecycle state);
// ClearLiabilityDispute removes it. A disputed liability cannot be scheduled.
func (s *PgStore) DisputeLiability(ctx context.Context, id string, expectedVersion int, reason string, mk LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	return s.setDispute(ctx, id, expectedVersion, reason, true, mk)
}

func (s *PgStore) ClearLiabilityDispute(ctx context.Context, id string, expectedVersion int, mk LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	return s.setDispute(ctx, id, expectedVersion, "", false, mk)
}

func (s *PgStore) setDispute(ctx context.Context, id string, expectedVersion int, reason string, dispute bool, mk LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PayrollLiability
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockLiabilityTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if err := checkVersion(cur, expectedVersion); err != nil {
			return err
		}
		if cur.State == domain.LiabSuperseded || cur.State == domain.LiabReconciled {
			return domain.ErrLiabilityState
		}
		if dispute {
			if cur.ExceptionState != domain.LiabExcNone {
				return domain.ErrLiabilityState
			}
			out, err = updateLiabilityTx(ctx, tx, tenantID, id, `exception_state = 'DISPUTED', dispute_reason = $3`, reason)
		} else {
			if cur.ExceptionState != domain.LiabExcDisputed {
				return domain.ErrLiabilityState
			}
			out, err = updateLiabilityTx(ctx, tx, tenantID, id, `exception_state = 'NONE', dispute_reason = NULL`)
		}
		if err != nil {
			return err
		}
		kind := "DISPUTE_CLEARED"
		if dispute {
			kind = "DISPUTED"
		}
		return enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: kind, Liability: out, Detail: map[string]any{"reason": reason}})
	})
	return out, err
}

type SupersedeOutcome struct {
	Superseded      []domain.PayrollLiability `json:"superseded"`
	NeedsAdjustment []domain.PayrollLiability `json:"needs_adjustment"`
}

// SupersedeLiabilitiesForRun follows a result's supersession: every unsettled
// liability of the superseded result becomes SUPERSEDED; one with settlements is
// returned for an explicit adjustment instead (money already moved). The result's
// open exceptions are resolved as RESULT_SUPERSEDED. The replacement result's
// liabilities are recognized separately, from that result.
func (s *PgStore) SupersedeLiabilitiesForRun(ctx context.Context, runRefID string, mk LiabilityEventBuilder) (*SupersedeOutcome, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := &SupersedeOutcome{Superseded: []domain.PayrollLiability{}, NeedsAdjustment: []domain.PayrollLiability{}}
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
		live, err := queryLiabilities(ctx, tx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
			WHERE tenant_id = $1 AND run_ref_id = $2::uuid AND state NOT IN ('SUPERSEDED', 'RECONCILED') ORDER BY class_code FOR UPDATE`, tenantID, runRefID)
		if err != nil {
			return err
		}
		for _, l := range live {
			if l.SettledAmount != domain.FormatDecimal(new(big.Rat)) {
				out.NeedsAdjustment = append(out.NeedsAdjustment, l)
				continue
			}
			upd, err := updateLiabilityTx(ctx, tx, tenantID, l.LiabilityID, `state = 'SUPERSEDED'`)
			if err != nil {
				return err
			}
			out.Superseded = append(out.Superseded, *upd)
			if err := enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "SUPERSEDED", Liability: upd, Detail: map[string]any{"reason": "RESULT_SUPERSEDED"}}); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE wfp_liability_exceptions SET state = 'RESOLVED', resolution = 'RESULT_SUPERSEDED', resolved_at = now()
			WHERE tenant_id = $1 AND state = 'OPEN' AND result_id IN (SELECT result_id FROM wfp_payroll_results WHERE run_ref_id = $2::uuid)`, tenantID, runRefID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── reconciliation to the posting ────────────────────────────────────────────

type LiabilityReconciliation struct {
	RunRefID string                 `json:"run_ref_id"`
	BridgeID string                 `json:"bridge_id"`
	Matched  bool                   `json:"matched"`
	Lines    []domain.ReconcileLine `json:"lines"`
}

// ReconcileLiabilities compares, per payable mapping key, the liabilities recognized
// from a result with the credits its POSTED payroll journal carried. A match marks the
// liabilities MATCHED (a fully settled one becomes RECONCILED); a difference marks them
// MISMATCH, raises a LIABILITY_GL_MISMATCH exception and emits the failure event. It
// compares the frozen posting proposal that the ledger accepted; it does not read live
// ledger account balances.
func (s *PgStore) ReconcileLiabilities(ctx context.Context, runRefID string, mk LiabilityEventBuilder) (*LiabilityReconciliation, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := &LiabilityReconciliation{RunRefID: runRefID}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var resultID, entityID string
		if err := tx.QueryRow(ctx, `SELECT result_id::text, legal_entity_id FROM wfp_payroll_results WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, runRefID).
			Scan(&resultID, &entityID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrResultNotFound
			}
			return err
		}
		bridge, err := scanBridge(tx.QueryRow(ctx, `SELECT `+bridgeCols+` FROM wfp_payroll_accounting_bridges
			WHERE tenant_id = $1 AND result_id = $2::uuid AND posting_purpose = 'PAYROLL_RESULT' AND state = 'POSTED'`, tenantID, resultID))
		if errors.Is(err, domain.ErrBridgeNotFound) {
			return domain.ErrNoPostedAccounting
		}
		if err != nil {
			return err
		}
		out.BridgeID = bridge.BridgeID

		posted := map[string]*big.Rat{}
		for _, ln := range bridge.Lines {
			if !strings.HasSuffix(ln.MappingKey, "_PAYABLE") || ln.MappingKey == domain.KeyNetPayable {
				continue
			}
			c, err := domain.ParseDecimal(ln.Credit)
			if err != nil {
				return err
			}
			if posted[ln.MappingKey] == nil {
				posted[ln.MappingKey] = new(big.Rat)
			}
			posted[ln.MappingKey].Add(posted[ln.MappingKey], c)
		}
		libs, err := queryLiabilities(ctx, tx, `SELECT `+liabCols+` FROM wfp_payroll_liabilities
			WHERE tenant_id = $1 AND result_id = $2::uuid AND state <> 'SUPERSEDED' ORDER BY class_code FOR UPDATE`, tenantID, resultID)
		if err != nil {
			return err
		}
		recognized := map[string]*big.Rat{}
		for _, l := range libs {
			for kind, amt := range l.KindAmounts {
				a, err := domain.ParseDecimal(amt)
				if err != nil {
					return err
				}
				key := domain.PayableKeyOf(kind)
				if recognized[key] == nil {
					recognized[key] = new(big.Rat)
				}
				recognized[key].Add(recognized[key], a)
			}
		}
		out.Lines = domain.ReconcileKeys(recognized, posted)
		out.Matched = true
		var bad []string
		for _, ln := range out.Lines {
			if !ln.Matches {
				out.Matched = false
				bad = append(bad, fmt.Sprintf("%s: liabilities %s, posted %s", ln.MappingKey, ln.Liabilities, ln.Posted))
			}
		}
		detail, err := json.Marshal(out)
		if err != nil {
			return err
		}
		recon := "MISMATCH"
		if out.Matched {
			recon = "MATCHED"
		}
		for _, l := range libs {
			if l.State == domain.LiabReconciled {
				continue
			}
			set := `reconciliation_state = $3, reconciled_at = now(), reconciliation_detail = $4::jsonb`
			if out.Matched && l.State == domain.LiabSettled {
				set += `, state = 'RECONCILED'`
			}
			upd, err := updateLiabilityTx(ctx, tx, tenantID, l.LiabilityID, set, recon, string(detail))
			if err != nil {
				return err
			}
			if !out.Matched {
				if err := enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "RECONCILIATION_FAILED", Liability: upd, Detail: map[string]any{"differences": bad}}); err != nil {
					return err
				}
			}
		}
		if out.Matched {
			_, err = tx.Exec(ctx, `UPDATE wfp_liability_exceptions SET state = 'RESOLVED', resolution = 'RECONCILED', resolved_at = now()
				WHERE tenant_id = $1 AND result_id = $2::uuid AND code = $3 AND state = 'OPEN'`, tenantID, resultID, domain.ExcGLMismatch)
			return err
		}
		return raiseExceptionTx(ctx, tx, tenantID, entityID, resultID, nil, domain.ExceptionDraft{
			Code: domain.ExcGLMismatch, SubjectKey: bridge.BridgeID,
			Detail: "recognized liabilities differ from the posted payroll journal: " + strings.Join(bad, "; "),
		}, mk)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
