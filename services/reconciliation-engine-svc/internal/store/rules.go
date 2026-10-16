package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/outbox"
)

// Definitions and rule versions (A11). A definition row is its identity; its
// rules are immutable rule versions, drafted, approved by someone other than
// their creator, and in effect from their effective date. The rules a
// definition read reports are those of its effective version.

const definitionColumns = `definition_id, tenant_id, legal_entity_id::text, name, version, created_at, created_by, updated_at`

func scanDefinition(row pgx.Row) (*domain.ReconciliationDefinition, error) {
	var d domain.ReconciliationDefinition
	var entity *string
	var legacyVersion int64
	if err := row.Scan(&d.DefinitionID, &d.TenantID, &entity, &d.Name, &legacyVersion, &d.CreatedAt, &d.CreatedBy, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if entity != nil {
		d.LegalEntityID = *entity
	}
	// A definition from before 000008 carries the version number its in-place
	// edits reached; new versions continue above it.
	d.LatestRuleVersion = legacyVersion
	return &d, nil
}

const ruleVersionColumns = `rule_version_id, tenant_id, definition_id, rule_version, rules, rules_sha256,
	to_char(effective_from, 'YYYY-MM-DD'), reason, created_by, created_at, approved_by, approved_at`

// scanRuleVersion also verifies the version's integrity: the stored rules must
// still hash to the digest sealed when the version was created.
func scanRuleVersion(row pgx.Row) (*domain.RuleVersion, error) {
	var v domain.RuleVersion
	var rules []byte
	if err := row.Scan(&v.RuleVersionID, &v.TenantID, &v.DefinitionID, &v.RuleVersion, &rules, &v.RulesSHA256,
		&v.EffectiveFrom, &v.Reason, &v.CreatedBy, &v.CreatedAt, &v.ApprovedBy, &v.ApprovedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rules, &v.Rules); err != nil {
		return nil, fmt.Errorf("decode rule version %d: %w", v.RuleVersion, err)
	}
	digest, err := v.Rules.Digest()
	if err != nil {
		return nil, err
	}
	if digest != v.RulesSHA256 {
		return nil, fmt.Errorf("rule version %d of %s fails its integrity check (sealed %s, now %s)",
			v.RuleVersion, v.DefinitionID, v.RulesSHA256, digest)
	}
	v.Status = "DRAFT"
	if v.ApprovedBy != nil {
		v.Status = "APPROVED"
	}
	return &v, nil
}

// loadDefinition reads the definition and resolves its effective rule
// version: the latest APPROVED version whose effective date has arrived (by
// effective date, then version). lock takes a share lock on the definition
// row, which CreateRuleVersion's update lock waits for.
func loadDefinition(ctx context.Context, tx pgx.Tx, tenantID, definitionID string, lock bool) (*domain.ReconciliationDefinition, error) {
	q := `SELECT ` + definitionColumns + ` FROM reconciliation_definitions WHERE tenant_id = $1 AND definition_id = $2`
	if lock {
		q += ` FOR SHARE`
	}
	d, err := scanDefinition(tx.QueryRow(ctx, q, tenantID, definitionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDefinitionNotFound
	}
	if err != nil {
		return nil, err
	}
	var maxVersion *int64
	if err := tx.QueryRow(ctx, `SELECT MAX(rule_version) FROM reconciliation_rule_versions WHERE tenant_id = $1 AND definition_id = $2`,
		tenantID, definitionID).Scan(&maxVersion); err != nil {
		return nil, fmt.Errorf("read latest rule version: %w", err)
	}
	if maxVersion != nil && *maxVersion > d.LatestRuleVersion {
		d.LatestRuleVersion = *maxVersion
	}
	v, err := scanRuleVersion(tx.QueryRow(ctx, `SELECT `+ruleVersionColumns+` FROM reconciliation_rule_versions
		WHERE tenant_id = $1 AND definition_id = $2 AND approved_by IS NOT NULL AND effective_from <= CURRENT_DATE
		ORDER BY effective_from DESC, rule_version DESC LIMIT 1`, tenantID, definitionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, nil // no rules in effect: Version 0
	}
	if err != nil {
		return nil, err
	}
	r := v.Rules
	d.Populations, d.MatchKeyFields, d.BalanceDimensions = r.Populations, r.MatchKeyFields, r.BalanceDimensions
	d.AmountScale, d.Tolerance = r.AmountScale, r.Tolerance
	d.Materiality, d.ExceptionPolicy, d.CertificationProfile = &r.Materiality, &r.ExceptionPolicy, &r.CertificationProfile
	d.Version, d.RulesSHA256, d.EffectiveFrom = v.RuleVersion, v.RulesSHA256, v.EffectiveFrom
	return d, nil
}

func (s *PgStore) GetDefinition(ctx context.Context, tenantID, definitionID string) (*domain.ReconciliationDefinition, error) {
	var out *domain.ReconciliationDefinition
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		d, err := loadDefinition(ctx, tx, tenantID, definitionID, false)
		out = d
		return err
	})
	return out, err
}

func insertRuleVersion(ctx context.Context, tx pgx.Tx, tenantID, definitionID string, version int64, rules domain.RuleSet,
	effectiveFrom, reason, actor string) (*domain.RuleVersion, error) {
	digest, err := rules.Digest()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("encode rules: %w", err)
	}
	var eff *string
	if effectiveFrom != "" {
		eff = &effectiveFrom
	}
	v, err := scanRuleVersion(tx.QueryRow(ctx, `
		INSERT INTO reconciliation_rule_versions (rule_version_id, tenant_id, definition_id, rule_version, rules, rules_sha256,
			effective_from, reason, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::date, CURRENT_DATE), $8, $9)
		RETURNING `+ruleVersionColumns,
		newID(domain.PrefixRuleVersion), tenantID, definitionID, version, raw, digest, eff, reason, actor))
	if err != nil {
		return nil, fmt.Errorf("create rule version: %w", err)
	}
	return v, nil
}

// DefineReconciliation creates a definition — identity only — and its first
// rule version as a draft. Nothing can run until that version is approved by
// someone else. A name already defined in the entity is refused: rules are
// never overwritten in place (ErrDefinitionExists; publish a rule version).
func (s *PgStore) DefineReconciliation(ctx context.Context, tenantID string, req domain.DefineReconciliationRequest, actor string,
	claim domain.IdempotencyClaim) (*domain.ReconciliationDefinition, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	rules, err := req.RulesInput.RuleSet()
	if err != nil {
		return nil, err
	}
	var out *domain.ReconciliationDefinition
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		id := newID(domain.PrefixDefinition)
		claim.ResourceID = id
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		// version 0: a definition born with rule versions has no in-place history.
		_, err := scanDefinition(tx.QueryRow(ctx, `
			INSERT INTO reconciliation_definitions (definition_id, tenant_id, legal_entity_id, name, version, created_by)
			VALUES ($1, $2, $3, $4, 0, $5)
			ON CONFLICT (tenant_id, legal_entity_id, name) DO NOTHING
			RETURNING `+definitionColumns, id, tenantID, req.LegalEntityID, req.Name, actor))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDefinitionExists
		}
		if err != nil {
			return fmt.Errorf("create reconciliation definition: %w", err)
		}
		v, err := insertRuleVersion(ctx, tx, tenantID, id, 1, rules, req.EffectiveFrom, "initial rules", actor)
		if err != nil {
			return err
		}
		d, err := loadDefinition(ctx, tx, tenantID, id, false)
		if err != nil {
			return err
		}
		out = d
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_definition", AggregateID: id,
			EventType: "CONTROL.RuleVersionPublished", TenantID: &tenantID, Payload: v})
	})
	return out, err
}

// CreateRuleVersion publishes a new draft version of a definition's rules. It
// must be based on the definition's latest version (optimistic check); the
// definition row is locked so concurrent publications serialise.
func (s *PgStore) CreateRuleVersion(ctx context.Context, tenantID, definitionID string, req domain.CreateRuleVersionRequest, actor string,
	claim domain.IdempotencyClaim) (*domain.RuleVersion, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	rules, err := req.RulesInput.RuleSet()
	if err != nil {
		return nil, err
	}
	var out *domain.RuleVersion
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM reconciliation_definitions WHERE tenant_id = $1 AND definition_id = $2 FOR UPDATE`,
			tenantID, definitionID); err != nil {
			return err
		}
		d, err := loadDefinition(ctx, tx, tenantID, definitionID, false)
		if err != nil {
			return err
		}
		if d.Legacy() {
			return domain.ErrLegacyRecord
		}
		if *req.BasedOnVersion != d.LatestRuleVersion {
			return fmt.Errorf("%w: latest is %d, the request is based on %d", domain.ErrRuleVersionConflict, d.LatestRuleVersion,
				*req.BasedOnVersion)
		}
		if err := checkNotBackdated(ctx, tx, d, req.EffectiveFrom); err != nil {
			return err
		}
		claim.ResourceID = definitionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		v, err := insertRuleVersion(ctx, tx, tenantID, definitionID, d.LatestRuleVersion+1, rules, req.EffectiveFrom, req.Reason, actor)
		if err != nil {
			return err
		}
		out = v
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_definition", AggregateID: definitionID,
			EventType: "CONTROL.RuleVersionPublished", TenantID: &tenantID, Payload: v})
	})
	return out, err
}

// ApproveRuleVersion records the independent approval of a draft: never by its
// creator (also a CHECK constraint), once, after which the version is
// immutable (trigger). The handler re-authorizes the approver at this moment.
func (s *PgStore) ApproveRuleVersion(ctx context.Context, tenantID, definitionID string, version int64, approver string,
	claim domain.IdempotencyClaim) (*domain.RuleVersion, error) {
	var out *domain.RuleVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanRuleVersion(tx.QueryRow(ctx, `SELECT `+ruleVersionColumns+` FROM reconciliation_rule_versions
			WHERE tenant_id = $1 AND definition_id = $2 AND rule_version = $3 FOR UPDATE`, tenantID, definitionID, version))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRuleVersionNotFound
		}
		if err != nil {
			return err
		}
		if cur.CreatedBy == approver {
			return domain.ErrSelfApproval
		}
		if cur.ApprovedBy != nil {
			return domain.ErrRuleVersionApproved
		}
		// Another version may have taken effect since this one was drafted.
		d, err := loadDefinition(ctx, tx, tenantID, definitionID, false)
		if err != nil {
			return err
		}
		if err := checkNotBackdated(ctx, tx, d, cur.EffectiveFrom); err != nil {
			return err
		}
		claim.ResourceID = cur.RuleVersionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		v, err := scanRuleVersion(tx.QueryRow(ctx, `UPDATE reconciliation_rule_versions SET approved_by = $2
			WHERE rule_version_id = $1 RETURNING `+ruleVersionColumns, cur.RuleVersionID, approver))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" {
				return domain.ErrSelfApproval
			}
			return fmt.Errorf("approve rule version: %w", err)
		}
		out = v
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_definition", AggregateID: definitionID,
			EventType: "CONTROL.RuleVersionApproved", TenantID: &tenantID, Payload: v})
	})
	return out, err
}

// checkNotBackdated refuses a version dated before the version in effect: the
// latest effective date governs, so such a version could never take effect.
// An empty date means today, which is never before the version in effect.
func checkNotBackdated(ctx context.Context, tx pgx.Tx, d *domain.ReconciliationDefinition, effectiveFrom string) error {
	if d.Version == 0 || effectiveFrom == "" {
		return nil
	}
	var before bool
	if err := tx.QueryRow(ctx, `SELECT $1::date < $2::date`, effectiveFrom, d.EffectiveFrom).Scan(&before); err != nil {
		return fmt.Errorf("compare effective dates: %w", err)
	}
	if before {
		return fmt.Errorf("%w (%s < %s, version %d)", domain.ErrBackdatedRuleVersion, effectiveFrom, d.EffectiveFrom, d.Version)
	}
	return nil
}

func (s *PgStore) ListRuleVersions(ctx context.Context, tenantID, definitionID string) ([]domain.RuleVersion, error) {
	var out []domain.RuleVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := loadDefinition(ctx, tx, tenantID, definitionID, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+ruleVersionColumns+` FROM reconciliation_rule_versions
			WHERE tenant_id = $1 AND definition_id = $2 ORDER BY rule_version`, tenantID, definitionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanRuleVersion(rows)
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return rows.Err()
	})
	return out, err
}
