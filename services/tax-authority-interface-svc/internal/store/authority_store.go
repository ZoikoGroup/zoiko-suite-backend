package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/tax-authority-interface-svc/internal/authority"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
)

var _ authority.Store = (*PgStore)(nil)

// WithEventRegion sets the residency region carried by events enqueued in the transactional outbox.
func (p *PgStore) WithEventRegion(region string) *PgStore {
	p.eventRegion = region
	return p
}

func (p *PgStore) withScope(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	return p.withTenant(middleware.WithTenant(ctx, tenant), fn)
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func pgConstraint(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

func unavailable(err error) error { return fmt.Errorf("%w: %v", authority.ErrStoreUnavailable, err) }

// enqueue builds and enqueues one event in the caller's own transaction: the event and the state change it
// reports commit together or not at all.
func (p *PgStore) enqueue(ctx context.Context, tx pgx.Tx, canonicalType, aggregateID, tenant, entity, actor, corr string, data any) error {
	env, err := envelope.New(envelope.Spec{
		Type: canonicalType, LegacyType: canonicalType, Service: "tax-authority-interface-svc", SchemaVersion: "1.0.0",
		OccurredAt: time.Now().UTC(), TenantID: tenant, LegalEntityID: entity, AggregateType: "tax-authority-attempt",
		AggregateID: aggregateID, CorrelationID: corr, ActorID: actor, ResidencyRegion: p.eventRegion,
		Classification: envelope.Confidential, Data: data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", canonicalType, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", canonicalType, err)
	}
	return nil
}

// ── authority profiles ──────────────────────────────────────────────────────

const profileCols = `provider_key, version, status, spec, spec_hash, created_by, COALESCE(certified_by, ''), certified_at, created_at`

func scanProfile(r pgx.Row) (*authority.ProfileRecord, error) {
	var pr authority.ProfileRecord
	var raw []byte
	if err := r.Scan(&pr.ProviderKey, &pr.Version, &pr.Status, &raw, &pr.SpecHash, &pr.CreatedBy, &pr.CertifiedBy, &pr.CertifiedAt, &pr.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &pr.Spec); err != nil {
		return nil, fmt.Errorf("stored authority profile spec: %w", err)
	}
	return &pr, nil
}

func (p *PgStore) profileEvent(ctx context.Context, tx pgx.Tx, what string, pr *authority.ProfileRecord, actor string) error {
	return p.enqueue(ctx, tx, "com.zoikosuite.tax.authority-profile."+what, pr.ProviderKey, authority.PlatformScopeID, "", actor, "",
		map[string]any{"provider_key": pr.ProviderKey, "version": pr.Version, "status": pr.Status, "spec_hash": pr.SpecHash})
}

func (p *PgStore) PutProfile(ctx context.Context, key string, spec adapter.Spec, createdBy string) (*authority.ProfileRecord, error) {
	spec = spec.Normalize()
	raw, _ := json.Marshal(spec)
	var out *authority.ProfileRecord
	err := p.withScope(ctx, authority.PlatformScopeID, func(tx pgx.Tx) error {
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM tax_authority_profiles WHERE provider_key = $1`, key).Scan(&next); err != nil {
			return err
		}
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `
			INSERT INTO tax_authority_profiles (provider_key, version, status, spec, spec_hash, created_by)
			VALUES ($1, $2, 'DRAFT', $3, $4, $5) RETURNING `+profileCols, key, next, raw, spec.Hash(), createdBy))
		if err != nil {
			return err
		}
		return p.profileEvent(ctx, tx, "published", out, createdBy)
	})
	if pgCode(err) == "23505" {
		return nil, authority.ErrProfileExists
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

func (p *PgStore) GetProfile(ctx context.Context, key string, version int) (*authority.ProfileRecord, error) {
	var out *authority.ProfileRecord
	err := p.withScope(ctx, authority.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		if version == 0 {
			out, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM tax_authority_profiles WHERE provider_key = $1 AND status = 'CERTIFIED' ORDER BY version DESC LIMIT 1`, key))
		} else {
			out, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM tax_authority_profiles WHERE provider_key = $1 AND version = $2`, key, version))
		}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, authority.ErrProfileNotFound
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

func (p *PgStore) CertifyProfile(ctx context.Context, key string, version int, by string) (*authority.ProfileRecord, error) {
	var out *authority.ProfileRecord
	err := p.withScope(ctx, authority.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `
			UPDATE tax_authority_profiles SET status = 'CERTIFIED', certified_by = $3, certified_at = now()
			WHERE provider_key = $1 AND version = $2 AND status = 'DRAFT' RETURNING `+profileCols, key, version, by))
		if err != nil {
			return err
		}
		return p.profileEvent(ctx, tx, "certified", out, by)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, authority.ErrProfileState
	case strings.Contains(pgConstraint(err), "tax_profile_certified_by_other"):
		return nil, authority.ErrProfileSelfCertify
	case err != nil:
		return nil, unavailable(err)
	}
	return out, nil
}

func (p *PgStore) SetProfileStatus(ctx context.Context, key string, version int, to string) (*authority.ProfileRecord, error) {
	var out *authority.ProfileRecord
	err := p.withScope(ctx, authority.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `UPDATE tax_authority_profiles SET status = $3 WHERE provider_key = $1 AND version = $2 RETURNING `+profileCols, key, version, to))
		if err != nil {
			return err
		}
		return p.profileEvent(ctx, tx, strings.ToLower(to), out, "platform")
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, authority.ErrProfileNotFound
	case pgCode(err) == "23000":
		return nil, authority.ErrProfileState
	case err != nil:
		return nil, unavailable(err)
	}
	return out, nil
}

func (p *PgStore) ListProfiles(ctx context.Context, key string) ([]authority.ProfileRecord, error) {
	var out []authority.ProfileRecord
	err := p.withScope(ctx, authority.PlatformScopeID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+profileCols+` FROM tax_authority_profiles WHERE ($1 = '' OR provider_key = $1) ORDER BY provider_key, version`, key)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			pr, err := scanProfile(rows)
			if err != nil {
				return err
			}
			out = append(out, *pr)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// ── filings ─────────────────────────────────────────────────────────────────

const filingCols = `s.submission_id, s.tenant_id, s.interface_id, i.legal_entity_id, i.jurisdiction, i.provider_key, i.connector_id, i.certificate_ref,
	i.certificate_not_after, i.escalate_before_hours, s.tax_period, s.filing_type, s.tax_amount::text, s.subject_ref, s.subject_fingerprint,
	COALESCE(s.document::text, ''), s.payload_hash, s.rail, s.profile_version, s.sent_payload_hash, s.status, s.send_count, s.validated_at,
	s.sent_at, s.in_flight_since, s.not_before, s.next_poll_at, s.deadline_at, s.authority_ref, s.authority_status, s.receipt_hash,
	s.last_failure, s.escalated_at, s.submitted_at`

const filingFrom = ` FROM tax_filing_submissions s JOIN tax_interfaces i ON i.interface_id = s.interface_id AND i.tenant_id = s.tenant_id `

func scanFiling(r pgx.Row) (*authority.Filing, error) {
	var f authority.Filing
	var doc string
	var hours int
	if err := r.Scan(&f.SubmissionID, &f.TenantID, &f.InterfaceID, &f.LegalEntityID, &f.Jurisdiction, &f.ProviderKey, &f.ConnectorID, &f.CertificateRef,
		&f.CertificateNotAt, &hours, &f.TaxPeriod, &f.FilingType, &f.AmountText, &f.SubjectRef, &f.SubjectFingerprint,
		&doc, &f.PayloadHash, &f.Rail, &f.ProfileVersion, &f.SentPayloadHash, &f.Status, &f.SendCount, &f.ValidatedAt,
		&f.SentAt, &f.InFlightSince, &f.NotBefore, &f.NextPollAt, &f.DeadlineAt, &f.AuthorityRef, &f.AuthorityStatus, &f.ReceiptHash,
		&f.LastFailure, &f.EscalatedAt, &f.SubmittedAt); err != nil {
		return nil, err
	}
	f.EscalateBefore = time.Duration(hours) * time.Hour
	if doc != "" {
		f.Document = []byte(doc)
	}
	return &f, nil
}

func (p *PgStore) loadIn(ctx context.Context, tx pgx.Tx, id string) (*authority.Filing, error) {
	return scanFiling(tx.QueryRow(ctx, `SELECT `+filingCols+filingFrom+`WHERE s.submission_id = $1`, id))
}

func (p *PgStore) LoadFiling(ctx context.Context, id string) (*authority.Filing, error) {
	var f *authority.Filing
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		f, err = p.loadIn(ctx, tx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, authority.ErrFilingNotFound
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return f, nil
}

func (p *PgStore) CreateFiling(ctx context.Context, in authority.CreateFiling) (*authority.Filing, bool, error) {
	tenant := middleware.GetTenantID(ctx)
	id := uuid.New().String()
	var f *authority.Filing
	created := true
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		if in.SubjectRef != "" {
			if ex, err := scanFiling(tx.QueryRow(ctx, `SELECT `+filingCols+filingFrom+`WHERE s.tenant_id = $1 AND s.subject_ref = $2`, tenant, in.SubjectRef)); err == nil {
				if ex.SubjectFingerprint != in.SubjectFingerprint {
					return authority.ErrSubjectConflict
				}
				f, created = ex, false
				return nil
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		amount := in.AmountText
		if amount == "" {
			amount = "0"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO tax_filing_submissions (submission_id, interface_id, tenant_id, tax_period, filing_type, tax_amount, status, submitted_at,
				subject_ref, subject_fingerprint, document, payload_hash, rail, deadline_at)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, 'PREPARED', now(), $7, $8, $9::jsonb, $10, $11, $12)`,
			id, in.InterfaceID, tenant, in.TaxPeriod, in.FilingType, amount, in.SubjectRef, in.SubjectFingerprint, string(in.Document), in.PayloadHash, in.Rail, in.DeadlineAt); err != nil {
			return err
		}
		var err error
		if f, err = p.loadIn(ctx, tx, id); err != nil {
			return err
		}
		return p.enqueue(ctx, tx, "com.zoikosuite.tax.authority-attempt.prepared", id, tenant, f.LegalEntityID, in.Actor, in.CorrelationID,
			map[string]any{"submission_id": id, "interface_id": in.InterfaceID, "tax_period": in.TaxPeriod, "filing_type": in.FilingType,
				"payload_hash": in.PayloadHash, "rail": in.Rail})
	})
	if errors.Is(err, authority.ErrSubjectConflict) {
		return nil, false, err
	}
	if pgCode(err) == "23505" {
		return nil, false, authority.ErrSubjectConflict
	}
	if err != nil {
		return nil, false, unavailable(err)
	}
	return f, created, nil
}

func (p *PgStore) MarkValidated(ctx context.Context, id, actor string) (*authority.Filing, error) {
	var f *authority.Filing
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = 'VALIDATED', validated_at = now() WHERE submission_id = $1 AND status = 'PREPARED'`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		f, err = p.loadIn(ctx, tx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, authority.ErrInFlight
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return f, nil
}

func (p *PgStore) Block(ctx context.Context, id, reason, actor, corr string) (*authority.Filing, error) {
	var f *authority.Filing
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = 'BLOCKED', last_failure = $2 WHERE submission_id = $1 AND status IN ('PREPARED', 'VALIDATED')`, id, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		if f, err = p.loadIn(ctx, tx, id); err != nil {
			return err
		}
		return p.enqueue(ctx, tx, "com.zoikosuite.tax.authority-attempt.blocked", id, f.TenantID, f.LegalEntityID, actor, corr,
			map[string]any{"submission_id": id, "reason": reason})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, authority.ErrInFlight
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return f, nil
}

// BeginSend is the compare-and-set that puts a filing in flight: it fails, changing nothing, unless the filing is
// in `from`, may be sent now, and carries the same profile version and transformed payload as any earlier send.
func (p *PgStore) BeginSend(ctx context.Context, id, from string, profileVersion int, sentPayloadHash, actor string) (*authority.Filing, error) {
	var f *authority.Filing
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tax_filing_submissions SET status = 'SUBMITTING', in_flight_since = now(), sent_at = now(), send_count = send_count + 1,
				profile_version = COALESCE(profile_version, $3),
				sent_payload_hash = CASE WHEN sent_payload_hash = '' THEN $4 ELSE sent_payload_hash END, not_before = NULL
			WHERE submission_id = $1 AND status = $2 AND (not_before IS NULL OR not_before <= now())
			  AND (sent_payload_hash = '' OR sent_payload_hash = $4) AND (profile_version IS NULL OR profile_version = $3)`,
			id, from, profileVersion, sentPayloadHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		f, err = p.loadIn(ctx, tx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		cur, lerr := p.LoadFiling(ctx, id)
		switch {
		case lerr != nil:
			return nil, lerr
		case cur.Status != from:
			return nil, authority.ErrInFlight
		case cur.NotBefore != nil && cur.NotBefore.After(time.Now()):
			return nil, authority.ErrNotBefore
		default:
			return nil, authority.ErrPayloadChanged
		}
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return f, nil
}

func (p *PgStore) insertTransport(ctx context.Context, tx pgx.Tx, tenant, id string, t authority.TransportEntry) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tax_transport_log (tenant_id, submission_id, provider_key, profile_version, operation, disposition, class, http_status,
			request_hash, response_hash, provider_ref, error_code, duration_ms, retry_after_s, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		tenant, id, t.ProviderKey, t.ProfileVersion, t.Operation, t.Disposition, t.Class, t.HTTPStatus,
		t.RequestHash, t.ResponseHash, t.ProviderRef, t.ErrorCode, t.DurationMS, t.RetryAfterS, t.Actor)
	return err
}

// Finish applies the outcome of an authority call, and records its transport evidence and events, in one
// transaction.
func (p *PgStore) Finish(ctx context.Context, id string, o authority.Outcome, actor, corr string) (*authority.Filing, error) {
	var f *authority.Filing
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var tag pgconn.CommandTag
		var err error
		switch o.Kind {
		case authority.OutSubmitted:
			tag, err = tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = 'SUBMITTED', authority_ref = CASE WHEN $2::text <> '' THEN $2::text ELSE authority_ref END,
				authority_status = $3, receipt_hash = $4, in_flight_since = NULL, not_before = NULL, last_failure = '', next_poll_at = $5
				WHERE submission_id = $1 AND status = $6`, id, o.AuthorityRef, o.Authority, o.ReceiptHash, o.NextPoll, o.From)
		case authority.OutAccepted:
			tag, err = tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = 'ACCEPTED', authority_ref = CASE WHEN $2::text <> '' THEN $2::text ELSE authority_ref END,
				authority_status = $3, receipt_hash = $4, in_flight_since = NULL, not_before = NULL, last_failure = '', next_poll_at = NULL, ack_reference = CASE WHEN $2::text <> '' THEN $2::text ELSE ack_reference END
				WHERE submission_id = $1 AND status = $5`, id, o.AuthorityRef, o.Authority, o.ReceiptHash, o.From)
		case authority.OutRejected:
			tag, err = tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = 'REJECTED', authority_ref = CASE WHEN $2::text <> '' THEN $2::text ELSE authority_ref END,
				authority_status = $3, receipt_hash = $4, in_flight_since = NULL, not_before = NULL, last_failure = $5, next_poll_at = NULL
				WHERE submission_id = $1 AND status = $6`, id, o.AuthorityRef, o.Authority, o.ReceiptHash, o.Reason, o.From)
		case authority.OutUnknown:
			tag, err = tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = 'PENDING_UNKNOWN', in_flight_since = NULL, last_failure = $2, next_poll_at = $3
				WHERE submission_id = $1 AND status = $4`, id, o.Failure, o.NextPoll, o.From)
		case authority.OutRelease:
			tag, err = tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = $2, in_flight_since = NULL, not_before = $3, last_failure = $4
				WHERE submission_id = $1 AND status = $5`, id, o.ReleaseTo, o.NotBefore, o.Failure, o.From)
		case authority.OutNone:
			tag, err = tx.Exec(ctx, `UPDATE tax_filing_submissions SET last_failure = $2, authority_status = CASE WHEN $3::text <> '' THEN $3::text ELSE authority_status END,
				next_poll_at = COALESCE($4, next_poll_at) WHERE submission_id = $1 AND status = $5`, id, o.Failure, o.Authority, o.NextPoll, o.From)
		default:
			return fmt.Errorf("unknown outcome %q", o.Kind)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		if f, err = p.loadIn(ctx, tx, id); err != nil {
			return err
		}
		o.Transport.Actor = actor
		if err := p.insertTransport(ctx, tx, f.TenantID, id, o.Transport); err != nil {
			return err
		}
		ev := func(kind string) error {
			return p.enqueue(ctx, tx, "com.zoikosuite.tax.authority-attempt."+kind, id, f.TenantID, f.LegalEntityID, actor, corr,
				map[string]any{"submission_id": id, "status": f.Status, "authority_ref": f.AuthorityRef, "authority_status": f.AuthorityStatus,
					"tax_period": f.TaxPeriod, "filing_type": f.FilingType, "failure": o.Failure})
		}
		switch o.Kind {
		case authority.OutSubmitted:
			return ev("submitted")
		case authority.OutAccepted, authority.OutRejected:
			return ev("response-received")
		case authority.OutUnknown:
			return ev("unknown-outcome")
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// not in the state this outcome was for (a sweeper moved it, or it was settled elsewhere). The call's
		// evidence is still worth keeping, but only for a filing this tenant can see: the evidence table's foreign
		// key does not honour row-level security.
		if _, lerr := p.LoadFiling(ctx, id); lerr == nil {
			_ = p.withTenant(ctx, func(tx pgx.Tx) error {
				o.Transport.Actor = actor
				return p.insertTransport(ctx, tx, middleware.GetTenantID(ctx), id, o.Transport)
			})
		}
		return nil, authority.ErrInFlight
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return f, nil
}

func (p *PgStore) Escalate(ctx context.Context, id, reason, actor, corr string) error {
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tax_filing_submissions SET escalated_at = now() WHERE submission_id = $1 AND escalated_at IS NULL
			AND status IN ('VALIDATED', 'SUBMITTED', 'PENDING_UNKNOWN', 'PREPARED')`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already escalated, or final
		}
		f, err := p.loadIn(ctx, tx, id)
		if err != nil {
			return err
		}
		return p.enqueue(ctx, tx, "com.zoikosuite.tax.authority-attempt.deadline-at-risk", id, f.TenantID, f.LegalEntityID, actor, corr,
			map[string]any{"submission_id": id, "status": f.Status, "deadline_at": f.DeadlineAt, "reason": reason, "tax_period": f.TaxPeriod, "filing_type": f.FilingType})
	})
	if err != nil {
		return unavailable(err)
	}
	return nil
}

func (p *PgStore) ListTransport(ctx context.Context, id string) ([]authority.TransportEntry, error) {
	var out []authority.TransportEntry
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT log_id::text, submission_id, provider_key, profile_version, operation, disposition, class, http_status, request_hash, response_hash,
				provider_ref, error_code, duration_ms, retry_after_s, actor, created_at
			FROM tax_transport_log WHERE submission_id = $1 ORDER BY created_at, log_id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t authority.TransportEntry
			if err := rows.Scan(&t.LogID, &t.SubmissionID, &t.ProviderKey, &t.ProfileVersion, &t.Operation, &t.Disposition, &t.Class, &t.HTTPStatus,
				&t.RequestHash, &t.ResponseHash, &t.ProviderRef, &t.ErrorCode, &t.DurationMS, &t.RetryAfterS, &t.Actor, &t.CreatedAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

func (p *PgStore) RecentCalls(ctx context.Context, providerKey string, since time.Time, limit int) ([]bool, error) {
	var out []bool
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT (disposition = 'ANSWERED' AND class IN ('ACKNOWLEDGED', 'PENDING', 'ACCEPTED', 'REJECTED', 'RATE_LIMITED'))
			FROM tax_transport_log WHERE provider_key = $1 AND operation = 'SUBMIT' AND created_at >= $2 ORDER BY created_at DESC LIMIT $3`, providerKey, since, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ok bool
			if err := rows.Scan(&ok); err != nil {
				return err
			}
			out = append(out, ok)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// workerScan lists work across tenants with a named flag (not an unscoped connection).
func (p *PgStore) workerScan(ctx context.Context, query string, args ...any) ([]authority.Due, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tax_worker', 'true', true)"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authority.Due
	for rows.Next() {
		var d authority.Due
		if err := rows.Scan(&d.SubmissionID, &d.TenantID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *PgStore) DuePolls(ctx context.Context, now time.Time, limit int) ([]authority.Due, error) {
	due, err := p.workerScan(ctx, `SELECT submission_id, tenant_id FROM tax_filing_submissions
		WHERE status IN ('SUBMITTED', 'PENDING_UNKNOWN') AND next_poll_at IS NOT NULL AND next_poll_at <= $1 ORDER BY next_poll_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, unavailable(err)
	}
	return due, nil
}

// SweepStale moves filings stuck in SUBMITTING since before staleBefore to PENDING_UNKNOWN. Whether the request
// reached the authority is precisely what is unknown, so nothing is resent.
func (p *PgStore) SweepStale(ctx context.Context, staleBefore time.Time, actor string) (int, error) {
	due, err := p.workerScan(ctx, `SELECT submission_id, tenant_id FROM tax_filing_submissions
		WHERE status = 'SUBMITTING' AND in_flight_since < $1 ORDER BY in_flight_since LIMIT 200`, staleBefore)
	if err != nil {
		return 0, unavailable(err)
	}
	n := 0
	for _, d := range due {
		err := p.withScope(ctx, d.TenantID, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE tax_filing_submissions SET status = 'PENDING_UNKNOWN', in_flight_since = NULL,
				last_failure = 'in-flight request was interrupted', next_poll_at = now() WHERE submission_id = $1 AND status = 'SUBMITTING' AND in_flight_since < $2`, d.SubmissionID, staleBefore)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return pgx.ErrNoRows
			}
			f, err := p.loadIn(ctx, tx, d.SubmissionID)
			if err != nil {
				return err
			}
			return p.enqueue(ctx, tx, "com.zoikosuite.tax.authority-attempt.unknown-outcome", d.SubmissionID, d.TenantID, f.LegalEntityID, actor, "",
				map[string]any{"submission_id": d.SubmissionID, "reason": "interrupted"})
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err == nil {
			n++
		}
	}
	return n, nil
}
