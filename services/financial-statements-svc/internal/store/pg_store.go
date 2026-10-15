// Package store is the Postgres implementation of service.Store.
//
// Every transaction installs app.tenant_id as a transaction-local setting;
// both tables FORCE row-level security on it, and every query ALSO filters
// on tenant_id explicitly — defence in depth, matching the rest of this
// platform's services (RLS alone is insufficient when the runtime pool
// connects as a non-superuser role; it is still not the ONLY thing standing
// between two tenants).
//
// Every state-changing method enqueues its outbox event inside the same
// transaction as the row update (services/eventing, ZS-EVENT-001 §6) — see
// enqueueEvent.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/financial-statements-svc/internal/domain"
	"zoiko.io/financial-statements-svc/internal/service"
)

type PgStore struct {
	pool *pgxpool.Pool

	// eventRegion is the residencyregion stamped on every emitted event.
	// Empty means no region was configured, and every event-emitting write
	// then fails (envelope.New refuses it) rather than emitting an event
	// with a guessed region.
	eventRegion string
}

type Option func(*PgStore)

func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

func New(pool *pgxpool.Pool, opts ...Option) *PgStore {
	s := &PgStore{pool: pool}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ service.Store = (*PgStore)(nil)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// withRLS runs fn in one transaction with app.tenant_id installed.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return domain.Errf(domain.CodeContextInvalid, "tenant context is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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

const setColumns = `
	statement_set_id, tenant_id, legal_entity_id, reporting_period, framework,
	presentation_currency, source_snapshot_id, source_snapshot_hash, mapping_version,
	status, version, validation_errors, supersedes_set_id, superseded_by_set_id,
	created_at, created_by_principal_id,
	last_refreshed_at, last_refreshed_by_principal_id,
	validated_at,
	submitted_for_review_at, submitted_for_review_by_principal_id,
	approved_at, approved_by_principal_id,
	certified_at, certified_by_principal_id, certification_reason,
	published_at, superseded_at`

func scanSet(row pgx.Row, set *domain.FinancialStatementSet) error {
	var validationErrorsJSON []byte
	err := row.Scan(
		&set.StatementSetID, &set.TenantID, &set.LegalEntityID, &set.ReportingPeriod, &set.Framework,
		&set.PresentationCurrency, &set.SourceSnapshotID, &set.SourceSnapshotHash, &set.MappingVersion,
		&set.Status, &set.Version, &validationErrorsJSON, &set.SupersedesSetID, &set.SupersededBySetID,
		&set.CreatedAt, &set.CreatedByPrincipalID,
		&set.LastRefreshedAt, &set.LastRefreshedByPrincipalID,
		&set.ValidatedAt,
		&set.SubmittedForReviewAt, &set.SubmittedForReviewByPrincipalID,
		&set.ApprovedAt, &set.ApprovedByPrincipalID,
		&set.CertifiedAt, &set.CertifiedByPrincipalID, &set.CertificationReason,
		&set.PublishedAt, &set.SupersededAt,
	)
	if err != nil {
		return err
	}
	if len(validationErrorsJSON) > 0 {
		return json.Unmarshal(validationErrorsJSON, &set.ValidationErrors)
	}
	return nil
}

// loadForUpdate reads one set under FOR UPDATE: every state-changing method
// locks the row before checking its version/status, so the check and the
// write can never race against a concurrent command on the same set.
func loadForUpdate(ctx context.Context, tx pgx.Tx, tenantID, setID string) (*domain.FinancialStatementSet, error) {
	row := tx.QueryRow(ctx, `SELECT `+setColumns+` FROM financial_statement_sets
		WHERE statement_set_id = $1 AND tenant_id = $2 FOR UPDATE`, setID, tenantID)
	var set domain.FinancialStatementSet
	if err := scanSet(row, &set); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrStatementSetNotFound
		}
		return nil, err
	}
	return &set, nil
}

func checkVersion(set *domain.FinancialStatementSet, expectedVersion int64) error {
	if set.Version != expectedVersion {
		return domain.Errf(domain.CodeVersionConflict, "statement set %q changed concurrently (expected version %d, found %d)", set.StatementSetID, expectedVersion, set.Version)
	}
	return nil
}

// enqueueEvent writes one lifecycle event to the transactional outbox inside
// tx, the transaction that made the change it reports.
func (s *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, fact, legacyType, actorID, correlationID string, set *domain.FinancialStatementSet, extra map[string]any) error {
	data := map[string]any{
		"statement_set_id":   set.StatementSetID,
		"tenant_id":          set.TenantID,
		"legal_entity_id":    set.LegalEntityID,
		"reporting_period":   set.ReportingPeriod,
		"framework":          set.Framework,
		"source_snapshot_id": set.SourceSnapshotID,
		"status":             string(set.Status),
		"version":            set.Version,
	}
	for k, v := range extra {
		data[k] = v
	}
	env, err := envelope.New(envelope.Spec{
		Type:             "com.zoikosuite.reporting.statement-set." + fact,
		LegacyType:       legacyType,
		Service:          "financial-statements-svc",
		SchemaVersion:    "1.0.0",
		OccurredAt:       time.Now().UTC(),
		TenantID:         set.TenantID,
		LegalEntityID:    set.LegalEntityID,
		AggregateType:    "financial-statement-set",
		AggregateID:      set.StatementSetID,
		AggregateVersion: &set.Version,
		CorrelationID:    correlationID,
		ActorID:          actorID,
		ResidencyRegion:  s.eventRegion,
		// Reported financial statements are confidential financial records.
		Classification: envelope.Confidential,
		Data:           data,
	})
	if err != nil {
		return fmt.Errorf("build statement-set.%s event: %w", fact, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue statement-set.%s event: %w", fact, err)
	}
	return nil
}

// ── commands ─────────────────────────────────────────────────────────────

func (s *PgStore) CreateSet(ctx context.Context, tenantID string, set *domain.FinancialStatementSet, correlationID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO financial_statement_sets (
				statement_set_id, tenant_id, legal_entity_id, reporting_period, framework,
				presentation_currency, source_snapshot_id, source_snapshot_hash, mapping_version,
				status, version, supersedes_set_id, created_at, created_by_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		`, set.StatementSetID, tenantID, set.LegalEntityID, set.ReportingPeriod, set.Framework,
			set.PresentationCurrency, set.SourceSnapshotID, set.SourceSnapshotHash, set.MappingVersion,
			string(set.Status), set.Version, set.SupersedesSetID, set.CreatedAt, set.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.Errf(domain.CodeContextInvalid, "an active (non-superseded) statement set already exists for this legal entity, reporting period and framework")
			}
			return err
		}
		return s.enqueueEvent(ctx, tx, "created", "FinancialStatementSetCreated", set.CreatedByPrincipalID, correlationID, set, nil)
	})
}

func (s *PgStore) RefreshFacts(ctx context.Context, tenantID, setID string, expectedVersion int64, facts []domain.StatementFact, actorID, correlationID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		set, err := loadForUpdate(ctx, tx, tenantID, setID)
		if err != nil {
			return err
		}
		if err := checkVersion(set, expectedVersion); err != nil {
			return err
		}
		if _, err := domain.Next(domain.CmdRefreshDraft, set.Status); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `DELETE FROM statement_facts WHERE statement_set_id = $1 AND tenant_id = $2`, setID, tenantID); err != nil {
			return fmt.Errorf("clear prior facts: %w", err)
		}
		for _, f := range facts {
			sources, err := marshalSourceAccounts(f.SourceAccounts)
			if err != nil {
				return fmt.Errorf("encode source accounts for fact %s: %w", f.AccountCode, err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO statement_facts (
					fact_id, tenant_id, statement_set_id, statement_type, category,
					account_code, account_name, amount, source_accounts, created_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9::jsonb,$10)
			`, f.FactID, tenantID, setID, string(f.StatementType), string(f.Category),
				f.AccountCode, f.AccountName, f.Amount, sources, f.CreatedAt); err != nil {
				return fmt.Errorf("insert fact %s: %w", f.AccountCode, err)
			}
		}

		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets
			SET version = version + 1, last_refreshed_at = $1, last_refreshed_by_principal_id = $2
			WHERE statement_set_id = $3 AND tenant_id = $4
		`, now, actorID, setID, tenantID); err != nil {
			return fmt.Errorf("update refresh timestamp: %w", err)
		}
		set.Version++
		set.LastRefreshedAt = &now
		set.LastRefreshedByPrincipalID = &actorID
		return s.enqueueEvent(ctx, tx, "draft-refreshed", "FinancialStatementDraftRefreshed", actorID, correlationID, set, map[string]any{"fact_count": len(facts)})
	})
}

func (s *PgStore) Validate(ctx context.Context, tenantID, setID string, expectedVersion int64, validationErrors []string, correlationID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		set, err := loadForUpdate(ctx, tx, tenantID, setID)
		if err != nil {
			return err
		}
		if err := checkVersion(set, expectedVersion); err != nil {
			return err
		}
		if _, err := domain.Next(domain.CmdValidate, set.Status); err != nil {
			return err
		}

		var errsJSON []byte
		if len(validationErrors) > 0 {
			errsJSON, err = json.Marshal(validationErrors)
			if err != nil {
				return fmt.Errorf("marshal validation errors: %w", err)
			}
		}

		now := time.Now().UTC()
		if len(validationErrors) == 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE financial_statement_sets
				SET status = $1, version = version + 1, validation_errors = NULL, validated_at = $2
				WHERE statement_set_id = $3 AND tenant_id = $4
			`, string(domain.StateValidated), now, setID, tenantID); err != nil {
				return fmt.Errorf("mark validated: %w", err)
			}
			set.Status = domain.StateValidated
			set.Version++
			set.ValidatedAt = &now
			set.ValidationErrors = nil
			return s.enqueueEvent(ctx, tx, "validated", "FinancialStatementsValidated", "", correlationID, set, nil)
		}

		// A failed validation is a legitimate recorded outcome, not a
		// transition — the set stays in DRAFT, version still bumps because
		// the row changed, no lifecycle event (nothing material happened).
		if _, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets
			SET version = version + 1, validation_errors = $1
			WHERE statement_set_id = $2 AND tenant_id = $3
		`, errsJSON, setID, tenantID); err != nil {
			return fmt.Errorf("record validation errors: %w", err)
		}
		return nil
	})
}

func (s *PgStore) SubmitForReview(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	return s.transition(ctx, tenantID, setID, expectedVersion, domain.CmdSubmitForReview, func(set *domain.FinancialStatementSet, tx pgx.Tx, now time.Time) error {
		_, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets
			SET status = $1, version = version + 1, submitted_for_review_at = $2, submitted_for_review_by_principal_id = $3
			WHERE statement_set_id = $4 AND tenant_id = $5
		`, string(domain.StateInReview), now, actorID, setID, tenantID)
		if err != nil {
			return err
		}
		set.SubmittedForReviewAt = &now
		set.SubmittedForReviewByPrincipalID = &actorID
		return nil
	}, "submitted-for-review", "FinancialStatementsSubmittedForReview", actorID, correlationID, nil)
}

func (s *PgStore) RequestChanges(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	return s.transition(ctx, tenantID, setID, expectedVersion, domain.CmdRequestChanges, func(set *domain.FinancialStatementSet, tx pgx.Tx, now time.Time) error {
		_, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets SET status = $1, version = version + 1
			WHERE statement_set_id = $2 AND tenant_id = $3
		`, string(domain.StateDraft), setID, tenantID)
		return err
	}, "changes-requested", "FinancialStatementsChangesRequested", actorID, correlationID, nil)
}

func (s *PgStore) Approve(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		set, err := loadForUpdate(ctx, tx, tenantID, setID)
		if err != nil {
			return err
		}
		if err := checkVersion(set, expectedVersion); err != nil {
			return err
		}
		if _, err := domain.Next(domain.CmdApprove, set.Status); err != nil {
			return err
		}
		// Re-checked here, inside the lock: the service layer already
		// checked this, but the row could have changed (a concurrent
		// RequestChanges + RefreshDraft replacing the preparer) between that
		// check and this transaction acquiring the lock.
		if actorID == set.Preparer() {
			return domain.Errf(domain.CodeSoDDenied, "the preparer of a statement set may not also approve it")
		}
		if authored, err := authoredDisclosure(ctx, tx, tenantID, setID, actorID); err != nil {
			return fmt.Errorf("check disclosure authorship: %w", err)
		} else if authored {
			return domain.Errf(domain.CodeSoDDenied, "the author of a disclosure on a statement set may not also approve it")
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets
			SET status = $1, version = version + 1, approved_at = $2, approved_by_principal_id = $3
			WHERE statement_set_id = $4 AND tenant_id = $5
		`, string(domain.StateApproved), now, actorID, setID, tenantID); err != nil {
			return err
		}
		set.Status = domain.StateApproved
		set.Version++
		set.ApprovedAt = &now
		set.ApprovedByPrincipalID = &actorID
		return s.enqueueEvent(ctx, tx, "approved", "FinancialStatementsApproved", actorID, correlationID, set, nil)
	})
}

func (s *PgStore) Certify(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, reason, correlationID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		set, err := loadForUpdate(ctx, tx, tenantID, setID)
		if err != nil {
			return err
		}
		if err := checkVersion(set, expectedVersion); err != nil {
			return err
		}
		if _, err := domain.Next(domain.CmdCertify, set.Status); err != nil {
			return err
		}
		if actorID == set.Preparer() {
			return domain.Errf(domain.CodeSoDDenied, "the preparer of a statement set may not also certify it")
		}
		if authored, err := authoredDisclosure(ctx, tx, tenantID, setID, actorID); err != nil {
			return fmt.Errorf("check disclosure authorship: %w", err)
		} else if authored {
			return domain.Errf(domain.CodeSoDDenied, "the author of a disclosure on a statement set may not also certify it")
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets
			SET status = $1, version = version + 1, certified_at = $2, certified_by_principal_id = $3, certification_reason = $4
			WHERE statement_set_id = $5 AND tenant_id = $6
		`, string(domain.StateCertified), now, actorID, reason, setID, tenantID); err != nil {
			return err
		}
		set.Status = domain.StateCertified
		set.Version++
		set.CertifiedAt = &now
		set.CertifiedByPrincipalID = &actorID
		set.CertificationReason = &reason
		return s.enqueueEvent(ctx, tx, "certified", "FinancialStatementsCertified", actorID, correlationID, set, nil)
	})
}

func (s *PgStore) Publish(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	return s.transition(ctx, tenantID, setID, expectedVersion, domain.CmdPublish, func(set *domain.FinancialStatementSet, tx pgx.Tx, now time.Time) error {
		_, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets SET status = $1, version = version + 1, published_at = $2
			WHERE statement_set_id = $3 AND tenant_id = $4
		`, string(domain.StatePublished), now, setID, tenantID)
		if err != nil {
			return err
		}
		set.PublishedAt = &now
		return nil
	}, "published", "FinancialStatementsPublished", actorID, correlationID, nil)
}

// transition is the shared skeleton for commands whose SoD rule is "none
// beyond authorization-svc's own grant" — lock, check version, check the
// legal transition, mutate, enqueue, commit. Approve and Certify don't use
// it because they carry an extra in-transaction SoD check.
func (s *PgStore) transition(
	ctx context.Context, tenantID, setID string, expectedVersion int64, cmd domain.Command,
	mutate func(set *domain.FinancialStatementSet, tx pgx.Tx, now time.Time) error,
	fact, legacyType, actorID, correlationID string, extra map[string]any,
) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		set, err := loadForUpdate(ctx, tx, tenantID, setID)
		if err != nil {
			return err
		}
		if err := checkVersion(set, expectedVersion); err != nil {
			return err
		}
		to, err := domain.Next(cmd, set.Status)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := mutate(set, tx, now); err != nil {
			return err
		}
		set.Status = to
		set.Version++
		return s.enqueueEvent(ctx, tx, fact, legacyType, actorID, correlationID, set, extra)
	})
}

func (s *PgStore) Supersede(ctx context.Context, tenantID, oldSetID string, expectedOldVersion int64, newSet *domain.FinancialStatementSet, actorID, correlationID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		old, err := loadForUpdate(ctx, tx, tenantID, oldSetID)
		if err != nil {
			return err
		}
		if err := checkVersion(old, expectedOldVersion); err != nil {
			return err
		}
		if !domain.CanSupersede(old.Status) {
			return domain.Errf(domain.CodeInvalidTransition, "a statement set in state %s cannot be superseded", old.Status)
		}

		// Three statements, in this order, to satisfy two constraints that
		// pull in opposite directions: the partial unique index (at most one
		// non-SUPERSEDED row per entity/period/framework) requires old to
		// stop being active BEFORE new is inserted active; the
		// superseded_by_set_id foreign key requires new to already exist
		// before old can point at it. So: (1) mark old SUPERSEDED with no
		// forward pointer yet, (2) insert new (now the only active row), (3)
		// backfill old's pointer to new, now that new exists.
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets
			SET status = $1, version = version + 1, superseded_at = $2
			WHERE statement_set_id = $3 AND tenant_id = $4
		`, string(domain.StateSuperseded), now, oldSetID, tenantID); err != nil {
			return fmt.Errorf("mark old set superseded: %w", err)
		}
		old.Status = domain.StateSuperseded
		old.Version++
		old.SupersededAt = &now

		if _, err := tx.Exec(ctx, `
			INSERT INTO financial_statement_sets (
				statement_set_id, tenant_id, legal_entity_id, reporting_period, framework,
				presentation_currency, source_snapshot_id, source_snapshot_hash, mapping_version,
				status, version, supersedes_set_id, created_at, created_by_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		`, newSet.StatementSetID, tenantID, newSet.LegalEntityID, newSet.ReportingPeriod, newSet.Framework,
			newSet.PresentationCurrency, newSet.SourceSnapshotID, newSet.SourceSnapshotHash, newSet.MappingVersion,
			string(newSet.Status), newSet.Version, newSet.SupersedesSetID, newSet.CreatedAt, newSet.CreatedByPrincipalID); err != nil {
			return fmt.Errorf("insert replacement set: %w", err)
		}
		if err := s.enqueueEvent(ctx, tx, "created", "FinancialStatementSetCreated", actorID, correlationID, newSet, map[string]any{"supersedes_set_id": oldSetID}); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE financial_statement_sets SET superseded_by_set_id = $1
			WHERE statement_set_id = $2 AND tenant_id = $3
		`, newSet.StatementSetID, oldSetID, tenantID); err != nil {
			return fmt.Errorf("backfill old set's replacement pointer: %w", err)
		}
		old.SupersededBySetID = &newSet.StatementSetID
		old.SupersededBySetID = &newSet.StatementSetID
		return s.enqueueEvent(ctx, tx, "superseded", "FinancialStatementsSuperseded", actorID, correlationID, old, map[string]any{"superseded_by_set_id": newSet.StatementSetID})
	})
}

// ── queries ──────────────────────────────────────────────────────────────

func (s *PgStore) GetSet(ctx context.Context, tenantID, setID string) (*domain.FinancialStatementSet, error) {
	var set domain.FinancialStatementSet
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+setColumns+` FROM financial_statement_sets WHERE statement_set_id = $1 AND tenant_id = $2`, setID, tenantID)
		return scanSet(row, &set)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrStatementSetNotFound
	}
	if err != nil {
		return nil, err
	}
	return &set, nil
}

func (s *PgStore) ListSets(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.FinancialStatementSet, error) {
	var out []domain.FinancialStatementSet
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + setColumns + ` FROM financial_statement_sets WHERE tenant_id = $1 AND legal_entity_id = $2`
		args := []any{tenantID, legalEntityID}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var set domain.FinancialStatementSet
			if err := scanSet(rows, &set); err != nil {
				return err
			}
			out = append(out, set)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const factColumns = `fact_id, tenant_id, statement_set_id, statement_type, category, account_code, account_name, amount, source_accounts, created_at`

func scanFact(row pgx.Row, f *domain.StatementFact) error {
	var sources []byte
	if err := row.Scan(&f.FactID, &f.TenantID, &f.StatementSetID, &f.StatementType, &f.Category, &f.AccountCode, &f.AccountName, &f.Amount, &sources, &f.CreatedAt); err != nil {
		return err
	}
	f.SourceAccounts = nil
	if len(sources) > 0 {
		if err := json.Unmarshal(sources, &f.SourceAccounts); err != nil {
			return fmt.Errorf("decode source_accounts for fact %s: %w", f.FactID, err)
		}
	}
	if len(f.SourceAccounts) == 0 {
		f.SourceAccounts = nil
	}
	return nil
}

// marshalSourceAccounts encodes a fact's source accounts for the JSONB
// column; nil (a per-account fact) is stored as '[]', never NULL.
func marshalSourceAccounts(sources []domain.SnapshotContentAccount) (string, error) {
	if len(sources) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(sources)
	return string(b), err
}

func (s *PgStore) GetFacts(ctx context.Context, tenantID, setID string) ([]domain.StatementFact, error) {
	var out []domain.StatementFact
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+factColumns+` FROM statement_facts WHERE statement_set_id = $1 AND tenant_id = $2 ORDER BY account_code`, setID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f domain.StatementFact
			if err := scanFact(rows, &f); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetFact(ctx context.Context, tenantID, factID string) (*domain.StatementFact, error) {
	var f domain.StatementFact
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+factColumns+` FROM statement_facts WHERE fact_id = $1 AND tenant_id = $2`, factID, tenantID)
		return scanFact(row, &f)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFactNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ── disclosures ──────────────────────────────────────────────────────────

const disclosureColumns = `disclosure_id, tenant_id, statement_set_id, sequence, title, body, added_by_principal_id, added_at, COALESCE(idempotency_key, '')`

func scanDisclosure(row pgx.Row, d *domain.DisclosureInstance) error {
	return row.Scan(&d.DisclosureID, &d.TenantID, &d.StatementSetID, &d.Sequence, &d.Title, &d.Body, &d.AddedByPrincipalID, &d.AddedAt, &d.IdempotencyKey)
}

func (s *PgStore) AddDisclosure(ctx context.Context, tenantID, setID string, expectedVersion int64, d *domain.DisclosureInstance, correlationID string) (*domain.DisclosureInstance, bool, error) {
	var stored *domain.DisclosureInstance
	var replayed bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// The parent row lock serializes every add on this set with every
		// lifecycle command, so the DRAFT check, the sequence number and the
		// insert cannot interleave with a concurrent VALIDATE or another add.
		set, err := loadForUpdate(ctx, tx, tenantID, setID)
		if err != nil {
			return err
		}
		if d.IdempotencyKey != "" {
			var prior domain.DisclosureInstance
			err := scanDisclosure(tx.QueryRow(ctx, `SELECT `+disclosureColumns+` FROM statement_disclosures
				WHERE statement_set_id = $1 AND tenant_id = $2 AND idempotency_key = $3`, setID, tenantID, d.IdempotencyKey), &prior)
			switch {
			case err == nil:
				if !prior.SameContent(d.Title, d.Body) || prior.AddedByPrincipalID != d.AddedByPrincipalID {
					return domain.Errf(domain.CodeContextInvalid, "idempotency key %q was already used for a different disclosure on this set", d.IdempotencyKey)
				}
				stored, replayed = &prior, true
				return nil
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("check disclosure idempotency key: %w", err)
			}
		}
		if err := checkVersion(set, expectedVersion); err != nil {
			return err
		}
		if _, err := domain.Next(domain.CmdAddDisclosure, set.Status); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM statement_disclosures WHERE statement_set_id = $1 AND tenant_id = $2`,
			setID, tenantID).Scan(&d.Sequence); err != nil {
			return fmt.Errorf("next disclosure sequence: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO statement_disclosures (
				disclosure_id, tenant_id, statement_set_id, sequence, title, body,
				added_by_principal_id, added_at, idempotency_key
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9, ''))
		`, d.DisclosureID, tenantID, setID, d.Sequence, d.Title, d.Body, d.AddedByPrincipalID, d.AddedAt, d.IdempotencyKey); err != nil {
			return fmt.Errorf("insert disclosure: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE financial_statement_sets SET version = version + 1 WHERE statement_set_id = $1 AND tenant_id = $2`,
			setID, tenantID); err != nil {
			return fmt.Errorf("bump set version: %w", err)
		}
		set.Version++
		stored = d
		return s.enqueueEvent(ctx, tx, "disclosure-added", "FinancialStatementDisclosureAdded", d.AddedByPrincipalID, correlationID, set, map[string]any{
			"disclosure_id": d.DisclosureID,
			"sequence":      d.Sequence,
			"title":         d.Title,
		})
	})
	if err != nil {
		return nil, false, err
	}
	return stored, replayed, nil
}

func (s *PgStore) ListDisclosures(ctx context.Context, tenantID, setID string) ([]domain.DisclosureInstance, error) {
	var out []domain.DisclosureInstance
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+disclosureColumns+` FROM statement_disclosures WHERE statement_set_id = $1 AND tenant_id = $2 ORDER BY sequence`, setID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.DisclosureInstance
			if err := scanDisclosure(rows, &d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// authoredDisclosure reports, inside tx, whether actorID added any
// disclosure to setID: the lock-held half of the disclosure-author SoD
// check (service.denyDisclosureAuthor is the early half).
func authoredDisclosure(ctx context.Context, tx pgx.Tx, tenantID, setID, actorID string) (bool, error) {
	var authored bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM statement_disclosures WHERE statement_set_id = $1 AND tenant_id = $2 AND added_by_principal_id = $3)`,
		setID, tenantID, actorID).Scan(&authored)
	return authored, err
}
