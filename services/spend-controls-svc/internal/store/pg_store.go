package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/spend-controls-svc/internal/domain"
	svcmiddleware "zoiko.io/spend-controls-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// CreatePolicy sets a limit, end-dating any limit it replaces. Returns how many
// prior policies it superseded.
//
// The supersede is what makes active_flag mean anything. Before this, the column
// was written TRUE on create and never changed by any code path: creating a second
// limit for the same entity and category left BOTH rows active, while evaluation
// silently used only `ORDER BY created_at DESC LIMIT 1`. So a category could carry
// three "active" limits of which exactly one was ever enforced, and the console's
// register — which lists what it is told is active — showed limits that were not
// in force next to one that was, indistinguishably.
//
// Doing it in the same transaction as the insert matters: two operators setting a
// limit for the same category at once would otherwise both deactivate the other's
// row and leave nothing, or leave two.
func (s *PgStore) CreatePolicy(ctx context.Context, p *domain.SpendPolicy) (superseded int, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrTenantMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE spend_policies
			SET active_flag = FALSE, updated_at = $4
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND category = $3 AND active_flag = TRUE
		`, tenantID, p.LegalEntityID, p.Category, p.UpdatedAt)
		if err != nil {
			return err
		}
		superseded = int(tag.RowsAffected())

		_, err = tx.Exec(ctx, `
			INSERT INTO spend_policies (
				spend_policy_id, tenant_id, legal_entity_id, category, period,
				threshold_amount, currency_code, active_flag, created_by_principal_id,
				created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, p.SpendPolicyID, tenantID, p.LegalEntityID, p.Category, p.Period,
			p.ThresholdAmount, p.CurrencyCode, p.ActiveFlag, p.CreatedByPrincipalID,
			p.CreatedAt, p.UpdatedAt)
		return err
	})
	if err != nil {
		return 0, err
	}
	return superseded, nil
}

// DeactivatePolicy withdraws a limit, leaving the row in place.
//
// Without this there was no way to stop governing a category at all: active_flag
// could only ever be TRUE, so the closest available action was to set an absurdly
// high threshold and pretend. The row is kept rather than deleted, so the history
// of what was once enforced — and every consumption recorded against it — survives.
//
// Returns ErrPolicyNotFound when the id names no active policy in this tenant,
// which covers "already withdrawn" and "another tenant's policy" identically.
func (s *PgStore) DeactivatePolicy(ctx context.Context, spendPolicyID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrTenantMissing
	}

	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE spend_policies
			SET active_flag = FALSE, updated_at = $3
			WHERE tenant_id = $1 AND spend_policy_id = $2 AND active_flag = TRUE
		`, tenantID, spendPolicyID, time.Now().UTC())
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrPolicyNotFound
	}
	return nil
}

// ListPolicies returns policies for the tenant, newest first.
//
// activeOnly restricts the result to limits actually in force. The console asks
// for that by default: a register headed "limits in force" must not list withdrawn
// or superseded ones beside live ones, which is exactly what it did while
// active_flag was never set FALSE by anything.
func (s *PgStore) ListPolicies(ctx context.Context, legalEntityID, category string, activeOnly bool) ([]domain.SpendPolicy, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	var out []domain.SpendPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT spend_policy_id, tenant_id, legal_entity_id, category, period,
			       threshold_amount, currency_code, active_flag, created_by_principal_id,
			       created_at, updated_at
			FROM spend_policies
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if category != "" {
			args = append(args, category)
			query += fmt.Sprintf(" AND category = $%d", len(args))
		}
		if activeOnly {
			query += " AND active_flag = TRUE"
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var p domain.SpendPolicy
			if err := rows.Scan(
				&p.SpendPolicyID, &p.TenantID, &p.LegalEntityID, &p.Category, &p.Period,
				&p.ThresholdAmount, &p.CurrencyCode, &p.ActiveFlag, &p.CreatedByPrincipalID,
				&p.CreatedAt, &p.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SumConsumption sums ALLOWED consumption amounts for a policy since a given
// window start — the running total a new spend-check's amount is added to
// before comparing against the policy's threshold.
//
// Only sums rows whose currency matches the one asked for. Nothing in this
// platform holds an FX rate, so adding a USD row to a GBP total would produce a
// number that is not an amount of money in any currency.
//
// NOTE: this is exported for reporting and tests. It is NOT what the spend-check
// path uses — see EvaluateSpend, which must sum and insert in one transaction.
func (s *PgStore) SumConsumption(ctx context.Context, spendPolicyID, currencyCode string, since time.Time) (float64, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrTenantMissing
	}

	var total float64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sumConsumptionSQL, tenantID, spendPolicyID, currencyCode, since).Scan(&total)
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// One definition, used by both SumConsumption and EvaluateSpend, so the number
// a report shows and the number the threshold is enforced against cannot drift
// apart.
const sumConsumptionSQL = `
	SELECT COALESCE(SUM(amount), 0)
	FROM spend_consumptions
	WHERE tenant_id = $1 AND spend_policy_id = $2
	  AND decision_outcome = 'ALLOWED'
	  AND currency_code = $3
	  AND recorded_at >= $4
`

// periodWindowSQL is the enforcement window, expressed in SQL.
//
// It must mean exactly what periodStart means in Go, because both answer the same
// question — how much has been committed in the window this limit governs — and a
// disagreement between them shows a reader one number while the service enforces
// another. PER_TRANSACTION has no window: every row counts, since the figure is
// then a lifetime total rather than a budget.
const periodWindowSQL = `
	p.period = 'PER_TRANSACTION'
	OR c.recorded_at >= (
		CASE p.period
			WHEN 'ANNUAL' THEN date_trunc('year',  now() AT TIME ZONE 'UTC')
			ELSE               date_trunc('month', now() AT TIME ZONE 'UTC')
		END
	) AT TIME ZONE 'UTC'
`

// PolicyUsageTotals returns committed spend and refusal counts per active policy,
// aggregated in the database.
//
// This exists because the console was computing the same figures by loading every
// consumption row for the tenant and summing them in JavaScript, which was wrong in
// two ways. It grew without bound — one row per spend check, forever, fetched on
// every page render. And it applied **no period window**, so a MONTHLY limit's
// meter showed lifetime spend while enforcement counted only the current month: the
// register could report a budget as exhausted when this month was empty and the
// next check would in fact be permitted.
func (s *PgStore) PolicyUsageTotals(ctx context.Context, legalEntityID, category string) ([]domain.PolicyUsageTotal, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	var out []domain.PolicyUsageTotal
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT p.spend_policy_id,
			       COALESCE(SUM(
			         CASE WHEN c.decision_outcome = 'ALLOWED'
			               AND c.currency_code = p.currency_code
			               AND (` + periodWindowSQL + `)
			              THEN c.amount ELSE 0 END
			       ), 0) AS consumed,
			       COUNT(CASE WHEN c.decision_outcome = 'BLOCKED' THEN 1 END) AS refused
			FROM spend_policies p
			LEFT JOIN spend_consumptions c
			       ON c.spend_policy_id = p.spend_policy_id
			      AND c.tenant_id = p.tenant_id
			WHERE p.tenant_id = $1 AND p.active_flag = TRUE
		`
		args := []any{tenantID}
		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND p.legal_entity_id = $%d", len(args))
		}
		if category != "" {
			args = append(args, category)
			query += fmt.Sprintf(" AND p.category = $%d", len(args))
		}
		query += " GROUP BY p.spend_policy_id"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.PolicyUsageTotal
			if err := rows.Scan(&t.SpendPolicyID, &t.Consumed, &t.RefusedCount); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EvaluateSpend decides whether a proposed spend fits its policy and records
// the outcome — both inside a single transaction, with the policy row locked.
//
// The lock is the point. Previously the running total was summed in one
// transaction and the consumption inserted in another, so two checks arriving
// together each saw the same prior total, each concluded it fit, and each was
// recorded: a 10,000 threshold could be overspent without limit by simultaneous
// requests. SELECT ... FOR UPDATE on the policy serialises checks against the
// same policy, which is the granularity that matters — checks against different
// policies still run concurrently.
//
// Refusals are recorded too, as BLOCKED rows excluded from the running total.
// They used to exist only as Kafka events, which left no queryable trace of a
// refused attempt and made decision_outcome a column that was always 'ALLOWED'.
func (s *PgStore) EvaluateSpend(ctx context.Context, in domain.SpendEvaluation) (*domain.SpendDecision, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	decision := &domain.SpendDecision{}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// A prior decision for this correlation id wins outright: a retry must
		// replay it, never re-evaluate and book the spend a second time.
		existing, err := findConsumptionTx(ctx, tx, tenantID, in.CorrelationID)
		if err != nil {
			return err
		}
		if existing != nil {
			decision.Outcome = existing.DecisionOutcome
			decision.Basis = "replayed_prior_decision"
			decision.ConsumptionID = existing.ConsumptionID
			decision.Replayed = true
			decision.PriorConsumption = 0
			decision.ProjectedTotal = existing.Amount
			// Reload the policy so a replay can still report the figures the
			// decision was made against, rather than a decision with no basis.
			policy, err := findPolicyForUpdateTx(ctx, tx, tenantID, existing.LegalEntityID, in.Category, existing.SpendPolicyID)
			if err != nil {
				return err
			}
			decision.Policy = policy
			return nil
		}

		policy, err := findPolicyForUpdateTx(ctx, tx, tenantID, in.LegalEntityID, in.Category, "")
		if err != nil {
			return err
		}
		if policy == nil {
			// No policy is a distinct outcome from a policy that permits. The
			// spend is allowed because nothing constrains it, and nothing is
			// recorded because there is no budget to consume.
			decision.Outcome = "ALLOWED"
			decision.Basis = "no_policy_configured"
			decision.ProjectedTotal = in.Amount
			return nil
		}
		decision.Policy = policy

		if policy.CurrencyCode != in.CurrencyCode {
			return domain.ErrCurrencyMismatch
		}

		var prior float64
		if policy.Period != "PER_TRANSACTION" {
			if err := tx.QueryRow(ctx, sumConsumptionSQL,
				tenantID, policy.SpendPolicyID, in.CurrencyCode, periodStart(policy.Period),
			).Scan(&prior); err != nil {
				return err
			}
		}
		projected := prior + in.Amount

		decision.PriorConsumption = prior
		decision.ProjectedTotal = projected

		if projected > policy.ThresholdAmount {
			decision.Outcome = "BLOCKED"
			decision.Basis = "threshold_exceeded"
		} else {
			decision.Outcome = "ALLOWED"
			decision.Basis = "within_threshold"
		}

		consumptionID := uuid.NewString()
		if _, err := tx.Exec(ctx, `
			INSERT INTO spend_consumptions (
				consumption_id, tenant_id, legal_entity_id, spend_policy_id, amount,
				currency_code, source_reference, correlation_id, decision_outcome,
				recorded_by_principal_id, recorded_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, consumptionID, tenantID, in.LegalEntityID, policy.SpendPolicyID, in.Amount,
			in.CurrencyCode, nullIfEmpty(in.SourceReference), in.CorrelationID, decision.Outcome,
			in.PrincipalID, time.Now().UTC()); err != nil {
			// A concurrent call with the same correlation_id got here first: the
			// replay read above found nothing because the winner had not
			// committed yet. Named so the caller below can replay the winner
			// rather than reporting a dead store.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.ErrConcurrentCorrelation
			}
			return err
		}
		decision.ConsumptionID = consumptionID
		return nil
	})
	// A concurrent call with the same correlation_id won the insert. Its decision
	// is the one that counts — this call must report THAT, not an error: the
	// spend was authorized exactly once, and the loser answering 503 is what made
	// a duplicate submit indistinguishable from an outage. Read the winner in a
	// fresh transaction (the failed one is aborted) and report it as a replay.
	if errors.Is(err, domain.ErrConcurrentCorrelation) {
		replayed, replayErr := s.replayConsumption(ctx, tenantID, in)
		if replayErr != nil {
			return nil, replayErr
		}
		return replayed, nil
	}
	if err != nil {
		return nil, err
	}
	return decision, nil
}

// replayConsumption re-reads the decision a concurrent caller recorded for this
// correlation id and reports it exactly as a sequential retry would.
//
// It cannot loop: the unique index guarantees the row exists once the losing
// insert has failed, so one read is enough. If it is somehow gone, that is a
// real fault rather than a race, and it is reported as one.
func (s *PgStore) replayConsumption(ctx context.Context, tenantID string, in domain.SpendEvaluation) (*domain.SpendDecision, error) {
	decision := &domain.SpendDecision{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		existing, err := findConsumptionTx(ctx, tx, tenantID, in.CorrelationID)
		if err != nil {
			return err
		}
		if existing == nil {
			return fmt.Errorf("%w: concurrent correlation_id %q resolved to no consumption row",
				domain.ErrStoreUnavailable, in.CorrelationID)
		}
		decision.Outcome = existing.DecisionOutcome
		decision.Basis = "replayed_prior_decision"
		decision.ConsumptionID = existing.ConsumptionID
		decision.Replayed = true
		decision.PriorConsumption = 0
		decision.ProjectedTotal = existing.Amount
		policy, err := findPolicyForUpdateTx(ctx, tx, tenantID, existing.LegalEntityID, in.Category, existing.SpendPolicyID)
		if err != nil {
			return err
		}
		decision.Policy = policy
		return nil
	})
	if err != nil {
		return nil, err
	}
	return decision, nil
}

// periodStart returns the start of the current enforcement window — the point
// consumption is summed from. Lives beside the sum it governs.
func periodStart(period string) time.Time {
	now := time.Now().UTC()
	if period == "ANNUAL" {
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC) // MONTHLY
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// findPolicyForUpdateTx locks the matching active policy for the duration of the
// transaction. When policyID is non-empty it looks that policy up directly,
// which is what a replay needs; otherwise it resolves the active policy for the
// entity and category.
func findPolicyForUpdateTx(ctx context.Context, tx pgx.Tx, tenantID, legalEntityID, category, policyID string) (*domain.SpendPolicy, error) {
	var (
		p    domain.SpendPolicy
		row  pgx.Row
		cols = `spend_policy_id, tenant_id, legal_entity_id, category, period,
		         threshold_amount, currency_code, active_flag, created_by_principal_id,
		         created_at, updated_at`
	)

	if policyID != "" {
		row = tx.QueryRow(ctx, `SELECT `+cols+`
			FROM spend_policies WHERE tenant_id = $1 AND spend_policy_id = $2 FOR UPDATE`,
			tenantID, policyID)
	} else {
		row = tx.QueryRow(ctx, `SELECT `+cols+`
			FROM spend_policies
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND category = $3 AND active_flag = TRUE
			ORDER BY created_at DESC
			LIMIT 1
			FOR UPDATE`,
			tenantID, legalEntityID, category)
	}

	if err := row.Scan(
		&p.SpendPolicyID, &p.TenantID, &p.LegalEntityID, &p.Category, &p.Period,
		&p.ThresholdAmount, &p.CurrencyCode, &p.ActiveFlag, &p.CreatedByPrincipalID,
		&p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func findConsumptionTx(ctx context.Context, tx pgx.Tx, tenantID, correlationID string) (*domain.SpendConsumption, error) {
	var c domain.SpendConsumption
	err := tx.QueryRow(ctx, `
		SELECT consumption_id, tenant_id, legal_entity_id, spend_policy_id, amount,
		       currency_code, COALESCE(source_reference, ''), correlation_id,
		       decision_outcome, recorded_by_principal_id, recorded_at
		FROM spend_consumptions
		WHERE tenant_id = $1 AND correlation_id = $2
	`, tenantID, correlationID).Scan(
		&c.ConsumptionID, &c.TenantID, &c.LegalEntityID, &c.SpendPolicyID, &c.Amount,
		&c.CurrencyCode, &c.SourceReference, &c.CorrelationID,
		&c.DecisionOutcome, &c.RecordedByPrincipalID, &c.RecordedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) ListConsumptions(ctx context.Context, legalEntityID, spendPolicyID string) ([]domain.SpendConsumption, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	var out []domain.SpendConsumption
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT consumption_id, tenant_id, legal_entity_id, spend_policy_id, amount,
			       currency_code, COALESCE(source_reference, ''), correlation_id,
			       decision_outcome, recorded_by_principal_id, recorded_at
			FROM spend_consumptions
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if spendPolicyID != "" {
			args = append(args, spendPolicyID)
			query += fmt.Sprintf(" AND spend_policy_id = $%d", len(args))
		}
		query += " ORDER BY recorded_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.SpendConsumption
			if err := rows.Scan(
				&c.ConsumptionID, &c.TenantID, &c.LegalEntityID, &c.SpendPolicyID, &c.Amount,
				&c.CurrencyCode, &c.SourceReference, &c.CorrelationID,
				&c.DecisionOutcome, &c.RecordedByPrincipalID, &c.RecordedAt,
			); err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
