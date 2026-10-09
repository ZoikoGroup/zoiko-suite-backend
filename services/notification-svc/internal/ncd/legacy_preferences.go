package ncd

import (
	"context"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// LegacyPreferenceSource lets the direct send guard (internal/policy) read the
// recipient preferences this plane owns (POST /v1/preferences), so the legacy
// send path and the plane honour ONE preference store. Without it the guard
// read a second table that nothing wrote once the plane's preference API won
// the /v1/preferences route.
//
// The tenant comes from the context, where the guard puts the notification's
// own tenant. A recipient who never set a preference is
// domain.ErrPreferencesNotFound, which the guard treats as "no constraints".
type LegacyPreferenceSource struct {
	Svc *Service
}

func (l LegacyPreferenceSource) GetPreferences(ctx context.Context, principalID string) (*domain.RecipientPreferences, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	p, err := l.Svc.GetPreference(ctx, Actor{TenantID: tenantID, PrincipalID: principalID})
	if err != nil {
		return nil, err
	}
	if p == nil || p.Version == 0 {
		return nil, domain.ErrPreferencesNotFound
	}
	out := &domain.RecipientPreferences{
		TenantID:      p.TenantID,
		PrincipalID:   p.PrincipalID,
		TimeZone:      p.TimeZone,
		MutedChannels: p.MutedChannels,
		Version:       p.Version,
		UpdatedBy:     p.UpdatedBy,
		UpdatedAt:     p.UpdatedAt,
	}
	if p.QuietHoursStart != "" && p.QuietHoursEnd != "" {
		start, end := p.QuietHoursStart, p.QuietHoursEnd
		out.QuietStart, out.QuietEnd = &start, &end
	}
	if out.MutedChannels == nil {
		out.MutedChannels = []string{}
	}
	return out, nil
}
