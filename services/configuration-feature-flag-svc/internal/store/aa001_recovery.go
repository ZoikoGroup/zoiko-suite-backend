package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

// ── recovery: rollback (INV-17) and the emergency retrospective (INV-15) ─────
//
// §0 gives the control plane's reason for existing as reversibility: a bad
// value must be undoable as a governed act, not by someone remembering what
// the old value was and writing it again.

// RollbackChange proposes the change that restores a VERIFIED change's
// before-state. It does not apply anything: the rollback is itself a change
// (000006: "a rollback is a new change pointing at its rollback_change_id"),
// of the same class and approval requirement as the change it undoes, so it
// goes through the same approve → activate path. Activation marks the target
// ROLLED_BACK.
//
// The parts are computed from the target's before snapshot — the immutable
// imprint pinned when the target was created — not from anyone's memory of
// the old values. For each scope the target wrote:
//   - a value existed before → write that value back;
//   - nothing existed before → Remove, ending the value the target created.
//
// Rollback restores configuration state only (INV-17). It never claims to
// reverse a domain side effect a consumer took while the value was live.
func (s *PgStore) RollbackChange(ctx context.Context, targetID, callerTenantID, actor, correlationID string) (*domain.ConfigChange, error) {
	if callerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}

	var target *domain.ConfigChange
	var before map[string]domain.ManifestEntry
	err := s.withTenantTx(ctx, callerTenantID, func(tx pgx.Tx) error {
		t, err := scanChange(tx.QueryRow(ctx, `
			SELECT `+changeColumns+` FROM config_changes WHERE change_id = $1`, targetID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrChangeNotFound
		}
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		// APPLYING counts: a bad change still propagating is exactly the one
		// that most needs undoing.
		if (t.Status != domain.ChangeStatusVerified && t.Status != domain.ChangeStatusApplying) || t.BeforeSnapshotID == nil {
			return domain.ErrRollbackTargetInvalid
		}
		if t.TenantID != nil && *t.TenantID != callerTenantID {
			return domain.ErrScopeNotAllowed
		}
		var content []byte
		if err := tx.QueryRow(ctx, `
			SELECT content FROM config_snapshots WHERE snapshot_id = $1`, *t.BeforeSnapshotID).Scan(&content); err != nil {
			return fmt.Errorf("%w: before snapshot: %v", domain.ErrStoreUnavailable, err)
		}
		before, err = parseManifest(content)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		target = t
		return nil
	})
	if err != nil {
		return nil, err
	}

	var targetParts []domain.ChangePart
	if err := json.Unmarshal(target.Parts, &targetParts); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	parts := make([]domain.ChangePart, 0, len(targetParts))
	for _, p := range targetParts {
		restore := domain.ChangePart{Kind: p.Kind, Key: p.Key, Scope: p.Scope}
		prev, existed := before[domain.PartManifestKey(p)]
		switch {
		case !existed:
			restore.Remove = true
		case prev.Kind == domain.ManifestKindFlag:
			restore.NewEnabled = prev.Enabled
			restore.RolloutPercentage = prev.RolloutPercentage
		default:
			restore.NewValue = prev.Value
		}
		parts = append(parts, restore)
	}
	partsJSON, err := json.Marshal(parts)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	rollbackOf := target.ChangeID
	change := &domain.ConfigChange{
		ChangeClass:          target.ChangeClass,
		Environment:          target.Environment,
		TenantID:             target.TenantID,
		ApprovalRequired:     target.ApprovalRequired,
		RollbackChangeID:     &rollbackOf,
		CreatedByPrincipalID: actor,
		Parts:                partsJSON,
	}
	return s.doCreateChange(ctx, callerTenantID, change, partsJSON)
}

// markRolledBack flips the change a verified rollback undid to ROLLED_BACK,
// inside the rollback's own activation transaction. Only a VERIFIED target
// moves; anything else means the rollback was proposed against a change that
// has since changed state, and the activation is refused.
func markRolledBack(ctx context.Context, tx pgx.Tx, targetID string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE config_changes SET status = $2, updated_at = NOW()
		WHERE change_id = $1 AND status IN ($3, $4)`,
		targetID, domain.ChangeStatusRolledBack, domain.ChangeStatusVerified, domain.ChangeStatusApplying)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrRollbackTargetInvalid
	}
	return nil
}

// endCurrentValue applies a Remove part: the scope's current row is ended and
// nothing replaces it. A scope with no current value is already in the
// requested state.
func endCurrentValue(ctx context.Context, tx pgx.Tx, flag bool, key, environment string, tenantID *string) error {
	table := "config_entries"
	if flag {
		table = "feature_flags"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE `+table+` SET effective_to = NOW()
		WHERE key = $1 AND environment = $2
		  AND COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID) = COALESCE($3::uuid, '`+nilScopeUUID+`'::UUID)
		  AND effective_to IS NULL`, key, environment, tenantID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// CloseEmergencyRetrospective records the mandatory retrospective review of an
// expired emergency change (INV-15) and closes it. It is the only way out of
// RETROSPECTIVE_PENDING, and it requires a review reference — the record in
// the incident/WFC process — so "closed" always points at the review.
func (s *PgStore) CloseEmergencyRetrospective(ctx context.Context, emergencyChangeID, reference, callerTenantID, actor string) (*domain.EmergencyChange, error) {
	if callerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil, domain.ErrValueConstraintFailed
	}

	var out *domain.EmergencyChange
	err := s.withOps(ctx, func(tx pgx.Tx) error {
		e := &domain.EmergencyChange{EmergencyChangeID: emergencyChangeID}
		if err := tx.QueryRow(ctx, `
			SELECT key, environment, tenant_id, status, expires_at, reverted_to_prior, actor_principal_id
			FROM emergency_changes WHERE emergency_change_id = $1 FOR UPDATE`, emergencyChangeID).
			Scan(&e.Key, &e.Environment, &e.TenantID, &e.Status, &e.ExpiresAt, &e.RevertedToPrior, &e.ActorPrincipalID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrChangeNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if e.TenantID != nil && *e.TenantID != callerTenantID {
			return domain.ErrScopeNotAllowed
		}
		if e.Status != domain.EmergencyStatusRetrospectivePending {
			return domain.ErrRetrospectiveNotPending
		}
		if err := tx.QueryRow(ctx, `
			UPDATE emergency_changes
			SET status = $2, retrospective_closed_at = NOW(), retrospective_reference = $3
			WHERE emergency_change_id = $1
			RETURNING status, retrospective_closed_at, retrospective_reference`,
			emergencyChangeID, domain.EmergencyStatusClosed, reference,
		).Scan(&e.Status, &e.RetrospectiveClosedAt, &e.RetrospectiveReference); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TargetScope returns the tenant scope (nil = global) of an existing change
// ("change") or emergency change ("emergency"), so the handler can authorize
// by the target's scope before acting on it. Read under the ops escape: the
// answer is only ever a scope, and the store methods that follow still refuse
// a foreign tenant's target on their own.
func (s *PgStore) TargetScope(ctx context.Context, kind, id string) (*string, error) {
	var query string
	switch kind {
	case "change":
		query = `SELECT tenant_id FROM config_changes WHERE change_id::text = $1`
	case "emergency":
		query = `SELECT tenant_id FROM emergency_changes WHERE emergency_change_id::text = $1`
	default:
		return nil, fmt.Errorf("%w: unknown target kind %q", domain.ErrStoreUnavailable, kind)
	}
	var tenantID *string
	err := s.withOps(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id).Scan(&tenantID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrChangeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return tenantID, nil
}
