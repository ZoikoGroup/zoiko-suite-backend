// Package channeldecision works out which channels a communication may use for a
// recipient, in order, and why each of the others may not (ZS-SVC-Y-001 NCD-02 section
// 5.5, POST /channel-decision; NCD-010 CHANNEL_SUPPRESSED, NCD-011 NO_COMPLIANT_CHANNEL,
// NCD-012 QUIET_HOUR_DEFERRED, NCD-013 PROVIDER_ROUTE_UNAVAILABLE).
//
// It is a pure function over facts the caller has gathered, so every rule is testable
// without a database or a network. It is advisory: the delivery guard still decides
// again, immediately before the provider, because facts change between the question and
// the send.
package channeldecision

import (
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/preference"
)

// Codes returned for a channel that may not be used.
const (
	CodeSuppressed  = "NCD-010 CHANNEL_SUPPRESSED"
	CodeNoCompliant = "NCD-011 NO_COMPLIANT_CHANNEL"
	CodeQuietHour   = "NCD-012 QUIET_HOUR_DEFERRED"
	CodeNoRoute     = "NCD-013 PROVIDER_ROUTE_UNAVAILABLE"
)

// DirectChannels are the channels this service can deliver on, best first. SMS and push
// have no provider route yet, so they are never eligible and say so.
var DirectChannels = []string{domain.ChannelEmail, domain.ChannelInApp}

// Facts about the recipient and the channels, gathered by the caller.
type Facts struct {
	Class string
	// IntentChannels is the set the governing intent allows; nil means no intent governs.
	IntentChannels []string
	// Requested narrows and orders the candidates; empty means every channel.
	Requested []string
	Prefs     *domain.RecipientPreferences
	Now       time.Time

	// Email facts. EmailUnavailable is why no usable address exists (empty when one does).
	EmailUnavailable string
	EmailSuppressed  string // non-empty: the suppression reason
}

// Channel is one candidate's verdict.
type Channel struct {
	Channel string `json:"channel"`
	// Code and Reason explain a refusal, or a deferral for an eligible channel.
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// NotBefore is when a routine message on this channel may be delivered (quiet hours).
	NotBefore *time.Time `json:"not_before,omitempty"`
}

// Result is the decision.
type Result struct {
	Eligible []Channel `json:"eligible_channels"`
	Rejected []Channel `json:"rejected_channels"`
	// Code is NCD-011 when nothing is eligible.
	Code string `json:"code,omitempty"`
}

// Decide evaluates each candidate channel.
func Decide(f Facts) Result {
	res := Result{Eligible: []Channel{}, Rejected: []Channel{}}
	candidates := f.Requested
	if len(candidates) == 0 {
		candidates = append(append([]string{}, DirectChannels...), "SMS", "PUSH")
	}
	seen := map[string]bool{}
	for _, ch := range candidates {
		if seen[ch] {
			continue
		}
		seen[ch] = true
		v := evaluate(f, ch)
		if v.reject {
			res.Rejected = append(res.Rejected, Channel{Channel: ch, Code: v.code, Reason: v.reason})
			continue
		}
		c := Channel{Channel: ch}
		if v.notBefore != nil {
			c.Code, c.Reason, c.NotBefore = CodeQuietHour, v.reason, v.notBefore
		}
		res.Eligible = append(res.Eligible, c)
	}
	if len(res.Eligible) == 0 {
		res.Code = CodeNoCompliant
	}
	return res
}

type verdict struct {
	reject    bool
	code      string
	reason    string
	notBefore *time.Time
}

func refuse(code, reason string) verdict { return verdict{reject: true, code: code, reason: reason} }

func evaluate(f Facts, ch string) verdict {
	if !contains(domain.IntentChannels, ch) {
		return refuse(CodeNoCompliant, "not a channel this platform knows")
	}
	if f.IntentChannels != nil && !contains(f.IntentChannels, ch) {
		return refuse(CodeNoCompliant, "the communication intent does not allow this channel")
	}
	if !contains(DirectChannels, ch) {
		return refuse(CodeNoRoute, "no provider route exists for this channel yet")
	}
	if ch == domain.ChannelEmail {
		if f.EmailUnavailable != "" {
			return refuse(CodeNoCompliant, "no usable email endpoint: "+f.EmailUnavailable)
		}
		if f.EmailSuppressed != "" {
			return refuse(CodeSuppressed, "the address is suppressed ("+f.EmailSuppressed+")")
		}
	}
	d := preference.Evaluate(f.Prefs, f.Class, ch, f.Now)
	switch d.Effect {
	case preference.Mute:
		return refuse(CodeSuppressed, d.Reason)
	case preference.Unusable:
		return refuse(CodeQuietHour, d.Reason)
	case preference.Defer:
		until := d.Until
		return verdict{notBefore: &until, reason: d.Reason}
	}
	return verdict{}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
