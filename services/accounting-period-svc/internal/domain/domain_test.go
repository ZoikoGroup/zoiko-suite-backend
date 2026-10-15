package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"zoiko.io/accounting-period-svc/internal/domain"
)

func TestPostingDecision_PureTable(t *testing.T) {
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	win := func(book, module string, d time.Duration) *domain.ReopenWindow {
		return &domain.ReopenWindow{BookScope: book, ModuleScope: module, ExpiresAt: now.Add(d)}
	}
	cases := []struct {
		name         string
		p            domain.Period
		book, module string
		exception    bool
		want         domain.Decision
	}{
		{"open", domain.Period{State: domain.StateOpen}, "", "", false, domain.Decision{Allowed: true, Mode: "ALLOWED", Reason: domain.ReasonOpen}},
		{"soft closed", domain.Period{State: domain.StateSoftClosed}, "", "", false, domain.Decision{Allowed: false, Mode: "RESTRICTED", Reason: domain.ReasonSoftClosed}},
		{"soft closed + exception", domain.Period{State: domain.StateSoftClosed}, "", "", true, domain.Decision{Allowed: true, Mode: "RESTRICTED", Reason: domain.ReasonSoftClosedException}},
		{"hard closed", domain.Period{State: domain.StateHardClosed}, "", "", true, domain.Decision{Allowed: false, Mode: "BLOCKED", Reason: domain.ReasonHardClosed}},
		{"reclosed", domain.Period{State: domain.StateReclosed}, "", "", true, domain.Decision{Allowed: false, Mode: "BLOCKED", Reason: domain.ReasonReclosed}},
		{"reopen in window, all scope", domain.Period{State: domain.StateReopenAuthorized, Reopen: win("", "", time.Minute)}, "b", "m", false, domain.Decision{Allowed: true, Mode: "RESTRICTED", Reason: domain.ReasonReopenInScope}},
		{"reopen scoped, matching", domain.Period{State: domain.StateReopenAuthorized, Reopen: win("b", "m", time.Minute)}, "b", "m", false, domain.Decision{Allowed: true, Mode: "RESTRICTED", Reason: domain.ReasonReopenInScope}},
		{"reopen scoped, other book", domain.Period{State: domain.StateReopenAuthorized, Reopen: win("b", "m", time.Minute)}, "x", "m", false, domain.Decision{Allowed: false, Mode: "BLOCKED", Reason: domain.ReasonReopenOutOfScope}},
		{"reopen expired", domain.Period{State: domain.StateReopenAuthorized, Reopen: win("", "", -time.Second)}, "", "", false, domain.Decision{Allowed: false, Mode: "BLOCKED", Reason: domain.ReasonReopenExpired}},
		{"reopen expires exactly now", domain.Period{State: domain.StateReopenAuthorized, Reopen: win("", "", 0)}, "", "", false, domain.Decision{Allowed: false, Mode: "BLOCKED", Reason: domain.ReasonReopenExpired}},
		{"reopen without a window is blocked", domain.Period{State: domain.StateReopenAuthorized}, "", "", false, domain.Decision{Allowed: false, Mode: "BLOCKED", Reason: domain.ReasonReopenExpired}},
		{"unknown state never opens", domain.Period{State: "WEIRD"}, "", "", true, domain.Decision{Allowed: false, Mode: "BLOCKED", Reason: domain.ReasonUnknownState}},
		{"empty state never opens", domain.Period{}, "", "", true, domain.Decision{Allowed: false, Mode: "BLOCKED", Reason: domain.ReasonUnknownState}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.p.PostingDecision(now, c.book, c.module, c.exception))
		})
	}
}

func TestCloseStatusMapping(t *testing.T) {
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	live := &domain.ReopenWindow{ExpiresAt: now.Add(time.Hour)}
	dead := &domain.ReopenWindow{ExpiresAt: now.Add(-time.Hour)}
	for _, c := range []struct {
		p    domain.Period
		want string
	}{
		{domain.Period{State: domain.StateOpen}, "OPEN"},
		{domain.Period{State: domain.StateReopenAuthorized, Reopen: live}, "OPEN"},
		{domain.Period{State: domain.StateReopenAuthorized, Reopen: dead}, "CLOSED"},
		{domain.Period{State: domain.StateReopenAuthorized}, "CLOSED"},
		{domain.Period{State: domain.StateSoftClosed}, "CLOSED"},
		{domain.Period{State: domain.StateReclosed}, "CLOSED"},
		{domain.Period{State: domain.StateHardClosed}, "LOCKED"},
		{domain.Period{State: "WEIRD"}, "LOCKED"},
	} {
		assert.Equal(t, c.want, c.p.CloseStatus(now), "%s", c.p.State)
	}
	assert.Less(t, domain.CloseStatusRank("OPEN"), domain.CloseStatusRank("CLOSED"))
	assert.Less(t, domain.CloseStatusRank("CLOSED"), domain.CloseStatusRank("LOCKED"))
}

func TestDecisionFingerprint(t *testing.T) {
	exp := time.Date(2026, 3, 20, 13, 0, 0, 0, time.UTC)
	base := domain.DecisionFingerprint("p1", domain.CmdHardClose, domain.StateSoftClosed, "wf", "snap", nil)
	assert.Len(t, base, 64)
	assert.Equal(t, base, domain.DecisionFingerprint("p1", domain.CmdHardClose, domain.StateSoftClosed, "wf", "snap", nil))
	for name, other := range map[string]string{
		"period":   domain.DecisionFingerprint("p2", domain.CmdHardClose, domain.StateSoftClosed, "wf", "snap", nil),
		"command":  domain.DecisionFingerprint("p1", domain.CmdReclose, domain.StateSoftClosed, "wf", "snap", nil),
		"state":    domain.DecisionFingerprint("p1", domain.CmdHardClose, domain.StateOpen, "wf", "snap", nil),
		"workflow": domain.DecisionFingerprint("p1", domain.CmdHardClose, domain.StateSoftClosed, "wf2", "snap", nil),
		"snapshot": domain.DecisionFingerprint("p1", domain.CmdHardClose, domain.StateSoftClosed, "wf", "snap2", nil),
		"window":   domain.DecisionFingerprint("p1", domain.CmdHardClose, domain.StateSoftClosed, "wf", "snap", &domain.ReopenWindow{ExpiresAt: exp}),
	} {
		assert.NotEqual(t, base, other, name)
	}
}

func TestVocabulary(t *testing.T) {
	assert.Len(t, domain.AllStates, 5)
	assert.True(t, domain.StateOpen.Valid())
	assert.False(t, domain.State("").Valid())
	assert.True(t, domain.KindNormal.Valid() && domain.KindSpecial.Valid())
	assert.False(t, domain.Kind("X").Valid())
	assert.True(t, domain.NeedsSoD(domain.CmdHardClose) && domain.NeedsSoD(domain.CmdAuthorizeReopen))
	assert.False(t, domain.NeedsSoD(domain.CmdSoftClose) || domain.NeedsSoD(domain.CmdReclose))
}
