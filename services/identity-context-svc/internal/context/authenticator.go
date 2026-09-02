package context

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/credential"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
)

// Sentinel errors for the authentication path — mapped to HTTP status codes in
// handler.go.
var (
	// ErrInvalidCredentials is the ONLY rejection an authentication client
	// ever sees. It covers four internally-distinct outcomes: no principal
	// with that email, a principal with no password, a locked account, and a
	// wrong password.
	//
	// They are deliberately indistinguishable on the wire. A client that can
	// tell them apart can enumerate every account on the platform, and then
	// discover which of those accounts it has already locked — turning a
	// guessing attempt into a reconnaissance tool and a denial-of-service
	// primitive. The distinction is preserved where it is actually needed:
	// in access_decision_log's decision_basis, in the emitted event's
	// failure_reason, and in the SIEM stream. An operator answering a support
	// call reads it there; the caller does not.
	ErrInvalidCredentials = errors.New("invalid credentials")

	// ErrAuthUnavailable means the attempt could not be decided — the store
	// was unreachable, or the stored digest could not be parsed.
	//
	// Kept separate from ErrInvalidCredentials so it surfaces as 503 rather
	// than 401. Reporting "we cannot tell" as "you are wrong" is the same
	// category error the console's 403-vs-503 handling exists to avoid: it
	// sends a user to reset a password that was never the problem.
	ErrAuthUnavailable = errors.New("authentication unavailable")

	// ErrAuthRequestInvalid means a mandatory field was absent.
	ErrAuthRequestInvalid = errors.New("tenant_id, email and password are all required")
)

// CredentialStore is the data-access contract for password material.
// Narrow by design: the authenticator has no reason to reach any other table,
// and a wider interface would make it possible for this path to read or write
// principal state it has no business touching.
type CredentialStore interface {
	// FindActiveCredentialByEmail returns the HUMAN principal owning email
	// within tenantID and its ACTIVE password credential.
	//
	// Returns domain.ErrPrincipalNotFound when no such principal exists, and
	// domain.ErrCredentialNotFound (with a non-nil principal) when one exists
	// but holds no password.
	FindActiveCredentialByEmail(ctx context.Context, email, tenantID string) (*domain.Principal, *domain.PrincipalCredential, error)

	// RecordAuthSuccess clears lockout state and appends evidence. A non-empty
	// newHash replaces the stored digest in the same transaction.
	RecordAuthSuccess(ctx context.Context, credentialID, principalID, tenantID, correlationID, newHash string) error

	// RecordAuthFailure atomically increments the failure counter, engages the
	// lock at maxAttempts, appends evidence, and reports the resulting state.
	RecordAuthFailure(ctx context.Context, credentialID, principalID, tenantID, correlationID string, maxAttempts int, lockDuration time.Duration) (locked bool, attempts int, err error)

	// RecordAuthDenied appends evidence for an attempt that never reached
	// password verification.
	RecordAuthDenied(ctx context.Context, principalID, tenantID, basis, correlationID string) error

	// UpsertPasswordCredential retires any active password row and inserts a
	// replacement. secretHash must already be a PHC-encoded digest.
	UpsertPasswordCredential(ctx context.Context, principalID, tenantID, secretHash, algorithm string) error
}

// TokenIssuer mints the bearer token /v1/context/resolve accepts.
type TokenIssuer interface {
	Issue(subject, tenantID string, mfaDone bool) (string, error)
	TTLSeconds() int
}

// AuthEventPublisher is the authenticator's slice of the event backbone.
// Separate from EventPublisher so adding authentication events did not force
// every existing resolver test double to grow two methods it never calls.
type AuthEventPublisher interface {
	PublishAuthenticationSucceeded(ctx context.Context, principalID, tenantID, correlationID string) error
	PublishAuthenticationFailed(ctx context.Context, subject, principalID, tenantID, correlationID, reason string) error
}

// LockoutPolicy bounds online password guessing.
//
// This is the only rate limit on the endpoint. It bounds guesses per account,
// not per source: a spray across many accounts at one guess each never trips
// it. That is a real and known limit — the mitigation is a gateway-level rate
// limit on /v1/authenticate, which belongs in GTRM's compiled routing config
// rather than in this service, since a per-process counter is useless the
// moment there are two replicas.
type LockoutPolicy struct {
	// MaxFailedAttempts is the count at which the lock engages.
	MaxFailedAttempts int
	// LockDuration is how long the lock holds. It expires on its own — a
	// permanent lock needs an administrator to unlock it, which turns every
	// mistyped password into a support ticket and, at scale, trains operators
	// to unlock accounts without checking who asked.
	LockDuration time.Duration
}

// Authenticator verifies a human's password and mints the bearer token that
// POST /v1/context/resolve exchanges for a signed identity envelope.
//
// It deliberately stops there. Authentication answers "is this who they say
// they are"; it does not answer "may they act", "on which entity", or "under
// what trust posture" — those are the resolver's six dimensions, evaluated
// against live state at resolve time. Fusing the two would mean a password
// alone yielded an envelope, and any role revoked between login and action
// would keep working until the token expired.
type Authenticator struct {
	log         *zap.Logger
	credentials CredentialStore
	hasher      *credential.Hasher
	issuer      TokenIssuer
	events      AuthEventPublisher
	siem        *siem.Client
	lockout     LockoutPolicy

	// wg tracks fire-and-forget event goroutines so Drain can wait on them,
	// matching Resolver's shutdown contract.
	wg sync.WaitGroup
}

// NewAuthenticator wires the authentication path.
func NewAuthenticator(
	log *zap.Logger,
	credentials CredentialStore,
	hasher *credential.Hasher,
	issuer TokenIssuer,
	events AuthEventPublisher,
	siemClient *siem.Client,
	lockout LockoutPolicy,
) *Authenticator {
	return &Authenticator{
		log:         log,
		credentials: credentials,
		hasher:      hasher,
		issuer:      issuer,
		events:      events,
		siem:        siemClient,
		lockout:     lockout,
	}
}

// Drain waits for in-flight event goroutines, bounded by ctx. Called from
// main.go after srv.Shutdown(), alongside Resolver.Drain.
func (a *Authenticator) Drain(ctx context.Context) error { return waitCtx(ctx, &a.wg) }

// Authenticate verifies a password and returns a short-lived bearer token.
//
// Every rejection path performs one argon2id derivation before returning, so
// an unknown email, an account with no password, a locked account and a wrong
// password all cost the same wall-clock time. Skipping the derivation on the
// paths where there is nothing to compare against would make the endpoint a
// user-enumeration oracle measurable with a stopwatch.
func (a *Authenticator) Authenticate(ctx context.Context, req domain.AuthenticateRequest) (*domain.AuthenticateResponse, error) {
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if req.TenantID == "" || email == "" || req.Password == "" {
		return nil, ErrAuthRequestInvalid
	}

	principal, cred, err := a.credentials.FindActiveCredentialByEmail(ctx, email, req.TenantID)
	switch {
	case errors.Is(err, domain.ErrPrincipalNotFound):
		// No principal, so nothing to compare and no row to write evidence
		// against — access_decision_log.principal_id carries a foreign key.
		// The attempt is still emitted and streamed, because a run of these
		// is what an enumeration sweep looks like.
		a.burnHash(req.Password)
		a.reject(ctx, email, "", req.TenantID, req.CorrelationID, "no_such_principal", siem.SeverityMedium)
		return nil, ErrInvalidCredentials

	case errors.Is(err, domain.ErrCredentialNotFound):
		a.burnHash(req.Password)
		a.denyWithEvidence(ctx, principal.PrincipalID, req.TenantID, "no_active_credential", req.CorrelationID)
		a.reject(ctx, email, principal.PrincipalID, req.TenantID, req.CorrelationID, "no_active_credential", siem.SeverityMedium)
		return nil, ErrInvalidCredentials

	case err != nil:
		a.log.Error("credential lookup failed — failing closed",
			zap.String("tenant_id", req.TenantID),
			zap.String("correlation_id", req.CorrelationID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: credential store: %v", ErrAuthUnavailable, err)
	}

	// A lock that has already expired is not a lock. Checking the timestamp
	// rather than a boolean column is what lets the lock lapse without a
	// sweeper job needing to clear it.
	if cred.LockedUntil != nil && cred.LockedUntil.After(time.Now().UTC()) {
		a.burnHash(req.Password)
		a.denyWithEvidence(ctx, principal.PrincipalID, req.TenantID, "account_locked", req.CorrelationID)
		a.reject(ctx, email, principal.PrincipalID, req.TenantID, req.CorrelationID, "account_locked", siem.SeverityHigh)
		a.log.Warn("authentication attempt against locked credential",
			zap.String("principal_id", principal.PrincipalID),
			zap.Time("locked_until", *cred.LockedUntil),
			zap.String("correlation_id", req.CorrelationID),
		)
		return nil, ErrInvalidCredentials
	}

	needsRehash, verifyErr := a.hasher.Verify(req.Password, cred.SecretHash)
	switch {
	case errors.Is(verifyErr, credential.ErrMalformedHash),
		errors.Is(verifyErr, credential.ErrUnsupportedAlgorithm):
		// The stored digest is broken, not the password. Answering 401 would
		// send this user to a password reset that cannot fix it, and would
		// bury an operational fault as routine login noise.
		a.log.Error("stored credential digest is unusable",
			zap.String("principal_id", principal.PrincipalID),
			zap.String("credential_id", cred.CredentialID),
			zap.String("algorithm", cred.Algorithm),
			zap.Error(verifyErr),
		)
		a.siem.Stream(ctx, req.TenantID, "credential.digest_unusable", siem.SeverityHigh,
			fmt.Sprintf("Stored credential digest unusable for principal %s", principal.PrincipalID))
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, verifyErr)

	case verifyErr != nil:
		locked, attempts, recErr := a.credentials.RecordAuthFailure(
			ctx, cred.CredentialID, principal.PrincipalID, req.TenantID, req.CorrelationID,
			a.lockout.MaxFailedAttempts, a.lockout.LockDuration,
		)
		if recErr != nil {
			// The password was wrong either way, so the answer to the caller
			// does not change. Log loudly: a lockout counter that is not
			// incrementing means online guessing is unbounded.
			a.log.Error("failed to record authentication failure — lockout not advanced",
				zap.String("principal_id", principal.PrincipalID),
				zap.Error(recErr),
			)
		}

		reason, severity := "password_mismatch", siem.SeverityMedium
		if locked {
			reason, severity = "password_mismatch_account_locked", siem.SeverityHigh
		}
		a.reject(ctx, email, principal.PrincipalID, req.TenantID, req.CorrelationID, reason, severity)
		a.log.Warn("authentication failed",
			zap.String("principal_id", principal.PrincipalID),
			zap.Int("failed_attempts", attempts),
			zap.Bool("locked", locked),
			zap.String("correlation_id", req.CorrelationID),
		)
		return nil, ErrInvalidCredentials
	}

	// ── Verified ────────────────────────────────────────────────────────────

	// Upgrade the digest in place if it was produced under weaker parameters
	// than the service now uses. This is the only moment the plaintext exists,
	// so a cost-factor raise propagates as people log in, instead of needing
	// an estate-wide forced reset.
	var newHash string
	if needsRehash {
		upgraded, hashErr := a.hasher.Hash(req.Password)
		if hashErr != nil {
			// Not fatal: the credential verified. Leaving it at the older
			// parameters is worse than nothing but far better than refusing a
			// correct password over a housekeeping failure.
			a.log.Error("failed to rehash credential at current parameters",
				zap.String("principal_id", principal.PrincipalID),
				zap.Error(hashErr),
			)
		} else {
			newHash = upgraded
		}
	}

	if err := a.credentials.RecordAuthSuccess(
		ctx, cred.CredentialID, principal.PrincipalID, req.TenantID, req.CorrelationID, newHash,
	); err != nil {
		// Fail closed. The evidence write and the lockout reset share this
		// transaction, so a failure here means both were lost: the attempt is
		// absent from access_decision_log and the failure counter still holds
		// this principal's earlier misses. Issuing a token anyway would mean
		// an unlogged login, and would leave the account closer to locking
		// after a success that should have cleared it.
		a.log.Error("failed to record successful authentication — refusing to issue token",
			zap.String("principal_id", principal.PrincipalID),
			zap.String("correlation_id", req.CorrelationID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: evidence write: %v", ErrAuthUnavailable, err)
	}

	// The token asserts the IdP subject, not the principal ID: VerifyBearer
	// hands its sub claim to FindByIDPSubject, which looks up
	// identity_provider_subject. Sending principal_id here would resolve to
	// nothing and every login would 401 one step later.
	//
	// mfaDone is false unconditionally. A password is one factor, and no
	// step-up challenge exists anywhere in the estate to have supplied a
	// second. Passing true would lift the resolved trust posture to
	// MFA_VERIFIED on the strength of a password alone.
	token, err := a.issuer.Issue(principal.IdentityProviderSubject, principal.TenantID, false)
	if err != nil {
		a.log.Error("failed to mint bearer token after successful verification",
			zap.String("principal_id", principal.PrincipalID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: token issuance: %v", ErrAuthUnavailable, err)
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := detach(ctx)
		defer cancel()
		if err := a.events.PublishAuthenticationSucceeded(ctx, principal.PrincipalID, principal.TenantID, req.CorrelationID); err != nil {
			a.log.Error("event publish failed",
				zap.String("event_type", "identity.authentication.succeeded"),
				zap.String("principal_id", principal.PrincipalID),
				zap.Error(err),
			)
		}
	}()

	a.log.Info("identity.authentication.succeeded",
		zap.String("principal_id", principal.PrincipalID),
		zap.String("tenant_id", principal.TenantID),
		zap.Bool("credential_rehashed", newHash != ""),
		zap.String("correlation_id", req.CorrelationID),
	)

	// MFARequired is false because it is currently true of nothing. No step-up
	// factor exists anywhere in the estate — mfaDone is passed false above for
	// that same reason — so a client honouring `true` would prompt for a second
	// factor it cannot obtain and could never complete a login.
	//
	// The field stays in the response rather than being removed: it is the slot
	// a real factor reports through, and clients already read it. When TOTP or
	// WebAuthn lands, this becomes a per-principal answer sourced from that
	// factor's enrolment state, and MFA_VERIFIED trust posture becomes
	// reachable for the first time.
	return &domain.AuthenticateResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   a.issuer.TTLSeconds(),
		PrincipalID: principal.PrincipalID,
		TenantID:    principal.TenantID,
		MFARequired: false,
	}, nil
}

// SetPassword hashes and stores a password for a principal.
//
// Exposed for the seeding and rotation paths so that no caller anywhere else
// needs to know the hashing parameters, and so a plaintext password never
// reaches SQL. The store rejects anything that is not a PHC digest as a
// backstop.
func (a *Authenticator) SetPassword(ctx context.Context, principalID, tenantID, password string) error {
	if principalID == "" || tenantID == "" || password == "" {
		return ErrAuthRequestInvalid
	}
	hash, err := a.hasher.Hash(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	return a.credentials.UpsertPasswordCredential(ctx, principalID, tenantID, hash, "argon2id")
}

// ── Private helpers ──────────────────────────────────────────────────────────

// burnHash performs one derivation whose result is discarded, so a rejection
// that never reached a stored digest still costs what a real comparison costs.
// Passing an empty stored hash routes Verify to its precomputed decoy.
func (a *Authenticator) burnHash(password string) {
	_, _ = a.hasher.Verify(password, "")
}

// denyWithEvidence appends an append-only access_decision_log row. Best-effort
// by necessity — the caller is already on a rejection path and has nothing
// better to return if the write fails — but the failure is logged, because a
// silently missing evidence row is worse than a noisy one.
func (a *Authenticator) denyWithEvidence(ctx context.Context, principalID, tenantID, basis, correlationID string) {
	if err := a.credentials.RecordAuthDenied(ctx, principalID, tenantID, basis, correlationID); err != nil {
		a.log.Error("failed to append authentication denial to access_decision_log",
			zap.String("principal_id", principalID),
			zap.String("basis", basis),
			zap.Error(err),
		)
	}
}

// reject emits the failure event and streams to SIEM.
//
// Doc 05 §13.2 names authentication failures as a required SIEM signal, and
// gateway-auth-svc already streams its own. Streaming failures but not
// successes matches how authorization-svc treats DENIED: the volume of
// successful logins would drown the signal that matters.
func (a *Authenticator) reject(ctx context.Context, subject, principalID, tenantID, correlationID, reason string, severity siem.Severity) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := detach(ctx)
		defer cancel()
		if err := a.events.PublishAuthenticationFailed(ctx, subject, principalID, tenantID, correlationID, reason); err != nil {
			a.log.Error("event publish failed",
				zap.String("event_type", "identity.authentication.failed"),
				zap.String("failure_reason", reason),
				zap.Error(err),
			)
		}
		a.siem.Stream(ctx, tenantID, "identity.authentication.failed", severity,
			fmt.Sprintf("Authentication failed (%s) for subject %s", reason, subject))
	}()
}
