package store

// REF-07 Accounting Policy (migration 000009): drafting, reading and the
// candidates resolution ranks. The lifecycle itself is the GOV-05 machinery in
// pg_store.go, applied to these versions like any others.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/policy-svc/internal/domain"
)

// requireCallerTenant refuses an accounting read or write for a tenant other
// than the caller's verified one. Row-level security is set from the tenant the
// operation names, so without this a caller could name another tenant and be
// shown its rows.
func requireCallerTenant(ctx context.Context, tenantID string) error {
	if tenantID == "" || contextTenantID(ctx) != tenantID {
		return domain.ErrInvalidScope
	}
	return nil
}

// accountingFamilyCode is the policy_code of the family for one
// (domain, book, jurisdiction). It is derived, so the same applicability is
// always the same family, and short enough for the column however long the ids.
func accountingFamilyCode(policyDomain string, bookID, jurisdictionID *string) string {
	sum := sha256.Sum256([]byte(domain.FamilyKey(policyDomain, bookID, jurisdictionID)))
	return "ACCT:" + policyDomain + ":" + hex.EncodeToString(sum[:])[:16]
}

func accountingFamilyName(policyDomain string, bookID, jurisdictionID *string) string {
	name := "Accounting policy " + policyDomain
	if bookID != nil {
		name += ", book " + *bookID
	}
	if jurisdictionID != nil {
		name += ", jurisdiction " + *jurisdictionID
	}
	return name
}

// accountingVersionColumns is the SELECT list of a version with its accounting
// content; the trailing columns are scanned by scanAccounting.
const accountingTail = `p.policy_code, a.policy_domain, a.framework, a.book_id, a.jurisdiction_id, a.created_at,
	d.semantic_version, d.precedence, d.framework_source, d.policy_text_ref, d.impact_analysis`

const accountingFrom = `
	FROM policy_versions pv
	JOIN policies p ON p.policy_id = pv.policy_id
	JOIN accounting_policy_applicability a ON a.policy_id = pv.policy_id
	JOIN accounting_policy_version_details d ON d.policy_version_id = pv.policy_version_id`

func scanAccounting(row pgx.Row) (*domain.AccountingPolicyVersion, error) {
	av := &domain.AccountingPolicyVersion{RuleMappings: []domain.RuleMapping{}}
	var impact []byte
	v := &domain.PolicyVersion{}
	dest := append(versionDest(v),
		&av.PolicyCode, &av.Applicability.PolicyDomain, &av.Applicability.Framework, &av.Applicability.BookID,
		&av.Applicability.JurisdictionID, &av.Applicability.CreatedAt,
		&av.Details.SemanticVersion, &av.Details.Precedence, &av.Details.FrameworkSource, &av.Details.PolicyTextRef, &impact)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	v.ScopeType = domain.DeriveScopeType(v.TenantID, v.LegalEntityID)
	av.PolicyVersion = *v
	av.Applicability.PolicyID = v.PolicyID
	if err := json.Unmarshal(impact, &av.Details.Impact); err != nil {
		return nil, fmt.Errorf("stored impact analysis of %s is unreadable: %w", v.PolicyVersionID, err)
	}
	return av, nil
}

func loadRuleMappings(ctx context.Context, tx pgx.Tx, versionID string) ([]domain.RuleMapping, error) {
	rows, err := tx.Query(ctx, `
		SELECT mapping_no, ref_kind, ref, ref_version, mapping_rationale, COALESCE(maps_to, ''), depends_on_version_id
		FROM accounting_policy_rule_mappings WHERE policy_version_id = $1 ORDER BY mapping_no;`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RuleMapping{}
	for rows.Next() {
		var m domain.RuleMapping
		if err := rows.Scan(&m.MappingNo, &m.RefKind, &m.Ref, &m.RefVersion, &m.Rationale, &m.MapsTo, &m.DependsOnVersionID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func loadAccountingVersion(ctx context.Context, tx pgx.Tx, versionID string) (*domain.AccountingPolicyVersion, error) {
	av, err := scanAccounting(tx.QueryRow(ctx, `SELECT `+versionColumns("pv")+`, `+accountingTail+accountingFrom+`
		WHERE pv.policy_version_id = $1;`, versionID))
	if err != nil {
		return nil, err
	}
	if av.RuleMappings, err = loadRuleMappings(ctx, tx, versionID); err != nil {
		return nil, err
	}
	return av, nil
}

// GetAccountingPolicyVersion returns an accounting policy version with its
// applicability, details and rule mappings, if it belongs to the caller's
// tenant. A version that is not an accounting policy version is not found.
func (s *PgStore) GetAccountingPolicyVersion(ctx context.Context, versionID string) (*domain.AccountingPolicyVersion, error) {
	var out *domain.AccountingPolicyVersion
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		var err error
		out, err = loadAccountingVersion(ctx, tx, versionID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPolicyVersionNotFound
	}
	if err != nil {
		if isInvalidUUID(err) {
			return nil, domain.ErrPolicyVersionNotFound
		}
		return nil, s.domainError("GetAccountingPolicyVersion", err)
	}
	return out, nil
}

func isInvalidUUID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 22P02")
}

// IsAccountingPolicy reports whether the family is an accounting policy, which
// is managed only through the accounting commands.
func (s *PgStore) IsAccountingPolicy(ctx context.Context, policyID string) (bool, error) {
	var is bool
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounting_policy_applicability WHERE policy_id = $1::uuid);`, policyID).Scan(&is)
	})
	if err != nil {
		if isInvalidUUID(err) {
			return false, nil
		}
		return false, s.domainError("IsAccountingPolicy", err)
	}
	return is, nil
}

// CreateAccountingDraft is DraftAccountingPolicy: it finds or creates the
// family for the applicability, creates the DRAFT version, and records its
// details and rule mappings, all in one transaction.
//
// The same semantic version for the same family and entity scope is the same
// draft: an identical repeat replays it (false), different content is
// ErrIdempotencyMismatch. A rule mapping naming a policy version that does not
// exist is ErrAccountingDependencyNotFound.
func (s *PgStore) CreateAccountingDraft(ctx context.Context, p domain.DraftAccountingPolicyParams) (*domain.AccountingPolicyVersion, bool, error) {
	if err := requireCallerTenant(ctx, p.TenantID); err != nil {
		return nil, false, err
	}
	var out *domain.AccountingPolicyVersion
	var created bool
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		code := accountingFamilyCode(p.PolicyDomain, p.BookID, p.JurisdictionID)
		// One draft per family at a time, so numbering and the semantic
		// version check cannot race.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1));`, "policy-svc:acct-family:"+p.TenantID+":"+code); err != nil {
			return err
		}
		family, err := s.ensureAccountingFamily(ctx, tx, p, code)
		if err != nil {
			return err
		}

		params := domain.CreatePolicyVersionParams{
			PolicyVersionID: p.PolicyVersionID, PolicyID: family.PolicyID,
			TenantID: &p.TenantID, LegalEntityID: p.LegalEntityID,
			RulePayload: p.RulePayload(), EffectiveFrom: p.EffectiveFrom, EffectiveTo: p.EffectiveTo,
			Source: p.Source, Rationale: p.Rationale, CreatedByPrincipalID: p.CreatedByPrincipalID,
			RequestKey: p.RequestKey, Meta: p.Meta,
		}
		if params.Source == "" {
			params.Source = "internal"
		}

		// The natural key: this semantic version, in this family and scope.
		var existingID string
		err = tx.QueryRow(ctx, `
			SELECT policy_version_id FROM accounting_policy_version_details
			WHERE policy_id = $1
			  AND COALESCE(legal_entity_id, '`+nilScopeUUID+`'::UUID) = COALESCE($2::uuid, '`+nilScopeUUID+`'::UUID)
			  AND semantic_version = $3;`, family.PolicyID, p.LegalEntityID, p.SemanticVersion).Scan(&existingID)
		switch {
		case err == nil:
			existing, err := scanPolicyVersion(tx.QueryRow(ctx, `SELECT `+policyVersionColumns+` FROM policy_versions WHERE policy_version_id = $1;`, existingID))
			if err != nil {
				return err
			}
			if !sameVersionContent(existing, params) {
				return domain.ErrIdempotencyMismatch
			}
			out, err = loadAccountingVersion(ctx, tx, existingID)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		v, isNew, err := s.createVersionTx(ctx, tx, params)
		if err != nil {
			return err
		}
		if isNew {
			if err := insertAccountingDetails(ctx, tx, v, p); err != nil {
				return err
			}
			created = true
		}
		out, err = loadAccountingVersion(ctx, tx, v.PolicyVersionID)
		return err
	})
	if err != nil {
		return nil, false, s.domainError("CreateAccountingDraft", err)
	}
	return out, created, nil
}

// ensureAccountingFamily returns the family for the applicability, creating it
// (and announcing policy.created) the first time. A family that exists with a
// different framework is a conflict: a book has one framework.
func (s *PgStore) ensureAccountingFamily(ctx context.Context, tx pgx.Tx, p domain.DraftAccountingPolicyParams, code string) (*domain.Policy, error) {
	pol, err := scanPolicy(tx.QueryRow(ctx, `
		INSERT INTO policies (policy_id, policy_code, policy_name, policy_type, tenant_id, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING
		RETURNING `+policyColumns+`;`,
		uuid.New().String(), code, accountingFamilyName(p.PolicyDomain, p.BookID, p.JurisdictionID),
		domain.PolicyTypeAccounting, p.TenantID, p.CreatedByPrincipalID))
	if err == nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO accounting_policy_applicability (policy_id, tenant_id, policy_domain, framework, book_id, jurisdiction_id, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7);`,
			pol.PolicyID, p.TenantID, p.PolicyDomain, p.Framework, p.BookID, p.JurisdictionID, p.CreatedByPrincipalID); err != nil {
			return nil, err
		}
		return pol, s.enqueue(ctx, tx, policyEvent("policy.created", pol, p.Meta))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	pol, err = scanPolicy(tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM policies WHERE tenant_id = $1::uuid AND policy_code = $2;`, p.TenantID, code))
	if err != nil {
		return nil, err
	}
	var framework string
	err = tx.QueryRow(ctx, `SELECT framework FROM accounting_policy_applicability WHERE policy_id = $1;`, pol.PolicyID).Scan(&framework)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && framework != p.Framework) {
		// Not an accounting family at all, or a different framework.
		return nil, domain.ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return pol, nil
}

func insertAccountingDetails(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, p domain.DraftAccountingPolicyParams) error {
	impact, err := json.Marshal(p.Impact)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO accounting_policy_version_details
			(policy_version_id, policy_id, tenant_id, legal_entity_id, semantic_version, precedence, framework_source, policy_text_ref, impact_analysis)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);`,
		v.PolicyVersionID, v.PolicyID, p.TenantID, p.LegalEntityID, p.SemanticVersion, p.Precedence, p.FrameworkSource, p.PolicyTextRef, impact); err != nil {
		return err
	}
	for i, m := range p.RuleMappings {
		var dependsOn *string
		if m.RefKind == domain.MappingKindPolicyVersion {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounting_policy_version_details WHERE policy_version_id = $1::uuid);`, m.Ref).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return domain.ErrAccountingDependencyNotFound
			}
			ref := m.Ref
			dependsOn = &ref
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO accounting_policy_rule_mappings
				(policy_version_id, tenant_id, mapping_no, ref_kind, ref, ref_version, mapping_rationale, maps_to, depends_on_version_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);`,
			v.PolicyVersionID, p.TenantID, i+1, m.RefKind, m.Ref, m.RefVersion, m.Rationale, nilIfEmpty(m.MapsTo), dependsOn); err != nil {
			return err
		}
	}
	return nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AccountingCandidates returns every accounting version of the tenant that was
// ever bound (activated or scheduled) and could apply to the request: its scope
// fits the entity, and its family applies to the book and jurisdiction. A
// family narrowed to a book or jurisdiction does not apply to a request that
// does not name it. Which candidate governs at a time is decided by package
// resolve, so the same candidates serve every as_of and known_at.
func (s *PgStore) AccountingCandidates(ctx context.Context, q domain.AccountingCandidateQuery) ([]*domain.AccountingCandidate, error) {
	if err := requireCallerTenant(ctx, q.TenantID); err != nil {
		return nil, err
	}
	var out []*domain.AccountingCandidate
	err := s.withRLS(ctx, q.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+versionColumns("pv")+`, `+accountingTail+accountingFrom+`
			WHERE pv.activated_at IS NOT NULL
			  AND pv.tenant_id = $1::uuid
			  AND (pv.legal_entity_id IS NULL OR pv.legal_entity_id = $2::uuid)
			  AND ($3 = '' OR a.policy_domain = $3)
			  AND (a.book_id IS NULL OR a.book_id = $4::uuid)
			  AND (a.jurisdiction_id IS NULL OR a.jurisdiction_id = $5::uuid)
			ORDER BY pv.activated_at, pv.policy_version_id;`,
			q.TenantID, q.LegalEntityID, q.PolicyDomain, q.BookID, q.JurisdictionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			av, err := scanAccounting(rows)
			if err != nil {
				return err
			}
			c := &domain.AccountingCandidate{
				Applicability: av.Applicability, SemanticVersion: av.Details.SemanticVersion, Precedence: av.Details.Precedence,
			}
			c.PolicyVersion = av.PolicyVersion
			c.PolicyCode = av.PolicyCode
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.domainError("AccountingCandidates", err)
	}
	return out, nil
}

// accountingApplicability is the family's applicability, or nil when the
// policy is not an accounting policy.
func (s *PgStore) accountingApplicability(ctx context.Context, tx pgx.Tx, policyID string) (*domain.AccountingApplicability, error) {
	a := &domain.AccountingApplicability{PolicyID: policyID}
	err := tx.QueryRow(ctx, `
		SELECT policy_domain, framework, book_id, jurisdiction_id, created_at
		FROM accounting_policy_applicability WHERE policy_id = $1;`, policyID).
		Scan(&a.PolicyDomain, &a.Framework, &a.BookID, &a.JurisdictionID, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// guardAccountingOverlap blocks an activation that would leave two accounting
// policies that could both apply to one request, equally specific and of equal
// precedence, governing the same period. Such a pair would make resolution
// ambiguous, and an ambiguous policy blocks calculation, so it is refused when
// it is made rather than discovered when a posting is attempted.
//
// "No overlapping contradictory active versions without explicit precedence."
// Two policies of different specificity are not contradictory (the narrower
// overrides), and neither are two that cannot meet in one request (different
// books, say). The advisory lock serialises activations of one tenant's domain.
func (s *PgStore) guardAccountingOverlap(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, fam *domain.AccountingApplicability) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1));`, "policy-svc:accounting:"+derefOrEmpty(v.TenantID)+":"+fam.PolicyDomain); err != nil {
		return err
	}
	var precedence int
	if err := tx.QueryRow(ctx, `SELECT precedence FROM accounting_policy_version_details WHERE policy_version_id = $1;`, v.PolicyVersionID).Scan(&precedence); err != nil {
		return err
	}
	var conflicting string
	err := tx.QueryRow(ctx, `
		SELECT pv.policy_version_id
		FROM policy_versions pv
		JOIN accounting_policy_applicability a ON a.policy_id = pv.policy_id
		JOIN accounting_policy_version_details d ON d.policy_version_id = pv.policy_version_id
		WHERE pv.policy_id <> $1
		  AND a.policy_domain = $2
		  AND pv.tenant_id IS NOT DISTINCT FROM $3::uuid
		  AND pv.legal_entity_id IS NOT DISTINCT FROM $4::uuid
		  AND pv.version_status IN ('ACTIVE', 'SCHEDULED')
		  AND pv.withdrawn_at IS NULL
		  AND pv.effective_from < COALESCE($6::timestamptz, 'infinity'::timestamptz)
		  AND COALESCE(pv.effective_to, 'infinity'::timestamptz) > $5
		  AND d.precedence = $7
		  AND (a.book_id IS NULL)::int + (a.jurisdiction_id IS NULL)::int = $8
		  AND (a.book_id IS NULL OR $9::uuid IS NULL OR a.book_id = $9::uuid)
		  AND (a.jurisdiction_id IS NULL OR $10::uuid IS NULL OR a.jurisdiction_id = $10::uuid)
		LIMIT 1;`,
		v.PolicyID, fam.PolicyDomain, v.TenantID, v.LegalEntityID, v.EffectiveFrom, v.EffectiveTo,
		precedence, 2-fam.Specificity(), fam.BookID, fam.JurisdictionID).Scan(&conflicting)
	if err == nil {
		return fmt.Errorf("%w: version %s of another %s accounting policy could apply to the same requests over the same period with the same specificity and precedence; give one an explicit higher precedence",
			domain.ErrPolicyConflicted, conflicting, fam.PolicyDomain)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}
