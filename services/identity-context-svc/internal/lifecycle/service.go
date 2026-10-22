package lifecycle

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/credential"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/store"
)

// Store is the lifecycle's slice of the Postgres store.
type Store interface {
	CreatePrincipal(ctx context.Context, np domain.NewPrincipal, recs ...outbox.Record) error
	GetPrincipalDetail(ctx context.Context, tenantID, principalID string) (*store.PrincipalDetail, error)
	FindPrincipalBy(ctx context.Context, tenantID, field, value string) (*store.PrincipalDetail, error)
	SetPrincipalLifecycle(ctx context.Context, tenantID, principalID string, status domain.PrincipalStatus,
		actor, reason, correlationID string, recs ...outbox.Record) (bool, error)
	IssueCredentialToken(ctx context.Context, t domain.CredentialToken) error
	ConsumeCredentialToken(ctx context.Context, tokenHash, purpose string, now time.Time,
		apply func(ctx context.Context, tx pgx.Tx, t domain.CredentialToken) ([]outbox.Record, error)) (*domain.CredentialToken, error)
	SetPasswordTx(ctx context.Context, tx pgx.Tx, tenantID, principalID, hash string) error
	ActivatePrincipalTx(ctx context.Context, tx pgx.Tx, tenantID, principalID string) error
	ChangePassword(ctx context.Context, tenantID, principalID, hash string, recs ...outbox.Record) error
	RecentPasswordHashes(ctx context.Context, tenantID, principalID string, n int) ([]string, error)
	CreateResetRequest(ctx context.Context, r domain.PasswordResetRequest, recs ...outbox.Record) error
	GetResetRequest(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error)
	DecideResetRequest(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
		issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error)
	CompleteResetRequestTx(ctx context.Context, tx pgx.Tx, requestID string) error
	InsertCredentialTokenTx(ctx context.Context, tx pgx.Tx, t domain.CredentialToken) error
	FindActiveCredentialByEmail(ctx context.Context, email, tenantID string) (*domain.Principal, *domain.PrincipalCredential, error)
}

// SessionEnder ends every session a principal holds (login sessions, their
// refresh tokens, and every resolved session context).
type SessionEnder func(ctx context.Context, tenantID, principalID, reason, actor, correlationID string) (int, error)

// Config is the lifecycle's policy.
type Config struct {
	InviteTTL        time.Duration
	ResetTTL         time.Duration
	PasswordHistory  int
	ResetApprovals   int
	PublicAppURL     string
	ActivationPath   string
	ResetPath        string
	IntentActivation string
	IntentReset      string
	ResetRequestTTL  time.Duration
	StepUpMaxAge     time.Duration
	ForgotPerIPPerHr int
}

// Service runs the lifecycle commands.
type Service struct {
	store    Store
	hasher   *credential.Hasher
	policy   Policy
	notifier Notifier
	end      SessionEnder
	cfg      Config
	log      *zap.Logger
	now      func() time.Time
	budget   func(ctx context.Context, name, key string, max int, window time.Duration) bool
}

// New builds the service. budget may be nil (no forgot-password budget).
func New(st Store, hasher *credential.Hasher, policy Policy, notifier Notifier, end SessionEnder, cfg Config,
	budget func(ctx context.Context, name, key string, max int, window time.Duration) bool, log *zap.Logger) *Service {
	if cfg.ResetRequestTTL <= 0 {
		cfg.ResetRequestTTL = 24 * time.Hour
	}
	if cfg.StepUpMaxAge <= 0 {
		cfg.StepUpMaxAge = 15 * time.Minute
	}
	if cfg.IntentActivation == "" {
		cfg.IntentActivation = "identity.subject.activation"
	}
	if cfg.IntentReset == "" {
		cfg.IntentReset = "identity.password.reset"
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{store: st, hasher: hasher, policy: policy, notifier: notifier, end: end, cfg: cfg, log: log,
		now: func() time.Time { return time.Now().UTC() }, budget: budget}
}

// Actor is the verified caller of an administrative command.
type Actor struct {
	PrincipalID   string
	TenantID      string
	MFAVerified   bool
	AuthTime      time.Time
	CorrelationID string
}

// Errors the handler maps.
var (
	ErrInvalid       = errors.New("invalid request")
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrForbidden     = errors.New("not permitted")
	ErrStepUp        = errors.New("a recent second-factor authentication is required")
	ErrBadCredential = errors.New("current password is incorrect")
	ErrLinkInvalid   = errors.New("link invalid, expired or already used")
)

// CreateSubjectRequest is POST /v1/principals.
type CreateSubjectRequest struct {
	Email         string `json:"email"`
	DisplayName   string `json:"display_name"`
	PrincipalType string `json:"principal_type"`
	ExternalID    string `json:"external_id,omitempty"`
	// Invite sends the activation link now (default true for HUMAN).
	Invite *bool `json:"invite,omitempty"`
}

// CreateSubject creates a PENDING_ACTIVATION subject and, for a person,
// sends the activation link.
func (s *Service) CreateSubject(ctx context.Context, a Actor, req CreateSubjectRequest, source string) (*store.PrincipalDetail, error) {
	email := strings.ToLower(strings.TrimSpace(req.Email))
	name := strings.TrimSpace(req.DisplayName)
	ptype := domain.PrincipalType(strings.ToUpper(strings.TrimSpace(req.PrincipalType)))
	if ptype == "" {
		ptype = domain.PrincipalTypeHuman
	}
	switch ptype {
	case domain.PrincipalTypeHuman, domain.PrincipalTypeServiceAccount, domain.PrincipalTypeAPIClient:
	default:
		return nil, fmt.Errorf("%w: principal_type must be HUMAN, SERVICE_ACCOUNT or API_CLIENT", ErrInvalid)
	}
	if name == "" || (ptype == domain.PrincipalTypeHuman && !strings.Contains(email, "@")) {
		return nil, fmt.Errorf("%w: display_name and (for a person) a valid email are required", ErrInvalid)
	}
	if source == "" {
		source = domain.PrincipalSourceLocal
	}
	id := ulid.Make().String()
	status := domain.PrincipalStatusPendingActivation
	if ptype != domain.PrincipalTypeHuman {
		// A workload never activates by email; it is ACTIVE and has no
		// password at all (IAM §16: workloads authenticate by certificate).
		status = domain.PrincipalStatusActive
	}
	payload := map[string]any{"principal_id": id, "tenant_id": a.TenantID, "principal_type": string(ptype),
		"status": string(status), "source": source, "created_by": a.PrincipalID, "external_id": req.ExternalID}
	rec, err := events.Render(events.EventSubjectCreated, a.TenantID, "", a.PrincipalID, a.CorrelationID, id, payload)
	if err != nil {
		return nil, err
	}
	if err := s.store.CreatePrincipal(ctx, domain.NewPrincipal{PrincipalID: id, TenantID: a.TenantID, PrincipalType: ptype,
		Email: email, DisplayName: name, ExternalID: req.ExternalID, Source: source, Status: status,
		CreatedBy: a.PrincipalID}, rec); err != nil {
		if errors.Is(err, store.ErrPrincipalConflict) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return nil, err
	}
	invite := ptype == domain.PrincipalTypeHuman
	if req.Invite != nil {
		invite = *req.Invite && ptype == domain.PrincipalTypeHuman
	}
	if invite {
		if err := s.sendLink(ctx, a.TenantID, id, domain.CredentialTokenActivation, "", a.CorrelationID); err != nil {
			s.log.Warn("subject created but the invitation was not delivered", zap.String("principal_id", id), zap.Error(err))
		}
	}
	return s.store.GetPrincipalDetail(ctx, a.TenantID, id)
}

// Invite (re)sends an activation link to a PENDING_ACTIVATION person.
func (s *Service) Invite(ctx context.Context, a Actor, principalID string) error {
	p, err := s.store.GetPrincipalDetail(ctx, a.TenantID, principalID)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound
	}
	if p.Status != domain.PrincipalStatusPendingActivation || p.PrincipalType != domain.PrincipalTypeHuman {
		return fmt.Errorf("%w: only a person awaiting activation can be invited", ErrConflict)
	}
	return s.sendLink(ctx, a.TenantID, principalID, domain.CredentialTokenActivation, "", a.CorrelationID)
}

func (s *Service) linkFor(purpose, token string) string {
	path := s.cfg.ActivationPath
	if purpose == domain.CredentialTokenReset {
		path = s.cfg.ResetPath
	}
	return s.cfg.PublicAppURL + path + "?token=" + url.QueryEscape(token)
}

func (s *Service) ttlFor(purpose string) time.Duration {
	if purpose == domain.CredentialTokenReset {
		return s.cfg.ResetTTL
	}
	return s.cfg.InviteTTL
}

func (s *Service) intentFor(purpose string) string {
	if purpose == domain.CredentialTokenReset {
		return s.cfg.IntentReset
	}
	return s.cfg.IntentActivation
}

// sendLink issues a one-time link (only the newest works) and delivers it.
func (s *Service) sendLink(ctx context.Context, tenantID, principalID, purpose, requestID, corr string) error {
	if s.notifier == nil {
		return ErrNotifierUnconfigured
	}
	token := randomToken()
	exp := s.now().Add(s.ttlFor(purpose))
	if err := s.store.IssueCredentialToken(ctx, domain.CredentialToken{TokenHash: hashToken(token), TenantID: tenantID,
		PrincipalID: principalID, Purpose: purpose, RequestID: requestID, ExpiresAt: exp}); err != nil {
		return err
	}
	return s.notifier.Notify(ctx, Notice{TenantID: tenantID, PrincipalID: principalID, Intent: s.intentFor(purpose),
		Link: s.linkFor(purpose, token), ExpiresAt: exp, SourceEventID: purpose + "-" + hashToken(token)[:24], CorrelationID: corr})
}

// Activate redeems an activation link: the person sets their password and
// the subject becomes ACTIVE.
func (s *Service) Activate(ctx context.Context, token, password, corr string) (*store.PrincipalDetail, error) {
	return s.redeem(ctx, token, domain.CredentialTokenActivation, password, corr)
}

// CompleteReset redeems a reset link and ends every session the person held.
func (s *Service) CompleteReset(ctx context.Context, token, password, corr string) (*store.PrincipalDetail, error) {
	return s.redeem(ctx, token, domain.CredentialTokenReset, password, corr)
}

func (s *Service) redeem(ctx context.Context, token, purpose, password, corr string) (*store.PrincipalDetail, error) {
	if token == "" || password == "" {
		return nil, fmt.Errorf("%w: token and password are required", ErrInvalid)
	}
	var policyErr error
	var hash string
	t, err := s.store.ConsumeCredentialToken(ctx, hashToken(token), purpose, s.now(),
		func(ctx context.Context, tx pgx.Tx, ct domain.CredentialToken) ([]outbox.Record, error) {
			if err := s.acceptable(ctx, ct.TenantID, ct.PrincipalID, password); err != nil {
				policyErr = err
				return nil, err
			}
			var err error
			if hash, err = s.hasher.Hash(password); err != nil {
				return nil, err
			}
			if err := s.store.SetPasswordTx(ctx, tx, ct.TenantID, ct.PrincipalID, hash); err != nil {
				return nil, err
			}
			evt := events.EventPasswordChanged
			payload := map[string]any{"principal_id": ct.PrincipalID, "tenant_id": ct.TenantID, "via": strings.ToLower(purpose)}
			if purpose == domain.CredentialTokenActivation {
				if err := s.store.ActivatePrincipalTx(ctx, tx, ct.TenantID, ct.PrincipalID); err != nil {
					return nil, err
				}
				evt = events.EventSubjectEnabled
				payload["new_status"] = string(domain.PrincipalStatusActive)
			} else if err := s.store.CompleteResetRequestTx(ctx, tx, ct.RequestID); err != nil {
				return nil, err
			}
			rec, err := events.Render(evt, ct.TenantID, "", ct.PrincipalID, corr, ct.PrincipalID, payload)
			return []outbox.Record{rec}, err
		})
	if policyErr != nil {
		return nil, policyErr
	}
	if errors.Is(err, domain.ErrCredentialTokenUsed) {
		return nil, ErrLinkInvalid
	}
	if err != nil {
		return nil, err
	}
	if purpose == domain.CredentialTokenReset && s.end != nil {
		if _, err := s.end(ctx, t.TenantID, t.PrincipalID, domain.SessionEndPasswordChanged, t.PrincipalID, corr); err != nil {
			s.log.Error("sessions not ended after a password reset", zap.String("principal_id", t.PrincipalID), zap.Error(err))
		}
	}
	return s.store.GetPrincipalDetail(ctx, t.TenantID, t.PrincipalID)
}

// acceptable runs the policy and the reuse check.
func (s *Service) acceptable(ctx context.Context, tenantID, principalID, password string) error {
	p, err := s.store.GetPrincipalDetail(ctx, tenantID, principalID)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound
	}
	if err := s.policy.Check(ctx, password, p.Email, p.DisplayName); err != nil {
		return err
	}
	if s.cfg.PasswordHistory > 0 {
		prior, err := s.store.RecentPasswordHashes(ctx, tenantID, principalID, s.cfg.PasswordHistory)
		if err != nil {
			return err
		}
		for _, h := range prior {
			if verifyMatches(s.hasher, password, h) {
				return &PolicyError{"reused", fmt.Sprintf("choose a password you have not used in your last %d", s.cfg.PasswordHistory)}
			}
		}
	}
	return nil
}

// verifyMatches reports whether password matches the PHC digest h. The
// hasher's Verify returns (needsRehash, err): a nil error is a match.
func verifyMatches(h *credential.Hasher, password, digest string) bool {
	_, err := h.Verify(password, digest)
	return err == nil
}

// ChangePassword is the self-service change: the current password proves
// possession, the new one meets the policy, and every OTHER session ends.
func (s *Service) ChangePassword(ctx context.Context, a Actor, current, next string) error {
	p, err := s.store.GetPrincipalDetail(ctx, a.TenantID, a.PrincipalID)
	if err != nil {
		return err
	}
	if p == nil || p.Status != domain.PrincipalStatusActive {
		return ErrForbidden
	}
	_, cred, err := s.store.FindActiveCredentialByEmail(ctx, p.Email, a.TenantID)
	if err != nil || cred == nil || !verifyMatches(s.hasher, current, cred.SecretHash) {
		return ErrBadCredential
	}
	if err := s.acceptable(ctx, a.TenantID, a.PrincipalID, next); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(next)
	if err != nil {
		return err
	}
	rec, err := events.Render(events.EventPasswordChanged, a.TenantID, "", a.PrincipalID, a.CorrelationID, a.PrincipalID,
		map[string]any{"principal_id": a.PrincipalID, "tenant_id": a.TenantID, "via": "self_service"})
	if err != nil {
		return err
	}
	if err := s.store.ChangePassword(ctx, a.TenantID, a.PrincipalID, hash, rec); err != nil {
		return err
	}
	if s.end != nil {
		if _, err := s.end(ctx, a.TenantID, a.PrincipalID, domain.SessionEndPasswordChanged, a.PrincipalID, a.CorrelationID); err != nil {
			s.log.Error("sessions not ended after a password change", zap.Error(err))
		}
	}
	return nil
}

// ForgotPassword sends a reset link to an active person's own address. It
// answers the same whether or not the account exists (no enumeration).
func (s *Service) ForgotPassword(ctx context.Context, tenantID, email, clientIP, corr string) {
	if s.budget != nil && !s.budget(ctx, "forgot", clientIP, s.cfg.ForgotPerIPPerHr, time.Hour) {
		return
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if tenantID == "" || email == "" {
		return
	}
	if s.budget != nil && !s.budget(ctx, "forgot-account", tenantID+"/"+email, 3, time.Hour) {
		return
	}
	p, err := s.store.FindPrincipalBy(ctx, tenantID, "email", email)
	if err != nil || p == nil || p.Status != domain.PrincipalStatusActive || p.PrincipalType != domain.PrincipalTypeHuman {
		return
	}
	if err := s.sendLink(ctx, tenantID, p.PrincipalID, domain.CredentialTokenReset, "", corr); err != nil {
		s.log.Warn("reset link not delivered", zap.String("principal_id", p.PrincipalID), zap.Error(err))
	}
}

// RequestReset opens an administrator-initiated reset. It issues nothing
// until independently approved.
func (s *Service) RequestReset(ctx context.Context, a Actor, principalID, reason string) (*domain.PasswordResetRequest, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	if principalID == a.PrincipalID {
		return nil, fmt.Errorf("%w: change your own password with the current one, or use the forgotten-password link", ErrForbidden)
	}
	p, err := s.store.GetPrincipalDetail(ctx, a.TenantID, principalID)
	if err != nil {
		return nil, err
	}
	if p == nil || p.PrincipalType != domain.PrincipalTypeHuman {
		return nil, ErrNotFound
	}
	now := s.now()
	r := domain.PasswordResetRequest{RequestID: ulid.Make().String(), TenantID: a.TenantID, PrincipalID: principalID,
		RequestedBy: a.PrincipalID, Reason: reason, ApprovalsRequired: s.cfg.ResetApprovals, Approvals: []domain.ResetApproval{},
		Status: "PENDING_APPROVAL", CreatedAt: now, ExpiresAt: now.Add(s.cfg.ResetRequestTTL)}
	rec, err := events.Render(events.EventPasswordResetRequest, a.TenantID, "", a.PrincipalID, a.CorrelationID, r.RequestID,
		map[string]any{"request_id": r.RequestID, "principal_id": principalID, "requested_by": a.PrincipalID,
			"approvals_required": r.ApprovalsRequired})
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateResetRequest(ctx, r, rec); err != nil {
		return nil, err
	}
	return &r, nil
}

// DecideReset records an approver's decision. The approver is neither the
// requester nor the subject and holds a recent second-factor authentication;
// the link is issued and delivered when the approvals are complete.
func (s *Service) DecideReset(ctx context.Context, a Actor, requestID, decision string) (*domain.PasswordResetRequest, error) {
	decision = strings.ToUpper(strings.TrimSpace(decision))
	if decision != "APPROVE" && decision != "REJECT" {
		return nil, fmt.Errorf("%w: decision must be APPROVE or REJECT", ErrInvalid)
	}
	if !a.MFAVerified || a.AuthTime.IsZero() || s.now().Sub(a.AuthTime) > s.cfg.StepUpMaxAge {
		return nil, ErrStepUp
	}
	r, err := s.store.GetResetRequest(ctx, a.TenantID, requestID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNotFound
	}
	if a.PrincipalID == r.RequestedBy || a.PrincipalID == r.PrincipalID {
		return nil, fmt.Errorf("%w: the requester and the subject cannot approve a reset (independent approval)", ErrForbidden)
	}
	var issued string
	rec, err := events.Render(events.EventPasswordResetDecided, a.TenantID, "", a.PrincipalID, a.CorrelationID, requestID,
		map[string]any{"request_id": requestID, "principal_id": r.PrincipalID, "decided_by": a.PrincipalID, "decision": decision})
	if err != nil {
		return nil, err
	}
	out, err := s.store.DecideResetRequest(ctx, a.TenantID, requestID,
		domain.ResetApproval{ApproverID: a.PrincipalID, At: s.now(), Decision: decision},
		func(ctx context.Context, tx pgx.Tx, rr *domain.PasswordResetRequest) error {
			issued = randomToken()
			return s.store.InsertCredentialTokenTx(ctx, tx, domain.CredentialToken{TokenHash: hashToken(issued),
				TenantID: rr.TenantID, PrincipalID: rr.PrincipalID, Purpose: domain.CredentialTokenReset,
				RequestID: rr.RequestID, ExpiresAt: s.now().Add(s.cfg.ResetTTL)})
		}, rec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if out != nil && out.Status == "ISSUED" && issued != "" {
		if s.notifier == nil {
			return out, ErrNotifierUnconfigured
		}
		if err := s.notifier.Notify(ctx, Notice{TenantID: out.TenantID, PrincipalID: out.PrincipalID, Intent: s.cfg.IntentReset,
			Link: s.linkFor(domain.CredentialTokenReset, issued), ExpiresAt: s.now().Add(s.cfg.ResetTTL),
			SourceEventID: "reset-" + out.RequestID, CorrelationID: a.CorrelationID}); err != nil {
			s.log.Warn("approved reset link not delivered", zap.String("request_id", requestID), zap.Error(err))
		}
	}
	return out, nil
}

// GetReset reads a reset request.
func (s *Service) GetReset(ctx context.Context, a Actor, requestID string) (*domain.PasswordResetRequest, error) {
	r, err := s.store.GetResetRequest(ctx, a.TenantID, requestID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNotFound
	}
	return r, nil
}

// SetStatus suspends, disables or re-enables a subject. A disable or a
// suspension ends every session immediately (termination priority, IAM §17).
func (s *Service) SetStatus(ctx context.Context, a Actor, principalID string, status domain.PrincipalStatus, reason string) error {
	switch status {
	case domain.PrincipalStatusActive, domain.PrincipalStatusSuspended, domain.PrincipalStatusDisabled:
	default:
		return fmt.Errorf("%w: status must be ACTIVE, SUSPENDED or DISABLED", ErrInvalid)
	}
	if principalID == a.PrincipalID && status != domain.PrincipalStatusActive {
		return fmt.Errorf("%w: you cannot disable your own account", ErrForbidden)
	}
	payload := map[string]any{"principal_id": principalID, "tenant_id": a.TenantID, "new_status": string(status),
		"actor": a.PrincipalID, "reason": reason, "correlation_id": a.CorrelationID}
	recs := []outbox.Record{}
	for _, et := range eventsForStatus(status) {
		rec, err := events.Render(et, a.TenantID, "", a.PrincipalID, a.CorrelationID, principalID, payload)
		if err != nil {
			return err
		}
		recs = append(recs, rec)
	}
	changed, err := s.store.SetPrincipalLifecycle(ctx, a.TenantID, principalID, status, a.PrincipalID, reason, a.CorrelationID, recs...)
	if err != nil {
		return err
	}
	if changed && status != domain.PrincipalStatusActive && s.end != nil {
		if _, err := s.end(ctx, a.TenantID, principalID, domain.SessionEndDeprovisioned, a.PrincipalID, a.CorrelationID); err != nil {
			s.log.Error("sessions not ended after a disable", zap.String("principal_id", principalID), zap.Error(err))
		}
	}
	return nil
}

func eventsForStatus(st domain.PrincipalStatus) []string {
	switch st {
	case domain.PrincipalStatusActive:
		return []string{events.EventPrincipalStatusChanged, events.EventSubjectEnabled}
	default:
		return []string{events.EventPrincipalStatusChanged, events.EventSubjectDisabled}
	}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}
