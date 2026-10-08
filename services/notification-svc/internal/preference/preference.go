// Package preference applies a recipient's convenience preferences to a routine message
// (ZS-SVC-Y-001 NCD-02 5.3, INV-23, NP-19, NP-20).
//
// A preference selects when and whether a routine message arrives. It is separate from
// privacy permission (which the privacy gate decides) and from mandatory notices: security
// (S0) and transactional (T0) messages are never delayed or muted by a preference, because
// overriding a mandatory notice needs an explicit policy and a convenience setting is not
// one. Only operational notices (A1) are subject to it here.
//
// Quiet hours are civil time in the recipient's own zone, so daylight saving is the zone
// database's business and nothing here assumes a fixed offset.
package preference

import (
	"fmt"
	"time"
	_ "time/tzdata" // zone data travels with the binary: a slim container image has none

	"zoiko.io/notification-svc/internal/domain"
)

// Effect is what a preference does to one delivery.
type Effect int

const (
	// Proceed: no preference applies.
	Proceed Effect = iota
	// Defer: wait until Until (QUIET_HOUR_DEFERRED, NCD-012).
	Defer
	// Mute: the recipient muted this channel for routine messages (CHANNEL_SUPPRESSED, NCD-010).
	Mute
	// Unusable: the profile cannot be applied (for example its zone is unknown). The
	// caller withholds the message rather than guess.
	Unusable
)

// Decision is the outcome of Evaluate.
type Decision struct {
	Effect Effect
	Until  time.Time
	Reason string
}

// Subject reports whether a communication class is subject to convenience preferences.
func Subject(class string) bool { return class == "A1" }

// Evaluate applies a profile to a delivery of the given class and channel at time now.
func Evaluate(p *domain.RecipientPreferences, class, channel string, now time.Time) Decision {
	if p == nil || !Subject(class) {
		return Decision{}
	}
	for _, ch := range p.MutedChannels {
		if ch == channel {
			return Decision{Effect: Mute, Reason: fmt.Sprintf("NCD-010 CHANNEL_SUPPRESSED: the recipient muted %s for routine messages", channel)}
		}
	}
	if p.QuietStart == nil || p.QuietEnd == nil {
		return Decision{}
	}
	loc, err := time.LoadLocation(p.TimeZone)
	if err != nil {
		return Decision{Effect: Unusable, Reason: fmt.Sprintf("NCD-012: the recipient's time zone %q is unusable; delivery withheld rather than guessed", p.TimeZone)}
	}
	start, ok1 := parseClock(*p.QuietStart)
	end, ok2 := parseClock(*p.QuietEnd)
	if !ok1 || !ok2 {
		return Decision{Effect: Unusable, Reason: "NCD-012: the recipient's quiet hours are unreadable; delivery withheld"}
	}
	local := now.In(loc)
	minute := local.Hour()*60 + local.Minute()
	var inQuiet bool
	if start < end {
		inQuiet = minute >= start && minute < end
	} else { // overnight, e.g. 22:00 to 07:00
		inQuiet = minute >= start || minute < end
	}
	if !inQuiet {
		return Decision{}
	}
	// The next end-of-window after now, in the recipient's calendar. time.Date resolves a
	// local time that does not exist (the spring-forward gap) to the instant after it.
	day := local
	if start > end && minute >= start { // overnight and already past the start: ends tomorrow
		day = local.AddDate(0, 0, 1)
	}
	until := time.Date(day.Year(), day.Month(), day.Day(), end/60, end%60, 0, 0, loc)
	if !until.After(now) { // a shifted or ambiguous instant must still move forward
		until = until.Add(time.Hour)
	}
	return Decision{Effect: Defer, Until: until.UTC(),
		Reason: fmt.Sprintf("NCD-012 QUIET_HOUR_DEFERRED: routine message held until %s (recipient quiet hours %s to %s, %s)",
			until.UTC().Format(time.RFC3339), *p.QuietStart, *p.QuietEnd, p.TimeZone)}
}

func parseClock(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}
