package channeldecision

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
)

func sp(s string) *string { return &s }

func names(cs []Channel) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Channel)
	}
	return out
}

func find(cs []Channel, ch string) Channel {
	for _, c := range cs {
		if c.Channel == ch {
			return c
		}
	}
	return Channel{}
}

var noon = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

func TestDefault_EmailThenInAppAreEligibleAndSMSAndPushHaveNoRoute(t *testing.T) {
	r := Decide(Facts{Class: "T0", Now: noon})
	assert.Equal(t, []string{"EMAIL", "IN_APP"}, names(r.Eligible), "ordered best first")
	assert.Equal(t, []string{"SMS", "PUSH"}, names(r.Rejected))
	assert.Contains(t, find(r.Rejected, "SMS").Code, "NCD-013")
	assert.Empty(t, r.Code)
}

func TestRequestedChannelsNarrowAndOrder(t *testing.T) {
	r := Decide(Facts{Class: "T0", Now: noon, Requested: []string{"IN_APP", "EMAIL", "IN_APP", "FAX"}})
	assert.Equal(t, []string{"IN_APP", "EMAIL"}, names(r.Eligible), "the caller's order, no duplicates")
	assert.Equal(t, []string{"FAX"}, names(r.Rejected))
	assert.Contains(t, find(r.Rejected, "FAX").Code, "NCD-011")
}

func TestIntentChannelsConstrain(t *testing.T) {
	r := Decide(Facts{Class: "T0", Now: noon, IntentChannels: []string{"EMAIL"}})
	assert.Equal(t, []string{"EMAIL"}, names(r.Eligible))
	in := find(r.Rejected, "IN_APP")
	assert.Contains(t, in.Code, "NCD-011")
	assert.Contains(t, in.Reason, "intent")
	// An intent that allows only a channel with no route leaves nothing.
	r = Decide(Facts{Class: "T0", Now: noon, IntentChannels: []string{"SMS"}})
	assert.Empty(t, r.Eligible)
	assert.Equal(t, CodeNoCompliant, r.Code)
	// An empty (non-nil) intent set allows nothing.
	r = Decide(Facts{Class: "T0", Now: noon, IntentChannels: []string{}})
	assert.Empty(t, r.Eligible)
	assert.Equal(t, CodeNoCompliant, r.Code)
}

func TestEmailNeedsAUsableUnsuppressedEndpoint(t *testing.T) {
	r := Decide(Facts{Class: "T0", Now: noon, EmailUnavailable: "recipient has no email address"})
	assert.Equal(t, []string{"IN_APP"}, names(r.Eligible))
	assert.Contains(t, find(r.Rejected, "EMAIL").Reason, "no email address")

	r = Decide(Facts{Class: "T0", Now: noon, EmailSuppressed: "HARD_BOUNCE"})
	assert.Equal(t, []string{"IN_APP"}, names(r.Eligible))
	e := find(r.Rejected, "EMAIL")
	assert.Contains(t, e.Code, "NCD-010")
	assert.Contains(t, e.Reason, "HARD_BOUNCE")

	// Email alone, unavailable: nothing complies.
	r = Decide(Facts{Class: "T0", Now: noon, Requested: []string{"EMAIL"}, EmailSuppressed: "COMPLAINT"})
	assert.Equal(t, CodeNoCompliant, r.Code)
}

func TestPreferencesApplyToRoutineMessagesOnly(t *testing.T) {
	night := time.Date(2026, 1, 10, 23, 30, 0, 0, time.UTC)
	prefs := &domain.RecipientPreferences{TimeZone: "Europe/London", QuietStart: sp("22:00"), QuietEnd: sp("07:00"), MutedChannels: []string{"IN_APP"}}

	r := Decide(Facts{Class: "A1", Now: night, Prefs: prefs})
	assert.Equal(t, []string{"EMAIL"}, names(r.Eligible))
	email := find(r.Eligible, "EMAIL")
	require.NotNil(t, email.NotBefore, "eligible, but not until quiet hours end")
	assert.Equal(t, time.Date(2026, 1, 11, 7, 0, 0, 0, time.UTC), *email.NotBefore)
	assert.Contains(t, email.Code, "NCD-012")
	assert.Contains(t, find(r.Rejected, "IN_APP").Code, "NCD-010", "muted")

	for _, class := range []string{"S0", "T0"} {
		r = Decide(Facts{Class: class, Now: night, Prefs: prefs})
		assert.Equal(t, []string{"EMAIL", "IN_APP"}, names(r.Eligible), class)
		assert.Nil(t, find(r.Eligible, "EMAIL").NotBefore, class)
	}
}

func TestUnusablePreferencesRefuseRatherThanGuess(t *testing.T) {
	prefs := &domain.RecipientPreferences{TimeZone: "Not/AZone", QuietStart: sp("22:00"), QuietEnd: sp("07:00")}
	r := Decide(Facts{Class: "A1", Now: noon, Prefs: prefs})
	assert.Empty(t, r.Eligible)
	assert.Equal(t, CodeNoCompliant, r.Code)
	assert.Contains(t, find(r.Rejected, "EMAIL").Reason, "guessed")
}
