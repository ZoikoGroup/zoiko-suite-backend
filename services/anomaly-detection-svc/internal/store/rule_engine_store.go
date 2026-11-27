package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// Rule-engine surface: anomaly_detection_rules and anomaly_records.

const (
	resourceTypeRule    = "anomaly_rule"
	resourceTypeRecord  = "anomaly_record"
	defaultZScoreCutoff = 3.00
)

// ─────────────────────────────────────────────────────────────────────────────
// Rules
// ─────────────────────────────────────────────────────────────────────────────

// ruleColumns casts NUMERIC to float8 and coalesces the nullable columns of
// the original schema, so every value scans into a plain Go type.
const ruleColumns = `rule_id, tenant_id, rule_name, domain_name, metric_type,
	threshold_value::float8, COALESCE(z_score_cutoff, 3.00)::float8, COALESCE(is_active, TRUE),
	version, created_by, created_at, updated_at`

func scanRule(row pgx.Row) (*domain.AnomalyRule, error) {
	var rule domain.AnomalyRule
	err := row.Scan(
		&rule.RuleID, &rule.TenantID, &rule.RuleName, &rule.DomainName, &rule.MetricType,
		&rule.ThresholdValue, &rule.ZScoreCutoff, &rule.IsActive,
		&rule.Version, &rule.CreatedBy, &rule.CreatedAt, &rule.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

func loadRule(ctx context.Context, tx pgx.Tx, tenantID, ruleID string) (*domain.AnomalyRule, error) {
	rule, err := scanRule(tx.QueryRow(ctx,
		`SELECT `+ruleColumns+` FROM anomaly_detection_rules WHERE tenant_id = $1 AND rule_id = $2`,
		tenantID, ruleID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAnomalyRuleNotFound
	}
	return rule, err
}

// CreateRule inserts a new active rule. A zero z_score_cutoff becomes 3.00.
func (s *PgStore) CreateRule(ctx context.Context, cmd domain.CommandContext, req domain.CreateRuleRequest) (domain.CommandResult[domain.AnomalyRule], error) {
	var result domain.CommandResult[domain.AnomalyRule]

	err := s.inTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		ruleID := newID(domain.PrefixAnomalyRule)

		claim, err := claimIdempotencyKey(ctx, tx, cmd, resourceTypeRule, ruleID)
		if err != nil {
			return err
		}
		if claim.IsReplay {
			original, err := loadRule(ctx, tx, cmd.TenantID, claim.OriginalResourceID)
			if err != nil {
				return err
			}
			result = domain.CommandResult[domain.AnomalyRule]{Resource: original, Replayed: true}
			return nil
		}

		cutoff := req.ZScoreCutoff
		if cutoff == 0 {
			cutoff = defaultZScoreCutoff
		}

		created, err := scanRule(tx.QueryRow(ctx, `
			INSERT INTO anomaly_detection_rules
				(rule_id, tenant_id, rule_name, domain_name, metric_type, threshold_value, z_score_cutoff,
				 is_active, version, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, 1, $8)
			RETURNING `+ruleColumns,
			ruleID, cmd.TenantID, req.RuleName, req.DomainName, req.MetricType, req.ThresholdValue, cutoff, cmd.PrincipalID))
		if err != nil {
			return fmt.Errorf("insert anomaly rule: %w", err)
		}
		result = domain.CommandResult[domain.AnomalyRule]{Resource: created}
		return nil
	})
	return result, err
}

// GetRule returns one rule of the tenant (404 otherwise).
func (s *PgStore) GetRule(ctx context.Context, tenantID, ruleID string) (*domain.AnomalyRule, error) {
	var rule *domain.AnomalyRule
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadRule(ctx, tx, tenantID, ruleID)
		rule = loaded
		return err
	})
	return rule, err
}

// ListRules returns one page of the tenant's rules, newest first.
func (s *PgStore) ListRules(ctx context.Context, tenantID string, filter domain.RuleFilter, page domain.PageRequest) ([]domain.AnomalyRule, string, error) {
	var rules []domain.AnomalyRule
	cursorTime, cursorID := cursorArgs(page)

	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+ruleColumns+`
			FROM anomaly_detection_rules
			WHERE tenant_id = $1
			  AND ($2 = '' OR domain_name = $2)
			  AND ($3::timestamptz IS NULL OR (created_at, rule_id) < ($3::timestamptz, $4::text))
			ORDER BY created_at DESC, rule_id DESC
			LIMIT $5`,
			tenantID, filter.DomainName, cursorTime, cursorID, pageLimitPlusOne(page))
		if err != nil {
			return fmt.Errorf("list anomaly rules: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			rule, err := scanRule(rows)
			if err != nil {
				return fmt.Errorf("scan anomaly rule: %w", err)
			}
			rules = append(rules, *rule)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}

	pageRows, nextCursor := splitPage(rules, page, func(rule domain.AnomalyRule) string {
		return domain.EncodeCursor(rule.CreatedAt, rule.RuleID)
	})
	return pageRows, nextCursor, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Anomaly records
// ─────────────────────────────────────────────────────────────────────────────

const recordColumns = `anomaly_id, tenant_id, legal_entity_id, domain_name, source_entity_id,
	COALESCE(rule_id, ''), severity, anomaly_score::float8, observed_value::float8, expected_value::float8,
	description, status, detected_by, COALESCE(investigated_by, ''), investigated_at, COALESCE(resolution_notes, ''),
	version, detected_at, created_at, updated_at`

func scanRecord(row pgx.Row) (*domain.AnomalyRecord, error) {
	var record domain.AnomalyRecord
	var severity, status string
	err := row.Scan(
		&record.AnomalyID, &record.TenantID, &record.LegalEntityID, &record.DomainName, &record.SourceEntityID,
		&record.RuleID, &severity, &record.AnomalyScore, &record.ObservedValue, &record.ExpectedValue,
		&record.Description, &status, &record.DetectedBy, &record.InvestigatedBy, &record.InvestigatedAt, &record.ResolutionNotes,
		&record.Version, &record.DetectedAt, &record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	record.Severity = domain.Severity(severity)
	record.Status = domain.AnomalyStatus(status)
	record.AssuranceState = domain.AssuranceStateAdvisory
	return &record, nil
}

func loadRecord(ctx context.Context, tx pgx.Tx, tenantID, anomalyID string, forUpdate bool) (*domain.AnomalyRecord, error) {
	query := `SELECT ` + recordColumns + ` FROM anomaly_records WHERE tenant_id = $1 AND anomaly_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	record, err := scanRecord(tx.QueryRow(ctx, query, tenantID, anomalyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAnomalyRecordNotFound
	}
	return record, err
}

// DetectAnomaly scores observed vs expected, stores an OPEN record and
// enqueues AI.AnomalyDetected. If rule_id is given it must name an active
// rule of the same tenant (422 otherwise).
func (s *PgStore) DetectAnomaly(ctx context.Context, cmd domain.CommandContext, req domain.DetectAnomalyRequest) (domain.CommandResult[domain.AnomalyRecord], error) {
	var result domain.CommandResult[domain.AnomalyRecord]

	err := s.inTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		anomalyID := newID(domain.PrefixAnomalyRecord)

		claim, err := claimIdempotencyKey(ctx, tx, cmd, resourceTypeRecord, anomalyID)
		if err != nil {
			return err
		}
		if claim.IsReplay {
			original, err := loadRecord(ctx, tx, cmd.TenantID, claim.OriginalResourceID, false)
			if err != nil {
				return err
			}
			result = domain.CommandResult[domain.AnomalyRecord]{Resource: original, Replayed: true}
			return nil
		}

		var ruleID *string
		if req.RuleID != "" {
			rule, err := loadRule(ctx, tx, cmd.TenantID, req.RuleID)
			if errors.Is(err, domain.ErrAnomalyRuleNotFound) {
				return domain.ErrAnomalyRuleReferenceInvalid
			}
			if err != nil {
				return err
			}
			if !rule.IsActive {
				return domain.ErrAnomalyRuleReferenceInvalid
			}
			ruleID = &rule.RuleID
		}

		score, severity := domain.CalculateAnomalyScore(req.ObservedValue, req.ExpectedValue, req.StdDeviation)
		description := strings.TrimSpace(req.Description)
		if description == "" {
			description = fmt.Sprintf("Anomaly detected in %s metric '%s': observed %.2f vs expected %.2f (score: %.2f)",
				req.DomainName, req.MetricType, req.ObservedValue, req.ExpectedValue, score)
		}

		created, err := scanRecord(tx.QueryRow(ctx, `
			INSERT INTO anomaly_records
				(anomaly_id, tenant_id, legal_entity_id, domain_name, source_entity_id, rule_id,
				 severity, anomaly_score, observed_value, expected_value, description, status,
				 detected_by, version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'OPEN', $12, 1)
			RETURNING `+recordColumns,
			anomalyID, cmd.TenantID, cmd.LegalEntityID, req.DomainName, req.SourceEntityID, ruleID,
			string(severity), score, req.ObservedValue, req.ExpectedValue, description, cmd.PrincipalID))
		if err != nil {
			return fmt.Errorf("insert anomaly record: %w", err)
		}

		err = enqueueEvent(ctx, tx, cmd, outboxEvent{
			EventType:     domain.EventAnomalyDetected,
			AggregateType: resourceTypeRecord,
			AggregateID:   created.AnomalyID,
			Payload:       recordEventPayload(created),
		})
		if err != nil {
			return err
		}

		result = domain.CommandResult[domain.AnomalyRecord]{Resource: created}
		return nil
	})
	return result, err
}

// GetAnomaly returns one record of the tenant (404 otherwise).
func (s *PgStore) GetAnomaly(ctx context.Context, tenantID, anomalyID string) (*domain.AnomalyRecord, error) {
	var record *domain.AnomalyRecord
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadRecord(ctx, tx, tenantID, anomalyID, false)
		record = loaded
		return err
	})
	return record, err
}

// ListAnomalies returns one page of records, newest first.
func (s *PgStore) ListAnomalies(ctx context.Context, tenantID string, filter domain.AnomalyFilter, page domain.PageRequest) ([]domain.AnomalyRecord, string, error) {
	var records []domain.AnomalyRecord
	cursorTime, cursorID := cursorArgs(page)

	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+recordColumns+`
			FROM anomaly_records
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id = $2)
			  AND ($3 = '' OR domain_name = $3)
			  AND ($4 = '' OR severity = $4)
			  AND ($5 = '' OR status = $5)
			  AND ($6::timestamptz IS NULL OR (created_at, anomaly_id) < ($6::timestamptz, $7::text))
			ORDER BY created_at DESC, anomaly_id DESC
			LIMIT $8`,
			tenantID, filter.LegalEntityID, filter.DomainName, string(filter.Severity), string(filter.Status),
			cursorTime, cursorID, pageLimitPlusOne(page))
		if err != nil {
			return fmt.Errorf("list anomaly records: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			record, err := scanRecord(rows)
			if err != nil {
				return fmt.Errorf("scan anomaly record: %w", err)
			}
			records = append(records, *record)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}

	pageRows, nextCursor := splitPage(records, page, func(record domain.AnomalyRecord) string {
		return domain.EncodeCursor(record.CreatedAt, record.AnomalyID)
	})
	return pageRows, nextCursor, nil
}

// UpdateAnomalyStatus moves a record along the rule-engine state machine.
// Reaching CONFIRMED_ANOMALY, FALSE_POSITIVE or RESOLVED is a disposition:
// the actor must differ from the principal who recorded the anomaly, and
// AI.AnomalyDispositioned is enqueued.
func (s *PgStore) UpdateAnomalyStatus(ctx context.Context, cmd domain.CommandContext, anomalyID string, req domain.UpdateStatusRequest) (domain.CommandResult[domain.AnomalyRecord], error) {
	var result domain.CommandResult[domain.AnomalyRecord]

	err := s.inTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		// Step 1: lock the record.
		current, err := loadRecord(ctx, tx, cmd.TenantID, anomalyID, true)
		if err != nil {
			return err
		}

		// Step 2: replay?
		claim, err := claimIdempotencyKey(ctx, tx, cmd, resourceTypeRecord, anomalyID)
		if err != nil {
			return err
		}
		if claim.IsReplay {
			result = domain.CommandResult[domain.AnomalyRecord]{Resource: current, Replayed: true}
			return nil
		}

		// Step 3: If-Match.
		if err := checkExpectedVersion(cmd.ExpectedVersion, current.Version); err != nil {
			return err
		}

		// Step 4: state machine and segregation of duties.
		if !domain.CanTransitionRecord(current.Status, req.Status) {
			return domain.ErrInvalidStatusTransition.WithDetail(fmt.Sprintf(
				"anomaly record cannot move from %s to %s", current.Status, req.Status))
		}
		isDisposition := domain.IsDispositionStatus(req.Status)
		if isDisposition && current.DetectedBy != "" && current.DetectedBy == cmd.PrincipalID {
			return domain.ErrSegregationOfDuties.WithDetail(
				"segregation of duties: the principal who recorded the anomaly cannot disposition it")
		}

		// Step 5: write the change (+ event).
		resolutionNotes := current.ResolutionNotes
		if strings.TrimSpace(req.ResolutionNotes) != "" {
			resolutionNotes = req.ResolutionNotes
		}
		updated, err := scanRecord(tx.QueryRow(ctx, `
			UPDATE anomaly_records
			SET status = $3, investigated_by = $4, investigated_at = NOW(), resolution_notes = $5,
			    version = version + 1, updated_at = NOW()
			WHERE tenant_id = $1 AND anomaly_id = $2
			RETURNING `+recordColumns,
			cmd.TenantID, anomalyID, string(req.Status), cmd.PrincipalID, resolutionNotes))
		if err != nil {
			return fmt.Errorf("update anomaly status: %w", err)
		}

		if isDisposition {
			err = enqueueEvent(ctx, tx, cmd, outboxEvent{
				EventType:     domain.EventAnomalyDispositioned,
				AggregateType: resourceTypeRecord,
				AggregateID:   updated.AnomalyID,
				Payload: map[string]any{
					"surface":          "rule_engine",
					"anomaly_id":       updated.AnomalyID,
					"previous_status":  current.Status,
					"status":           updated.Status,
					"resolution_notes": updated.ResolutionNotes,
					"decided_by":       cmd.PrincipalID,
					"advisory":         true,
				},
			})
			if err != nil {
				return err
			}
		}

		result = domain.CommandResult[domain.AnomalyRecord]{Resource: updated}
		return nil
	})
	return result, err
}

// recordEventPayload is the AI.AnomalyDetected payload for a rule-engine
// record. "advisory": true tells every consumer this is not an instruction.
func recordEventPayload(record *domain.AnomalyRecord) map[string]any {
	return map[string]any{
		"surface":          "rule_engine",
		"anomaly_id":       record.AnomalyID,
		"legal_entity_id":  record.LegalEntityID,
		"domain_name":      record.DomainName,
		"source_entity_id": record.SourceEntityID,
		"rule_id":          record.RuleID,
		"severity":         record.Severity,
		"anomaly_score":    record.AnomalyScore,
		"status":           record.Status,
		"advisory":         true,
		"assurance_state":  domain.AssuranceStateAdvisory,
	}
}
