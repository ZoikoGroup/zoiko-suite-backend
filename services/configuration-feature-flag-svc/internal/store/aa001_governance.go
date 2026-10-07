package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/events"
)

// ── change classification, verification, retirement (AA-001 §8, §10.1) ──────

// gateDirectWrite refuses a direct value write — POST /v1/config, POST
// /v1/flags, PUT /v1/config/overrides — to an S2/S3 key. §8: changing a
// material runtime value is as consequential as deploying code, so it needs
// impact analysis, approval, effective time, propagation verification and a
// rollback point. The change set carries all of that; a direct write carries
// none of it, and used to be accepted for any key.
func gateDirectWrite(ctx context.Context, q queryer, key string) error {
	def, err := publishedDefinition(ctx, q, key)
	if err != nil {
		return err
	}
	if domain.IsMaterial(def.SafetyClass) {
		return domain.ErrMaterialKeyRequiresChange
	}
	return nil
}

// gateChangePart validates one change part against the key's published
// declaration: scope admitted, value valid, and — the part the change set
// used to skip — the change's class high enough for the key's safety class
// (S2 → C2, S3 → C3). It runs when the change is created, so an invalid
// change is refused up front instead of sitting PROPOSED until activation
// discovers it, and again at activation, since the declaration may have moved
// in between. Returns whether the key is a feature flag.
func gateChangePart(ctx context.Context, q queryer, p domain.ChangePart, changeClass string) (bool, error) {
	def, err := publishedDefinition(ctx, q, p.Key)
	if err != nil {
		return false, err
	}
	if !domain.ChangeClassCovers(changeClass, domain.MinChangeClass(def.SafetyClass)) {
		return false, domain.ErrChangeClassInsufficient
	}
	flag := def.FlagClass != nil
	layer := domain.PartLayer(p)
	if domain.IsExtendedLayer(layer) {
		if flag {
			return true, fmt.Errorf("%w: %v", domain.ErrScopeNotAllowed, errLayeredFlag)
		}
		if err := domain.ValidLayerTenancy(layer, p.Scope.TenantID, p.Scope.ScopeID); err != nil {
			return false, err
		}
	} else if p.Scope.Layer != "" && p.Scope.Layer != layer {
		return false, domain.ErrScopeNotAllowed
	}
	if err := domain.ValidateScope(def, layer); err != nil {
		return flag, err
	}
	if p.Remove {
		return flag, nil
	}
	if domain.IsExtendedLayer(layer) {
		// The layer was validated above; gateConfigWrite would re-derive a
		// TENANT/ENVIRONMENT layer from the tenant id.
		return false, domain.ValidateValue(p.NewValue, def, p.Scope.Environment)
	}
	if flag {
		enabled, rollout := flagPartValue(p)
		return true, gateFlagWrite(ctx, q, p.Key, p.Scope.Environment, p.Scope.TenantID, enabled, rollout)
	}
	return false, gateConfigWrite(ctx, q, def, p.Key, p.Scope.Environment, p.Scope.TenantID, p.NewValue)
}

// fleetConverged reports whether every runtime with a fresh attestation in the
// environment reports at least epoch. A runtime whose attestation has lapsed
// is not counted here — the drift sweep reports it as UNKNOWN — and an
// environment with no attesting runtime has nothing that can be behind.
func fleetConverged(ctx context.Context, tx pgx.Tx, environment string, epoch int64) (bool, error) {
	var behind int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT DISTINCT ON (runtime_id, COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID))
			       observed_epoch, freshness_deadline
			FROM runtime_attestations
			WHERE environment = $1
			ORDER BY runtime_id, COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID), reported_at DESC
		) latest
		WHERE freshness_deadline > NOW() AND observed_epoch < $2`, environment, epoch).Scan(&behind)
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return behind == 0, nil
}

// markVerified moves an APPLYING change to VERIFIED and announces it. NP-25:
// a change used to be VERIFIED the instant it was applied, while runtimes
// were still serving the previous snapshot; now it is VERIFIED only once the
// attesting fleet has caught up.
func markVerified(ctx context.Context, tx pgx.Tx, change *domain.ConfigChange, actor, callerTenantID string) error {
	now := time.Now()
	if err := tx.QueryRow(ctx, `
		UPDATE config_changes SET status = $2, verified_at = $3, updated_at = NOW()
		WHERE change_id = $1 RETURNING verified_at`,
		change.ChangeID, domain.ChangeStatusVerified, now).Scan(&change.VerifiedAt); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	change.Status = domain.ChangeStatusVerified
	outEv, evErr := events.ChangeVerified(*change, actor, "")
	return enqueueEvent(ctx, tx, callerTenantID, outEv, evErr)
}

// verifyApplyingChanges is the sweep's half of NP-25: every APPLYING change in
// the environment whose after-snapshot the attesting fleet has now reached
// becomes VERIFIED. Returns how many were verified.
func verifyApplyingChanges(ctx context.Context, tx pgx.Tx, environment string) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+prefixed("c.", changeColumns)+`, s.epoch
		FROM config_changes c
		JOIN config_snapshots s ON s.snapshot_id = c.proposed_snapshot_id
		WHERE c.environment = $1 AND c.status = $2
		FOR UPDATE OF c SKIP LOCKED`, environment, domain.ChangeStatusApplying)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	type applying struct {
		change *domain.ConfigChange
		epoch  int64
	}
	var pending []applying
	for rows.Next() {
		c := &domain.ConfigChange{}
		var epoch int64
		if err := rows.Scan(changeScanTargets(c, &epoch)...); err != nil {
			rows.Close()
			return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		pending = append(pending, applying{c, epoch})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	verified := 0
	for _, a := range pending {
		ok, err := fleetConverged(ctx, tx, environment, a.epoch)
		if err != nil {
			return verified, err
		}
		if !ok {
			continue
		}
		if err := markVerified(ctx, tx, a.change, "system:ops_sweep", deref(a.change.TenantID)); err != nil {
			return verified, err
		}
		verified++
	}
	return verified, nil
}

// prefixed qualifies each column of a comma-separated column list.
func prefixed(prefix, columns string) string {
	parts := strings.Split(columns, ",")
	for i, c := range parts {
		parts[i] = prefix + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// snapshotRefreshDue reports whether the environment's newest snapshot is
// within SnapshotRefreshMargin of its freshness deadline, so the sweep should
// re-mint it (same content, next epoch) before INV-13 withholds its material
// keys.
func snapshotRefreshDue(ctx context.Context, tx pgx.Tx, environment string) (bool, error) {
	snap, err := latestSnapshotRowQ(ctx, tx, environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return time.Until(snap.FreshnessDeadline) < domain.SnapshotRefreshMargin, nil
}

// MarkFlagRemovedParams carries POST /v1/flags/{key}/removal.
type MarkFlagRemovedParams struct {
	Key                  string
	Environment          string
	ConsumerScanEvidence json.RawMessage
	CallerTenantID       string
	ActorPrincipalID     string
}

// MarkFlagRemoved turns a RETIRED flag key into a REMOVED one, which is what
// finally makes the key reusable (INV-21: "flag removal requires
// consumer/code dependency verification; RETIRED is not equivalent to
// REMOVED"). The verification is the consumer-scan evidence, and it is
// required: a removal without it is refused.
func (s *PgStore) MarkFlagRemoved(ctx context.Context, params MarkFlagRemovedParams) (*domain.FlagRetirement, error) {
	if params.CallerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	var evidence map[string]any
	if err := json.Unmarshal(params.ConsumerScanEvidence, &evidence); err != nil || len(evidence) == 0 {
		return nil, domain.ErrValueConstraintFailed
	}
	out := &domain.FlagRetirement{Key: params.Key, Environment: params.Environment}
	err := s.withOps(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE flag_retirements
			SET reusable = true, removed_at = NOW(), consumer_scan_evidence = $3
			WHERE key = $1 AND environment = $2 AND reusable = false
			RETURNING retirement_id, tenant_id, final_enabled, final_rollout, reusable,
			          retired_by_principal_id, retired_at, removed_at`,
			params.Key, params.Environment, clampJSON(params.ConsumerScanEvidence),
		).Scan(&out.RetirementID, &out.TenantID, &out.FinalEnabled, &out.FinalRollout, &out.Reusable,
			&out.RetiredByPrincipalID, &out.RetiredAt, &out.RemovedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFlagNotRetired
		}
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if out.TenantID != nil && *out.TenantID != params.CallerTenantID {
			return domain.ErrScopeNotAllowed
		}
		out.ConsumerScanEvidence = params.ConsumerScanEvidence
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
