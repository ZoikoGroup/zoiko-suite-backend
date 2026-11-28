package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/policy-svc/internal/domain"
)

// maxGoverningPerType bounds the governing versions a status card lists.
const maxGoverningPerType = 50

// FamilyStatuses builds the status card of every policy family visible to the
// caller's tenant (its own and platform families), keyed by policy type.
func (s *PgStore) FamilyStatuses(ctx context.Context) (map[string]*domain.FamilyStatus, error) {
	tenantID := contextTenantID(ctx)
	out := map[string]*domain.FamilyStatus{}
	card := func(policyType string) *domain.FamilyStatus {
		c, ok := out[policyType]
		if !ok {
			c = &domain.FamilyStatus{PolicyType: policyType, VersionsByStatus: map[string]int64{},
				GoverningVersions: []domain.GoverningVersion{}, Decisions24h: map[string]int64{}}
			out[policyType] = c
		}
		return c
	}
	since := s.now().Add(-24 * time.Hour)
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT policy_type, COUNT(*) FROM policies GROUP BY policy_type;`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t string
			var n int64
			if err := rows.Scan(&t, &n); err != nil {
				rows.Close()
				return err
			}
			card(t).Families = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT p.policy_type, pv.version_status, COUNT(*)
			FROM policy_versions pv JOIN policies p ON p.policy_id = pv.policy_id
			GROUP BY p.policy_type, pv.version_status;`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t, st string
			var n int64
			if err := rows.Scan(&t, &st, &n); err != nil {
				rows.Close()
				return err
			}
			card(t).VersionsByStatus[st] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT policy_type, policy_code, policy_id, policy_version_id, version_number, version_status,
			       tenant_id, legal_entity_id, effective_from, effective_to, artifact_digest,
			       created_by_principal_id, submitted_by_principal_id, approved_by_principal_id, approved_at,
			       approval_reference, activated_by_principal_id, activated_at
			FROM (
				SELECT p.policy_type, p.policy_code, pv.*,
				       row_number() OVER (PARTITION BY p.policy_type ORDER BY pv.activated_at DESC NULLS LAST, pv.policy_version_id) AS rn
				FROM policy_versions pv JOIN policies p ON p.policy_id = pv.policy_id
				WHERE pv.version_status IN ('ACTIVE', 'SCHEDULED')
			) g
			WHERE rn <= $1
			ORDER BY policy_type, activated_at DESC NULLS LAST, policy_version_id;`, maxGoverningPerType)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t string
			var g domain.GoverningVersion
			if err := rows.Scan(&t, &g.PolicyCode, &g.PolicyID, &g.PolicyVersionID, &g.VersionNumber, &g.VersionStatus,
				&g.TenantID, &g.LegalEntityID, &g.EffectiveFrom, &g.EffectiveTo, &g.ArtifactDigest,
				&g.CreatedByPrincipalID, &g.SubmittedByPrincipalID, &g.ApprovedByPrincipalID, &g.ApprovedAt,
				&g.ApprovalReference, &g.ActivatedByPrincipalID, &g.ActivatedAt); err != nil {
				rows.Close()
				return err
			}
			g.ScopeType = domain.DeriveScopeType(g.TenantID, g.LegalEntityID)
			c := card(t)
			c.GoverningVersions = append(c.GoverningVersions, g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT policy_type, COALESCE(decision_outcome, result), COUNT(*)
			FROM policy_evaluations
			WHERE tenant_id = $1::uuid AND evaluated_at >= $2
			GROUP BY 1, 2;`, tenantID, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t, o string
			var n int64
			if err := rows.Scan(&t, &o, &n); err != nil {
				return err
			}
			card(t).Decisions24h[o] = n
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.domainError("FamilyStatuses", err)
	}
	return out, nil
}
