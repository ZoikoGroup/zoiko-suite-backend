package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func regulatedIntent() *IntentVersion {
	return &IntentVersion{VersionID: "iv-1", LegalEntityID: "le-1", PurposeClass: "T0", EvidenceClass: "E3", AllowedChannels: []string{"EMAIL"}}
}

func validParams() CreateNoticeParams {
	d := t0.Add(72 * time.Hour)
	return CreateNoticeParams{LegalEntityID: "le-1", IntentVersionID: "iv-1", RecipientPrincipalID: "p1", Locale: "en",
		Subject: "Notice of change", Body: "Your terms change on 1 January.", PolicyRef: "PDC-RULE-9", AckRequirement: AckReceipt, DeadlineAt: &d}
}

func TestValidateNotice(t *testing.T) {
	if err := ValidateNotice(validParams(), regulatedIntent(), t0); err != nil {
		t.Fatalf("a valid notice was refused: %v", err)
	}
	past := t0.Add(-time.Hour)
	bad := map[string]func(*CreateNoticeParams, *IntentVersion){
		"no recipient":              func(p *CreateNoticeParams, _ *IntentVersion) { p.RecipientPrincipalID = "" },
		"intent of another entity":  func(_ *CreateNoticeParams, iv *IntentVersion) { iv.LegalEntityID = "le-2" },
		"intent below E3":           func(_ *CreateNoticeParams, iv *IntentVersion) { iv.EvidenceClass = "E2" },
		"intent without email":      func(_ *CreateNoticeParams, iv *IntentVersion) { iv.AllowedChannels = []string{"IN_APP"} },
		"marketing class":           func(_ *CreateNoticeParams, iv *IntentVersion) { iv.PurposeClass = "M1" },
		"no body":                   func(p *CreateNoticeParams, _ *IntentVersion) { p.Body = "  " },
		"no policy basis":           func(p *CreateNoticeParams, _ *IntentVersion) { p.PolicyRef = "" },
		"unknown ack requirement":   func(p *CreateNoticeParams, _ *IntentVersion) { p.AckRequirement = "SIGNATURE" },
		"response needs a deadline": func(p *CreateNoticeParams, _ *IntentVersion) { p.DeadlineAt = nil },
		"deadline in the past":      func(p *CreateNoticeParams, _ *IntentVersion) { p.DeadlineAt = &past },
		"bad effective date":        func(p *CreateNoticeParams, _ *IntentVersion) { s := "1 Jan"; p.EffectiveDate = &s },
		"oversize subject":          func(p *CreateNoticeParams, _ *IntentVersion) { p.Subject = strings.Repeat("x", 256) },
	}
	for name, mutate := range bad {
		p, iv := validParams(), regulatedIntent()
		mutate(&p, iv)
		err := ValidateNotice(p, iv, t0)
		if !errors.Is(err, ErrNoticeInvalid) {
			t.Errorf("%s: want ErrNoticeInvalid, got %v", name, err)
		}
	}
	// A notice that needs no response needs no deadline.
	p := validParams()
	p.AckRequirement, p.DeadlineAt = AckNone, nil
	if err := ValidateNotice(p, regulatedIntent(), t0); err != nil {
		t.Errorf("NONE without a deadline should be valid: %v", err)
	}
}

func TestNoticeContentHash(t *testing.T) {
	a := NoticeContentHash("s", "b", "en")
	if a != NoticeContentHash("s", "b", "en") || len(a) != 64 {
		t.Fatal("the hash must be stable and 64 hex characters")
	}
	// The three fields cannot be shuffled into one another.
	for _, other := range [][3]string{{"s", "b", "fr"}, {"s2", "b", "en"}, {"s", "b2", "en"}, {"sb", "", "en"}, {"", "sb", "en"}} {
		if NoticeContentHash(other[0], other[1], other[2]) == a {
			t.Errorf("%v hashes the same as the original", other)
		}
	}
}

func TestAckOutcome(t *testing.T) {
	cases := []struct {
		req, action, want string
		err               error
	}{
		{AckReceipt, ActionAcknowledge, NoticeAcknowledged, nil},
		{AckReceipt, ActionDispute, NoticeDisputed, nil},
		{AckReceipt, ActionAccept, "", ErrNoticeInvalid},
		{AckReceipt, ActionDecline, "", ErrNoticeInvalid},
		{AckAcceptDecline, ActionAccept, NoticeAcknowledged, nil},
		{AckAcceptDecline, ActionDecline, NoticeDeclined, nil},
		{AckAcceptDecline, ActionDispute, NoticeDisputed, nil},
		{AckAcceptDecline, ActionAcknowledge, "", ErrNoticeInvalid},
		{AckNone, ActionAcknowledge, "", ErrAckNotRequired},
		{AckNone, ActionDispute, "", ErrAckNotRequired},
		{AckReceipt, "OPENED", "", ErrNoticeInvalid},
		{AckReceipt, "", "", ErrNoticeInvalid},
	}
	for _, c := range cases {
		got, err := AckOutcome(c.req, c.action)
		if got != c.want || !errors.Is(err, c.err) && !(err == nil && c.err == nil) {
			t.Errorf("%s/%s = %q, %v; want %q, %v", c.req, c.action, got, err, c.want, c.err)
		}
	}
}

func TestDecideNoticeProgress(t *testing.T) {
	deadline := t0.Add(time.Hour)
	inProgress := func() *Notice {
		return &Notice{Status: NoticeDeliveryInProgess, AckRequirement: AckReceipt, DeadlineAt: &deadline}
	}
	facts := func(fs ...string) []DeliveryEvidence {
		var out []DeliveryEvidence
		for _, f := range fs {
			out = append(out, DeliveryEvidence{Fact: f})
		}
		return out
	}

	t.Run("provider acceptance alone is not delivery evidence", func(t *testing.T) {
		if p, ok := DecideNoticeProgress(inProgress(), StatusSent, "", nil, t0); ok {
			t.Fatalf("SENT with no mailbox fact must not advance, got %+v", p)
		}
		if _, ok := DecideNoticeProgress(inProgress(), StatusSent, "", facts(EvidenceDeferred), t0); ok {
			t.Fatal("a deferral is not evidence either")
		}
	})
	t.Run("mailbox acceptance after a send is evidence, and says what it does not prove", func(t *testing.T) {
		p, ok := DecideNoticeProgress(inProgress(), StatusSent, "", facts(EvidenceMailboxAccepted), t0)
		if !ok || p.To != NoticeDeliveryEvidenced || !strings.Contains(p.Reason, "does not prove") {
			t.Fatalf("got %+v ok=%v", p, ok)
		}
		// A fact without the notification having been SENT is not enough: both must hold.
		if _, ok := DecideNoticeProgress(inProgress(), StatusPending, "", facts(EvidenceMailboxAccepted), t0); ok {
			t.Fatal("evidence for a notification that is still pending must not advance the notice")
		}
	})
	t.Run("failure and bounce are exceptions, never evidence", func(t *testing.T) {
		if p, ok := DecideNoticeProgress(inProgress(), StatusFailed, "550 no such user", nil, t0); !ok || p.To != NoticeException {
			t.Fatalf("failed delivery: %+v %v", p, ok)
		}
		if p, ok := DecideNoticeProgress(inProgress(), StatusSent, "", facts(EvidenceMailboxAccepted, EvidenceBounced), t0); !ok || p.To != NoticeException {
			t.Fatalf("a bounce beats an acceptance: %+v %v", p, ok)
		}
	})
	t.Run("a deadline that passes before delivery is evidenced is an exception", func(t *testing.T) {
		if p, ok := DecideNoticeProgress(inProgress(), StatusSent, "", nil, deadline); !ok || p.To != NoticeException {
			t.Fatalf("got %+v %v", p, ok)
		}
	})
	t.Run("evidenced moves on by the acknowledgement requirement", func(t *testing.T) {
		n := &Notice{Status: NoticeDeliveryEvidenced, AckRequirement: AckNone}
		if p, ok := DecideNoticeProgress(n, "", "", nil, t0); !ok || p.To != NoticeSatisfiedByPolicy || !strings.Contains(p.Reason, "not a finding of legal service") {
			t.Fatalf("NONE: %+v %v", p, ok)
		}
		n.AckRequirement = AckAcceptDecline
		if p, ok := DecideNoticeProgress(n, "", "", nil, t0); !ok || p.To != NoticeAckPending {
			t.Fatalf("response needed: %+v %v", p, ok)
		}
	})
	t.Run("no response by the deadline expires and never acknowledges", func(t *testing.T) {
		n := &Notice{Status: NoticeAckPending, AckRequirement: AckReceipt, DeadlineAt: &deadline}
		if _, ok := DecideNoticeProgress(n, "", "", nil, deadline.Add(-time.Second)); ok {
			t.Fatal("before the deadline nothing moves")
		}
		p, ok := DecideNoticeProgress(n, "", "", nil, deadline)
		if !ok || p.To != NoticeExpired {
			t.Fatalf("got %+v %v", p, ok)
		}
	})
	t.Run("settled states never move", func(t *testing.T) {
		for _, s := range []string{NoticePrepared, NoticeReady, NoticeException, NoticeSatisfiedByPolicy, NoticeAcknowledged, NoticeDeclined, NoticeExpired, NoticeDisputed} {
			if _, ok := DecideNoticeProgress(&Notice{Status: s, AckRequirement: AckReceipt, DeadlineAt: &deadline}, StatusSent, "", facts(EvidenceMailboxAccepted), deadline.Add(time.Hour)); ok {
				t.Errorf("%s must not be moved by the delivery rule", s)
			}
		}
	})
}

// No status or phrase in the lifecycle claims legal service (8.5).
func TestNoticeLifecycleNeverClaimsLegalService(t *testing.T) {
	for _, s := range []string{NoticePrepared, NoticeReady, NoticeDeliveryInProgess, NoticeDeliveryEvidenced, NoticeException, NoticeSatisfiedByPolicy,
		NoticeAckPending, NoticeAcknowledged, NoticeDeclined, NoticeExpired, NoticeDisputed} {
		for _, banned := range []string{"SERVED", "LEGAL", "COMPLETE", "EFFECTIVE"} {
			if strings.Contains(s, banned) {
				t.Errorf("status %s claims legal service", s)
			}
		}
	}
}
