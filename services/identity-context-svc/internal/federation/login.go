package federation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
)

// Sentinels of the federated login. The handler maps them to the same three answers /v1/authenticate
// gives: 400 for a malformed request, 401 for any refusal, 503 when no decision could be made.
var (
	ErrLoginRequestInvalid = errors.New("tenant_id and id_token are required")
	// ErrLoginRejected is the ONLY refusal a login caller ever sees. Wrong issuer, wrong audience, an
	// expired or forged token, an unknown subject and a suspended account are indistinguishable on the
	// wire; the reason is in the evidence, the event stream and the SIEM stream.
	ErrLoginRejected    = errors.New("invalid credentials")
	ErrLoginUnavailable = errors.New("authentication unavailable")
)

// PrincipalReader is the one thing the login needs from the principal store.
type PrincipalReader interface {
	FindByID(ctx context.Context, principalID, tenantID string) (*domain.Principal, error)
}

// TokenIssuer mints the first-party bearer token POST /v1/context/resolve accepts.
type TokenIssuer interface {
	Issue(subject, tenantID string, mfaDone bool) (string, error)
	TTLSeconds() int
}

// LoginRequest is the exchange of a tenant IdP's ID token for a first-party bearer token.
type LoginRequest struct {
	TenantID      string `json:"tenant_id"`
	IDToken       string `json:"id_token"`
	Nonce         string `json:"nonce"`
	CorrelationID string `json:"correlation_id"`
}

// Alerts is the SIEM stream; *siem.Client satisfies it.
type Alerts interface {
	Stream(ctx context.Context, tenantID, eventType string, severity siem.Severity, message string)
}

// Provisioner creates a principal, link and baseline assignments for a verified first login when a
// connection allows just-in-time provisioning. Nil means JIT is not available in this deployment.
type Provisioner interface {
	JustInTime(ctx context.Context, conn Connection, ver Version, verified Verified, correlationID string) (*Link, error)
}

// Login authenticates a person through their tenant's identity provider.
//
// It issues the same bearer token the password path does, so the resolver, the envelope and every
// downstream check are unchanged. What differs is the proof: a signature from the tenant's approved key
// set, over claims that name this tenant's approved issuer and this client. The person is found by
// (tenant, issuer, stable subject); an email address plays no part, so an address shared across issuers
// or reassigned cannot reach someone else's principal.
type Login struct {
	repo       Repo
	verifier   *Verifier
	principals PrincipalReader
	issuer     TokenIssuer
	alerts     Alerts
	jit        Provisioner
	log        *zap.Logger
	now        func() time.Time
}

// NewLogin wires the federated login. alerts, jit may be nil.
func NewLogin(repo Repo, verifier *Verifier, principals PrincipalReader, issuer TokenIssuer, alerts Alerts, log *zap.Logger) *Login {
	if log == nil {
		log = zap.NewNop()
	}
	return &Login{repo: repo, verifier: verifier, principals: principals, issuer: issuer, alerts: alerts, log: log, now: time.Now}
}

// WithProvisioner enables just-in-time provisioning for connections that opt in.
func (l *Login) WithProvisioner(p Provisioner) *Login { l.jit = p; return l }

// Authenticate runs the federated login.
func (l *Login) Authenticate(ctx context.Context, req LoginRequest) (*domain.AuthenticateResponse, error) {
	req.IDToken = strings.TrimSpace(req.IDToken)
	if req.TenantID == "" || req.IDToken == "" {
		return nil, ErrLoginRequestInvalid
	}

	iss, err := PeekIssuer(req.IDToken)
	if err != nil {
		return nil, l.refuse(ctx, req, "", "malformed", siem.SeverityMedium)
	}
	conn, ver, err := l.repo.FindLoginConnection(ctx, req.TenantID, iss)
	if err != nil {
		l.log.Error("federation connection lookup failed — failing closed", zap.String("tenant_id", req.TenantID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}
	if conn == nil || ver == nil {
		return nil, l.refuse(ctx, req, iss, "no_connection", siem.SeverityMedium)
	}

	verified, err := l.verifier.Verify(ctx, *conn, ver.Config, req.IDToken, req.Nonce)
	if err != nil {
		if errors.Is(err, ErrKeysUnavailable) {
			l.log.Error("federation signing keys unavailable — failing closed",
				zap.String("tenant_id", req.TenantID), zap.String("connection_id", conn.ConnectionID), zap.Error(err))
			l.stream(ctx, req.TenantID, "identity.federation.keys_unavailable", siem.SeverityHigh,
				fmt.Sprintf("Signing keys unavailable for federation connection %s", conn.ConnectionID))
			return nil, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
		}
		reason := ReasonOf(err)
		if reason == "" {
			reason = "invalid"
		}
		if SignatureVerified(verified) {
			return nil, l.deny(ctx, req, conn, ver, verified, "", reason)
		}
		return nil, l.refuse(ctx, req, iss, reason, siem.SeverityMedium)
	}

	link, err := l.repo.FindLink(ctx, req.TenantID, verified.Issuer, verified.Subject)
	if err != nil {
		return nil, fmt.Errorf("%w: link lookup: %v", ErrLoginUnavailable, err)
	}
	if link == nil && ver.Config.JITEnabled && l.jit != nil {
		link, err = l.jit.JustInTime(ctx, *conn, *ver, *verified, req.CorrelationID)
		if err != nil {
			l.log.Warn("just-in-time provisioning refused", zap.String("tenant_id", req.TenantID), zap.Error(err))
			return nil, l.deny(ctx, req, conn, ver, verified, "", "jit_refused")
		}
	}
	if link == nil {
		return nil, l.deny(ctx, req, conn, ver, verified, "", "no_link")
	}
	if link.Status != LinkActive {
		return nil, l.deny(ctx, req, conn, ver, verified, link.PrincipalID, "link_inactive")
	}

	principal, err := l.principals.FindByID(ctx, link.PrincipalID, req.TenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: principal lookup: %v", ErrLoginUnavailable, err)
	}
	if principal == nil || principal.Status != domain.PrincipalStatusActive {
		return nil, l.deny(ctx, req, conn, ver, verified, link.PrincipalID, "principal_inactive")
	}

	// The token asserts the principal's IdP subject: VerifyBearer hands its sub to FindByIDPSubject.
	token, err := l.issuer.Issue(principal.IdentityProviderSubject, principal.TenantID, verified.MFA)
	if err != nil {
		return nil, fmt.Errorf("%w: token issuance: %v", ErrLoginUnavailable, err)
	}

	expiry := verified.ExpiresAt
	if expiry.IsZero() {
		expiry = l.now().Add(time.Duration(ver.Config.MaxTokenAgeSeconds) * time.Second)
	}
	rec := l.record(req, conn, ver, verified, principal.PrincipalID, "login")
	rec.Outcome = OutcomeSucceeded
	if err := l.repo.CompleteLogin(ctx, rec, link.LinkID, verified.TokenHash, expiry.Add(time.Duration(ver.Config.ClockSkewSeconds)*time.Second)); err != nil {
		if errors.Is(err, ErrTokenReplayed) {
			return nil, l.deny(ctx, req, conn, ver, verified, principal.PrincipalID, "token_replayed")
		}
		// fail closed: an unlogged login is not issued
		l.log.Error("could not record the federated login — refusing to issue a token",
			zap.String("principal_id", principal.PrincipalID), zap.Error(err))
		return nil, fmt.Errorf("%w: evidence write: %v", ErrLoginUnavailable, err)
	}

	return &domain.AuthenticateResponse{
		AccessToken: token, TokenType: "Bearer", ExpiresIn: l.issuer.TTLSeconds(),
		PrincipalID: principal.PrincipalID, TenantID: principal.TenantID, MFARequired: false,
	}, nil
}

func (l *Login) record(req LoginRequest, conn *Connection, ver *Version, v *Verified, principalID, reason string) LoginRecord {
	return LoginRecord{
		TenantID: req.TenantID, ConnectionID: conn.ConnectionID, ConfigVersion: ver.Version,
		Issuer: v.Issuer, Subject: v.Subject, PrincipalID: principalID, Reason: reason,
		KeyID: v.KeyID, MFAAsserted: v.MFA, DegradedKeys: v.Degraded, CorrelationID: req.CorrelationID,
	}
}

// deny records a refusal of an authentic assertion: the signature verified, so the attempt is evidence.
func (l *Login) deny(ctx context.Context, req LoginRequest, conn *Connection, ver *Version, v *Verified, principalID, reason string) error {
	if v.Subject == "" {
		v.Subject = "(none)"
	}
	rec := l.record(req, conn, ver, v, principalID, reason)
	rec.Outcome = OutcomeDenied
	if err := l.repo.RecordDenied(ctx, rec); err != nil {
		l.log.Error("could not record a refused federated login", zap.String("reason", reason), zap.Error(err))
	}
	l.stream(ctx, req.TenantID, "identity.federation.login_denied", siem.SeverityMedium,
		fmt.Sprintf("Federated login refused (%s) on connection %s", reason, conn.ConnectionID))
	return ErrLoginRejected
}

// refuse handles input that has NOT proven a signature: nothing durable is written for it.
func (l *Login) refuse(ctx context.Context, req LoginRequest, claimedIssuer, reason string, sev siem.Severity) error {
	l.log.Warn("federated login refused", zap.String("tenant_id", req.TenantID), zap.String("reason", reason),
		zap.String("correlation_id", req.CorrelationID))
	l.stream(ctx, req.TenantID, "identity.federation.login_rejected", sev,
		fmt.Sprintf("Federated login rejected (%s)", reason))
	return ErrLoginRejected
}

func (l *Login) stream(ctx context.Context, tenantID, eventType string, sev siem.Severity, msg string) {
	if l.alerts != nil {
		l.alerts.Stream(context.WithoutCancel(ctx), tenantID, eventType, sev, msg)
	}
}
