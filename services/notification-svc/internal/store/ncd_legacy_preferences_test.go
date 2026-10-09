package store_test

import (
	"errors"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/ncd"
)

// Since the 7 Oct merge the direct send guard reads recipient preferences
// through ncd.LegacyPreferenceSource, so the legacy send path honours the one
// preference store POST /v1/preferences writes.
func TestNCD02_LegacySendPathReadsThePlanePreferences(t *testing.T) {
	h := newHarness(t)
	r := h.recipient("pref-bridge@example.com")
	actor := h.as(r)
	src := ncd.LegacyPreferenceSource{Svc: h.svc}
	ctx := svcmiddleware.WithTenant(h.ctx, actor.TenantID)

	// Nothing set yet: the guard must see "no constraints", not an empty profile.
	if _, err := src.GetPreferences(ctx, r); !errors.Is(err, domain.ErrPreferencesNotFound) {
		t.Fatalf("want ErrPreferencesNotFound before any preference is set, got %v", err)
	}

	_, err := h.svc.SetPreference(h.ctx, actor, ncd.PreferenceInput{
		MutedChannels: []string{"EMAIL"}, QuietHoursStart: "22:00", QuietHoursEnd: "07:00", TimeZone: "Europe/London",
	})
	h.must(err)

	got, err := src.GetPreferences(ctx, r)
	h.must(err)
	if len(got.MutedChannels) != 1 || got.MutedChannels[0] != "EMAIL" {
		t.Fatalf("muted channels not carried over: %+v", got.MutedChannels)
	}
	if got.TimeZone != "Europe/London" || got.QuietStart == nil || *got.QuietStart != "22:00" || got.QuietEnd == nil || *got.QuietEnd != "07:00" {
		t.Fatalf("quiet hours not carried over: %+v", got)
	}

	// Another tenant's context sees nothing of this recipient.
	other := svcmiddleware.WithTenant(h.ctx, actor.TenantID+"-other")
	if _, err := src.GetPreferences(other, r); !errors.Is(err, domain.ErrPreferencesNotFound) {
		t.Fatalf("a foreign tenant must not read this preference, got %v", err)
	}
}
