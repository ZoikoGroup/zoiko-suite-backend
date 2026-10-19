package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

func (p *PgStore) tenant(ctx context.Context) string { return middleware.GetTenantID(ctx) }

// ensureConnector confirms the connector exists in the caller's tenant and
// returns its status, holding a share lock so it cannot be revoked between the
// check and the write that follows.
func (p *PgStore) ensureConnector(ctx context.Context, tx pgx.Tx, id string) (status string, err error) {
	err = tx.QueryRow(ctx, `SELECT status FROM api_bridges WHERE bridge_id = $1 AND tenant_id = $2 FOR SHARE`,
		id, p.tenant(ctx)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrBridgeNotFound
	}
	return status, err
}

func (p *PgStore) ListChanges(ctx context.Context, id string) ([]domain.Change, error) {
	out := make([]domain.Change, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := p.ensureConnector(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT change_id::text, connector_id, tenant_id, from_version, to_version, change_kind, changed_by, changed_fields,
			       scopes_added, scopes_removed, prior_fingerprint, new_fingerprint, reason, changed_at
			FROM connector_changes WHERE connector_id = $1 AND tenant_id = $2 ORDER BY changed_at, to_version`, id, p.tenant(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.Change
			if err := rows.Scan(&c.ChangeID, &c.ConnectorID, &c.TenantID, &c.FromVersion, &c.ToVersion, &c.Kind, &c.ChangedBy,
				&c.ChangedFields, &c.ScopesAdded, &c.ScopesRemoved, &c.PriorFingerprint, &c.NewFingerprint, &c.Reason, &c.ChangedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func (p *PgStore) ListApprovals(ctx context.Context, id string) ([]domain.Approval, error) {
	out := make([]domain.Approval, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := p.ensureConnector(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT approval_id::text, connector_id, tenant_id, connector_version, fingerprint, decision, requested_by, decided_by,
			       reason, decided_at
			FROM connector_approvals WHERE connector_id = $1 AND tenant_id = $2 ORDER BY decided_at`, id, p.tenant(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.Approval
			if err := rows.Scan(&a.ApprovalID, &a.ConnectorID, &a.TenantID, &a.ConnectorVersion, &a.Fingerprint, &a.Decision,
				&a.RequestedBy, &a.DecidedBy, &a.Reason, &a.DecidedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

func (p *PgStore) GetHealth(ctx context.Context, id string) (*domain.Health, error) {
	var h *domain.Health
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := p.ensureConnector(ctx, tx, id); err != nil {
			return err
		}
		var err error
		h, err = p.readHealth(ctx, tx, id)
		return err
	})
	return h, err
}

func (p *PgStore) ListHealthTransitions(ctx context.Context, id string, limit int) ([]domain.HealthTransition, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	out := make([]domain.HealthTransition, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := p.ensureConnector(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT transition_id::text, connector_id, tenant_id, from_state, to_state, reason_code, detail, at
			FROM connector_health_transitions WHERE connector_id = $1 AND tenant_id = $2 ORDER BY at DESC LIMIT $3`,
			id, p.tenant(ctx), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.HealthTransition
			if err := rows.Scan(&t.TransitionID, &t.ConnectorID, &t.TenantID, &t.FromState, &t.ToState, &t.ReasonCode, &t.Detail, &t.At); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// ── Checkpoints ───────────────────────────────────────────────────────────────

const checkpointCols = `connector_id, tenant_id, stream, cursor, watermark, checkpoint_version, updated_by, updated_at`

func scanCheckpoint(row pgx.Row, c *domain.Checkpoint) error {
	return row.Scan(&c.ConnectorID, &c.TenantID, &c.Stream, &c.Cursor, &c.Watermark, &c.Version, &c.UpdatedBy, &c.UpdatedAt)
}

func (p *PgStore) ListCheckpoints(ctx context.Context, id string) ([]domain.Checkpoint, error) {
	out := make([]domain.Checkpoint, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := p.ensureConnector(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+checkpointCols+` FROM sync_checkpoints WHERE connector_id = $1 AND tenant_id = $2 ORDER BY stream`,
			id, p.tenant(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.Checkpoint
			if err := scanCheckpoint(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func (p *PgStore) AdvanceCheckpoint(ctx context.Context, id, stream, cursor string, watermark *time.Time,
	expectedVersion int64, actor string, ev domain.OutboxEvent) (*domain.Checkpoint, error) {
	var out domain.Checkpoint
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		// A connector that is not live must not move data, so it must not move
		// its checkpoint either (INT-NP-020: no unauthorized continuation).
		var b domain.ApiBridge
		err := scanBridge(tx.QueryRow(ctx, `SELECT `+bridgeCols+` FROM api_bridges WHERE bridge_id = $1 AND tenant_id = $2 FOR SHARE`,
			id, p.tenant(ctx)), &b)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrBridgeNotFound
		}
		if err != nil {
			return err
		}
		if !b.Live() {
			return domain.ErrInvalidTransition
		}

		var cur domain.Checkpoint
		err = scanCheckpoint(tx.QueryRow(ctx, `SELECT `+checkpointCols+` FROM sync_checkpoints
			WHERE connector_id = $1 AND stream = $2 AND tenant_id = $3 FOR UPDATE`, id, stream, p.tenant(ctx)), &cur)
		now := time.Now().UTC()
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if expectedVersion != 0 {
				return domain.ErrCheckpointConflict
			}
			out = domain.Checkpoint{ConnectorID: id, TenantID: p.tenant(ctx), Stream: stream, Cursor: cursor,
				Watermark: watermark, Version: 1, UpdatedBy: actor, UpdatedAt: now}
			_, err = tx.Exec(ctx, `INSERT INTO sync_checkpoints (`+checkpointCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				out.ConnectorID, out.TenantID, out.Stream, out.Cursor, out.Watermark, out.Version, out.UpdatedBy, out.UpdatedAt)
			if err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if expectedVersion != cur.Version {
				return domain.ErrCheckpointConflict
			}
			if cur.Watermark != nil && (watermark == nil || watermark.Before(*cur.Watermark)) {
				return domain.ErrCheckpointRegress
			}
			out = cur
			out.Cursor, out.Watermark, out.Version, out.UpdatedBy, out.UpdatedAt = cursor, watermark, cur.Version+1, actor, now
			_, err = tx.Exec(ctx, `UPDATE sync_checkpoints SET cursor=$4, watermark=$5, checkpoint_version=$6, updated_by=$7, updated_at=$8
				WHERE connector_id=$1 AND stream=$2 AND tenant_id=$3`,
				id, stream, p.tenant(ctx), out.Cursor, out.Watermark, out.Version, out.UpdatedBy, out.UpdatedAt)
			if err != nil {
				return err
			}
		}
		ev.AggregateVersion = &out.Version
		ev.AggregateType = "sync-checkpoint"
		ev.AggregateID = id + "/" + stream
		ev.LegalEntityID = b.LegalEntityID
		return p.enqueue(ctx, tx, b.TenantID, ev)
	})
	if err != nil {
		if pgCode(err) == "23000" { // the database's own no-rollback guard
			return nil, domain.ErrCheckpointRegress
		}
		return nil, err
	}
	return &out, nil
}

const resetCols = `request_id::text, connector_id, tenant_id, stream, to_cursor, to_watermark, reason, requested_by, status,
	COALESCE(decided_by, ''), decided_at, created_at`

func scanReset(row pgx.Row, r *domain.ResetRequest) error {
	return row.Scan(&r.RequestID, &r.ConnectorID, &r.TenantID, &r.Stream, &r.ToCursor, &r.ToWatermark, &r.Reason,
		&r.RequestedBy, &r.Status, &r.DecidedBy, &r.DecidedAt, &r.CreatedAt)
}

func (p *PgStore) CreateResetRequest(ctx context.Context, r *domain.ResetRequest, ev domain.OutboxEvent) error {
	r.RequestID = uuid.New().String()
	r.TenantID = p.tenant(ctx)
	r.Status = domain.ResetPending
	r.CreatedAt = time.Now().UTC()
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		var b domain.ApiBridge
		err := scanBridge(tx.QueryRow(ctx, `SELECT `+bridgeCols+` FROM api_bridges WHERE bridge_id = $1 AND tenant_id = $2 FOR SHARE`,
			r.ConnectorID, r.TenantID), &b)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrBridgeNotFound
		}
		if err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_checkpoints WHERE connector_id=$1 AND stream=$2 AND tenant_id=$3)`,
			r.ConnectorID, r.Stream, r.TenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrCheckpointNotFound
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO checkpoint_reset_requests (request_id, connector_id, tenant_id, stream, to_cursor, to_watermark, reason,
				requested_by, status, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			r.RequestID, r.ConnectorID, r.TenantID, r.Stream, r.ToCursor, r.ToWatermark, r.Reason, r.RequestedBy, r.Status, r.CreatedAt); err != nil {
			return err
		}
		ev.AggregateType, ev.AggregateID, ev.LegalEntityID = "sync-checkpoint", r.ConnectorID+"/"+r.Stream, b.LegalEntityID
		return p.enqueue(ctx, tx, r.TenantID, ev)
	})
}

func (p *PgStore) DecideResetRequest(ctx context.Context, requestID, actor string, approve bool, ev domain.OutboxEvent) (*domain.ResetRequest, error) {
	var out domain.ResetRequest
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		err := scanReset(tx.QueryRow(ctx, `SELECT `+resetCols+` FROM checkpoint_reset_requests WHERE request_id = $1::uuid AND tenant_id = $2 FOR UPDATE`,
			requestID, p.tenant(ctx)), &out)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrResetNotFound
		}
		if err != nil {
			return err
		}
		if out.Status != domain.ResetPending {
			return domain.ErrResetNotPending
		}
		if actor == "" || actor == out.RequestedBy {
			return domain.ErrSelfApproval
		}
		var legalEntity string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id FROM api_bridges WHERE bridge_id=$1 AND tenant_id=$2 FOR SHARE`,
			out.ConnectorID, out.TenantID).Scan(&legalEntity); err != nil {
			return err
		}
		now := time.Now().UTC()
		status := domain.ResetRejected
		if approve {
			status = domain.ResetApplied
			// The one sanctioned way for a checkpoint to move backwards. The
			// flag lives for this transaction only.
			if _, err := tx.Exec(ctx, `SELECT set_config('app.checkpoint_reset', 'true', true)`); err != nil {
				return err
			}
			var ver int64
			err := tx.QueryRow(ctx, `SELECT checkpoint_version FROM sync_checkpoints WHERE connector_id=$1 AND stream=$2 AND tenant_id=$3 FOR UPDATE`,
				out.ConnectorID, out.Stream, out.TenantID).Scan(&ver)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCheckpointNotFound
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE sync_checkpoints SET cursor=$4, watermark=$5, checkpoint_version=$6, updated_by=$7, updated_at=$8
				WHERE connector_id=$1 AND stream=$2 AND tenant_id=$3`,
				out.ConnectorID, out.Stream, out.TenantID, out.ToCursor, out.ToWatermark, ver+1, actor, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.checkpoint_reset', '', true)`); err != nil {
				return err
			}
			v := ver + 1
			ev.AggregateVersion = &v
		}
		if _, err := tx.Exec(ctx, `UPDATE checkpoint_reset_requests SET status=$2, decided_by=$3, decided_at=$4 WHERE request_id=$1::uuid`,
			out.RequestID, status, actor, now); err != nil {
			return err
		}
		out.Status, out.DecidedBy, out.DecidedAt = status, actor, &now
		ev.AggregateType, ev.AggregateID, ev.LegalEntityID = "sync-checkpoint", out.ConnectorID+"/"+out.Stream, legalEntity
		return p.enqueue(ctx, tx, out.TenantID, ev)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Profiles ──────────────────────────────────────────────────────────────────

const profileCols = `profile_id::text, provider_key, provider_type, version, status, allowed_regions, allowed_host_suffixes,
	supported_scopes, schema_ref, certification_ref, published_by, created_at, deprecated_at, sunset_at`

func scanProfile(row pgx.Row, p *domain.Profile) error {
	return row.Scan(&p.ProfileID, &p.ProviderKey, &p.ProviderType, &p.Version, &p.Status, &p.AllowedRegions,
		&p.AllowedHostSuffixes, &p.SupportedScopes, &p.SchemaRef, &p.CertificationRef, &p.PublishedBy, &p.CreatedAt,
		&p.DeprecatedAt, &p.SunsetAt)
}

func (p *PgStore) PublishProfile(ctx context.Context, pr *domain.Profile, ev domain.OutboxEvent) error {
	pr.ProfileID = uuid.New().String()
	pr.CreatedAt = time.Now().UTC()
	if pr.Status == "" {
		pr.Status = domain.ProfileDraft
	}
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO external_profiles (profile_id, provider_key, provider_type, version, status, allowed_regions,
				allowed_host_suffixes, supported_scopes, schema_ref, certification_ref, published_by, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			pr.ProfileID, pr.ProviderKey, pr.ProviderType, pr.Version, pr.Status, pr.AllowedRegions,
			pr.AllowedHostSuffixes, pr.SupportedScopes, pr.SchemaRef, pr.CertificationRef, pr.PublishedBy, pr.CreatedAt)
		if err != nil {
			return err
		}
		ev.AggregateType = "external-profile"
		ev.AggregateID = pr.ProviderKey + "@" + itoa(pr.Version)
		v := int64(pr.Version)
		ev.AggregateVersion = &v
		return p.enqueue(ctx, tx, p.tenant(ctx), ev)
	})
	switch pgCode(err) {
	case "23505":
		return domain.ErrProfileExists
	case "42501":
		return domain.ErrPlatformScopeRequired
	}
	return err
}

func (p *PgStore) GetProfile(ctx context.Context, key string, version int) (*domain.Profile, error) {
	var pr domain.Profile
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM external_profiles WHERE provider_key = $1 AND version = $2`,
			key, version), &pr)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

func (p *PgStore) ListProfiles(ctx context.Context, key string) ([]domain.Profile, error) {
	out := make([]domain.Profile, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+profileCols+` FROM external_profiles WHERE ($1 = '' OR provider_key = $1)
			ORDER BY provider_key, version`, key)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pr domain.Profile
			if err := scanProfile(rows, &pr); err != nil {
				return err
			}
			out = append(out, pr)
		}
		return rows.Err()
	})
	return out, err
}

func (p *PgStore) SetProfileStatus(ctx context.Context, key string, version int, status string, at time.Time, ev domain.OutboxEvent) (*domain.Profile, error) {
	var pr domain.Profile
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		err := scanProfile(tx.QueryRow(ctx, `
			UPDATE external_profiles SET status = $3::text,
				deprecated_at = CASE WHEN $3::text IN ('DEPRECATED', 'UNSUPPORTED') AND deprecated_at IS NULL THEN $4::timestamptz ELSE deprecated_at END,
				sunset_at = CASE WHEN $3::text = 'UNSUPPORTED' AND sunset_at IS NULL THEN $4::timestamptz ELSE sunset_at END
			WHERE provider_key = $1 AND version = $2
			RETURNING `+profileCols, key, version, status, at), &pr)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrProfileNotFound
		}
		if err != nil {
			return err
		}
		ev.AggregateType = "external-profile"
		ev.AggregateID = key + "@" + itoa(version)
		v := int64(version)
		ev.AggregateVersion = &v
		return p.enqueue(ctx, tx, p.tenant(ctx), ev)
	})
	if pgCode(err) == "23000" {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
