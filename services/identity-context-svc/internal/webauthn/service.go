// Package webauthn is the passkey / security-key factor (AU-11, ZS-IAM-001
// §3 and §13: the preferred phishing-resistant factor).
//
// A passkey is registered by its owner from a live session and is then
// usable in two places: as the second step of the hosted login
// (/oauth2/login/passkey/*, internal/oidc) and as a step-up
// (/v1/authenticate/step-up/passkey/*). Either way the assertion is checked
// against a challenge this service issued, for a credential the principal
// owns, with user verification required, so an envelope minted from it
// carries amr "hwk" and assurance PHISHING_RESISTANT.
//
// Ceremony state (the challenge) lives in Redis between begin and finish and
// is taken exactly once. The verified credential, its sign counter and every
// decision about it live in Postgres, with the event in the same transaction.
package webauthn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	gowa "github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/metrics"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/siem"
)

var (
	// ErrNoPasskey: the principal has no active passkey to assert with. 409.
	ErrNoPasskey = errors.New("no passkey is registered")
	// ErrCeremonyInvalid: the ceremony handle is unknown, expired, already
	// used, or was issued to someone else or for another purpose. 400.
	ErrCeremonyInvalid = errors.New("the passkey ceremony is unknown or has expired; start again")
	// ErrRejected: the authenticator's response did not verify. One answer
	// for every reason (wrong origin, bad signature, unknown credential, a
	// clone signal): the reason is in the evidence and the SIEM stream. 401.
	ErrRejected = errors.New("the passkey was not accepted")
	// ErrDuplicate: that credential is already registered. 409.
	ErrDuplicate = errors.New("this passkey is already registered")
	// ErrUnavailable: the store or the ceremony store failed. 503. The text
	// carries "authentication unavailable", which is how the hosted login
	// (oidc.isUnavailable) tells an outage from a wrong answer.
	ErrUnavailable = errors.New("passkey authentication unavailable")
	// ErrInvalid: the request is malformed. 400.
	ErrInvalid = errors.New("invalid passkey request")
)

// Decision actions, as written to access_decision_log.action_type. They are
// the MFA service's names, so the console read models project a passkey
// exactly as they project TOTP.
const (
	ActionActivate = "MFA_ACTIVATE"
	ActionLogin    = "MFA_LOGIN"
	ActionStepUp   = "MFA_STEP_UP"
	ActionRevoke   = "MFA_REVOKE"
	ActionReset    = "MFA_RESET"
)

// Ceremony purposes. A handle is valid only for the purpose it was issued
// for: a login challenge cannot finish a step-up.
const (
	purposeRegister = "register"
	purposeLogin    = "login"
	purposeStepUp   = "step_up"
)

// Store is the service's slice of the Postgres store.
type Store interface {
	InsertWebAuthnCredential(ctx context.Context, c domain.WebAuthnCredential, correlationID string, recs ...outbox.Record) error
	ListWebAuthnCredentials(ctx context.Context, tenantID, principalID string) ([]domain.WebAuthnCredential, error)
	UseWebAuthnCredential(ctx context.Context, tenantID, principalID, credentialID string, signCount uint32, backupState bool,
		action, correlationID string, recs ...outbox.Record) error
	RetireWebAuthnCredential(ctx context.Context, tenantID, principalID, credentialID, reason, action, correlationID string,
		recs ...outbox.Record) (int64, error)
	HasActiveWebAuthn(ctx context.Context, tenantID, principalID string) (bool, error)
	RecordMFADecision(ctx context.Context, principalID, tenantID, action, outcome, basis, correlationID string) error
}

// Config is the relying party.
type Config struct {
	RPID          string
	RPDisplayName string
	RPOrigins     []string
	// Timeout bounds a ceremony, at the browser and at this service.
	Timeout time.Duration
}

// Service registers and verifies passkeys.
type Service struct {
	wa         *gowa.WebAuthn
	store      Store
	ceremonies Ceremonies
	siem       *siem.Client
	log        *zap.Logger
	ttl        time.Duration
}

// New builds the service. It refuses a relying party it cannot serve: a
// passkey registered under the wrong RP ID is unusable for ever.
func New(cfg Config, store Store, ceremonies Ceremonies, siemClient *siem.Client, log *zap.Logger) (*Service, error) {
	if store == nil || ceremonies == nil {
		return nil, errors.New("webauthn: store and ceremony store are required")
	}
	if strings.TrimSpace(cfg.RPID) == "" || len(cfg.RPOrigins) == 0 {
		return nil, errors.New("webauthn: WEBAUTHN_RP_ID and at least one WEBAUTHN_RP_ORIGINS entry are required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.RPDisplayName == "" {
		cfg.RPDisplayName = "ZoikoSuite"
	}
	timeout := gowa.TimeoutConfig{Enforce: true, Timeout: cfg.Timeout, TimeoutUVD: cfg.Timeout}
	wa, err := gowa.New(&gowa.Config{
		RPID:                  cfg.RPID,
		RPDisplayName:         cfg.RPDisplayName,
		RPOrigins:             cfg.RPOrigins,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationRequired,
		},
		Timeouts: gowa.TimeoutsConfig{Login: timeout, Registration: timeout},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: %w", err)
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{wa: wa, store: store, ceremonies: ceremonies, siem: siemClient, log: log, ttl: cfg.Timeout}, nil
}

// ceremony is what Ceremonies holds for one handle.
type ceremony struct {
	Purpose     string           `json:"purpose"`
	TenantID    string           `json:"tenant_id"`
	PrincipalID string           `json:"principal_id"`
	Session     gowa.SessionData `json:"session"`
}

// user adapts a principal and its stored credentials to the library.
type user struct {
	p     *domain.Principal
	creds []gowa.Credential
}

// WebAuthnID is the user handle: the principal id, never the email, so a
// changed email does not orphan a passkey and the handle reveals nothing.
func (u *user) WebAuthnID() []byte { return []byte(u.p.PrincipalID) }

func (u *user) WebAuthnName() string {
	if u.p.Email != "" {
		return u.p.Email
	}
	return u.p.PrincipalID
}

func (u *user) WebAuthnDisplayName() string {
	if u.p.DisplayName != "" {
		return u.p.DisplayName
	}
	return u.WebAuthnName()
}

func (u *user) WebAuthnCredentials() []gowa.Credential { return u.creds }

func (s *Service) loadUser(ctx context.Context, p *domain.Principal) (*user, error) {
	stored, err := s.store.ListWebAuthnCredentials(ctx, p.TenantID, p.PrincipalID)
	if err != nil {
		return nil, fmt.Errorf("%w: list credentials: %v", ErrUnavailable, err)
	}
	u := &user{p: p}
	for _, c := range stored {
		lc, err := toLibrary(c)
		if err != nil {
			// A row that does not decode can never verify; skipping it is
			// safer than refusing every other passkey the person holds.
			s.log.Error("stored webauthn credential unusable", zap.String("credential_id", c.CredentialID), zap.Error(err))
			continue
		}
		u.creds = append(u.creds, lc)
	}
	return u, nil
}

// HasPasskey reports whether the principal holds an active passkey.
func (s *Service) HasPasskey(ctx context.Context, tenantID, principalID string) (bool, error) {
	return s.store.HasActiveWebAuthn(ctx, tenantID, principalID)
}

// List returns the principal's active passkeys, without key material.
func (s *Service) List(ctx context.Context, tenantID, principalID string) ([]domain.WebAuthnCredential, error) {
	out, err := s.store.ListWebAuthnCredentials(ctx, tenantID, principalID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if out == nil {
		out = []domain.WebAuthnCredential{}
	}
	return out, nil
}

// BeginRegistration returns PublicKeyCredentialCreationOptions for p and the
// handle FinishRegistration needs. Credentials p already holds are excluded,
// so an authenticator cannot be registered twice. Whether p may register at
// all (fresh login, or step-up when a factor exists) is the caller's gate.
func (s *Service) BeginRegistration(ctx context.Context, p *domain.Principal) (json.RawMessage, string, error) {
	u, err := s.loadUser(ctx, p)
	if err != nil {
		return nil, "", err
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(u.creds))
	for i := range u.creds {
		exclude = append(exclude, u.creds[i].Descriptor())
	}
	creation, sess, err := s.wa.BeginRegistration(u, gowa.WithExclusions(exclude))
	if err != nil {
		return nil, "", fmt.Errorf("%w: begin registration: %v", ErrUnavailable, err)
	}
	return s.saveCeremony(ctx, purposeRegister, p, sess, creation)
}

// FinishRegistration verifies the authenticator's attestation response
// against the ceremony and stores the new credential with its event.
func (s *Service) FinishRegistration(ctx context.Context, p *domain.Principal, handle, nickname string, response []byte, correlationID string) (*domain.WebAuthnCredential, error) {
	nickname = strings.TrimSpace(nickname)
	if len(nickname) > 100 {
		return nil, fmt.Errorf("%w: nickname is at most 100 characters", ErrInvalid)
	}
	c, err := s.takeCeremony(ctx, purposeRegister, p, handle)
	if err != nil {
		return nil, err
	}
	u, err := s.loadUser(ctx, p)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		s.refused(ctx, p, ActionActivate, "passkey_registration_malformed", siem.SeverityLow, correlationID, err)
		return nil, ErrRejected
	}
	cred, err := s.wa.CreateCredential(u, c.Session, parsed)
	if err != nil {
		s.refused(ctx, p, ActionActivate, "passkey_registration_rejected", siem.SeverityMedium, correlationID, err)
		return nil, ErrRejected
	}
	stored := fromLibrary(cred, p, nickname)
	rec, err := events.Render(events.EventMFAFactorEnrolled, p.TenantID, "", p.PrincipalID, correlationID, p.PrincipalID,
		map[string]any{
			"principal_id":    p.PrincipalID,
			"tenant_id":       p.TenantID,
			"factor_id":       stored.CredentialID,
			"factor_type":     domain.CredentialTypeWebAuthn,
			"backup_eligible": stored.BackupEligible,
			"correlation_id":  correlationID,
		})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := s.store.InsertWebAuthnCredential(ctx, stored, correlationID, rec); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrDuplicate
		}
		return nil, fmt.Errorf("%w: store credential: %v", ErrUnavailable, err)
	}
	s.siem.Stream(ctx, p.TenantID, events.EventMFAFactorEnrolled, siem.SeverityLow,
		fmt.Sprintf("Principal %s registered a passkey", p.PrincipalID))
	stored.PublicKey = nil
	return &stored, nil
}

// BeginLogin returns assertion options for the hosted login's second step.
func (s *Service) BeginLogin(ctx context.Context, p *domain.Principal) (json.RawMessage, string, error) {
	return s.beginAssertion(ctx, purposeLogin, p)
}

// FinishLogin verifies the hosted login's assertion.
func (s *Service) FinishLogin(ctx context.Context, p *domain.Principal, handle string, assertion []byte) error {
	return s.finishAssertion(ctx, purposeLogin, ActionLogin, p, handle, assertion, "")
}

// BeginStepUp returns assertion options for a step-up of a live session.
func (s *Service) BeginStepUp(ctx context.Context, p *domain.Principal) (json.RawMessage, string, error) {
	return s.beginAssertion(ctx, purposeStepUp, p)
}

// FinishStepUp verifies a step-up assertion. The caller mints the stronger
// login token only when this returns nil.
func (s *Service) FinishStepUp(ctx context.Context, p *domain.Principal, handle string, assertion []byte, correlationID string) error {
	return s.finishAssertion(ctx, purposeStepUp, ActionStepUp, p, handle, assertion, correlationID)
}

// Retire retires one of p's passkeys, or all of them when credentialID is
// empty, with the event (revoked by the owner, or reset by an
// administrator). Returns how many were retired.
func (s *Service) Retire(ctx context.Context, tenantID, principalID, credentialID, reason, actorPrincipalID, action, correlationID string) (int, error) {
	eventType := events.EventMFAFactorRevoked
	if action == ActionReset {
		eventType = events.EventMFAFactorReset
	}
	rec, err := events.Render(eventType, tenantID, "", actorPrincipalID, correlationID, principalID, map[string]any{
		"principal_id":   principalID,
		"tenant_id":      tenantID,
		"factor_id":      credentialID,
		"factor_type":    domain.CredentialTypeWebAuthn,
		"reason":         reason,
		"actor":          actorPrincipalID,
		"correlation_id": correlationID,
	})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	n, err := s.store.RetireWebAuthnCredential(ctx, tenantID, principalID, credentialID, reason, action, correlationID, rec)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return int(n), nil
}

func (s *Service) beginAssertion(ctx context.Context, purpose string, p *domain.Principal) (json.RawMessage, string, error) {
	u, err := s.loadUser(ctx, p)
	if err != nil {
		return nil, "", err
	}
	if len(u.creds) == 0 {
		return nil, "", ErrNoPasskey
	}
	assertion, sess, err := s.wa.BeginLogin(u, gowa.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", fmt.Errorf("%w: begin assertion: %v", ErrUnavailable, err)
	}
	return s.saveCeremony(ctx, purpose, p, sess, assertion)
}

func (s *Service) finishAssertion(ctx context.Context, purpose, action string, p *domain.Principal, handle string, assertion []byte, correlationID string) error {
	c, err := s.takeCeremony(ctx, purpose, p, handle)
	if err != nil {
		return err
	}
	u, err := s.loadUser(ctx, p)
	if err != nil {
		return err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil {
		s.refused(ctx, p, action, "passkey_assertion_malformed", siem.SeverityLow, correlationID, err)
		return ErrRejected
	}
	cred, err := s.wa.ValidateLogin(u, c.Session, parsed)
	if err != nil {
		s.refused(ctx, p, action, "passkey_assertion_rejected", siem.SeverityMedium, correlationID, err)
		return ErrRejected
	}
	if cred.Authenticator.CloneWarning {
		// The counter did not advance on an authenticator that keeps one: two
		// copies of the private key may exist. Refused, and loud.
		s.refused(ctx, p, action, "passkey_clone_warning", siem.SeverityHigh, correlationID, nil)
		return ErrRejected
	}
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	// The success event a TOTP step-up writes, with the passkey's amr; it
	// commits with the sign counter, so a counted assertion is a published one.
	rec, err := events.Render(events.EventStepUpSucceeded, p.TenantID, "", p.PrincipalID, correlationID, p.PrincipalID,
		map[string]any{
			"principal_id":   p.PrincipalID,
			"tenant_id":      p.TenantID,
			"factor_id":      id,
			"factor_type":    domain.CredentialTypeWebAuthn,
			"context":        purpose,
			"amr":            []string{domain.AMRPassword, domain.AMRHardwareKey},
			"correlation_id": correlationID,
		})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := s.store.UseWebAuthnCredential(ctx, p.TenantID, p.PrincipalID, id, cred.Authenticator.SignCount,
		cred.Flags.BackupState, action, correlationID, rec); err != nil {
		if errors.Is(err, domain.ErrCredentialNotFound) {
			// Retired between begin and finish.
			s.refused(ctx, p, action, "passkey_retired_during_ceremony", siem.SeverityMedium, correlationID, nil)
			return ErrRejected
		}
		return fmt.Errorf("%w: record assertion: %v", ErrUnavailable, err)
	}
	if ic8 := metrics.Default(); ic8 != nil {
		ic8.MFAVerifications.WithLabelValues("success").Inc()
	}
	return nil
}

func (s *Service) saveCeremony(ctx context.Context, purpose string, p *domain.Principal, sess *gowa.SessionData, options any) (json.RawMessage, string, error) {
	opts, err := json.Marshal(options)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	data, err := json.Marshal(ceremony{Purpose: purpose, TenantID: p.TenantID, PrincipalID: p.PrincipalID, Session: *sess})
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	handle, err := newHandle()
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := s.ceremonies.Put(ctx, handle, data, s.ttl); err != nil {
		return nil, "", fmt.Errorf("%w: store ceremony: %v", ErrUnavailable, err)
	}
	return opts, handle, nil
}

// takeCeremony consumes the ceremony and checks it belongs to p and purpose.
// It is consumed even when the check fails, so a handle is never tried twice.
func (s *Service) takeCeremony(ctx context.Context, purpose string, p *domain.Principal, handle string) (*ceremony, error) {
	if strings.TrimSpace(handle) == "" {
		return nil, ErrCeremonyInvalid
	}
	b, err := s.ceremonies.Take(ctx, handle)
	if errors.Is(err, errCeremonyNotFound) {
		return nil, ErrCeremonyInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("%w: ceremony lookup: %v", ErrUnavailable, err)
	}
	var c ceremony
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, ErrCeremonyInvalid
	}
	if c.Purpose != purpose || c.TenantID != p.TenantID || c.PrincipalID != p.PrincipalID ||
		!bytes.Equal(c.Session.UserID, []byte(p.PrincipalID)) {
		return nil, ErrCeremonyInvalid
	}
	return &c, nil
}

// refused records a refused ceremony: a DENIED decision (which the console
// shows as a failed sign-in or step-up) and a SIEM signal.
func (s *Service) refused(ctx context.Context, p *domain.Principal, action, basis string, sev siem.Severity, correlationID string, cause error) {
	if ic8 := metrics.Default(); ic8 != nil {
		ic8.MFAVerifications.WithLabelValues("failure").Inc()
	}
	fields := []zap.Field{zap.String("principal_id", p.PrincipalID), zap.String("action", action), zap.String("basis", basis)}
	if cause != nil {
		fields = append(fields, zap.Error(cause))
	}
	s.log.Warn("passkey refused", fields...)
	if err := s.store.RecordMFADecision(ctx, p.PrincipalID, p.TenantID, action, "DENIED", basis, correlationID); err != nil {
		s.log.Error("failed to append passkey decision", zap.String("principal_id", p.PrincipalID), zap.Error(err))
	}
	s.siem.Stream(ctx, p.TenantID, events.EventStepUpFailed, sev,
		fmt.Sprintf("Passkey refused (%s) for principal %s on %s", basis, p.PrincipalID, action))
}

func newHandle() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// toLibrary rebuilds the library's credential record from a stored row.
func toLibrary(c domain.WebAuthnCredential) (gowa.Credential, error) {
	id, err := base64.RawURLEncoding.DecodeString(c.CredentialID)
	if err != nil {
		return gowa.Credential{}, fmt.Errorf("credential id: %w", err)
	}
	var aaguid []byte
	if c.AAGUID != "" {
		u, err := uuid.Parse(c.AAGUID)
		if err != nil {
			return gowa.Credential{}, fmt.Errorf("aaguid: %w", err)
		}
		aaguid = u[:]
	}
	transports := make([]protocol.AuthenticatorTransport, 0, len(c.Transports))
	for _, t := range c.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(t))
	}
	return gowa.Credential{
		ID:              id,
		PublicKey:       c.PublicKey,
		AttestationType: c.AttestationType,
		Transport:       transports,
		Flags: gowa.CredentialFlags{
			UserPresent:    true,
			UserVerified:   c.UserVerified,
			BackupEligible: c.BackupEligible,
			BackupState:    c.BackupState,
		},
		Authenticator: gowa.Authenticator{AAGUID: aaguid, SignCount: c.SignCount},
	}, nil
}

// fromLibrary is the row for a freshly verified credential.
func fromLibrary(c *gowa.Credential, p *domain.Principal, nickname string) domain.WebAuthnCredential {
	var aaguid string
	if u, err := uuid.FromBytes(c.Authenticator.AAGUID); err == nil && u != uuid.Nil {
		aaguid = u.String()
	}
	transports := make([]string, 0, len(c.Transport))
	for _, t := range c.Transport {
		transports = append(transports, string(t))
	}
	attType := c.AttestationType
	if attType == "" {
		attType = "none"
	}
	return domain.WebAuthnCredential{
		CredentialID:    base64.RawURLEncoding.EncodeToString(c.ID),
		TenantID:        p.TenantID,
		PrincipalID:     p.PrincipalID,
		PublicKey:       c.PublicKey,
		AttestationType: attType,
		AAGUID:          aaguid,
		SignCount:       c.Authenticator.SignCount,
		Transports:      transports,
		BackupEligible:  c.Flags.BackupEligible,
		BackupState:     c.Flags.BackupState,
		UserVerified:    c.Flags.UserVerified,
		Nickname:        nickname,
		Status:          "ACTIVE",
		CreatedAt:       time.Now().UTC(),
	}
}
