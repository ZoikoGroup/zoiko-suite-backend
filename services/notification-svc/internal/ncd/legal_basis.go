package ncd

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"zoiko.io/notification-svc/internal/legalbasis"
)

// S7-C2 (ZS-SVC-Y-001 s8.1): with a policy authority wired, a notice legal_basis_ref is the id of an ACTIVE LEGAL_BASIS
// policy version in force now, checked against policy-svc, instead of text the caller types. Without one the reference
// stays caller-supplied, as before. Deadlines are not covered: workflow-svc has no deadline clocks (WFC), so deadline_at
// stays caller-supplied.

// LegalBasisVerifier checks a legal basis reference. *legalbasis.Client implements it.
type LegalBasisVerifier interface {
	Verify(ctx context.Context, tenantID, correlationID, versionID string, at time.Time) (*legalbasis.Basis, error)
}

var policyVersionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// SetLegalBasisVerifier turns on verification of a notice legal basis against the policy authority.
func (s *Service) SetLegalBasisVerifier(v LegalBasisVerifier) { s.legalBasis = v }

// verifyLegalBasis refuses a notice whose legal basis is not an active, in-force LEGAL_BASIS policy version.
// Network I/O, so it runs outside any database transaction.
func (s *Service) verifyLegalBasis(ctx context.Context, a Actor, ref string) error {
	if s.legalBasis == nil {
		return nil
	}
	ref = strings.TrimSpace(ref)
	if !policyVersionIDRe.MatchString(ref) {
		return Invalid("legal_basis_unverified", "legal_basis_ref must be the id of an active LEGAL_BASIS policy version in policy-svc, not free text")
	}
	_, err := s.legalBasis.Verify(ctx, a.TenantID, a.CorrelationID, ref, s.now())
	switch {
	case errors.Is(err, legalbasis.ErrNotUsable):
		return Invalid("legal_basis_unverified", "%v", err)
	case err != nil:
		return Unavailable("legal_basis_unavailable", "the legal basis could not be verified: %v; no notice was created", err)
	}
	return nil
}
