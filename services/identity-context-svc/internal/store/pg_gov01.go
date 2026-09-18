package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// ---------------------------------------------------------------------------
// Session context + outbox, atomically
// ---------------------------------------------------------------------------

// InsertSessionContextWithEvent writes the session evidence row and its
// domain event in ONE transaction.
//
// This is the transactional outbox at the only place in this service where
// atomicity between a business write and its evidence genuinely matters. The
// two failure modes it removes are both states an audit cannot explain:
//
//   - a session_contexts row with no identity.context.resolved event, so an
//     envelope was issued that the estate never heard about;
//   - an identity.context.resolved event for a session that failed to
//     persist, so consumers act on a session with no evidence behind it.
//
// Before this, the event was published from a goroutine holding a context
// detached from the request, after the row was written on a separate call
// that was logged-and-swallowed on failure. Either half could fail alone.
func (s *PgStore) InsertSessionContextWithEvent(
	ctx context.Context,
	sc domain.SessionContext,
	rec outbox.Record,
) error {
	if sc.TenantID == "" {
		return errors.New("InsertSessionContextWithEvent: tenant_id is required")
	}
	return s.withRLS(ctx, sc.TenantID, func(tx pgx.Tx) error {
		if err := insertSessionContextTx(ctx, tx, sc); err != nil {
			return err
		}
		if rec.EventID == "" {
			// No event asked for. Legitimate for a caller that only wants the
			// row — but not silently: every production path supplies one.
			return nil
		}
		return s.outboxEnqueueTx(ctx, tx, rec)
	})
}

// outboxEnqueueTx is the outbox insert, inlined here so the store does not
// depend on an outbox.Store instance just to write one row inside a
// transaction it already holds.
func (s *PgStore) outboxEnqueueTx(ctx context.Context, tx pgx.Tx, rec outbox.Record) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO event_outbox (event_id, event_type, tenant_id, partition_key, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (event_id) DO NOTHING`,
		rec.EventID, rec.EventType, rec.TenantID, rec.PartitionKey, rec.Payload,
	)
	if err != nil {
		return fmt.Errorf("enqueue outbox event %q: %w", rec.EventType, err)
	}
	return nil
}

// insertSessionContextTx is the shared INSERT used by both the plain and the
// transactional entry points, so the column list cannot drift between them.
func insertSessionContextTx(ctx context.Context, tx pgx.Tx, sc domain.SessionContext) error {
	var legalEntityID *string
	if sc.LegalEntityID != "" {
		legalEntityID = &sc.LegalEntityID
	}
	environment := sc.Environment
	if environment == "" {
		environment = domain.EnvironmentLocal
	}
	ingress := sc.IngressSource
	if ingress == "" {
		ingress = domain.IngressUnknown
	}
	retentionClass := sc.RetentionClass
	if retentionClass == "" {
		retentionClass = domain.RetentionClassSessionEvidence
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO session_contexts (
			session_context_id, principal_id, tenant_id, legal_entity_id,
			correlation_id, trust_posture, mfa_verified, device_trust_score,
			adaptive_risk_score, risk_signal_source, envelope_jwt_jti,
			issued_at, expires_at, data_residency_policy_id,
			source_service, schema_version,
			ingress_source, environment, evidence_id, support_context_id,
			retention_class, disposition_due_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (session_context_id) DO NOTHING`,
		sc.SessionContextID, sc.PrincipalID, sc.TenantID, legalEntityID,
		sc.CorrelationID, string(sc.TrustPosture), sc.MFAVerified, sc.DeviceTrustScore,
		sc.AdaptiveRiskScore, sc.RiskSignalSource, sc.EnvelopeJWTJTI,
		sc.IssuedAt, sc.ExpiresAt, sc.DataResidencyPolicyID,
		sc.SourceService, sc.SchemaVersion,
		ingress, string(environment), sc.EvidenceID, sc.SupportContextID,
		retentionClass, sc.DispositionDueAt,
	)
	if err != nil {
		return fmt.Errorf("insert session_context: %w", err)
	}
	return nil
}

// FindLiveSessionIDsForTenant returns every unexpired, uninvalidated session in
// a tenant. It backs InvalidateTenantContext.
//
// Returns ids only, like its per-principal sibling: the caller is revoking.
func (s *PgStore) FindLiveSessionIDsForTenant(
	ctx context.Context,
	tenantID string,
	now time.Time,
) ([]string, error) {
	if tenantID == "" {
		return nil, errors.New("FindLiveSessionIDsForTenant: tenant_id is required")
	}
	var ids []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT session_context_id
			  FROM session_contexts
			 WHERE tenant_id = $1
			   AND invalidated_at IS NULL
			   AND expires_at > $2`,
			tenantID, now,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("find live sessions for tenant: %w", err)
	}
	return ids, nil
}

// ---------------------------------------------------------------------------
// Tenant ingress bindings  (TenantRoutingHint cache)
// ---------------------------------------------------------------------------

// FindIngressBinding resolves a canonical ingress identifier to its tenant.
// Returns (nil, nil) when the identifier is unknown.
//
// NO withRLS, deliberately. This table has no row-level policy — see migration
// 000007 — because it is read BEFORE a tenant is resolved and a tenant-scoped
// policy would require the answer as its own input.
//
// The identifier is lowercased here rather than trusted from the caller. The
// column carries a CHECK that it is already lowercase, so a mixed-case lookup
// would silently miss on a row that exists, and a miss on this table means a
// refusal.
func (s *PgStore) FindIngressBinding(ctx context.Context, identifier string) (*domain.TenantIngressBinding, error) {
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	if identifier == "" {
		return nil, nil
	}
	var b domain.TenantIngressBinding
	var env string
	err := s.pool.QueryRow(ctx, `
		SELECT ingress_identifier, tenant_id, environment, active_flag,
		       source_version, refreshed_at
		  FROM tenant_ingress_bindings
		 WHERE ingress_identifier = $1 AND active_flag`,
		identifier,
	).Scan(&b.IngressIdentifier, &b.TenantID, &env, &b.ActiveFlag,
		&b.SourceVersion, &b.RefreshedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find ingress binding: %w", err)
	}
	b.Environment = domain.Environment(env)
	return &b, nil
}

// UpsertIngressBinding writes or refreshes one binding.
//
// Used by the registry sync and by seed tooling. Not reachable from any HTTP
// route: a caller that could write here could bind its own hostname to
// somebody else's tenant, which is the exact attack the table defends against.
func (s *PgStore) UpsertIngressBinding(ctx context.Context, b domain.TenantIngressBinding) error {
	identifier := strings.ToLower(strings.TrimSpace(b.IngressIdentifier))
	if identifier == "" || b.TenantID == "" {
		return errors.New("UpsertIngressBinding: ingress_identifier and tenant_id are required")
	}
	if !b.Environment.Valid() {
		return fmt.Errorf("UpsertIngressBinding: invalid environment %q", b.Environment)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_ingress_bindings
		    (ingress_identifier, tenant_id, environment, active_flag, source_version, refreshed_at)
		VALUES ($1,$2,$3,$4,$5,NOW())
		ON CONFLICT (ingress_identifier) DO UPDATE
		   SET tenant_id      = EXCLUDED.tenant_id,
		       environment    = EXCLUDED.environment,
		       active_flag    = EXCLUDED.active_flag,
		       source_version = EXCLUDED.source_version,
		       refreshed_at   = NOW()`,
		identifier, b.TenantID, string(b.Environment), b.ActiveFlag, b.SourceVersion,
	)
	if err != nil {
		return fmt.Errorf("upsert ingress binding: %w", err)
	}
	return nil
}

// TouchIngressBindings marks a tenant's bindings stale so the next resolution
// re-reads them, and reports how many were affected.
//
// This is what RefreshTenantContextCache actually does. It does NOT delete the
// rows: a deleted binding would make every request on that hostname fail
// closed until the registry sync ran again, turning a cache refresh into an
// outage. Setting refreshed_at to the epoch marks them stale instead, and the
// resolver revalidates a stale binding against the registry on next use.
func (s *PgStore) TouchIngressBindings(ctx context.Context, tenantID string, identifiers []string) (int, error) {
	if tenantID == "" {
		return 0, errors.New("TouchIngressBindings: tenant_id is required")
	}

	if len(identifiers) == 0 {
		tag, err := s.pool.Exec(ctx, `
			UPDATE tenant_ingress_bindings
			   SET refreshed_at = 'epoch'::timestamptz
			 WHERE tenant_id = $1`, tenantID)
		if err != nil {
			return 0, fmt.Errorf("touch ingress bindings: %w", err)
		}
		return int(tag.RowsAffected()), nil
	}

	lowered := make([]string, 0, len(identifiers))
	for _, id := range identifiers {
		lowered = append(lowered, strings.ToLower(strings.TrimSpace(id)))
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenant_ingress_bindings
		   SET refreshed_at = 'epoch'::timestamptz
		 WHERE tenant_id = $1 AND ingress_identifier = ANY($2)`, tenantID, lowered)
	if err != nil {
		return 0, fmt.Errorf("touch ingress bindings: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------
// Support contexts  (AttachSupportContext)
// ---------------------------------------------------------------------------

// InsertSupportContextWithEvent writes the grant and its event atomically.
//
// Same reasoning as the session path, with more at stake: a support context
// that exists with no SupportContextAttached event is a privileged elevation
// the security team was never told about.
func (s *PgStore) InsertSupportContextWithEvent(
	ctx context.Context,
	sc domain.SupportContext,
	rec outbox.Record,
) error {
	if sc.TenantID == "" {
		return errors.New("InsertSupportContextWithEvent: tenant_id is required")
	}
	if sc.ApproverPrincipalID == sc.SupportPrincipalID {
		// Also a schema CHECK. Refused here too so the error is this one
		// rather than a constraint violation, which a caller cannot act on.
		return domain.ErrSupportSelfApproval
	}
	return s.withRLS(ctx, sc.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO support_contexts (
				support_context_id, tenant_id, support_principal_id, subject_principal_id,
				reason_code, justification, ticket_ref, approver_principal_id,
				granted_at, expires_at, evidence_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			sc.SupportContextID, sc.TenantID, sc.SupportPrincipalID, sc.SubjectPrincipalID,
			sc.ReasonCode, sc.Justification, sc.TicketRef, sc.ApproverPrincipalID,
			sc.GrantedAt, sc.ExpiresAt, sc.EvidenceID, sc.CorrelationID,
		)
		if err != nil {
			return fmt.Errorf("insert support_context: %w", err)
		}
		if rec.EventID == "" {
			return nil
		}
		return s.outboxEnqueueTx(ctx, tx, rec)
	})
}

// FindSupportContext reads one grant within the caller's tenant.
// Returns (nil, nil) when absent or belonging to another tenant.
func (s *PgStore) FindSupportContext(ctx context.Context, supportContextID, tenantID string) (*domain.SupportContext, error) {
	if tenantID == "" {
		return nil, errors.New("FindSupportContext: tenant_id is required")
	}
	var sc domain.SupportContext
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanSupportContext(tx.QueryRow(ctx, supportContextColumns+`
			 WHERE support_context_id = $1 AND tenant_id = $2`,
			supportContextID, tenantID), &sc)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find support_context: %w", err)
	}
	return &sc, nil
}

// FindLiveSupportContext returns the support principal's usable grant in a
// tenant at instant `at`, or (nil, nil) when there is none.
//
// "Usable" is all three of granted, unrevoked and unexpired, enforced in SQL
// rather than by filtering in Go, so a caller cannot forget one of the three.
// Most recent grant wins when several overlap.
func (s *PgStore) FindLiveSupportContext(
	ctx context.Context,
	supportPrincipalID, tenantID string,
	at time.Time,
) (*domain.SupportContext, error) {
	if tenantID == "" {
		return nil, errors.New("FindLiveSupportContext: tenant_id is required")
	}
	var sc domain.SupportContext
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanSupportContext(tx.QueryRow(ctx, supportContextColumns+`
			 WHERE support_principal_id = $1
			   AND tenant_id            = $2
			   AND revoked_at IS NULL
			   AND granted_at <= $3
			   AND expires_at  > $3
			 ORDER BY granted_at DESC
			 LIMIT 1`,
			supportPrincipalID, tenantID, at), &sc)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find live support_context: %w", err)
	}
	return &sc, nil
}

// RevokeSupportContextWithEvent ends a grant early, atomically with its event.
//
// The WHERE carries `revoked_at IS NULL`, so the first revocation wins and a
// later one is a no-op — same append-only doctrine as session invalidation.
// Reports whether a row actually changed, so the caller can answer idempotently
// rather than claiming to have revoked something already revoked.
func (s *PgStore) RevokeSupportContextWithEvent(
	ctx context.Context,
	supportContextID, tenantID, reason string,
	at time.Time,
	rec outbox.Record,
) (bool, error) {
	if tenantID == "" {
		return false, errors.New("RevokeSupportContextWithEvent: tenant_id is required")
	}
	var changed bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE support_contexts
			   SET revoked_at = $1, revocation_reason = $2
			 WHERE support_context_id = $3
			   AND tenant_id = $4
			   AND revoked_at IS NULL`,
			at, reason, supportContextID, tenantID)
		if err != nil {
			return fmt.Errorf("revoke support_context: %w", err)
		}
		changed = tag.RowsAffected() > 0
		if !changed || rec.EventID == "" {
			return nil
		}
		return s.outboxEnqueueTx(ctx, tx, rec)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// FindUnreviewedExpiredSupportContexts returns grants that have ended and
// never been reviewed.
//
// This is the "followed by reconciliation/review" half of the break-glass
// invariant, and it is the half that is normally missing: the grant expires,
// nobody looks, and the control is decorative. The reconciler reports these.
func (s *PgStore) FindUnreviewedExpiredSupportContexts(
	ctx context.Context,
	tenantID string,
	before time.Time,
	limit int,
) ([]domain.SupportContext, error) {
	if tenantID == "" {
		return nil, errors.New("FindUnreviewedExpiredSupportContexts: tenant_id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	var out []domain.SupportContext
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, supportContextColumns+`
			 WHERE tenant_id = $1
			   AND reviewed_at IS NULL
			   AND (expires_at <= $2 OR revoked_at IS NOT NULL)
			 ORDER BY expires_at
			 LIMIT $3`, tenantID, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sc domain.SupportContext
			if err := scanSupportContext(rows, &sc); err != nil {
				return err
			}
			out = append(out, sc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("find unreviewed support contexts: %w", err)
	}
	return out, nil
}

// MarkSupportContextReviewed records the reconciliation.
func (s *PgStore) MarkSupportContextReviewed(
	ctx context.Context,
	supportContextID, tenantID, reviewer string,
	at time.Time,
) error {
	if tenantID == "" || reviewer == "" {
		return errors.New("MarkSupportContextReviewed: tenant_id and reviewer are required")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE support_contexts
			   SET reviewed_at = $1, reviewed_by = $2
			 WHERE support_context_id = $3 AND tenant_id = $4 AND reviewed_at IS NULL`,
			at, reviewer, supportContextID, tenantID)
		if err != nil {
			return fmt.Errorf("mark support_context reviewed: %w", err)
		}
		return nil
	})
}

// supportContextColumns is the shared SELECT list. One constant so the column
// order cannot drift out of step with scanSupportContext.
const supportContextColumns = `
	SELECT support_context_id, tenant_id, support_principal_id, subject_principal_id,
	       reason_code, justification, ticket_ref, approver_principal_id,
	       granted_at, expires_at, revoked_at, revocation_reason,
	       reviewed_at, reviewed_by, evidence_id, correlation_id
	  FROM support_contexts`

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSupportContext(r rowScanner, sc *domain.SupportContext) error {
	return r.Scan(
		&sc.SupportContextID, &sc.TenantID, &sc.SupportPrincipalID, &sc.SubjectPrincipalID,
		&sc.ReasonCode, &sc.Justification, &sc.TicketRef, &sc.ApproverPrincipalID,
		&sc.GrantedAt, &sc.ExpiresAt, &sc.RevokedAt, &sc.RevocationReason,
		&sc.ReviewedAt, &sc.ReviewedBy, &sc.EvidenceID, &sc.CorrelationID,
	)
}

// ---------------------------------------------------------------------------
// Legal hold projection  (READ MODEL of GOV-10)
// ---------------------------------------------------------------------------

// UpsertLegalHold projects an issued or rescoped hold.
//
// The last_event_at guard is what makes out-of-order delivery safe: an older
// event arriving after a newer one is ignored rather than applied. Without it,
// a redelivered LegalHoldIssued could resurrect a hold that was since
// released, and a stale HoldScopeChanged could silently widen or narrow one.
func (s *PgStore) UpsertLegalHold(ctx context.Context, hold domain.LegalHold) error {
	if hold.TenantID == "" || hold.HoldID == "" {
		return errors.New("UpsertLegalHold: hold_id and tenant_id are required")
	}
	return s.withRLS(ctx, hold.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO legal_hold_projection
			    (hold_id, tenant_id, matter_ref, principal_id, issued_at, last_event_id, last_event_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (hold_id) DO UPDATE
			   SET matter_ref    = EXCLUDED.matter_ref,
			       principal_id  = EXCLUDED.principal_id,
			       issued_at     = EXCLUDED.issued_at,
			       last_event_id = EXCLUDED.last_event_id,
			       last_event_at = EXCLUDED.last_event_at
			 WHERE legal_hold_projection.last_event_at <= EXCLUDED.last_event_at`,
			hold.HoldID, hold.TenantID, hold.MatterRef, hold.PrincipalID,
			hold.IssuedAt, hold.LastEventID, hold.LastEventAt,
		)
		if err != nil {
			return fmt.Errorf("upsert legal_hold_projection: %w", err)
		}
		return nil
	})
}

// ReleaseLegalHold marks a hold released so disposition may resume.
func (s *PgStore) ReleaseLegalHold(ctx context.Context, holdID, tenantID, eventID string, releasedAt time.Time) error {
	if tenantID == "" || holdID == "" {
		return errors.New("ReleaseLegalHold: hold_id and tenant_id are required")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE legal_hold_projection
			   SET released_at   = $1,
			       last_event_id = $2,
			       last_event_at = $1
			 WHERE hold_id = $3
			   AND tenant_id = $4
			   AND last_event_at <= $1`,
			releasedAt, eventID, holdID, tenantID)
		if err != nil {
			return fmt.Errorf("release legal_hold_projection: %w", err)
		}
		return nil
	})
}

// HasActiveLegalHold reports whether anything blocks disposing principalID's
// records in a tenant.
//
// A tenant-wide hold (principal_id IS NULL) blocks everything, so the
// predicate is "tenant-wide OR names this principal". Getting that backwards —
// checking only for a row naming the principal — would let a tenant-wide hold
// be ignored, which is the failure that deletes evidence during litigation.
func (s *PgStore) HasActiveLegalHold(ctx context.Context, tenantID, principalID string) (bool, error) {
	if tenantID == "" {
		return false, errors.New("HasActiveLegalHold: tenant_id is required")
	}
	var found bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM legal_hold_projection
			     WHERE tenant_id = $1
			       AND released_at IS NULL
			       AND (principal_id IS NULL OR principal_id = $2))`,
			tenantID, principalID).Scan(&found)
	})
	if err != nil {
		return false, fmt.Errorf("check legal hold: %w", err)
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Retention / disposition  (GOV-09)
// ---------------------------------------------------------------------------

// DisposableSession is one row the sweep is considering.
type DisposableSession struct {
	SessionContextID string
	PrincipalID      string
}

// FindDisposableSessions returns session rows whose retention period has
// elapsed and which have not yet been disposed.
//
// Legal holds are NOT filtered here. The sweep reads the candidates, then asks
// about holds per principal, and reports the held-back count separately —
// because "nothing was due" and "everything due was held" must not look the
// same in the evidence. A SQL-side anti-join would collapse them.
func (s *PgStore) FindDisposableSessions(
	ctx context.Context,
	tenantID string,
	due time.Time,
	limit int,
) ([]DisposableSession, error) {
	if tenantID == "" {
		return nil, errors.New("FindDisposableSessions: tenant_id is required")
	}
	if limit <= 0 {
		limit = 500
	}
	var out []DisposableSession
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT session_context_id, principal_id
			  FROM session_contexts
			 WHERE tenant_id = $1
			   AND disposed_at IS NULL
			   AND disposition_due_at IS NOT NULL
			   AND disposition_due_at <= $2
			 ORDER BY disposition_due_at
			 LIMIT $3`, tenantID, due, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d DisposableSession
			if err := rows.Scan(&d.SessionContextID, &d.PrincipalID); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("find disposable sessions: %w", err)
	}
	return out, nil
}

// DisposeSessions redacts the PII-bearing columns of the named sessions and
// stamps disposed_at.
//
// DISPOSITION IS REDACTION, NOT DELETION, and that is a deliberate choice
// rather than a half-measure. The row is the evidence that a session existed;
// deleting it would destroy the audit trail the retention policy exists to
// bound, and would break the foreign keys other evidence hangs off. What the
// retention period actually governs is the PERSONAL data — the correlation to
// a device, a risk score, an entity — so those are cleared and the skeleton of
// the decision remains.
func (s *PgStore) DisposeSessions(ctx context.Context, tenantID string, ids []string, at time.Time) (int, error) {
	if tenantID == "" {
		return 0, errors.New("DisposeSessions: tenant_id is required")
	}
	if len(ids) == 0 {
		return 0, nil
	}
	var n int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE session_contexts
			   SET disposed_at         = $1,
			       correlation_id      = 'DISPOSED',
			       risk_signal_source  = 'DISPOSED',
			       adaptive_risk_score = 0,
			       device_trust_score  = NULL,
			       ingress_source      = 'DISPOSED'
			 WHERE tenant_id = $2
			   AND session_context_id = ANY($3)
			   AND disposed_at IS NULL`,
			at, tenantID, ids)
		if err != nil {
			return fmt.Errorf("dispose sessions: %w", err)
		}
		n = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// PurgePublishedOutbox deletes outbox rows that were delivered before `before`.
//
// These carry no evidential weight of their own — the event is on the topic
// and the fact it attests is in its own table — so this is a true delete
// rather than a redaction. Without it the outbox grows without bound.
func (s *PgStore) PurgePublishedOutbox(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded on the commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return 0, fmt.Errorf("purge outbox: set relay scope: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM event_outbox
		 WHERE event_id IN (
		     SELECT event_id FROM event_outbox
		      WHERE published_at IS NOT NULL AND published_at < $1
		      LIMIT $2)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("purge outbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("purge outbox: commit: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// TenantsWithDisposableRecords lists the tenants that actually have rows due.
//
// Runs under app.retention_sweep, the named cross-tenant capability migration
// 000007 grants on exactly the two evidence tables this worker disposes. A
// sweep that had to be TOLD which tenants exist would silently skip any tenant
// nobody remembered to configure, and a retention obligation that applies only
// to remembered tenants is not one.
//
// This and withSweepScope below are the ONLY places the capability is set.
func (s *PgStore) TenantsWithDisposableRecords(ctx context.Context, due time.Time) ([]string, error) {
	var out []string
	err := s.withSweepScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT tenant_id
			  FROM session_contexts
			 WHERE disposed_at IS NULL
			   AND disposition_due_at IS NOT NULL
			   AND disposition_due_at <= $1`, due)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list tenants for disposition: %w", err)
	}
	return out, nil
}

// withSweepScope runs fn with the retention-sweep capability set.
//
// Deliberately NOT a general-purpose helper: it is unexported, it is called
// from one function, and widening its use is the thing a reviewer should
// object to. The capability grants cross-tenant visibility on session_contexts
// and access_decision_log and nothing else.
func (s *PgStore) withSweepScope(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded on the commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.retention_sweep', 'true', true)"); err != nil {
		return fmt.Errorf("set_config app.retention_sweep: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
