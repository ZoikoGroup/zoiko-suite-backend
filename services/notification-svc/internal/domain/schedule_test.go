package domain

import (
	"testing"
	"time"
)

func TestValidateSchedule(t *testing.T) {
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	if err := ValidateSchedule(ChannelEmail, nil, nil, now); err != nil {
		t.Errorf("no timing is fine: %v", err)
	}
	if err := ValidateSchedule(ChannelEmail, at(time.Hour), at(48*time.Hour), now); err != nil {
		t.Errorf("a normal window is fine: %v", err)
	}
	bad := map[string]struct {
		ch      string
		nb, exp *time.Time
	}{
		"in-app has nothing to hold":   {ChannelInApp, at(time.Hour), nil},
		"in-app has nothing to expire": {ChannelInApp, nil, at(time.Hour)},
		"too far ahead":                {ChannelEmail, at(MaxScheduleHorizon + time.Hour), nil},
		"already expired":              {ChannelEmail, nil, at(-time.Minute)},
		"expires exactly now":          {ChannelEmail, nil, at(0)},
		"expiry before the start":      {ChannelEmail, at(2 * time.Hour), at(time.Hour)},
		"expiry at the start":          {ChannelEmail, at(time.Hour), at(time.Hour)},
	}
	for name, c := range bad {
		if err := ValidateSchedule(c.ch, c.nb, c.exp, now); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestQueued(t *testing.T) {
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Minute), now.Add(-time.Minute)
	if !Queued(&future, now) || Queued(&past, now) || Queued(nil, now) || Queued(&now, now) {
		t.Error("only a not_before strictly in the future queues a send")
	}
}
