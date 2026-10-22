package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

// Every applied MatchIntercompany attempt is one row of
// intercompany_match_attempts (migration 000007), written in UpdateMatch's
// transaction. The pair's mismatch_reason is read from the latest row
// (latestMismatchReason) rather than stored on the pair, so no attempt ever
// overwrites another.

// latestMismatchReason is the select-list expression intercompanyEntryColumns
// uses for mismatch_reason: the latest attempt's message, NULL when the
// latest attempt matched or there has been none.
const latestMismatchReason = `(
		SELECT a.message FROM intercompany_match_attempts a
		WHERE a.tenant_id = intercompany_entries.tenant_id
		  AND a.intercompany_entry_id = intercompany_entries.intercompany_entry_id
		ORDER BY a.attempt_id DESC LIMIT 1)`

func decimalText(d *domain.Decimal) *string {
	if d == nil {
		return nil
	}
	s := string(*d)
	return &s
}

func insertMatchAttempt(ctx context.Context, tx pgx.Tx, tenantID string, a *domain.MatchAttempt) error {
	var correlationID *string
	if a.CorrelationID != nil && *a.CorrelationID != "" {
		correlationID = a.CorrelationID
	}
	examined := a.ExaminedJournalIDs
	if examined == nil {
		examined = []string{}
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO intercompany_match_attempts (
			tenant_id, intercompany_entry_id, outcome, failed_rule, message,
			source_journal_id, expected_source_amount, expected_source_currency,
			observed_source_amount, observed_source_currency, observed_source_legal_entity_id,
			observed_source_status, observed_source_fiscal_period,
			target_journal_id, expected_target_amount, expected_target_currency,
			observed_target_amount, observed_target_currency, observed_target_legal_entity_id,
			observed_target_status, observed_target_fiscal_period,
			examined_journal_ids, tolerance, fiscal_period, attempted_by, correlation_id)
		VALUES ($1, $2, $3, $4, $5,
			$6, $7::numeric, $8, $9::numeric, $10, $11, $12, $13,
			$14, $15::numeric, $16, $17::numeric, $18, $19, $20, $21,
			$22, $23::numeric, $24, $25, $26)
		RETURNING attempt_id, attempted_at
	`, tenantID, a.IntercompanyEntryID, a.Outcome, a.FailedRule, a.Message,
		a.Source.JournalID, string(a.Source.ExpectedAmount), a.Source.ExpectedCurrency,
		decimalText(a.Source.ObservedAmount), a.Source.ObservedCurrency, a.Source.ObservedLegalEntityID,
		a.Source.ObservedStatus, a.Source.ObservedFiscalPeriod,
		a.Target.JournalID, string(a.Target.ExpectedAmount), a.Target.ExpectedCurrency,
		decimalText(a.Target.ObservedAmount), a.Target.ObservedCurrency, a.Target.ObservedLegalEntityID,
		a.Target.ObservedStatus, a.Target.ObservedFiscalPeriod,
		examined, string(a.Tolerance), a.FiscalPeriod, a.AttemptedBy, correlationID,
	).Scan(&a.AttemptID, &a.AttemptedAt)
	if err != nil {
		return fmt.Errorf("record match attempt: %w", err)
	}
	return nil
}

// ListMatchAttempts returns every recorded match attempt for a pair, oldest
// first.
func (s *PgStore) ListMatchAttempts(ctx context.Context, id string) ([]domain.MatchAttempt, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	out := []domain.MatchAttempt{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT attempt_id, intercompany_entry_id::text, outcome, failed_rule, message,
			       source_journal_id, expected_source_amount::text, expected_source_currency,
			       observed_source_amount::text, observed_source_currency, observed_source_legal_entity_id,
			       observed_source_status, observed_source_fiscal_period,
			       target_journal_id, expected_target_amount::text, expected_target_currency,
			       observed_target_amount::text, observed_target_currency, observed_target_legal_entity_id,
			       observed_target_status, observed_target_fiscal_period,
			       examined_journal_ids, tolerance::text, fiscal_period, attempted_by, correlation_id, attempted_at
			FROM intercompany_match_attempts
			WHERE tenant_id = $1 AND intercompany_entry_id = $2
			ORDER BY attempt_id
		`, tenantID, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.MatchAttempt
			if err := rows.Scan(&a.AttemptID, &a.IntercompanyEntryID, &a.Outcome, &a.FailedRule, &a.Message,
				&a.Source.JournalID, &a.Source.ExpectedAmount, &a.Source.ExpectedCurrency,
				&a.Source.ObservedAmount, &a.Source.ObservedCurrency, &a.Source.ObservedLegalEntityID,
				&a.Source.ObservedStatus, &a.Source.ObservedFiscalPeriod,
				&a.Target.JournalID, &a.Target.ExpectedAmount, &a.Target.ExpectedCurrency,
				&a.Target.ObservedAmount, &a.Target.ObservedCurrency, &a.Target.ObservedLegalEntityID,
				&a.Target.ObservedStatus, &a.Target.ObservedFiscalPeriod,
				&a.ExaminedJournalIDs, &a.Tolerance, &a.FiscalPeriod, &a.AttemptedBy, &a.CorrelationID, &a.AttemptedAt,
			); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
