package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// Credential lookup and lockout bookkeeping.
//
// Every method here runs inside withRLS: principal_credentials has FORCE ROW
// LEVEL SECURITY and an unscoped connection reads zero rows. That is the
// intended failure mode — a query that forgets its tenant returns nothing
// rather than every tenant's password digests.

// FindActiveCredentialByEmail resolves the email a human typed to the
// principal that owns it and that principal's live password digest.
//
// Returns (nil, nil, domain.ErrPrincipalNotFound) when the email matches no
// HUMAN principal in the tenant, and (principal, nil, domain.ErrCredentialNotFound)
// when the principal exists but holds no ACTIVE password row. The caller must
// collapse both into one client-visible answer — the distinction is for the
// audit trail, not for the wire.
//
// Only HUMAN principals are considered. A SERVICE_ACCOUNT or API_CLIENT
// authenticates by workload credential, not by a password typed into a form,
// and letting one match here would put a machine identity behind a login page.
//
// Status is filtered to ACTIVE rather than "not DISABLED": FindByIDPSubject
// admits SUSPENDED because a suspended principal's context may still need
// resolving for an administrator to act on, but a suspended principal must not
// be able to authenticate itself back in.
func (s *PgStore) FindActiveCredentialByEmail(
	ctx context.Context,
	email, tenantID string,
) (*domain.Principal, *domain.PrincipalCredential, error) {
	if tenantID == "" {
		// withRLS would scope to nothing and the caller would read this as
		// "no such user". Refusing loudly keeps a missing tenant a bug rather
		// than a silent authentication failure nobody can explain.
		return nil, nil, errors.New("tenant_id is required for credential lookup")
	}

	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return nil, nil, domain.ErrPrincipalNotFound
	}

	const query = `
		SELECT p.principal_id, p.tenant_id, p.principal_type, p.identity_provider_subject,
		       p.email, p.display_name, p.status, p.created_at, p.data_classification,
		       c.credential_id, c.credential_type, c.secret_hash, c.algorithm, c.status,
		       c.failed_attempt_count, c.locked_until, c.last_authenticated_at
		FROM principals p
		LEFT JOIN principal_credentials c
		       ON c.principal_id = p.principal_id
		      AND c.tenant_id    = p.tenant_id
		      AND c.credential_type = $3
		      AND c.status = 'ACTIVE'
		WHERE p.tenant_id = $1
		  AND LOWER(p.email) = $2
		  AND p.principal_type = 'HUMAN'
		  AND p.status = 'ACTIVE'
	`

	var (
		principal  *domain.Principal
		credential *domain.PrincipalCredential
	)

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var (
			p         domain.Principal
			credID    *string
			credType  *string
			credHash  *string
			credAlgo  *string
			credStat  *string
			failCount *int
			lockedTil *time.Time
			lastAuth  *time.Time
		)
		row := tx.QueryRow(ctx, query, tenantID, normalized, domain.CredentialTypePassword)
		if err := row.Scan(
			&p.PrincipalID, &p.TenantID, &p.PrincipalType, &p.IdentityProviderSubject,
			&p.Email, &p.DisplayName, &p.Status, &p.CreatedAt, &p.DataClassification,
			&credID, &credType, &credHash, &credAlgo, &credStat,
			&failCount, &lockedTil, &lastAuth,
		); err != nil {
			return err
		}

		principal = &p
		// A NULL credential_id means the LEFT JOIN found no active row: the
		// principal is real but has no password. Reported distinctly to the
		// caller (which logs it) and identically to the client.
		if credID == nil {
			return domain.ErrCredentialNotFound
		}
		credential = &domain.PrincipalCredential{
			CredentialID:        *credID,
			PrincipalID:         p.PrincipalID,
			TenantID:            p.TenantID,
			CredentialType:      deref(credType),
			SecretHash:          deref(credHash),
			Algorithm:           deref(credAlgo),
			Status:              deref(credStat),
			FailedAttemptCount:  derefInt(failCount),
			LockedUntil:         lockedTil,
			LastAuthenticatedAt: lastAuth,
		}
		return nil
	})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil, domain.ErrPrincipalNotFound
	case errors.Is(err, domain.ErrCredentialNotFound):
		return principal, nil, domain.ErrCredentialNotFound
	case err != nil:
		return nil, nil, err
	}
	return principal, credential, nil
}

// RecordAuthSuccess clears the lockout counters, stamps last_authenticated_at,
// and appends an evidence row.
//
// newHash is optional: when the digest verified under weaker parameters than
// the service now uses, the caller passes an upgraded digest and it is written
// in the same transaction. Rehashing on successful login is the only moment
// the plaintext is in hand, so a parameter raise propagates as users return
// rather than needing a forced estate-wide reset.
func (s *PgStore) RecordAuthSuccess(
	ctx context.Context,
	credentialID, principalID, tenantID, correlationID, newHash string,
) error {
	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if newHash != "" {
			if _, err := tx.Exec(ctx, `
				UPDATE principal_credentials
				SET secret_hash = $1, secret_updated_at = $2,
				    failed_attempt_count = 0, locked_until = NULL,
				    last_authenticated_at = $2, updated_at = $2
				WHERE credential_id = $3 AND tenant_id = $4
			`, newHash, now, credentialID, tenantID); err != nil {
				return fmt.Errorf("rehash credential: %w", err)
			}
		} else if _, err := tx.Exec(ctx, `
			UPDATE principal_credentials
			SET failed_attempt_count = 0, locked_until = NULL,
			    last_authenticated_at = $1, updated_at = $1
			WHERE credential_id = $2 AND tenant_id = $3
		`, now, credentialID, tenantID); err != nil {
			return fmt.Errorf("clear lockout state: %w", err)
		}

		return s.appendDecision(ctx, tx, principalID, tenantID, "AUTHENTICATE", "ALLOWED", "password_verified", correlationID, now)
	})
}

// RecordAuthFailure increments the failure counter and engages the lock when
// the threshold is reached, in one atomic statement.
//
// The increment is computed in SQL rather than read-modify-written in Go: two
// concurrent guesses that both read count=4 would each write 5 and the account
// would never lock, which is exactly the race a parallel guessing attack
// produces. Returning the post-increment state lets the caller log whether
// this attempt was the one that locked the account.
func (s *PgStore) RecordAuthFailure(
	ctx context.Context,
	credentialID, principalID, tenantID, correlationID string,
	maxAttempts int,
	lockDuration time.Duration,
) (locked bool, attempts int, err error) {
	now := time.Now().UTC()
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// A lock whose window has already passed is treated as absent, so the
		// counter restarts from this attempt rather than resuming a run the
		// user already served the penalty for.
		//
		// locked_until is set to NULL whenever the new count is below the
		// threshold. That clearing is load-bearing, not tidiness: leaving an
		// expired timestamp in place would make the next failure match the
		// "lock has expired" branch again and reset the count to 1 a second
		// time, pinning it at 1 forever and disabling lockout entirely for
		// that credential. An account only reaches this statement when it is
		// not currently locked — the authenticator short-circuits a live lock
		// before verification — so there is never a real lock here to erase.
		//
		// The next count is computed once in a CTE rather than repeated in two
		// CASE expressions, so the value driving the threshold comparison is
		// necessarily the same one being written.
		row := tx.QueryRow(ctx, `
			WITH computed AS (
			    SELECT credential_id,
			           CASE
			               WHEN locked_until IS NOT NULL AND locked_until <= $1 THEN 1
			               ELSE failed_attempt_count + 1
			           END AS next_count
			    FROM principal_credentials
			    WHERE credential_id = $4 AND tenant_id = $5
			)
			UPDATE principal_credentials c
			SET failed_attempt_count = computed.next_count,
			    locked_until = CASE WHEN computed.next_count >= $2 THEN $3 ELSE NULL END,
			    updated_at = $1
			FROM computed
			WHERE c.credential_id = computed.credential_id AND c.tenant_id = $5
			RETURNING c.failed_attempt_count, (c.locked_until IS NOT NULL)
		`, now, maxAttempts, now.Add(lockDuration), credentialID, tenantID)

		if scanErr := row.Scan(&attempts, &locked); scanErr != nil {
			return fmt.Errorf("record failed attempt: %w", scanErr)
		}

		outcome, basis := "DENIED", "password_mismatch"
		if locked {
			basis = "password_mismatch_account_locked"
		}
		return s.appendDecision(ctx, tx, principalID, tenantID, "AUTHENTICATE", outcome, basis, correlationID, now)
	})
	return locked, attempts, err
}

// RecordAuthDenied appends an evidence row for an attempt that never reached
// password verification — a locked account, or a principal with no credential.
//
// It takes no credential_id because there may not be one. Separate from
// RecordAuthFailure so a lockout does not extend its own window every time the
// locked-out user retries, which would turn a 15-minute lock into a permanent
// one for anyone who keeps trying.
func (s *PgStore) RecordAuthDenied(
	ctx context.Context,
	principalID, tenantID, basis, correlationID string,
) error {
	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return s.appendDecision(ctx, tx, principalID, tenantID, "AUTHENTICATE", "DENIED", basis, correlationID, now)
	})
}

// UpsertPasswordCredential sets a principal's password, retiring any existing
// active row first.
//
// Rotation is retire-then-insert rather than an UPDATE so the superseded
// digest survives as evidence of when the credential changed. The partial
// unique index on (principal_id, credential_type) WHERE status = 'ACTIVE'
// permits the retired rows to accumulate while still allowing only one live
// credential.
//
// secretHash must already be a PHC-encoded digest — this method never sees a
// plaintext password, so there is no path by which one is written to the
// column by mistake.
func (s *PgStore) UpsertPasswordCredential(
	ctx context.Context,
	principalID, tenantID, secretHash, algorithm string,
) error {
	if secretHash == "" {
		return errors.New("secret hash is required")
	}
	if !strings.HasPrefix(secretHash, "$argon2id$") {
		// Cheap structural check, not a validation of the digest. It exists to
		// catch the one catastrophic caller error — passing the plaintext.
		return errors.New("secret hash is not a PHC-encoded argon2id digest")
	}

	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE principal_credentials
			SET status = 'RETIRED', updated_at = $1
			WHERE principal_id = $2 AND tenant_id = $3
			  AND credential_type = $4 AND status = 'ACTIVE'
		`, now, principalID, tenantID, domain.CredentialTypePassword); err != nil {
			return fmt.Errorf("retire existing credential: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO principal_credentials
				(credential_id, principal_id, tenant_id, credential_type, secret_hash,
				 algorithm, status, failed_attempt_count, locked_until,
				 secret_updated_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'ACTIVE', 0, NULL, $7, $7, $7)
		`, ulid.Make().String(), principalID, tenantID, domain.CredentialTypePassword,
			secretHash, algorithm, now); err != nil {
			return fmt.Errorf("insert credential: %w", err)
		}

		s.log.Info("store.UpsertPasswordCredential",
			zap.String("principal_id", principalID),
			zap.String("tenant_id", tenantID),
			zap.String("algorithm", algorithm),
		)
		return nil
	})
}

// appendDecision writes one append-only row to access_decision_log, the same
// evidence table UpdateStatus writes to.
//
// Authentication attempts belong here rather than in a table of their own:
// §14.1's obligation is that a governed action be reconstructable by actor and
// time, and "who tried to log in as whom, and was it allowed" is the first
// link in every other chain in the log.
//
// A failed attempt against an email matching no principal writes nothing —
// principal_id is NOT NULL and carries a foreign key, so there is no row to
// point at. Those attempts are observable as events and SIEM signals instead;
// see the authenticator.
func (s *PgStore) appendDecision(
	ctx context.Context,
	tx pgx.Tx,
	principalID, tenantID, actionType, outcome, basis, correlationID string,
	at time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO access_decision_log
			(decision_log_id, principal_id, tenant_id, action_type,
			 decision_outcome, decision_basis, correlation_id, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, ulid.Make().String(), principalID, tenantID, actionType, outcome, basis, correlationID, at); err != nil {
		return fmt.Errorf("append access decision: %w", err)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}
