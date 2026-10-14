package context

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/mfa"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/siem"
)

// The TOTP second factor and step-up (ZS-IAM-001 §3, §13, §14).
//
// Why this exists: the standard forbids password-only privileged sessions and
// requires "high-assurance step-up before session issue" for JIT and
// break-glass. Until this service could verify a second factor, mfa_verified
// was false on every envelope ever minted, so that requirement could not be
// met and authorization-svc's S-3 stayed ❌ (register A-03, Group 1 R-2).
//
// The shape follows the standard's own words, "Step-up creates a new assurance
// assertion with bounded age": a verified code mints a NEW login token whose
// auth_time is now and whose amr includes "otp". The caller resolves it into a
// new envelope with mfa_verified=true. authorization-svc then decides whether
// that assertion is strong and recent enough for the action.
//
// Every route here identifies its caller by the session envelope itself,
// verified with this service's own key, and by the live session record behind
// it. X-Principal-Id is never trusted for these routes: a forged header must
// not be able to enrol, revoke or use a factor on someone else's account.

var (
	// ErrMFAUnavailable: no encryption key is configured, so no factor can be
	// enrolled or used. 503.
	ErrMFAUnavailable = errors.New("second factor is not configured on this deployment")
	// ErrMFASessionInvalid: the bearer is not a live session of an active
	// human principal. 401.
	ErrMFASessionInvalid = errors.New("a valid session is required")
	// ErrReauthRequired: the session's authentication is too old for this
	// command. 401; the client re-authenticates.
	ErrReauthRequired = errors.New("recent authentication is required")
	// ErrMFAStepUpRequired: the command needs a recent second-factor
	// authentication (step-up). 403.
	ErrMFAStepUpRequired = errors.New("a recent second-factor authentication is required")
	// ErrMFANotEnrolled: step-up was asked of a principal with no factor. 409.
	ErrMFANotEnrolled = errors.New("no second factor is enrolled")
	// ErrMFAFactorNotFound: no such factor in a usable state for this
	// principal. 404.
	ErrMFAFactorNotFound = errors.New("factor not found")
	// ErrInvalidCode: wrong, replayed, or locked. One answer for all three,
	// for the reason ErrInvalidCredentials is one answer: the distinction is in
	// the evidence log and the event, not on the wire. 401.
	ErrInvalidCode = errors.New("invalid code")
)

// MFA actions as written to access_decision_log.action_type.
const (
	ActionMFAEnrol    = "MFA_ENROL"
	ActionMFAActivate = "MFA_ACTIVATE"
	ActionMFAStepUp   = "MFA_STEP_UP"
	ActionMFARevoke   = "MFA_REVOKE"
	ActionMFAReset    = "MFA_RESET"
)

// MFAStore is the MFA service's slice of the store.
type MFAStore interface {
	FindByID(ctx context.Context, principalID, tenantID string) (*domain.Principal, error)
	FindSessionContext(ctx context.Context, sessionContextID, tenantID string) (*domain.SessionContext, error)
	HasActiveTOTP(ctx context.Context, principalID, tenantID string) (bool, error)
	CreatePendingTOTP(ctx context.Context, f domain.TOTPFactorSecret) error
	FindTOTPFactor(ctx context.Context, factorID, principalID, tenantID string) (*domain.TOTPFactorSecret, error)
	FindActiveTOTP(ctx context.Context, principalID, tenantID string) (*domain.TOTPFactorSecret, error)
	AcceptTOTPCode(ctx context.Context, a domain.TOTPAcceptance, rec outbox.Record) error
	RecordTOTPFailure(ctx context.Context, factorID, principalID, tenantID, action, correlationID string, maxAttempts int, lockDuration time.Duration) (bool, int, error)
	RecordMFADecision(ctx context.Context, principalID, tenantID, action, outcome, basis, correlationID string) error
	RetireTOTPFactors(ctx context.Context, principalID, tenantID, factorID, reason, actorPrincipalID, action, correlationID string, rec outbox.Record) (int, error)
	ListMFAFactors(ctx context.Context, principalID, tenantID string) ([]domain.MFAFactor, error)
}

// EnvelopeVerifier verifies a session envelope this service signed.
type EnvelopeVerifier interface {
	Verify(token string) (*domain.IdentityContextEnvelope, error)
}

// StepUpEventPublisher emits refused step-up attempts.
type StepUpEventPublisher interface {
	PublishStepUpFailed(ctx context.Context, principalID, tenantID, correlationID, action, reason string) error
}

// MFAPolicy is the second factor's tunables (see config).
type MFAPolicy struct {
	MaxFailedAttempts int
	LockDuration      time.Duration
	// RecentAuth bounds how old an authentication may be to enrol a first
	// factor, or (as an MFA authentication) to replace, revoke or reset one.
	RecentAuth  time.Duration
	SkewSteps   int
	IssuerLabel string
}

// MFACaller is the verified caller of an MFA route.
type MFACaller struct {
	Envelope  *domain.IdentityContextEnvelope
	Principal *domain.Principal
	// AuthTime is the envelope's auth_time; zero when the envelope predates
	// the claim, which no freshness check accepts.
	AuthTime    time.Time
	MFAVerified bool
}

// MFAService owns enrolment, activation, step-up, revocation and reset.
type MFAService struct {
	log       *zap.Logger
	store     MFAStore
	keys      *mfa.Keyring
	envelopes EnvelopeVerifier
	issuer    TokenIssuer
	events    StepUpEventPublisher
	siem      *siem.Client
	policy    MFAPolicy
	now       func() time.Time

	wg sync.WaitGroup
}

// NewMFAService wires the second factor. keys may be nil (MFA unconfigured):
// every command then answers ErrMFAUnavailable rather than half-working.
func NewMFAService(log *zap.Logger, store MFAStore, keys *mfa.Keyring, envelopes EnvelopeVerifier,
	issuer TokenIssuer, ev StepUpEventPublisher, siemClient *siem.Client, policy MFAPolicy) *MFAService {
	if policy.IssuerLabel == "" {
		policy.IssuerLabel = "ZoikoSuite"
	}
	return &MFAService{log: log, store: store, keys: keys, envelopes: envelopes, issuer: issuer,
		events: ev, siem: siemClient, policy: policy, now: func() time.Time { return time.Now().UTC() }}
}

// Drain waits for in-flight event goroutines, bounded by ctx.
func (s *MFAService) Drain(ctx context.Context) error { return waitCtx(ctx, &s.wg) }

// Configured reports whether factors can be enrolled and used.
func (s *MFAService) Configured() bool { return s != nil && s.keys != nil }

// Authenticate verifies bearer as a live session of an ACTIVE HUMAN principal.
//
// The envelope proves the session was issued; the session record proves it
// has not been invalidated since (a logout, a revocation, a status change),
// which a signature alone cannot. A principal suspended after the envelope was
// minted is refused here too.
func (s *MFAService) Authenticate(ctx context.Context, bearer string) (*MFACaller, error) {
	if strings.TrimSpace(bearer) == "" {
		return nil, ErrMFASessionInvalid
	}
	env, err := s.envelopes.Verify(bearer)
	if err != nil {
		return nil, ErrMFASessionInvalid
	}
	p, err := s.store.FindByID(ctx, env.Principal.PrincipalID, env.TenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: principal lookup: %v", ErrAuthUnavailable, err)
	}
	if p == nil || p.Status != domain.PrincipalStatusActive || p.PrincipalType != domain.PrincipalTypeHuman {
		return nil, ErrMFASessionInvalid
	}
	sid := env.SessionTrustPosture.SessionContextID
	if sid == "" {
		return nil, ErrMFASessionInvalid
	}
	sc, err := s.store.FindSessionContext(ctx, sid, env.TenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: session lookup: %v", ErrAuthUnavailable, err)
	}
	if sc == nil || sc.InvalidatedAt != nil || sc.PrincipalID != p.PrincipalID {
		return nil, ErrMFASessionInvalid
	}
	c := &MFACaller{Envelope: env, Principal: p, MFAVerified: env.SessionTrustPosture.MFAVerified}
	if env.AuthTime > 0 {
		c.AuthTime = time.Unix(env.AuthTime, 0).UTC()
	}
	return c, nil
}

// fresh reports whether the caller authenticated within d (and not in the
// future, beyond a minute of clock skew).
func (s *MFAService) fresh(c *MFACaller, d time.Duration) bool {
	if c.AuthTime.IsZero() {
		return false
	}
	age := s.now().Sub(c.AuthTime)
	return age <= d && age >= -time.Minute
}

// requireRecentMFA is the gate on replacing, revoking and resetting factors:
// changing who can step up is itself a sensitive action (§3).
func (s *MFAService) requireRecentMFA(c *MFACaller) error {
	if !c.MFAVerified || !s.fresh(c, s.policy.RecentAuth) {
		return ErrMFAStepUpRequired
	}
	return nil
}

// Enrol starts a TOTP enrolment: a new PENDING factor whose secret is returned
// exactly once.
//
// A first factor needs a recent login (anyone holding an old session must not
// be able to bind their own device to it). Replacing an existing factor needs a
// recent step-up with that factor: otherwise a stolen password alone could
// swap the victim's second factor for the thief's.
func (s *MFAService) Enrol(ctx context.Context, c *MFACaller, correlationID string) (*domain.TOTPEnrollment, error) {
	if !s.Configured() {
		return nil, ErrMFAUnavailable
	}
	if c == nil {
		return nil, ErrMFASessionInvalid
	}
	var err error
	has, err := s.store.HasActiveTOTP(ctx, c.Principal.PrincipalID, c.Principal.TenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: factor lookup: %v", ErrAuthUnavailable, err)
	}
	if has {
		if err := s.requireRecentMFA(c); err != nil {
			s.evidence(ctx, c, ActionMFAEnrol, "DENIED", "replacement_requires_step_up", correlationID)
			return nil, err
		}
	} else if !s.fresh(c, s.policy.RecentAuth) {
		s.evidence(ctx, c, ActionMFAEnrol, "DENIED", "reauthentication_required", correlationID)
		return nil, ErrReauthRequired
	}

	secret, err := mfa.NewSecret()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	defer zero(secret)
	factorID := ulid.Make().String()
	ct, kid, err := s.keys.Seal(secret, mfa.FactorAAD(c.Principal.TenantID, c.Principal.PrincipalID, factorID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if err := s.store.CreatePendingTOTP(ctx, domain.TOTPFactorSecret{
		FactorID: factorID, PrincipalID: c.Principal.PrincipalID, TenantID: c.Principal.TenantID,
		Status: domain.CredentialStatusPending, Ciphertext: ct, KeyID: kid,
	}); err != nil {
		return nil, fmt.Errorf("%w: store factor: %v", ErrAuthUnavailable, err)
	}
	s.evidence(ctx, c, ActionMFAEnrol, "ALLOWED", "totp_enrolment_started", correlationID)

	account := c.Principal.Email
	if account == "" {
		account = c.Principal.PrincipalID
	}
	enc := mfa.EncodeSecret(secret)
	return &domain.TOTPEnrollment{
		FactorID:   factorID,
		Secret:     enc,
		OTPAuthURI: mfa.OTPAuthURI(s.policy.IssuerLabel, account, enc),
		Algorithm:  mfa.Algorithm,
		Digits:     mfa.Digits,
		Period:     mfa.PeriodSeconds,
		Status:     domain.CredentialStatusPending,
	}, nil
}

// Activate proves a PENDING factor with a code from it, making it ACTIVE and
// retiring any previous one. Activation is not a step-up: the session's
// assurance is unchanged until the person steps up.
func (s *MFAService) Activate(ctx context.Context, c *MFACaller, factorID, code, correlationID string) (*domain.MFAFactor, error) {
	if !s.Configured() {
		return nil, ErrMFAUnavailable
	}
	if c == nil {
		return nil, ErrMFASessionInvalid
	}
	var err error
	code, err = mfa.NormalizeCode(code)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestInvalid, err)
	}
	if strings.TrimSpace(factorID) == "" {
		return nil, fmt.Errorf("%w: factor_id is required", ErrRequestInvalid)
	}
	f, err := s.store.FindTOTPFactor(ctx, factorID, c.Principal.PrincipalID, c.Principal.TenantID)
	switch {
	case errors.Is(err, domain.ErrCredentialNotFound):
		return nil, ErrMFAFactorNotFound
	case err != nil:
		return nil, fmt.Errorf("%w: factor lookup: %v", ErrAuthUnavailable, err)
	case f.Status != domain.CredentialStatusPending:
		return nil, ErrMFAFactorNotFound
	}

	rec, err := events.Render(events.EventMFAFactorEnrolled, c.Principal.TenantID, "", c.Principal.PrincipalID,
		correlationID, c.Principal.PrincipalID, map[string]any{
			"principal_id":   c.Principal.PrincipalID,
			"tenant_id":      c.Principal.TenantID,
			"factor_id":      f.FactorID,
			"factor_type":    domain.CredentialTypeTOTP,
			"correlation_id": correlationID,
		})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if err := s.verifyCode(ctx, c, f, code, ActionMFAActivate, true, correlationID, rec); err != nil {
		return nil, err
	}
	now := s.now()
	return &domain.MFAFactor{
		FactorID: f.FactorID, PrincipalID: c.Principal.PrincipalID, Type: domain.CredentialTypeTOTP,
		Status: domain.CredentialStatusActive, ActivatedAt: &now, LastUsedAt: &now,
	}, nil
}

// StepUp verifies a code from the principal's ACTIVE factor and mints a new
// login token asserting a second factor, with auth_time = now. The client
// exchanges it at /v1/context/resolve for an envelope with mfa_verified=true.
//
// The verified client (azp) of the session being stepped up is carried over,
// so a step-up does not lose the session's verified source channel.
func (s *MFAService) StepUp(ctx context.Context, c *MFACaller, code, correlationID string) (*domain.AuthenticateResponse, error) {
	if !s.Configured() {
		return nil, ErrMFAUnavailable
	}
	if c == nil {
		return nil, ErrMFASessionInvalid
	}
	var err error
	code, err = mfa.NormalizeCode(code)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestInvalid, err)
	}
	f, err := s.store.FindActiveTOTP(ctx, c.Principal.PrincipalID, c.Principal.TenantID)
	switch {
	case errors.Is(err, domain.ErrCredentialNotFound):
		s.evidence(ctx, c, ActionMFAStepUp, "DENIED", "mfa_not_enrolled", correlationID)
		return nil, ErrMFANotEnrolled
	case err != nil:
		return nil, fmt.Errorf("%w: factor lookup: %v", ErrAuthUnavailable, err)
	}

	amr := []string{domain.AMRPassword, domain.AMROTP}
	rec, err := events.Render(events.EventStepUpSucceeded, c.Principal.TenantID, "", c.Principal.PrincipalID,
		correlationID, c.Principal.PrincipalID, map[string]any{
			"principal_id":       c.Principal.PrincipalID,
			"tenant_id":          c.Principal.TenantID,
			"factor_id":          f.FactorID,
			"session_context_id": c.Envelope.SessionTrustPosture.SessionContextID,
			"amr":                amr,
			"correlation_id":     correlationID,
		})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if err := s.verifyCode(ctx, c, f, code, ActionMFAStepUp, false, correlationID, rec); err != nil {
		return nil, err
	}

	authTime := s.now()
	token, err := s.issuer.IssueGrant(domain.TokenGrant{
		Subject:  c.Principal.IdentityProviderSubject,
		TenantID: c.Principal.TenantID,
		MFADone:  true,
		AuthTime: authTime,
		AMR:      amr,
		ClientID: c.Envelope.AuthorizedParty,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: token issuance: %v", ErrAuthUnavailable, err)
	}
	s.log.Info("identity.step_up.succeeded",
		zap.String("principal_id", c.Principal.PrincipalID),
		zap.String("tenant_id", c.Principal.TenantID),
		zap.String("correlation_id", correlationID))
	return &domain.AuthenticateResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   s.issuer.TTLSeconds(),
		PrincipalID: c.Principal.PrincipalID,
		TenantID:    c.Principal.TenantID,
		MFAEnrolled: true,
		AuthTime:    authTime.Unix(),
		AMR:         amr,
	}, nil
}

// List returns the caller's own factors.
func (s *MFAService) List(ctx context.Context, c *MFACaller) ([]domain.MFAFactor, error) {
	if c == nil {
		return nil, ErrMFASessionInvalid
	}
	var err error
	out, err := s.store.ListMFAFactors(ctx, c.Principal.PrincipalID, c.Principal.TenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	return out, nil
}

// Revoke retires one of the caller's own factors. It needs a recent step-up:
// removing the factor that guards privileged sessions is itself sensitive, and
// a stolen password alone must not be able to do it.
func (s *MFAService) Revoke(ctx context.Context, c *MFACaller, factorID, reasonCode, correlationID string) error {
	if c == nil {
		return ErrMFASessionInvalid
	}
	var err error
	if strings.TrimSpace(factorID) == "" {
		return fmt.Errorf("%w: factor_id is required", ErrRequestInvalid)
	}
	if err := s.requireRecentMFA(c); err != nil {
		s.evidence(ctx, c, ActionMFARevoke, "DENIED", "revocation_requires_step_up", correlationID)
		return err
	}
	reason := "owner_revoked"
	if rc := strings.TrimSpace(reasonCode); rc != "" {
		reason = "owner_revoked:" + truncate(rc, 60)
	}
	rec, err := events.Render(events.EventMFAFactorRevoked, c.Principal.TenantID, "", c.Principal.PrincipalID,
		correlationID, c.Principal.PrincipalID, map[string]any{
			"principal_id":   c.Principal.PrincipalID,
			"tenant_id":      c.Principal.TenantID,
			"factor_id":      factorID,
			"reason":         reason,
			"actor":          c.Principal.PrincipalID,
			"correlation_id": correlationID,
		})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	n, err := s.store.RetireTOTPFactors(ctx, c.Principal.PrincipalID, c.Principal.TenantID, factorID,
		reason, c.Principal.PrincipalID, ActionMFARevoke, correlationID, rec)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if n == 0 {
		return ErrMFAFactorNotFound
	}
	s.siem.Stream(ctx, c.Principal.TenantID, "identity.mfa_factor.revoked", siem.SeverityMedium,
		fmt.Sprintf("Principal %s revoked its own TOTP factor %s", c.Principal.PrincipalID, factorID))
	return nil
}

// ResetFactors retires every factor of subjectPrincipalID on an
// administrator's authority (a lost device). The handler has already verified
// the administrator's session, refused self-reset and checked
// identity.mfa.reset with authorization-svc; this requires the administrator's
// own recent step-up, because resetting someone else's factor is a credential
// recovery, which §3 says is "separately controlled".
func (s *MFAService) ResetFactors(ctx context.Context, admin *MFACaller, subjectPrincipalID, reasonCode, purpose, correlationID string) (int, error) {
	if err := s.requireRecentMFA(admin); err != nil {
		s.evidence(ctx, admin, ActionMFAReset, "DENIED", "reset_requires_step_up", correlationID)
		return 0, err
	}
	tenantID := admin.Principal.TenantID
	subject, err := s.store.FindByID(ctx, subjectPrincipalID, tenantID)
	if err != nil {
		return 0, fmt.Errorf("%w: principal lookup: %v", ErrAuthUnavailable, err)
	}
	if subject == nil {
		return 0, domain.ErrPrincipalNotFound
	}
	reason := "admin_reset:" + truncate(reasonCode, 60)
	rec, err := events.Render(events.EventMFAFactorReset, tenantID, "", admin.Principal.PrincipalID,
		correlationID, subjectPrincipalID, map[string]any{
			"principal_id":   subjectPrincipalID,
			"tenant_id":      tenantID,
			"reason_code":    reasonCode,
			"purpose":        purpose,
			"actor":          admin.Principal.PrincipalID,
			"correlation_id": correlationID,
		})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	n, err := s.store.RetireTOTPFactors(ctx, subjectPrincipalID, tenantID, "", reason,
		admin.Principal.PrincipalID, ActionMFAReset, correlationID, rec)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if n > 0 {
		s.siem.Stream(ctx, tenantID, "identity.mfa_factor.reset", siem.SeverityHigh,
			fmt.Sprintf("Administrator %s reset %d TOTP factor(s) of principal %s (%s)",
				admin.Principal.PrincipalID, n, subjectPrincipalID, reasonCode))
	}
	return n, nil
}

// verifyCode checks code against factor f and records the outcome.
func (s *MFAService) verifyCode(ctx context.Context, c *MFACaller, f *domain.TOTPFactorSecret,
	code, action string, activate bool, correlationID string, rec outbox.Record) error {
	pid, tid := c.Principal.PrincipalID, c.Principal.TenantID
	now := s.now()

	if f.LockedUntil != nil && f.LockedUntil.After(now) {
		s.evidence(ctx, c, action, "DENIED", "totp_factor_locked", correlationID)
		s.refused(ctx, c, action, "totp_factor_locked", siem.SeverityHigh, correlationID)
		return ErrInvalidCode
	}

	secret, err := s.keys.Open(f.Ciphertext, f.KeyID, mfa.FactorAAD(tid, pid, f.FactorID))
	if err != nil {
		// Not the person's fault: a rotated-away key, or a ciphertext that does
		// not belong to this row. 503 rather than "invalid code", which would
		// send them to retry something that can never succeed.
		s.log.Error("totp secret unusable", zap.String("principal_id", pid),
			zap.String("factor_id", f.FactorID), zap.String("key_id", f.KeyID), zap.Error(err))
		s.siem.Stream(ctx, tid, "identity.mfa_factor.secret_unusable", siem.SeverityHigh,
			fmt.Sprintf("TOTP secret for factor %s of principal %s could not be decrypted", f.FactorID, pid))
		return fmt.Errorf("%w: factor secret unusable", ErrAuthUnavailable)
	}
	defer zero(secret)

	step, ok := mfa.Match(secret, code, now, s.policy.SkewSteps)
	if !ok {
		locked, attempts, recErr := s.store.RecordTOTPFailure(ctx, f.FactorID, pid, tid, action, correlationID,
			s.policy.MaxFailedAttempts, s.policy.LockDuration)
		if recErr != nil {
			s.log.Error("failed to record totp failure — lockout not advanced",
				zap.String("principal_id", pid), zap.Error(recErr))
		}
		reason, sev := "totp_mismatch", siem.SeverityMedium
		if locked {
			reason, sev = "totp_mismatch_factor_locked", siem.SeverityHigh
		}
		s.log.Warn("totp code rejected", zap.String("principal_id", pid), zap.String("action", action),
			zap.Int("failed_attempts", attempts), zap.Bool("locked", locked))
		s.refused(ctx, c, action, reason, sev, correlationID)
		return ErrInvalidCode
	}

	err = s.store.AcceptTOTPCode(ctx, domain.TOTPAcceptance{
		FactorID: f.FactorID, PrincipalID: pid, TenantID: tid, Step: step,
		Activate: activate, Action: action, CorrelationID: correlationID,
	}, rec)
	switch {
	case errors.Is(err, domain.ErrTOTPReplayed):
		s.evidence(ctx, c, action, "DENIED", "totp_replayed", correlationID)
		s.refused(ctx, c, action, "totp_replayed", siem.SeverityHigh, correlationID)
		return ErrInvalidCode
	case err != nil:
		return fmt.Errorf("%w: accept code: %v", ErrAuthUnavailable, err)
	}
	return nil
}

// evidence appends a best-effort decision row; a failure is logged loudly.
func (s *MFAService) evidence(ctx context.Context, c *MFACaller, action, outcome, basis, correlationID string) {
	if err := s.store.RecordMFADecision(ctx, c.Principal.PrincipalID, c.Principal.TenantID, action, outcome, basis, correlationID); err != nil {
		s.log.Error("failed to append mfa decision to access_decision_log",
			zap.String("principal_id", c.Principal.PrincipalID), zap.String("basis", basis), zap.Error(err))
	}
}

// refused emits the failure event and streams to SIEM, off the request path.
func (s *MFAService) refused(ctx context.Context, c *MFACaller, action, reason string, sev siem.Severity, correlationID string) {
	pid, tid := c.Principal.PrincipalID, c.Principal.TenantID
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := detach(ctx)
		defer cancel()
		if s.events != nil {
			if err := s.events.PublishStepUpFailed(ctx, pid, tid, correlationID, action, reason); err != nil {
				s.log.Error("event publish failed", zap.String("event_type", events.EventStepUpFailed), zap.Error(err))
			}
		}
		s.siem.Stream(ctx, tid, events.EventStepUpFailed, sev,
			fmt.Sprintf("Second factor refused (%s) for principal %s on %s", reason, pid, action))
	}()
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
