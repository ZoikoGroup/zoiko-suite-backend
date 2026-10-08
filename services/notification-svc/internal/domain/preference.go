package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RecipientPreferences is a person's convenience profile (ZS-SVC-Y-001 NCD-02 5.3). It is a
// different state dimension from privacy permission and from mandatory notices: it may delay
// or mute a routine message, and never creates permission to send.
type RecipientPreferences struct {
	TenantID      string    `json:"tenant_id"`
	PrincipalID   string    `json:"principal_id"`
	TimeZone      string    `json:"time_zone"`             // IANA name, e.g. Europe/London
	QuietStart    *string   `json:"quiet_start,omitempty"` // local "HH:MM"
	QuietEnd      *string   `json:"quiet_end,omitempty"`   // local "HH:MM"; may be earlier than start (overnight)
	MutedChannels []string  `json:"muted_channels"`
	Version       int       `json:"version"`
	UpdatedBy     string    `json:"updated_by"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SetPreferencesParams changes the caller's own profile.
type SetPreferencesParams struct {
	PrincipalID   string
	TimeZone      string
	QuietStart    *string
	QuietEnd      *string
	MutedChannels []string
	UpdatedBy     string
}

// ErrPreferencesInvalid marks a profile that cannot be stored.
var ErrPreferencesInvalid = errors.New("preferences are invalid")

// ErrPreferencesNotFound means the recipient has expressed no preferences.
var ErrPreferencesNotFound = errors.New("no preferences recorded for this recipient")

// PreferenceProblem explains what is wrong with a profile.
type PreferenceProblem struct{ Msg string }

func (p PreferenceProblem) Error() string { return p.Msg }
func (p PreferenceProblem) Unwrap() error { return ErrPreferencesInvalid }

var clockRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// ValidateTimeZone accepts only a zone the zone database knows. A zone is never inferred
// from an address or a phone number (NP-20), so an unknown one is refused, not repaired.
func ValidateTimeZone(name string) error {
	if name == "" || strings.EqualFold(name, "local") {
		return PreferenceProblem{"time_zone must be an IANA zone name such as Europe/London"}
	}
	if _, err := time.LoadLocation(name); err != nil {
		return PreferenceProblem{fmt.Sprintf("time_zone %q is not a known IANA zone", name)}
	}
	return nil
}

// Validate checks everything about a profile that can be known without storing it.
func (p SetPreferencesParams) Validate() error {
	if err := ValidateTimeZone(p.TimeZone); err != nil {
		return err
	}
	if (p.QuietStart == nil) != (p.QuietEnd == nil) {
		return PreferenceProblem{"quiet_start and quiet_end are given together or not at all"}
	}
	if p.QuietStart != nil {
		if !clockRe.MatchString(*p.QuietStart) || !clockRe.MatchString(*p.QuietEnd) {
			return PreferenceProblem{"quiet_start and quiet_end are local times written HH:MM (24-hour)"}
		}
		if *p.QuietStart == *p.QuietEnd {
			return PreferenceProblem{"quiet_start and quiet_end must differ"}
		}
	}
	seen := map[string]bool{}
	for _, ch := range p.MutedChannels {
		if !inSet(IntentChannels, ch) || seen[ch] {
			return PreferenceProblem{"muted_channels must be distinct values of " + strings.Join(IntentChannels, ", ")}
		}
		seen[ch] = true
	}
	return nil
}
