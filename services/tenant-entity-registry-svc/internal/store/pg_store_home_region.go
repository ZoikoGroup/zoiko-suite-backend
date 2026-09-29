package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ChangeHomeRegion applies an approved ORG-02 ChangeHomeRegion (migration
// 000012). One transaction:
//
//  1. the approval decision, guarded on the request still being PENDING
//  2. the target region exists and is active (residency_regions is IaC-owned)
//  3. the tenant's version, guarded on expected_version and a state that may
//     still change region (not OFFBOARDING / TERMINATED)
//  4. the default residency policy's region — the home-region pointer
//  5. the lineage row, with the home-region decision reference (§4.2 evidence;
//     tlh_home_region_evidenced refuses the row without it)
//  6. the event, stamped with the region it moved from
func (s *PgStore) ChangeHomeRegion(ctx context.Context, p registry.HomeRegionChange, ev *outbox.Record) (*registry.HomeRegionChangeResult, error) {
	tid := tenantFromCtxOrFallback(ctx, p.TenantID)
	var out registry.HomeRegionChangeResult

	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		now := time.Now().UTC()

		if err := decideApprovalTx(ctx, tx, tid, p.Approval, domain.ApprovalApproved); err != nil {
			return err
		}

		var active bool
		err := tx.QueryRow(ctx,
			`SELECT active_flag FROM residency_regions WHERE residency_region_id = $1`,
			p.ResidencyRegionID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
			return fmt.Errorf("%w: residency_region_id %s is unknown or inactive", registry.ErrReferenceInvalid, p.ResidencyRegionID)
		}
		if err != nil {
			return err
		}

		var policyID, state string
		err = tx.QueryRow(ctx, `
			UPDATE tenants
			   SET record_version = record_version + 1,
			       home_region_decision_ref = $1, home_region_changed_at = $2,
			       updated_at = $2, updated_by_principal_id = $3
			 WHERE tenant_id = $4 AND tenant_id = $5 AND record_version = $6
			   AND lifecycle_state IN ('ONBOARDING', 'ACTIVE', 'SUSPENDED')
			RETURNING default_data_residency_policy_id, lifecycle_state, record_version`,
			p.DecisionRef, now, p.Approval.DecidedByPrincipalID,
			p.TenantID, tid, p.ExpectedVersion).Scan(&policyID, &state, &out.NewVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrVersionConflict
		}
		if err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `
			SELECT residency_region_id::text FROM data_residency_policies
			 WHERE data_residency_policy_id = $1 AND tenant_id = $2 FOR UPDATE`,
			policyID, tid).Scan(&out.FromRegionID); err != nil {
			return fmt.Errorf("read current home region: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE data_residency_policies
			   SET residency_region_id = $1, updated_at = $2, updated_by_principal_id = $3
			 WHERE data_residency_policy_id = $4 AND tenant_id = $5`,
			p.ResidencyRegionID, now, p.Approval.DecidedByPrincipalID, policyID, tid); err != nil {
			return fmt.Errorf("re-point home region: %w", err)
		}
		out.ToRegionID = p.ResidencyRegionID

		if _, err := tx.Exec(ctx, `
			INSERT INTO tenant_lifecycle_history (
				lifecycle_event_id, tenant_id, from_state, to_state, command_name, reason,
				actor_principal_id, approved_by_principal_id, approval_request_id,
				correlation_id, occurred_at,
				home_region_decision_ref, from_region_id, to_region_id
			) VALUES ($1, $2, $3, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			uuid.New().String(), p.TenantID, state, string(domain.TenantCommandChangeHomeRegion),
			p.Reason, p.ActorID, p.Approval.DecidedByPrincipalID, p.Approval.ApprovalRequestID,
			nullableString(p.CorrelationID), now,
			p.DecisionRef, out.FromRegionID, p.ResidencyRegionID); err != nil {
			return fmt.Errorf("home-region lineage: %w", err)
		}

		if err := events.StampVersion(ev, out.NewVersion, map[string]any{
			"from_region_id": out.FromRegionID,
		}); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
