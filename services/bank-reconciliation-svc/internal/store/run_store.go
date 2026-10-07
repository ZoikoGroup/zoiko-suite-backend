package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// scanRun reads one reconciliation_runs row into a domain.ReconciliationRun.
func scanRun(row interface{ Scan(...any) error }, r *domain.ReconciliationRun) error {
	var status string
	if err := row.Scan(
		&r.RunID, &r.TenantID, &r.LegalEntityID, &r.BankAccountID, &r.StatementDate,
		&status, &r.PolicyID, &r.PolicyVersion, &r.MaxUnmatchedCount, &r.MaxUnmatchedPct,
		&r.PopulationID, &r.PopulationVersion,
		&r.CertifiedByPrincipalID, &r.CertifiedAt,
		&r.SupersededByRunID, &r.PriorRunID,
		&r.CreatedByPrincipalID, &r.CreatedAt, &r.UpdatedAt, &r.CorrelationID,
	); err != nil {
		return err
	}
	r.Status = domain.ReconciliationRunStatus(status)
	return nil
}

const runSelectColumns = `
	run_id, tenant_id, legal_entity_id, bank_account_id, statement_date::text,
	status, policy_id, policy_version, max_unmatched_count, max_unmatched_pct,
	population_id, population_version,
	certified_by_principal_id, certified_at,
	superseded_by_run_id, prior_run_id,
	created_by_principal_id, created_at, updated_at, correlation_id
`

func scanPolicy(row interface{ Scan(...any) error }, p *domain.ReconciliationPolicy) error {
	return row.Scan(
		&p.PolicyID, &p.TenantID, &p.LegalEntityID, &p.PolicyVersion,
		&p.MaxUnmatchedCount, &p.MaxUnmatchedPct, &p.MaxUnresolvedAmount, &p.Currency,
		&p.Rationale, &p.EffectiveFrom, &p.EffectiveTo,
		&p.CreatedByPrincipalID, &p.CreatedAt,
	)
}

const policySelectColumns = `
	policy_id, tenant_id, legal_entity_id, policy_version,
	max_unmatched_count, max_unmatched_pct, max_unresolved_amount, currency,
	rationale, effective_from::text, effective_to::text,
	created_by_principal_id, created_at
`

// activeRunStatuses are statuses that count as "active" for policy-widening
// checks: if any run in these statuses is bound to a policy, a new wider
// policy cannot be created.
var activeRunStatuses = []string{
	string(domain.RunStatusRunning),
	string(domain.RunStatusExceptionsOpen),
	string(domain.RunStatusReperformed),
	string(domain.RunStatusReadyForCertification),
}

// terminalRunStatuses are statuses where a new run for the same account+date
// may be started (the prior run is done).
var terminalRunStatuses = []string{
	string(domain.RunStatusCertified),
	string(domain.RunStatusFailed),
	string(domain.RunStatusSuperseded),
}

// ── StartRun ─────────────────────────────────────────────────────────────────

// StartRun creates a new DRAFT run. Idempotent on (tenant_id,
// bank_account_id, statement_date, correlation_id): a retry with the same
// correlation_id returns the existing run unchanged (created=false).
// Returns ErrRunAlreadyExists if a non-terminal run already exists for
// the account+date with a DIFFERENT correlation_id.
func (s *PgStore) StartRun(ctx context.Context, tenantID string, req domain.StartRunRequest, principalID string) (*domain.ReconciliationRun, bool, error) {
	var run domain.ReconciliationRun
	var created bool

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Check for existing active run for same account+date.
		row := tx.QueryRow(ctx, `
			SELECT `+runSelectColumns+`
			FROM reconciliation_runs
			WHERE tenant_id = $1
			  AND bank_account_id = $2
			  AND statement_date = $3
			  AND status NOT IN ('CERTIFIED','FAILED','SUPERSEDED')
			ORDER BY created_at DESC
			LIMIT 1
		`, tenantID, req.BankAccountID, req.StatementDate)

		existing := domain.ReconciliationRun{}
		err := scanRun(row, &existing)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			// Active run exists.
			if existing.CorrelationID == req.CorrelationID && req.CorrelationID != "" {
				// Idempotent retry: same correlation_id → return original.
				run = existing
				created = false
				return nil
			}
			return domain.ErrRunAlreadyExists
		}

		// 2. No active run — insert a new DRAFT.
		runRow := tx.QueryRow(ctx, `
			INSERT INTO reconciliation_runs (
				tenant_id, legal_entity_id, bank_account_id, statement_date,
				status, created_by_principal_id, correlation_id
			) VALUES ($1,$2,$3,$4,'DRAFT',$5,$6)
			RETURNING `+runSelectColumns,
			tenantID, req.LegalEntityID, req.BankAccountID, req.StatementDate,
			principalID, req.CorrelationID,
		)
		if err := scanRun(runRow, &run); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, mapPgError(err)
	}
	return &run, created, nil
}

// ── GetRun ───────────────────────────────────────────────────────────────────

func (s *PgStore) GetRun(ctx context.Context, tenantID, runID string) (*domain.ReconciliationRun, error) {
	var run domain.ReconciliationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+runSelectColumns+`
			FROM reconciliation_runs
			WHERE run_id = $1 AND tenant_id = $2
		`, runID, tenantID)
		return scanRun(row, &run)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRunNotFound
		}
		return nil, mapPgError(err)
	}
	return &run, nil
}

// ── FreezePopulation ─────────────────────────────────────────────────────────

// FreezePopulation snapshots all current statement lines for the run's
// account+date into an immutable population. Transitions run DRAFT→RUNNING.
// Idempotent: if the run already has a population, returns the existing one.
func (s *PgStore) FreezePopulation(ctx context.Context, tenantID, runID, principalID, correlationID string) (*domain.ReconciliationPopulation, bool, error) {
	var pop domain.ReconciliationPopulation
	var created bool

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Load run.
		runRow := tx.QueryRow(ctx, `
			SELECT `+runSelectColumns+`
			FROM reconciliation_runs
			WHERE run_id = $1 AND tenant_id = $2
		`, runID, tenantID)
		var run domain.ReconciliationRun
		if err := scanRun(runRow, &run); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRunNotFound
			}
			return err
		}

		// 2. Already frozen → return existing population.
		if run.PopulationID != nil {
			popRow := tx.QueryRow(ctx, `
				SELECT population_id, run_id, tenant_id, legal_entity_id, bank_account_id,
				       statement_date::text, snapshot_version, line_count, total_amount_cents,
				       bank_population_hash, frozen_at, frozen_by_principal_id,
				       source_watermark, policy_id, correlation_id
				FROM reconciliation_populations
				WHERE population_id = $1 AND tenant_id = $2
			`, *run.PopulationID, tenantID)
			if err := scanPopulation(popRow, &pop); err != nil {
				return err
			}
			created = false
			return nil
		}

		if run.Status != domain.RunStatusDraft {
			return domain.ErrRunInvalidTransition
		}

		// 3. Load all statement lines for this run's scope.
		rows, err := tx.Query(ctx, `
			SELECT statement_line_id, amount, currency_code, bank_reference,
			       status, created_at, statement_date
			FROM statement_lines
			WHERE tenant_id = $1
			  AND bank_account_id = $2
			  AND statement_date = $3
		`, tenantID, run.BankAccountID, run.StatementDate)
		if err != nil {
			return err
		}
		defer rows.Close()

		var lineItems []domain.PopulationLineItem
		var totalAmountCents int64
		var latestWatermark *time.Time

		for rows.Next() {
			var lineID, currencyCode, bankRef, status string
			var amount float64
			var createdAt time.Time
			var stmtDate time.Time
			if err := rows.Scan(&lineID, &amount, &currencyCode, &bankRef, &status, &createdAt, &stmtDate); err != nil {
				return err
			}
			cents := int64(math.Round(amount * 100))
			totalAmountCents += cents

			// Row hash: deterministic canonical serialization.
			h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s", lineID, cents, currencyCode, bankRef, status)))
			rowHash := fmt.Sprintf("%x", h)

			if latestWatermark == nil || createdAt.After(*latestWatermark) {
				t := createdAt
				latestWatermark = &t
			}

			lineItems = append(lineItems, domain.PopulationLineItem{
				TenantID:        tenantID,
				StatementLineID: lineID,
				SourceSystem:    "bank-reconciliation-svc",
				SourceRecordID:  lineID,
				TransactionDate: stmtDate,
				AmountCents:     cents,
				Currency:        currencyCode,
				BankReference:   bankRef,
				LineStatus:      status,
				Included:        true,
				RowHash:         rowHash,
				SourceWatermark: &createdAt,
			})
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// 4. Compute population hash.
		popHash := domain.HashPopulationLines(lineItems)
		lineCount := len(lineItems)

		// 5. Write population row.
		popRow := tx.QueryRow(ctx, `
			INSERT INTO reconciliation_populations (
				run_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
				snapshot_version, line_count, total_amount_cents, bank_population_hash,
				frozen_by_principal_id, source_watermark, policy_id, correlation_id
			) VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11,$12)
			RETURNING population_id, run_id, tenant_id, legal_entity_id, bank_account_id,
			          statement_date::text, snapshot_version, line_count, total_amount_cents,
			          bank_population_hash, frozen_at, frozen_by_principal_id,
			          source_watermark, policy_id, correlation_id
		`,
			runID, tenantID, run.LegalEntityID, run.BankAccountID, run.StatementDate,
			lineCount, totalAmountCents, popHash,
			principalID, latestWatermark, run.PolicyID, correlationID,
		)
		if err := scanPopulation(popRow, &pop); err != nil {
			return err
		}

		// 6. Write population line items.
		for _, item := range lineItems {
			if _, err := tx.Exec(ctx, `
				INSERT INTO reconciliation_population_lines (
					population_id, tenant_id, statement_line_id, source_system,
					source_record_id, transaction_date, amount_cents, currency,
					bank_reference, line_status, included, row_hash, source_watermark
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			`,
				pop.PopulationID, tenantID, item.StatementLineID, item.SourceSystem,
				item.SourceRecordID, item.TransactionDate, item.AmountCents, item.Currency,
				item.BankReference, item.LineStatus, item.Included, item.RowHash, item.SourceWatermark,
			); err != nil {
				return err
			}
		}

		// 7. Advance run: set population_id, increment population_version, status→RUNNING.
		if _, err := tx.Exec(ctx, `
			UPDATE reconciliation_runs
			SET population_id = $1, population_version = 1,
			    status = 'RUNNING', updated_at = now()
			WHERE run_id = $2 AND tenant_id = $3
		`, pop.PopulationID, runID, tenantID); err != nil {
			return err
		}

		created = true
		return nil
	})
	if err != nil {
		return nil, false, mapPgError(err)
	}
	return &pop, created, nil
}

func scanPopulation(row interface{ Scan(...any) error }, p *domain.ReconciliationPopulation) error {
	return row.Scan(
		&p.PopulationID, &p.RunID, &p.TenantID, &p.LegalEntityID, &p.BankAccountID,
		&p.StatementDate, &p.SnapshotVersion, &p.LineCount, &p.TotalAmountCents,
		&p.BankPopulationHash, &p.FrozenAt, &p.FrozenByPrincipalID,
		&p.SourceWatermark, &p.PolicyID, &p.CorrelationID,
	)
}

// ── GetPopulation ─────────────────────────────────────────────────────────────

func (s *PgStore) GetPopulation(ctx context.Context, tenantID, populationID string) (*domain.ReconciliationPopulation, error) {
	var pop domain.ReconciliationPopulation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT population_id, run_id, tenant_id, legal_entity_id, bank_account_id,
			       statement_date::text, snapshot_version, line_count, total_amount_cents,
			       bank_population_hash, frozen_at, frozen_by_principal_id,
			       source_watermark, policy_id, correlation_id
			FROM reconciliation_populations
			WHERE population_id = $1 AND tenant_id = $2
		`, populationID, tenantID)
		return scanPopulation(row, &pop)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPopulationNotFound
		}
		return nil, mapPgError(err)
	}
	return &pop, nil
}

// ── AdvanceRunStatus ──────────────────────────────────────────────────────────

func (s *PgStore) AdvanceRunStatus(ctx context.Context, tenantID, runID string, from, to domain.ReconciliationRunStatus) error {
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE reconciliation_runs
			SET status = $1, updated_at = now()
			WHERE run_id = $2 AND tenant_id = $3 AND status = $4
		`, string(to), runID, tenantID, string(from))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrRunInvalidTransition
		}
		return nil
	})
	return mapPgError(err)
}

// ── CreatePolicy ─────────────────────────────────────────────────────────────

// CreatePolicy creates a new immutable policy version.
// The new policy CANNOT widen thresholds while an active run is bound to
// a policy with stricter values.
func (s *PgStore) CreatePolicy(ctx context.Context, tenantID string, req domain.CreatePolicyRequest, principalID string) (*domain.ReconciliationPolicy, error) {
	var pol domain.ReconciliationPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Find the highest active-run-bound policy for this legal entity.
		// If any active run is bound to a policy that is stricter than what
		// we're trying to create, reject (cannot widen during active run).
		row := tx.QueryRow(ctx, `
			SELECT p.max_unmatched_count, p.max_unmatched_pct
			FROM reconciliation_runs r
			JOIN reconciliation_policies p ON p.policy_id = r.policy_id
			WHERE r.tenant_id = $1
			  AND r.legal_entity_id = $2
			  AND r.status = ANY($3)
			  AND r.policy_id IS NOT NULL
			ORDER BY p.policy_version DESC
			LIMIT 1
		`, tenantID, req.LegalEntityID, activeRunStatuses)

		var boundCount int
		var boundPct float64
		err := row.Scan(&boundCount, &boundPct)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			// A policy is bound to an active run. Reject if new policy is wider.
			if req.MaxUnmatchedCount > boundCount || req.MaxUnmatchedPct > boundPct {
				return domain.ErrPolicyWouldWidenActiveRun
			}
		}

		// 2. Assign next version number for this tenant+legal_entity.
		var maxVersion int
		vRow := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(policy_version),0)
			FROM reconciliation_policies
			WHERE tenant_id = $1 AND legal_entity_id = $2
		`, tenantID, req.LegalEntityID)
		if err := vRow.Scan(&maxVersion); err != nil {
			return err
		}
		nextVersion := maxVersion + 1

		// 3. Insert.
		polRow := tx.QueryRow(ctx, `
			INSERT INTO reconciliation_policies (
				tenant_id, legal_entity_id, policy_version,
				max_unmatched_count, max_unmatched_pct, max_unresolved_amount,
				currency, rationale, effective_from, created_by_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			RETURNING `+policySelectColumns,
			tenantID, req.LegalEntityID, nextVersion,
			req.MaxUnmatchedCount, req.MaxUnmatchedPct, req.MaxUnresolvedAmount,
			req.Currency, req.Rationale, req.EffectiveFrom, principalID,
		)
		return scanPolicy(polRow, &pol)
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return &pol, nil
}

// ── GetCurrentPolicy ─────────────────────────────────────────────────────────

func (s *PgStore) GetCurrentPolicy(ctx context.Context, tenantID, legalEntityID string) (*domain.ReconciliationPolicy, error) {
	var pol domain.ReconciliationPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+policySelectColumns+`
			FROM reconciliation_policies
			WHERE tenant_id = $1 AND legal_entity_id = $2
			ORDER BY policy_version DESC
			LIMIT 1
		`, tenantID, legalEntityID)
		return scanPolicy(row, &pol)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPolicyNotFound
		}
		return nil, mapPgError(err)
	}
	return &pol, nil
}

// ── GetPolicy ────────────────────────────────────────────────────────────────

func (s *PgStore) GetPolicy(ctx context.Context, tenantID, policyID string) (*domain.ReconciliationPolicy, error) {
	var pol domain.ReconciliationPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+policySelectColumns+`
			FROM reconciliation_policies
			WHERE policy_id = $1 AND tenant_id = $2
		`, policyID, tenantID)
		return scanPolicy(row, &pol)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPolicyNotFound
		}
		return nil, mapPgError(err)
	}
	return &pol, nil
}

// ── BindPolicy ────────────────────────────────────────────────────────────────

// BindPolicy attaches a policy to a DRAFT run. Once the run moves past DRAFT
// the policy reference is locked.
func (s *PgStore) BindPolicy(ctx context.Context, tenantID, runID, policyID string) (*domain.ReconciliationRun, error) {
	var run domain.ReconciliationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Verify policy belongs to tenant.
		var polVersion int
		var maxCount int
		var maxPct float64
		polRow := tx.QueryRow(ctx, `
			SELECT policy_version, max_unmatched_count, max_unmatched_pct
			FROM reconciliation_policies
			WHERE policy_id = $1 AND tenant_id = $2
		`, policyID, tenantID)
		if err := polRow.Scan(&polVersion, &maxCount, &maxPct); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPolicyNotFound
			}
			return err
		}

		tag, err := tx.Exec(ctx, `
			UPDATE reconciliation_runs
			SET policy_id = $1, policy_version = $2,
			    max_unmatched_count = $3, max_unmatched_pct = $4,
			    updated_at = now()
			WHERE run_id = $5 AND tenant_id = $6 AND status = 'DRAFT'
		`, policyID, polVersion, maxCount, maxPct, runID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrRunInvalidTransition
		}

		row := tx.QueryRow(ctx, `SELECT `+runSelectColumns+` FROM reconciliation_runs WHERE run_id = $1 AND tenant_id = $2`, runID, tenantID)
		return scanRun(row, &run)
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return &run, nil
}

// ── CertifyRun ────────────────────────────────────────────────────────────────

// CertifyRun certifies the run if policy thresholds are met. Writes a
// reconciliation_certificates row and transitions run to CERTIFIED.
// Idempotent on the same run_id.
func (s *PgStore) CertifyRun(ctx context.Context, tenantID, runID, certifierPrincipalID, correlationID string) (*domain.ReconciliationCertificate, bool, error) {
	var cert domain.ReconciliationCertificate
	var created bool

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Load run.
		runRow := tx.QueryRow(ctx, `SELECT `+runSelectColumns+` FROM reconciliation_runs WHERE run_id = $1 AND tenant_id = $2`, runID, tenantID)
		var run domain.ReconciliationRun
		if err := scanRun(runRow, &run); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRunNotFound
			}
			return err
		}

		// 2. Idempotent: already certified → return existing cert.
		if run.Status == domain.RunStatusCertified {
			certRow := tx.QueryRow(ctx, `
				SELECT certificate_id, tenant_id, legal_entity_id, bank_account_id,
				       statement_date::text, matched_line_count, certified_by_principal_id,
				       certified_at, correlation_id, run_id, population_id, population_version
				FROM reconciliation_certificates
				WHERE run_id = $1 AND tenant_id = $2
			`, runID, tenantID)
			if err := certRow.Scan(
				&cert.CertificateID, &cert.TenantID, &cert.LegalEntityID, &cert.BankAccountID,
				&cert.StatementDate, &cert.MatchedLineCount, &cert.CertifiedByPrincipalID,
				&cert.CertifiedAt, &cert.CorrelationID,
				&cert.RunID, &cert.PopulationID, &cert.PopulationVersion,
			); err != nil {
				return err
			}
			created = false
			return nil
		}

		// 3. Validate state: must be RUNNING, EXCEPTIONS_OPEN, or READY_FOR_CERTIFICATION.
		switch run.Status {
		case domain.RunStatusRunning, domain.RunStatusExceptionsOpen,
			domain.RunStatusReadyForCertification, domain.RunStatusReperformed:
			// ok
		case domain.RunStatusSuperseded:
			return domain.ErrRunSuperseded
		default:
			return domain.ErrRunInvalidTransition
		}

		// 4. Population must be frozen.
		if run.PopulationID == nil {
			return domain.ErrRunPopulationNotFrozen
		}

		// 4b. Run-level SoD: the certifying principal must not be the
		// matcher of record for any MATCHED line in this run's frozen
		// population — otherwise the same person who did the matching
		// could also sign off on it. Line-level maker-checker already
		// blocks self-confirmation for an individual PENDING_CONFIRMATION
		// match (ConfirmMatch); this is the run-level analogue, closing
		// the same class of gap at certification.
		var selfCertifiedCount int
		scRow := tx.QueryRow(ctx, `
			SELECT COUNT(*)
			FROM statement_lines sl
			JOIN reconciliation_population_lines pl ON pl.statement_line_id = sl.statement_line_id
			WHERE pl.population_id = $1 AND pl.tenant_id = $2::uuid AND pl.included = TRUE
			  AND sl.status = 'MATCHED' AND sl.matched_by_principal_id = $3
		`, *run.PopulationID, tenantID, certifierPrincipalID)
		if err := scRow.Scan(&selfCertifiedCount); err != nil {
			return err
		}
		if selfCertifiedCount > 0 {
			return domain.ErrRunSelfCertificationForbidden
		}

		// 5. Count unmatched lines within the frozen population (not live statement_lines).
		var unmatchedCount int
		umRow := tx.QueryRow(ctx, `
			SELECT COUNT(*)
			FROM statement_lines sl
			JOIN reconciliation_population_lines pl ON pl.statement_line_id = sl.statement_line_id
			WHERE pl.population_id = $1 AND pl.tenant_id = $2::uuid AND pl.included = TRUE
			  AND sl.status = 'UNMATCHED'
		`, *run.PopulationID, tenantID)
		if err := umRow.Scan(&unmatchedCount); err != nil {
			return err
		}

		// 6. Count matched lines within the frozen population.
		var matchedCount int
		mRow := tx.QueryRow(ctx, `
			SELECT COUNT(*)
			FROM statement_lines sl
			JOIN reconciliation_population_lines pl ON pl.statement_line_id = sl.statement_line_id
			WHERE pl.population_id = $1 AND pl.tenant_id = $2::uuid AND pl.included = TRUE
			  AND sl.status = 'MATCHED'
		`, *run.PopulationID, tenantID)
		if err := mRow.Scan(&matchedCount); err != nil {
			return err
		}

		// 7. Policy check.
		if run.MaxUnmatchedCount >= 0 && unmatchedCount > run.MaxUnmatchedCount {
			// Check pct tolerance only if count check fails.
			totalLines := unmatchedCount + matchedCount
			if totalLines > 0 {
				unmatchedPct := float64(unmatchedCount) / float64(totalLines)
				if unmatchedPct > run.MaxUnmatchedPct {
					return domain.ErrMaterialResidualBlocked
				}
			} else {
				return domain.ErrMaterialResidualBlocked
			}
		}

		now := time.Now().UTC()

		// 8. Write certificate.
		certRow := tx.QueryRow(ctx, `
			INSERT INTO reconciliation_certificates (
				certificate_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
				matched_line_count, certified_by_principal_id, certified_at,
				correlation_id, run_id, population_id, population_version
			) VALUES (gen_random_uuid(), $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING certificate_id, tenant_id, legal_entity_id, bank_account_id,
			          statement_date::text, matched_line_count, certified_by_principal_id,
			          certified_at, correlation_id, run_id, population_id, population_version
		`,
			tenantID, run.LegalEntityID, run.BankAccountID, run.StatementDate,
			matchedCount, certifierPrincipalID, now,
			correlationID, runID, run.PopulationID, run.PopulationVersion,
		)
		if err := certRow.Scan(
			&cert.CertificateID, &cert.TenantID, &cert.LegalEntityID, &cert.BankAccountID,
			&cert.StatementDate, &cert.MatchedLineCount, &cert.CertifiedByPrincipalID,
			&cert.CertifiedAt, &cert.CorrelationID,
			&cert.RunID, &cert.PopulationID, &cert.PopulationVersion,
		); err != nil {
			return err
		}

		// 9. Advance run to CERTIFIED.
		if _, err := tx.Exec(ctx, `
			UPDATE reconciliation_runs
			SET status = 'CERTIFIED',
			    certified_by_principal_id = $1, certified_at = $2,
			    updated_at = now()
			WHERE run_id = $3 AND tenant_id = $4
		`, certifierPrincipalID, now, runID, tenantID); err != nil {
			return err
		}

		created = true
		return nil
	})
	if err != nil {
		return nil, false, mapPgError(err)
	}
	return &cert, created, nil
}

// ── SupersedeRun ──────────────────────────────────────────────────────────────

// SupersedeRun marks an existing run as SUPERSEDED and creates a new DRAFT
// run for the same account+date. Used when late data arrives after certification
// and reperformance is required. The original run and its certificate are
// preserved (append-only). Returns the new DRAFT run.
func (s *PgStore) SupersedeRun(ctx context.Context, tenantID, existingRunID, principalID, correlationID string) (*domain.ReconciliationRun, error) {
	var newRun domain.ReconciliationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Load existing run.
		runRow := tx.QueryRow(ctx, `SELECT `+runSelectColumns+` FROM reconciliation_runs WHERE run_id = $1 AND tenant_id = $2`, existingRunID, tenantID)
		var existing domain.ReconciliationRun
		if err := scanRun(runRow, &existing); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRunNotFound
			}
			return err
		}
		if existing.Status == domain.RunStatusSuperseded {
			return domain.ErrRunSuperseded
		}

		// 2. Mark existing run as SUPERSEDED.
		newRunRow := tx.QueryRow(ctx, `
			INSERT INTO reconciliation_runs (
				tenant_id, legal_entity_id, bank_account_id, statement_date,
				status, prior_run_id, created_by_principal_id, correlation_id,
				policy_id, policy_version, max_unmatched_count, max_unmatched_pct
			) VALUES ($1,$2,$3,$4,'DRAFT',$5,$6,$7,$8,$9,$10,$11)
			RETURNING `+runSelectColumns,
			tenantID, existing.LegalEntityID, existing.BankAccountID, existing.StatementDate,
			existingRunID, principalID, correlationID,
			existing.PolicyID, existing.PolicyVersion,
			existing.MaxUnmatchedCount, existing.MaxUnmatchedPct,
		)
		if err := scanRun(newRunRow, &newRun); err != nil {
			return err
		}

		// 3. Point the existing run's superseded_by_run_id at the new run.
		if _, err := tx.Exec(ctx, `
			UPDATE reconciliation_runs
			SET status = 'SUPERSEDED', superseded_by_run_id = $1, updated_at = now()
			WHERE run_id = $2 AND tenant_id = $3
		`, newRun.RunID, existingRunID, tenantID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, mapPgError(err)
	}

	s.log.Info("run superseded", zap.String("old_run_id", existingRunID), zap.String("new_run_id", newRun.RunID))
	return &newRun, nil
}

// ListUnmatchedLinesInPopulation returns every UNMATCHED statement line
// belonging to a run's frozen population — the same join CertifyRun uses
// to count unmatched lines, reused here so RunAutomaticMatching can only
// ever act on lines that are genuinely part of THIS run's snapshot, not
// on any UNMATCHED line the caller happens to name.
func (s *PgStore) ListUnmatchedLinesInPopulation(ctx context.Context, tenantID, populationID string) ([]domain.StatementLine, error) {
	var lines []domain.StatementLine
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT sl.statement_line_id, sl.tenant_id, sl.legal_entity_id, sl.bank_account_id, sl.statement_date,
			       sl.amount, sl.currency_code, sl.bank_reference, sl.status,
			       sl.matched_journal_id, sl.matched_transaction_id, sl.matched_by_principal_id, sl.matched_at,
			       sl.exception_reason, sl.flagged_by_principal_id, sl.flagged_at,
			       sl.gl_cash_account_code, sl.correlation_id, sl.created_at,
			       sl.proposed_journal_id, sl.proposed_transaction_id, sl.proposed_by_principal_id, sl.proposed_at
			FROM statement_lines sl
			JOIN reconciliation_population_lines pl ON pl.statement_line_id = sl.statement_line_id
			WHERE pl.population_id = $1 AND pl.tenant_id = $2::uuid AND pl.included = TRUE
			  AND sl.status = 'UNMATCHED'
		`, populationID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.StatementLine
			if err := scanLine(rows, &l); err != nil {
				return err
			}
			lines = append(lines, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return lines, nil
}
