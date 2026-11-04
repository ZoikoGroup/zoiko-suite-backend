package context

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
	"zoiko.io/identity-context-svc/internal/webauthn"
)

// Passkeys as a second factor (AU-11).
//
// The gates are the TOTP gates, for the same reasons: registering a first
// factor needs a recent login; adding one when a factor already exists, or
// removing one, needs a recent step-up; resetting someone else's is an
// administrator's recovery. A passkey step-up mints the same kind of login
// token a TOTP step-up does, with amr ["pwd","hwk"], which resolves to an
// envelope with mfa_verified=true and assurance PHISHING_RESISTANT.

// ErrPasskeysNotConfigured: no relying party is configured. 501.
var ErrPasskeysNotConfigured = errors.New("passkeys are not configured on this deployment")

// PasskeyFactor is the WebAuthn factor (internal/webauthn).
type PasskeyFactor interface {
	HasPasskey(ctx context.Context, tenantID, principalID string) (bool, error)
	List(ctx context.Context, tenantID, principalID string) ([]domain.WebAuthnCredential, error)
	BeginRegistration(ctx context.Context, p *domain.Principal) (json.RawMessage, string, error)
	FinishRegistration(ctx context.Context, p *domain.Principal, handle, nickname string, response []byte, correlationID string) (*domain.WebAuthnCredential, error)
	BeginStepUp(ctx context.Context, p *domain.Principal) (json.RawMessage, string, error)
	FinishStepUp(ctx context.Context, p *domain.Principal, handle string, assertion []byte, correlationID string) error
	Retire(ctx context.Context, tenantID, principalID, credentialID, reason, actorPrincipalID, action, correlationID string) (int, error)
}

// WithPasskeys wires the passkey factor. Nil leaves passkeys unconfigured.
func (s *MFAService) WithPasskeys(p PasskeyFactor) *MFAService {
	s.passkeys = p
	return s
}

// PasskeysConfigured reports whether passkeys can be registered and used.
func (s *MFAService) PasskeysConfigured() bool { return s != nil && s.passkeys != nil }

// hasAnyFactor reports whether the caller holds an active TOTP factor or
// passkey: what decides whether adding a factor needs a step-up.
func (s *MFAService) hasAnyFactor(ctx context.Context, c *MFACaller) (bool, error) {
	has, err := s.store.HasActiveTOTP(ctx, c.Principal.PrincipalID, c.Principal.TenantID)
	if err != nil || has || s.passkeys == nil {
		return has, err
	}
	return s.passkeys.HasPasskey(ctx, c.Principal.TenantID, c.Principal.PrincipalID)
}

// BeginPasskeyRegistration returns creation options and a ceremony handle.
func (s *MFAService) BeginPasskeyRegistration(ctx context.Context, c *MFACaller, correlationID string) (json.RawMessage, string, error) {
	if s.passkeys == nil {
		return nil, "", ErrPasskeysNotConfigured
	}
	if c == nil {
		return nil, "", ErrMFASessionInvalid
	}
	has, err := s.hasAnyFactor(ctx, c)
	if err != nil {
		return nil, "", fmt.Errorf("%w: factor lookup: %v", ErrAuthUnavailable, err)
	}
	if has {
		if err := s.requireRecentMFA(c); err != nil {
			s.evidence(ctx, c, ActionMFAEnrol, "DENIED", "passkey_registration_requires_step_up", correlationID)
			return nil, "", err
		}
	} else if !s.fresh(c, s.policy.RecentAuth) {
		s.evidence(ctx, c, ActionMFAEnrol, "DENIED", "reauthentication_required", correlationID)
		return nil, "", ErrReauthRequired
	}
	opts, handle, err := s.passkeys.BeginRegistration(ctx, c.Principal)
	if err != nil {
		return nil, "", err
	}
	s.evidence(ctx, c, ActionMFAEnrol, "ALLOWED", "passkey_registration_started", correlationID)
	return opts, handle, nil
}

// FinishPasskeyRegistration verifies and stores the new passkey. The ceremony
// was issued only past BeginPasskeyRegistration's gate, to this principal.
func (s *MFAService) FinishPasskeyRegistration(ctx context.Context, c *MFACaller, handle, nickname string, response []byte, correlationID string) (*domain.WebAuthnCredential, error) {
	if s.passkeys == nil {
		return nil, ErrPasskeysNotConfigured
	}
	if c == nil {
		return nil, ErrMFASessionInvalid
	}
	return s.passkeys.FinishRegistration(ctx, c.Principal, handle, nickname, response, correlationID)
}

// BeginPasskeyStepUp returns assertion options for a step-up of c's session.
func (s *MFAService) BeginPasskeyStepUp(ctx context.Context, c *MFACaller, correlationID string) (json.RawMessage, string, error) {
	if s.passkeys == nil {
		return nil, "", ErrPasskeysNotConfigured
	}
	if c == nil {
		return nil, "", ErrMFASessionInvalid
	}
	opts, handle, err := s.passkeys.BeginStepUp(ctx, c.Principal)
	if errors.Is(err, webauthn.ErrNoPasskey) {
		s.evidence(ctx, c, ActionMFAStepUp, "DENIED", "passkey_not_registered", correlationID)
		return nil, "", ErrMFANotEnrolled
	}
	return opts, handle, err
}

// FinishPasskeyStepUp verifies the assertion and mints the stepped-up login
// token, exactly as a TOTP step-up does.
func (s *MFAService) FinishPasskeyStepUp(ctx context.Context, c *MFACaller, handle string, assertion []byte, correlationID string) (*domain.AuthenticateResponse, error) {
	if s.passkeys == nil {
		return nil, ErrPasskeysNotConfigured
	}
	if c == nil {
		return nil, ErrMFASessionInvalid
	}
	if err := s.passkeys.FinishStepUp(ctx, c.Principal, handle, assertion, correlationID); err != nil {
		return nil, err
	}
	return s.issueStepUp(c, []string{domain.AMRPassword, domain.AMRHardwareKey}, correlationID)
}

// ListPasskeys returns the caller's own active passkeys.
func (s *MFAService) ListPasskeys(ctx context.Context, c *MFACaller) ([]domain.WebAuthnCredential, error) {
	if s.passkeys == nil {
		return nil, ErrPasskeysNotConfigured
	}
	if c == nil {
		return nil, ErrMFASessionInvalid
	}
	return s.passkeys.List(ctx, c.Principal.TenantID, c.Principal.PrincipalID)
}

// RevokePasskey retires one of the caller's own passkeys, behind a recent
// step-up, like RevokeTOTP.
func (s *MFAService) RevokePasskey(ctx context.Context, c *MFACaller, credentialID, reasonCode, correlationID string) error {
	if s.passkeys == nil {
		return ErrPasskeysNotConfigured
	}
	if c == nil {
		return ErrMFASessionInvalid
	}
	if strings.TrimSpace(credentialID) == "" {
		return fmt.Errorf("%w: credential_id is required", ErrRequestInvalid)
	}
	if err := s.requireRecentMFA(c); err != nil {
		s.evidence(ctx, c, ActionMFARevoke, "DENIED", "revocation_requires_step_up", correlationID)
		return err
	}
	reason := "owner_revoked"
	if rc := strings.TrimSpace(reasonCode); rc != "" {
		reason = "owner_revoked:" + truncate(rc, 60)
	}
	n, err := s.passkeys.Retire(ctx, c.Principal.TenantID, c.Principal.PrincipalID, credentialID, reason,
		c.Principal.PrincipalID, webauthn.ActionRevoke, correlationID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrMFAFactorNotFound
	}
	s.siem.Stream(ctx, c.Principal.TenantID, "identity.mfa_factor.revoked", siem.SeverityMedium,
		fmt.Sprintf("Principal %s revoked its own passkey %s", c.Principal.PrincipalID, credentialID))
	return nil
}

// passkeyFactors maps the caller's passkeys onto the factor list.
func (s *MFAService) passkeyFactors(ctx context.Context, c *MFACaller) ([]domain.MFAFactor, error) {
	if s.passkeys == nil {
		return nil, nil
	}
	creds, err := s.passkeys.List(ctx, c.Principal.TenantID, c.Principal.PrincipalID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.MFAFactor, 0, len(creds))
	for _, k := range creds {
		created := k.CreatedAt
		out = append(out, domain.MFAFactor{
			FactorID: k.CredentialID, PrincipalID: k.PrincipalID, Type: domain.CredentialTypeWebAuthn,
			Status: domain.CredentialStatusActive, CreatedAt: created, ActivatedAt: &created, LastUsedAt: k.LastUsedAt,
		})
	}
	return out, nil
}
