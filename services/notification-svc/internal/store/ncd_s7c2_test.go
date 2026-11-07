package store_test

import (
	"context"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/legalbasis"
	"zoiko.io/notification-svc/internal/ncd"
)

type scriptedBasis struct {
	err   error
	asked []string
}

func (s *scriptedBasis) Verify(_ context.Context, tenantID, _ string, versionID string, _ time.Time) (*legalbasis.Basis, error) {
	s.asked = append(s.asked, tenantID+"/"+versionID)
	if s.err != nil {
		return nil, s.err
	}
	return &legalbasis.Basis{PolicyVersionID: versionID, PolicyCode: "UK-CONSULT-7"}, nil
}

const legalVersion = "7c1f0c3e-4b8a-4f4e-9d52-0a5d1d2c9b11"

func prepared(h *harness) *ncd.Communication {
	h.t.Helper()
	i := h.regulatedIntent()
	r := h.recipient("emp@example.com")
	c := h.comm(i.IntentID, r, nil)
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal != nil {
		h.t.Fatalf("prepare refused: %+v", p.Refusal)
	}
	return c
}

// S7-C2: with a policy authority wired, a notice legal basis must be an active LEGAL_BASIS policy version.
func TestS7C2_ANoticeLegalBasisIsVerifiedAgainstThePolicyAuthority(t *testing.T) {
	h := newHarness(t)
	deadline := h.clock.now().Add(72 * time.Hour)
	input := func(c *ncd.Communication, ref string) ncd.NoticeInput {
		return ncd.NoticeInput{CommunicationID: c.CommunicationID, LegalBasisRef: ref, RecipientCapacity: "employee", DeadlineAt: &deadline, WFCObligationRef: "wfc-1"}
	}
	basis := &scriptedBasis{}
	h.svc.SetLegalBasisVerifier(basis)
	c := prepared(h)

	// Free text is no longer a legal basis.
	_, err := h.svc.CreateNotice(h.ctx, h.author, input(c, "PDC-RULE-UK-CONSULT-7"))
	h.kind(err, ncd.KindInvalid, "legal_basis_unverified")
	if len(basis.asked) != 0 {
		t.Fatalf("free text never reaches the policy authority: %v", basis.asked)
	}

	// A version the authority does not accept is refused, and so is an outage; no notice is created for either.
	basis.err = legalbasis.ErrNotUsable
	_, err = h.svc.CreateNotice(h.ctx, h.author, input(c, legalVersion))
	h.kind(err, ncd.KindInvalid, "legal_basis_unverified")
	basis.err = legalbasis.ErrUnavailable
	_, err = h.svc.CreateNotice(h.ctx, h.author, input(c, legalVersion))
	h.kind(err, ncd.KindUnavailable, "legal_basis_unavailable")

	// An active, in-force legal basis is accepted, asked under the sender own tenant, and the version id is kept.
	basis.err = nil
	n, err := h.svc.CreateNotice(h.ctx, h.author, input(c, legalVersion))
	h.must(err)
	if n.LegalBasisRef != legalVersion {
		t.Fatalf("the notice keeps the exact policy version it rests on: %q", n.LegalBasisRef)
	}
	if got := basis.asked[len(basis.asked)-1]; got != h.tenant+"/"+legalVersion {
		t.Fatalf("asked under the sender tenant: %s", got)
	}
}

func TestS7C2_WithoutAPolicyAuthorityTheReferenceStaysCallerSupplied(t *testing.T) {
	h := newHarness(t)
	c := prepared(h)
	deadline := h.clock.now().Add(72 * time.Hour)
	n, err := h.svc.CreateNotice(h.ctx, h.author, ncd.NoticeInput{CommunicationID: c.CommunicationID, LegalBasisRef: "PDC-RULE-UK-CONSULT-7",
		RecipientCapacity: "employee", DeadlineAt: &deadline, WFCObligationRef: "wfc-1"})
	h.must(err)
	if n.LegalBasisRef != "PDC-RULE-UK-CONSULT-7" {
		t.Fatalf("%q", n.LegalBasisRef)
	}
}
