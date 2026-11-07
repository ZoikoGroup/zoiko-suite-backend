package registry

// ORG-09 / MDM-03 identifier verification: VerifyIdentifier and the read of its observations (ZS-SVC-U-001 s13.2,
// s13.3, s14.1). The service does not call an authority: whoever called one through the exchange layer, or a steward
// holding the evidence, records what it said, and the rules in domain.DecideObservation decide what that changes.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) identifierVerificationStore() (IdentifierVerificationStore, error) {
	vs, ok := s.store.(IdentifierVerificationStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return vs, nil
}

// VerifyPartyIdentifier is VerifyIdentifier: it records an observation of an identifier and applies the verification
// rules. A sensitive identifier's value is masked in the answer.
func (s *Service) VerifyPartyIdentifier(ctx context.Context, identifierID string, req domain.VerifyIdentifierRequest) (*IdentifierVerifyOutcome, error) {
	if err := req.Validate(time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "party.identifier", "verify"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	vs, err := s.identifierVerificationStore()
	if err != nil {
		return nil, err
	}
	out, err := vs.VerifyPartyIdentifier(ctx, identifierID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.VerifyPartyIdentifier: %w", err)
	}
	if out.Identifier.Scheme.Sensitive() {
		out.Identifier.Value = domain.MaskIdentifier(out.Identifier.Scheme, out.Identifier.Value)
		out.Identifier.Masked = true
	}
	return out, nil
}

// ListIdentifierVerifications lists the observations recorded for an identifier.
func (s *Service) ListIdentifierVerifications(ctx context.Context, identifierID string) ([]domain.IdentifierObservation, error) {
	vs, err := s.identifierVerificationStore()
	if err != nil {
		return nil, err
	}
	out, err := vs.ListIdentifierVerifications(ctx, identifierID)
	if err != nil {
		return nil, fmt.Errorf("store.ListIdentifierVerifications: %w", err)
	}
	return out, nil
}
