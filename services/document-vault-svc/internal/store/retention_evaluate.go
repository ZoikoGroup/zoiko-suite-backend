package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

// RecordLegalEntities returns each record's owning legal entity, tenant-scoped.
// A record that does not exist in the caller's tenant — including one that
// belongs to another tenant — fails the whole call with ErrRecordNotFound, so a
// caller can neither probe other tenants' ids nor receive a partial answer.
func (s *PgStore) RecordLegalEntities(ctx context.Context, recordIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(recordIDs))
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		for _, id := range recordIDs {
			var entity string
			err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM records WHERE record_id::text = $1 AND tenant_id::text = $2`,
				id, tenantID).Scan(&entity)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrRecordNotFound) {
					return domain.ErrRecordNotFound
				}
				return fmt.Errorf("document store unavailable: %w", err)
			}
			out[id] = entity
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EvaluateRetention resolves the retention schedule for each record
// (ZS-SVC-S-001 §9.2 POST /v1/retention:evaluate, §5.3 "Rule resolution
// principles"). It is read-only: it binds nothing, computes no date and
// publishes nothing.
//
// Applicability is the same test BindRetentionRule applies: an ACTIVE rule
// version whose record class and jurisdiction selector equal the record's own.
// Then, deliberately without a precedence table (the §17 open decisions OD-S02 /
// OD-S03 have not been taken):
//
//   - no applicable rule      -> NO_APPLICABLE_RULE. Nothing is defaulted
//     ("no silent defaults", DRC-I09).
//   - one applicable rule     -> RESOLVED with that rule.
//   - several, all with the same trigger, duration and disposition action
//     -> RESOLVED; each contributing rule is listed, with the reason (§5.3 "the
//     resolved schedule stores each contributing rule and the reason").
//   - several that differ in trigger, duration or action -> RETENTION_CONFLICT
//     with every contributing rule. The engine does not pick the longest, the
//     newest or any other winner (§5.3: it "must not assume the longest duration
//     is always legally correct"); the conflict is for legal/compliance to
//     resolve, and it blocks automatic disposition (DRC-I09).
//
// The record's existing schedule state, if one is bound, and any ACTIVE legal
// hold over it are reported alongside, so the caller sees the whole position.
// Effective-dated applicability (§5.2 effective_from/to, §5.3 "effective date")
// is evaluated at AsOf: a rule applies when effective_from <= AsOf < effective_to,
// an absent bound being open. Applicability against legal entity and data class
// is NOT evaluated: §5.2 gives a rule version no legal-entity or data-class
// field to compare them with.
//
// EvaluateRetention is the read-only form (AsOf = now, no event). The route uses
// EvaluateRetentionAt, which also publishes retention_conflict_detected.
func (s *PgStore) EvaluateRetention(ctx context.Context, recordIDs []string) ([]domain.RetentionEvaluation, error) {
	return s.evaluateRetention(ctx, domain.EvaluateRetentionParams{RecordIDs: recordIDs, AsOf: time.Now().UTC()}, false)
}

// EvaluateRetentionAt resolves like EvaluateRetention at p.AsOf and, for each
// record whose rules conflict, publishes §9.3 retention_conflict_detected
// (record_id, contributing_rule_ids, conflict_code) through the transactional
// outbox, in the same transaction as the evaluation. The event is idempotent:
// re-evaluating the same record against the same set of contributing rules does
// not publish it again (a changed rule set is a new detection).
func (s *PgStore) EvaluateRetentionAt(ctx context.Context, p domain.EvaluateRetentionParams) ([]domain.RetentionEvaluation, error) {
	if p.AsOf.IsZero() {
		p.AsOf = time.Now().UTC()
	}
	return s.evaluateRetention(ctx, p, true)
}

func conflictDedupeKey(recordID string, ruleIDs []string) string {
	ids := append([]string(nil), ruleIDs...)
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	return "retention-conflict:" + recordID + ":" + hex.EncodeToString(sum[:8])
}

func (s *PgStore) evaluateRetention(ctx context.Context, p domain.EvaluateRetentionParams, publish bool) ([]domain.RetentionEvaluation, error) {
	recordIDs := p.RecordIDs
	out := make([]domain.RetentionEvaluation, 0, len(recordIDs))
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		for _, id := range recordIDs {
			ev := domain.RetentionEvaluation{RecordID: id, AsOf: p.AsOf}
			var legalEntityID string
			err := tx.QueryRow(ctx, `SELECT record_class, jurisdiction_scope, legal_entity_id::text FROM records WHERE record_id::text = $1 AND tenant_id::text = $2`,
				id, tenantID).Scan(&ev.RecordClass, &ev.JurisdictionScope, &legalEntityID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrRecordNotFound) {
					return domain.ErrRecordNotFound
				}
				return fmt.Errorf("document store unavailable: %w", err)
			}

			rows, err := tx.Query(ctx, `
				SELECT retention_rule_version_id::text, trigger_type, duration_days, disposition_action, legal_basis_ref, purpose_ref
				FROM retention_rule_versions
				WHERE tenant_id::text = $1 AND status = 'ACTIVE' AND record_class = $2 AND jurisdiction_selector = $3
				  AND (effective_from IS NULL OR effective_from <= $4)
				  AND (effective_to IS NULL OR $4 < effective_to)
				ORDER BY created_at ASC, retention_rule_version_id ASC`,
				tenantID, ev.RecordClass, ev.JurisdictionScope, p.AsOf)
			if err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			for rows.Next() {
				var r domain.RetentionRuleSummary
				if err := rows.Scan(&r.RetentionRuleVersionID, &r.TriggerType, &r.DurationDays, &r.DispositionAction,
					&r.LegalBasisRef, &r.PurposeRef); err != nil {
					rows.Close()
					return fmt.Errorf("document store unavailable: %w", err)
				}
				ev.ApplicableRules = append(ev.ApplicableRules, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			domain.ResolveRetention(&ev)
			if publish && ev.Resolution == domain.ResolutionConflict {
				if err := s.publishRetentionConflict(ctx, tx, tenantID, legalEntityID, p.ActorPrincipalID, &ev); err != nil {
					return err
				}
			}

			var st domain.RecordRetentionState
			stRow := tx.QueryRow(ctx, `SELECT `+retentionStateColumns+` FROM record_retention_states WHERE record_id::text = $1 AND tenant_id::text = $2`,
				id, tenantID)
			switch err := scanRetentionState(stRow, &st); {
			case err == nil:
				ev.ScheduleState = &st
			case errors.Is(err, pgx.ErrNoRows):
			default:
				return fmt.Errorf("document store unavailable: %w", err)
			}

			holdRows, err := tx.Query(ctx, `
				SELECT DISTINCT hold_id::text FROM legal_hold_targets
				WHERE record_id::text = $1 AND tenant_id::text = $2 AND applied_at IS NOT NULL AND released_at IS NULL
				ORDER BY 1`, id, tenantID)
			if err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			for holdRows.Next() {
				var hid string
				if err := holdRows.Scan(&hid); err != nil {
					holdRows.Close()
					return fmt.Errorf("document store unavailable: %w", err)
				}
				ev.LegalHold.HoldIDs = append(ev.LegalHold.HoldIDs, hid)
			}
			holdRows.Close()
			if err := holdRows.Err(); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			ev.LegalHold.Blocked = len(ev.LegalHold.HoldIDs) > 0
			if ev.LegalHold.HoldIDs == nil {
				ev.LegalHold.HoldIDs = []string{}
			}
			out = append(out, ev)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// publishRetentionConflict writes retention_conflict_detected once per
// (record, contributing rule set). The dedupe key is the event's correlation id,
// so a repeat evaluation finds the earlier event and publishes nothing.
func (s *PgStore) publishRetentionConflict(ctx context.Context, tx pgx.Tx, tenantID, legalEntityID, actor string, ev *domain.RetentionEvaluation) error {
	ruleIDs := make([]string, 0, len(ev.ApplicableRules))
	for _, r := range ev.ApplicableRules {
		ruleIDs = append(ruleIDs, r.RetentionRuleVersionID)
	}
	key := conflictDedupeKey(ev.RecordID, ruleIDs)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outbox_events WHERE tenant_id::text = $1 AND event_type = $2 AND correlation_id = $3)`,
		tenantID, evtRetentionConflict, key).Scan(&exists); err != nil {
		return fmt.Errorf("document store unavailable: %w", err)
	}
	if exists {
		return nil
	}
	return enqueueDRCEvent(ctx, tx, "RECORD", ev.RecordID, evtRetentionConflict, tenantID, legalEntityID, actor, key, map[string]any{
		"record_id":             ev.RecordID,
		"contributing_rule_ids": ruleIDs,
		"conflict_code":         ev.ConflictCode,
	})
}
