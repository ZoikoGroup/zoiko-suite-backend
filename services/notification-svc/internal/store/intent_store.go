package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// ZS-SVC-Y-001 NCD-01 persistence: the communication intent registry (migration
// 000020). Same shape as the template store: tenant-scoped under row-level security,
// fetch-then-guard transitions, and the database as the second line of defence.

const intentColumns = `intent_id, tenant_id, legal_entity_id, intent_key, display_name, domain_owner, status,
	created_by_principal_id, created_at, retired_at`

func scanIntent(s scannable, i *domain.CommunicationIntent) error {
	return s.Scan(&i.IntentID, &i.TenantID, &i.LegalEntityID, &i.IntentKey, &i.DisplayName, &i.DomainOwner, &i.Status,
		&i.CreatedByPrincipalID, &i.CreatedAt, &i.RetiredAt)
}

const intentVersionColumns = `v.version_id, v.intent_id, v.tenant_id, v.legal_entity_id, v.version_number,
	v.purpose_class, v.evidence_class, v.allowed_channels, v.marketing_allowed, v.record_requirement, v.variable_contract,
	v.status, v.created_by_principal_id, v.created_at, v.validated_at, v.approved_by_principal_id, v.approved_at,
	v.effective_from, v.published_at, v.published_by_principal_id, v.privacy_activity_id, v.privacy_purpose_id`

func scanIntentVersion(s scannable, v *domain.IntentVersion) error {
	var channels, contract []byte
	if err := s.Scan(&v.VersionID, &v.IntentID, &v.TenantID, &v.LegalEntityID, &v.VersionNumber,
		&v.PurposeClass, &v.EvidenceClass, &channels, &v.MarketingAllowed, &v.RecordRequirement, &contract,
		&v.Status, &v.CreatedByPrincipalID, &v.CreatedAt, &v.ValidatedAt, &v.ApprovedByPrincipalID, &v.ApprovedAt,
		&v.EffectiveFrom, &v.PublishedAt, &v.PublishedByPrincipalID, &v.PrivacyActivityID, &v.PrivacyPurposeID); err != nil {
		return err
	}
	if err := json.Unmarshal(channels, &v.AllowedChannels); err != nil {
		return fmt.Errorf("decode allowed_channels: %w", err)
	}
	v.VariableContract = map[string]domain.VariableSpec{}
	if len(contract) > 0 {
		if err := json.Unmarshal(contract, &v.VariableContract); err != nil {
			return fmt.Errorf("decode variable_contract: %w", err)
		}
	}
	return nil
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// CreateIntent registers the stable identity of a purpose.
func (s *PgStore) CreateIntent(ctx context.Context, p domain.CreateIntentParams) (*domain.CommunicationIntent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if err := domain.ValidateIntentKey(p.IntentKey); err != nil {
		return nil, err
	}
	var out domain.CommunicationIntent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanIntent(tx.QueryRow(ctx, `
			INSERT INTO communication_intents (tenant_id, legal_entity_id, intent_key, display_name, domain_owner, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+intentColumns,
			tenantID, p.LegalEntityID, p.IntentKey, p.DisplayName, p.DomainOwner, p.CreatedByPrincipalID), &out)
	})
	if pgCode(err) == "23505" {
		return nil, domain.ErrIntentKeyTaken
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetIntent reads one intent. An id that cannot be a UUID names no intent.
func (s *PgStore) GetIntent(ctx context.Context, intentID string) (*domain.CommunicationIntent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.CommunicationIntent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanIntent(tx.QueryRow(ctx, `SELECT `+intentColumns+` FROM communication_intents
			WHERE intent_id::text = $1 AND tenant_id = $2`, intentID, tenantID), &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateIntentVersion writes a new DRAFT version. Everything that can be known without
// storing it is checked first, so an invalid contract never becomes a version.
func (s *PgStore) CreateIntentVersion(ctx context.Context, p domain.CreateIntentVersionParams) (*domain.IntentVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	p.VariableContract = domain.NormalizeContract(p.VariableContract)
	if err := domain.ValidateIntentVersion(p); err != nil {
		return nil, err
	}
	channels, err := json.Marshal(p.AllowedChannels)
	if err != nil {
		return nil, err
	}
	contract, err := json.Marshal(p.VariableContract)
	if err != nil {
		return nil, err
	}
	var out domain.IntentVersion
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var legalEntityID, status string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id, status FROM communication_intents
			WHERE intent_id::text = $1 AND tenant_id = $2 FOR UPDATE`, p.IntentID, tenantID).Scan(&legalEntityID, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrIntentNotFound
			}
			return err
		}
		if status == "RETIRED" {
			return domain.ErrIntentRetired
		}
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM communication_intent_versions
			WHERE intent_id::text = $1`, p.IntentID).Scan(&next); err != nil {
			return err
		}
		return scanIntentVersion(tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO communication_intent_versions
					(intent_id, tenant_id, legal_entity_id, version_number, purpose_class, evidence_class, allowed_channels,
					 marketing_allowed, record_requirement, variable_contract, created_by_principal_id,
					 privacy_activity_id, privacy_purpose_id)
				VALUES ($1::uuid,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10::jsonb,$11,$12,$13) RETURNING *)
			SELECT `+intentVersionColumns+` FROM ins v`,
			p.IntentID, tenantID, legalEntityID, next, p.PurposeClass, p.EvidenceClass, string(channels),
			p.MarketingAllowed, p.RecordRequirement, string(contract), p.CreatedByPrincipalID,
			p.PrivacyActivityID, p.PrivacyPurposeID), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetIntentVersion reads one version.
func (s *PgStore) GetIntentVersion(ctx context.Context, versionID string) (*domain.IntentVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.IntentVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanIntentVersion(tx.QueryRow(ctx, `SELECT `+intentVersionColumns+` FROM communication_intent_versions v
			WHERE v.version_id::text = $1 AND v.tenant_id = $2`, versionID, tenantID), &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntentVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// transitionIntentVersion locks a version, checks its current status, and applies one
// guarded update.
func (s *PgStore) transitionIntentVersion(ctx context.Context, versionID, wantStatus string, notWanted error,
	pre func(*domain.IntentVersion) error, update string, args ...any) (*domain.IntentVersion, error) {

	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.IntentVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var cur domain.IntentVersion
		if err := scanIntentVersion(tx.QueryRow(ctx, `SELECT `+intentVersionColumns+` FROM communication_intent_versions v
			WHERE v.version_id::text = $1 AND v.tenant_id = $2 FOR UPDATE`, versionID, tenantID), &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrIntentVersionNotFound
			}
			return err
		}
		if cur.Status != wantStatus {
			return notWanted
		}
		if pre != nil {
			if err := pre(&cur); err != nil {
				return err
			}
		}
		all := append([]any{versionID, tenantID}, args...)
		return scanIntentVersion(tx.QueryRow(ctx, update, all...), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ValidateIntentVersion re-checks a DRAFT version and moves it to REVIEW.
func (s *PgStore) ValidateIntentVersion(ctx context.Context, versionID string) (*domain.IntentVersion, error) {
	return s.transitionIntentVersion(ctx, versionID, domain.IntentVersionDraft, domain.ErrIntentVersionNotDraft,
		func(v *domain.IntentVersion) error {
			return domain.ValidateIntentVersion(domain.CreateIntentVersionParams{PurposeClass: v.PurposeClass, EvidenceClass: v.EvidenceClass,
				AllowedChannels: v.AllowedChannels, MarketingAllowed: v.MarketingAllowed, RecordRequirement: v.RecordRequirement, VariableContract: v.VariableContract})
		},
		`WITH u AS (UPDATE communication_intent_versions SET status='REVIEW', validated_at=now()
			WHERE version_id::text=$1 AND tenant_id=$2 AND status='DRAFT' RETURNING *)
		 SELECT `+intentVersionColumns+` FROM u v`)
}

// ApproveIntentVersion approves a REVIEW version. The creator cannot approve it
// (maker-checker; also a CHECK in the schema).
func (s *PgStore) ApproveIntentVersion(ctx context.Context, p domain.ApproveIntentVersionParams) (*domain.IntentVersion, error) {
	return s.transitionIntentVersion(ctx, p.VersionID, domain.IntentVersionReview, domain.ErrIntentVersionNotReview,
		func(v *domain.IntentVersion) error {
			if v.CreatedByPrincipalID == p.ApprovedByPrincipalID {
				return domain.ErrIntentVersionSelfApproval
			}
			return nil
		},
		`WITH u AS (UPDATE communication_intent_versions SET status='APPROVED', approved_by_principal_id=$3, approved_at=now()
			WHERE version_id::text=$1 AND tenant_id=$2 AND status='REVIEW' RETURNING *)
		 SELECT `+intentVersionColumns+` FROM u v`, p.ApprovedByPrincipalID)
}

// PublishIntentVersion publishes an APPROVED version. effective_from defaults to now,
// is never in the past, and must be later than every version already published.
func (s *PgStore) PublishIntentVersion(ctx context.Context, p domain.PublishIntentVersionParams) (*domain.IntentVersion, error) {
	var effective *time.Time
	if p.EffectiveFrom != nil {
		t := p.EffectiveFrom.UTC()
		effective = &t
	}
	v, err := s.transitionIntentVersion(ctx, p.VersionID, domain.IntentVersionApproved, domain.ErrIntentVersionNotApproved, nil,
		`WITH u AS (UPDATE communication_intent_versions SET status='PUBLISHED', published_at=now(),
				effective_from=COALESCE($3::timestamptz, now()), published_by_principal_id=$4
			WHERE version_id::text=$1 AND tenant_id=$2 AND status='APPROVED' RETURNING *)
		 SELECT `+intentVersionColumns+` FROM u v`, effective, p.PublishedByPrincipalID)
	if pgCode(err) == "23514" || pgCode(err) == "23505" {
		return nil, domain.ErrIntentEffectiveFromInvalid
	}
	return v, err
}

// RetireIntent retires an intent: it takes no new versions, and from that moment it has
// no version in force. History stays resolvable as of any earlier time.
func (s *PgStore) RetireIntent(ctx context.Context, intentID, actor string) (*domain.CommunicationIntent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.CommunicationIntent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanIntent(tx.QueryRow(ctx, `UPDATE communication_intents SET status='RETIRED', retired_at=now(), retired_by_principal_id=$3
			WHERE intent_id::text=$1 AND tenant_id=$2 AND status='ACTIVE' RETURNING `+intentColumns, intentID, tenantID, actor), &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "no such intent" from "already retired".
		if _, gerr := s.GetIntent(ctx, intentID); gerr != nil {
			return nil, gerr
		}
		return nil, domain.ErrIntentRetired
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// EffectiveIntentVersion resolves the version in force at transaction time `at`, as the
// platform knew it at knowledge time `knownAt` (NCD-01 GET /intents/{id}/effective).
//
// A version counts when it was published at or before knownAt and its effective_from is
// at or before at; the latest effective_from wins. A retirement blocks resolution only
// when it happened at or before BOTH times, so a message from before the retirement
// still reconstructs exactly, even when asked about long afterwards.
func (s *PgStore) EffectiveIntentVersion(ctx context.Context, intentID string, at, knownAt time.Time) (*domain.IntentVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if _, err := s.GetIntent(ctx, intentID); err != nil {
		return nil, err
	}
	var out domain.IntentVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanIntentVersion(tx.QueryRow(ctx, `
			SELECT `+intentVersionColumns+`
			FROM communication_intent_versions v JOIN communication_intents i ON i.intent_id = v.intent_id
			WHERE v.intent_id::text = $1 AND v.tenant_id = $2
			  AND v.published_at IS NOT NULL AND v.published_at <= $4 AND v.effective_from <= $3
			  AND (i.retired_at IS NULL OR i.retired_at > $3 OR i.retired_at > $4)
			ORDER BY v.effective_from DESC LIMIT 1`, intentID, tenantID, at, knownAt), &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntentNotEffective
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// effectiveContractTx returns the intent version in force NOW for a bound template, read
// inside the caller's transaction. A nil intentID (an unbound template) returns nil, nil:
// nothing to conform to. A bound template whose intent has no version in force yet is
// ErrIntentNotEffective: wording cannot be authored against a contract that does not exist.
func effectiveContractTx(ctx context.Context, tx pgx.Tx, tenantID string, intentID *string) (*domain.IntentVersion, error) {
	if intentID == nil {
		return nil, nil
	}
	var out domain.IntentVersion
	err := scanIntentVersion(tx.QueryRow(ctx, `
		SELECT `+intentVersionColumns+`
		FROM communication_intent_versions v JOIN communication_intents i ON i.intent_id = v.intent_id
		WHERE v.intent_id::text = $1 AND v.tenant_id = $2
		  AND v.published_at IS NOT NULL AND v.published_at <= now() AND v.effective_from <= now()
		  AND i.status = 'ACTIVE'
		ORDER BY v.effective_from DESC LIMIT 1`, *intentID, tenantID), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntentNotEffective
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// conformToContract checks a template version's wording against its intent's contract:
// every variable it uses is governed by the intent, and every variable its subject uses
// is subject-safe by sensitivity (INV-17), not by the author's say-so.
func conformToContract(iv *domain.IntentVersion, schema, subjectVariables []string, hasSubject bool) error {
	if iv == nil {
		return nil
	}
	if err := domain.CheckTemplateVariables(schema, iv.VariableContract); err != nil {
		return err
	}
	if hasSubject {
		return domain.CheckSubjectAgainstContract(subjectVariables, iv.VariableContract)
	}
	return nil
}

// checkBindableIntent verifies that a template may be bound to an intent: it exists for
// this tenant, is not retired, and belongs to the same legal entity as the template.
func checkBindableIntent(ctx context.Context, tx pgx.Tx, tenantID, intentID, legalEntityID string) error {
	var entity, status string
	err := tx.QueryRow(ctx, `SELECT legal_entity_id, status FROM communication_intents
		WHERE intent_id::text = $1 AND tenant_id = $2`, intentID, tenantID).Scan(&entity, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrIntentNotFound
	}
	if err != nil {
		return err
	}
	if status == "RETIRED" {
		return domain.ErrIntentRetired
	}
	if entity != legalEntityID {
		return domain.IntentProblem{Reason: "the intent belongs to a different legal entity than the template"}
	}
	return nil
}
