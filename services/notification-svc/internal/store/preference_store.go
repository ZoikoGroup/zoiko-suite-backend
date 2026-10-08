package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// ZS-SVC-Y-001 NCD-02 5.3: recipient convenience preferences (migration 000023).

const preferenceColumns = `tenant_id, principal_id, time_zone,
	to_char(quiet_start, 'HH24:MI'), to_char(quiet_end, 'HH24:MI'),
	muted_channels, version, updated_by, updated_at`

func scanPreferences(s scannable, p *domain.RecipientPreferences) error {
	var muted []byte
	if err := s.Scan(&p.TenantID, &p.PrincipalID, &p.TimeZone, &p.QuietStart, &p.QuietEnd,
		&muted, &p.Version, &p.UpdatedBy, &p.UpdatedAt); err != nil {
		return err
	}
	p.MutedChannels = []string{}
	if err := json.Unmarshal(muted, &p.MutedChannels); err != nil {
		return fmt.Errorf("decode muted_channels: %w", err)
	}
	return nil
}

// GetPreferences returns a recipient's profile, or ErrPreferencesNotFound when they have
// expressed none (which is not an error for a send: no preference, nothing to apply).
func (s *PgStore) GetPreferences(ctx context.Context, principalID string) (*domain.RecipientPreferences, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.RecipientPreferences
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanPreferences(tx.QueryRow(ctx, `SELECT `+preferenceColumns+` FROM recipient_preferences
			WHERE tenant_id = $1 AND principal_id = $2`, tenantID, principalID), &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPreferencesNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetPreferences replaces a recipient's profile (a new version each time).
func (s *PgStore) SetPreferences(ctx context.Context, p domain.SetPreferencesParams) (*domain.RecipientPreferences, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	muted := p.MutedChannels
	if muted == nil {
		muted = []string{}
	}
	mutedJSON, err := json.Marshal(muted)
	if err != nil {
		return nil, err
	}
	var out domain.RecipientPreferences
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanPreferences(tx.QueryRow(ctx, `
			INSERT INTO recipient_preferences (tenant_id, principal_id, time_zone, quiet_start, quiet_end, muted_channels, updated_by)
			VALUES ($1,$2,$3,$4::time,$5::time,$6::jsonb,$7)
			ON CONFLICT (tenant_id, principal_id) DO UPDATE SET
				time_zone = EXCLUDED.time_zone, quiet_start = EXCLUDED.quiet_start, quiet_end = EXCLUDED.quiet_end,
				muted_channels = EXCLUDED.muted_channels, updated_by = EXCLUDED.updated_by,
				version = recipient_preferences.version + 1, updated_at = now()
			RETURNING `+preferenceColumns,
			tenantID, p.PrincipalID, p.TimeZone, p.QuietStart, p.QuietEnd, string(mutedJSON), p.UpdatedBy), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
