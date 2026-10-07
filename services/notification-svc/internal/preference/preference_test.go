package preference

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
)

func sp(s string) *string { return &s }

func prof(tz, start, end string, muted ...string) *domain.RecipientPreferences {
	p := &domain.RecipientPreferences{TimeZone: tz, MutedChannels: muted}
	if start != "" {
		p.QuietStart, p.QuietEnd = sp(start), sp(end)
	}
	return p
}

func at(t *testing.T, tz, local string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	require.NoError(t, err)
	v, err := time.ParseInLocation("2006-01-02 15:04", local, loc)
	require.NoError(t, err)
	return v.UTC()
}

func TestOnlyRoutineMessagesAreSubjectToPreferences(t *testing.T) {
	p := prof("Europe/London", "22:00", "07:00", "EMAIL")
	now := at(t, "Europe/London", "2026-01-10 23:30")
	for _, class := range []string{"S0", "T0", "L1", "M1", ""} {
		assert.Equal(t, Proceed, Evaluate(p, class, "EMAIL", now).Effect, "class %q is never delayed or muted by a convenience preference", class)
	}
	assert.Equal(t, Mute, Evaluate(p, "A1", "EMAIL", now).Effect)
	assert.Equal(t, Proceed, Evaluate(nil, "A1", "EMAIL", now).Effect, "no profile, no preference")
}

func TestMutedChannelIsRefusedOnlyOnThatChannel(t *testing.T) {
	p := prof("Europe/London", "", "", "SMS")
	now := at(t, "Europe/London", "2026-01-10 12:00")
	assert.Equal(t, Mute, Evaluate(p, "A1", "SMS", now).Effect)
	assert.Equal(t, Proceed, Evaluate(p, "A1", "EMAIL", now).Effect)
	assert.Contains(t, Evaluate(p, "A1", "SMS", now).Reason, "NCD-010")
}

func TestQuietHours_SameDayWindow(t *testing.T) {
	p := prof("America/New_York", "13:00", "15:00")
	d := Evaluate(p, "A1", "EMAIL", at(t, "America/New_York", "2026-03-02 13:30"))
	assert.Equal(t, Defer, d.Effect)
	assert.Equal(t, at(t, "America/New_York", "2026-03-02 15:00"), d.Until)
	assert.Contains(t, d.Reason, "NCD-012")

	assert.Equal(t, Proceed, Evaluate(p, "A1", "EMAIL", at(t, "America/New_York", "2026-03-02 12:59")).Effect)
	assert.Equal(t, Proceed, Evaluate(p, "A1", "EMAIL", at(t, "America/New_York", "2026-03-02 15:00")).Effect, "the end is exclusive")
	assert.Equal(t, Defer, Evaluate(p, "A1", "EMAIL", at(t, "America/New_York", "2026-03-02 13:00")).Effect, "the start is inclusive")
}

func TestQuietHours_OvernightWindowEndsOnTheRightDay(t *testing.T) {
	p := prof("Europe/London", "22:00", "07:00")
	// Before midnight: ends tomorrow morning.
	d := Evaluate(p, "A1", "EMAIL", at(t, "Europe/London", "2026-01-10 23:30"))
	require.Equal(t, Defer, d.Effect)
	assert.Equal(t, at(t, "Europe/London", "2026-01-11 07:00"), d.Until)
	// After midnight: ends this morning.
	d = Evaluate(p, "A1", "EMAIL", at(t, "Europe/London", "2026-01-11 02:15"))
	require.Equal(t, Defer, d.Effect)
	assert.Equal(t, at(t, "Europe/London", "2026-01-11 07:00"), d.Until)
	// Daytime proceeds.
	assert.Equal(t, Proceed, Evaluate(p, "A1", "EMAIL", at(t, "Europe/London", "2026-01-11 12:00")).Effect)
}

func TestQuietHours_UseTheRecipientsCivilTimeNotUTC(t *testing.T) {
	p := prof("Asia/Tokyo", "22:00", "07:00")
	// 14:00 UTC is 23:00 in Tokyo: quiet. The same instant is mid-afternoon in London: not.
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	assert.Equal(t, Defer, Evaluate(p, "A1", "EMAIL", now).Effect)
	assert.Equal(t, Proceed, Evaluate(prof("Europe/London", "22:00", "07:00"), "A1", "EMAIL", now).Effect)
}

func TestQuietHours_DaylightSavingIsTheZoneDatabasesBusiness(t *testing.T) {
	p := prof("Europe/London", "22:00", "07:00")
	// Clocks go forward at 01:00 on 29 March 2026: the night is an hour shorter in UTC.
	d := Evaluate(p, "A1", "EMAIL", at(t, "Europe/London", "2026-03-28 23:00"))
	require.Equal(t, Defer, d.Effect)
	assert.Equal(t, time.Date(2026, 3, 29, 6, 0, 0, 0, time.UTC), d.Until, "07:00 BST is 06:00 UTC")
	// A week earlier the same wall-clock end is 07:00 UTC.
	d = Evaluate(p, "A1", "EMAIL", at(t, "Europe/London", "2026-03-21 23:00"))
	assert.Equal(t, time.Date(2026, 3, 22, 7, 0, 0, 0, time.UTC), d.Until)

	// A window end inside the spring-forward gap (01:30 does not exist in London that day)
	// still moves forward, never backwards.
	gap := prof("Europe/London", "23:00", "01:30")
	d = Evaluate(gap, "A1", "EMAIL", at(t, "Europe/London", "2026-03-28 23:30"))
	require.Equal(t, Defer, d.Effect)
	assert.True(t, d.Until.After(at(t, "Europe/London", "2026-03-28 23:30")))
	assert.False(t, d.Until.After(at(t, "Europe/London", "2026-03-29 03:00")), "no more than an hour of drift")
}

func TestUnusableProfileWithholdsRatherThanGuesses(t *testing.T) {
	now := at(t, "UTC", "2026-01-10 12:00")
	d := Evaluate(prof("Not/AZone", "22:00", "07:00"), "A1", "EMAIL", now)
	assert.Equal(t, Unusable, d.Effect)
	assert.Contains(t, d.Reason, "guessed")
	d = Evaluate(prof("UTC", "25:00", "07:00"), "A1", "EMAIL", now)
	assert.Equal(t, Unusable, d.Effect)
	// With no quiet hours, an unknown zone is irrelevant (nothing to apply).
	assert.Equal(t, Proceed, Evaluate(prof("Not/AZone", "", ""), "A1", "EMAIL", now).Effect)
}

func TestValidation(t *testing.T) {
	ok := domain.SetPreferencesParams{TimeZone: "Europe/London", QuietStart: sp("22:00"), QuietEnd: sp("07:00"), MutedChannels: []string{"SMS"}}
	require.NoError(t, ok.Validate())
	bad := []func(*domain.SetPreferencesParams){
		func(p *domain.SetPreferencesParams) { p.TimeZone = "" },
		func(p *domain.SetPreferencesParams) { p.TimeZone = "Mars/Olympus" },
		func(p *domain.SetPreferencesParams) { p.TimeZone = "Local" },
		func(p *domain.SetPreferencesParams) { p.QuietEnd = nil },
		func(p *domain.SetPreferencesParams) { p.QuietStart = sp("7pm") },
		func(p *domain.SetPreferencesParams) { p.QuietEnd = sp("22:00") },
		func(p *domain.SetPreferencesParams) { p.MutedChannels = []string{"FAX"} },
		func(p *domain.SetPreferencesParams) { p.MutedChannels = []string{"SMS", "SMS"} },
	}
	for i, mut := range bad {
		p := ok
		mut(&p)
		assert.ErrorIs(t, p.Validate(), domain.ErrPreferencesInvalid, "case %d", i)
	}
}
