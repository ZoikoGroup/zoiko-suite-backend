package store

// ORG-09 registrations on Postgres (migration 000020). See registry.RegistrationStore for the contract.
//
// Every command takes the registration's row lock, checks expected_version under it, updates the row and appends a
// history row and an outbox event in the same transaction. The table refuses what the service also refuses: an
// ACTIVE registration that was never verified, a material registration verified by its creator, a second live
// registration of one number, and any edit to a revoked or superseded one.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var _ registry.RegistrationStore = (*PgStore)(nil)

const sweepActor = "system:expiry-sweep"

const registrationColumns = `registration_id, tenant_id, registration_type, issuing_authority, jurisdiction, number,
	COALESCE(holder_legal_entity_id::text,''), COALESCE(holder_party_id::text,''), status, material, issue_date, effective_from,
	expiry_date, source_class, evidence_ref, COALESCE(evidence_hash,''), COALESCE(supersedes_id::text,''),
	COALESCE(superseded_by_id::text,''), record_version, created_by, created_at, COALESCE(verified_by,''), verified_at,
	COALESCE(last_grant_by,'')`

func scanRegistration(row pgx.Row) (*domain.Registration, error) {
	var r domain.Registration
	var issue, exp *time.Time
	var eff time.Time
	if err := row.Scan(&r.RegistrationID, &r.TenantID, &r.RegistrationType, &r.IssuingAuthority, &r.Jurisdiction, &r.Number,
		&r.HolderLegalEntityID, &r.HolderPartyID, &r.Status, &r.Material, &issue, &eff, &exp, &r.SourceClass, &r.EvidenceRef,
		&r.EvidenceHash, &r.SupersedesID, &r.SupersededByID, &r.RecordVersion, &r.CreatedBy, &r.CreatedAt, &r.VerifiedBy,
		&r.VerifiedAt, &r.LastGrantBy); err != nil {
		return nil, err
	}
	r.IssueDate, r.EffectiveFrom, r.ExpiryDate = fmtDatePtr(issue), fmtDate(eff), fmtDatePtr(exp)
	return &r, nil
}

const registrationHistoryColumns = `seq, command, from_status, to_status, effective_date, expiry_after, COALESCE(reason,''),
	evidence_ref, COALESCE(evidence_hash,''), actor, COALESCE(version_before,0), version_after, occurred_at`

func scanRegistrationHistory(row pgx.Row) (*domain.RegistrationTransition, error) {
	var t domain.RegistrationTransition
	var from *string
	var eff time.Time
	var exp *time.Time
	if err := row.Scan(&t.Seq, &t.Command, &from, &t.To, &eff, &exp, &t.Reason, &t.EvidenceRef, &t.EvidenceHash, &t.Actor,
		&t.VersionBefore, &t.VersionAfter, &t.OccurredAt); err != nil {
		return nil, err
	}
	if from != nil {
		t.From = domain.RegistrationStatus(*from)
	}
	t.EffectiveDate, t.ExpiryAfter = fmtDate(eff), fmtDatePtr(exp)
	return &t, nil
}

func lockRegistration(ctx context.Context, tx pgx.Tx, tid, id string) (*domain.Registration, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrNotFound
	}
	r, err := scanRegistration(tx.QueryRow(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE registration_id = $1 AND tenant_id = $2 FOR UPDATE`, id, tid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return r, err
}

func checkRegistrationVersion(supplied, current int64, allowUnversioned bool) error {
	if supplied == 0 {
		if !allowUnversioned {
			return registry.ErrVersionRequired
		}
		return nil
	}
	if supplied != current {
		return fmt.Errorf("%w: you supplied expected_version %d but this registration is at %d — reload and re-evaluate",
			registry.ErrVersionConflict, supplied, current)
	}
	return nil
}

func insertRegistrationHistory(ctx context.Context, tx pgx.Tx, tid, id, command string, from *string, to domain.RegistrationStatus,
	eff time.Time, expiryAfter *time.Time, reason, evidence, hash, actor string, before *int64, after int64, corr string) (*domain.RegistrationTransition, error) {
	return scanRegistrationHistory(tx.QueryRow(ctx, `INSERT INTO registration_history
		(history_id, tenant_id, registration_id, seq, command, from_status, to_status, effective_date, expiry_after, reason, evidence_ref,
		 evidence_hash, actor, version_before, version_after, correlation_id)
		VALUES ($1,$2,$3,(SELECT COALESCE(MAX(seq),0)+1 FROM registration_history WHERE registration_id = $3),$4,$5,$6,$7,$8,
		        NULLIF($9,''),$10,NULLIF($11,''),$12,$13,$14,NULLIF($15,'')) RETURNING `+registrationHistoryColumns,
		uuid.NewString(), tid, id, command, from, string(to), eff, expiryAfter, reason, evidence, hash, actor, before, after, corr))
}

func dateOrNil(s string) any {
	if s == "" {
		return nil
	}
	t, _ := domain.ParseDate(s)
	return t
}

func datePtr(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, _ := domain.ParseDate(s)
	return &t
}

// CreateRegistration records a DRAFT registration, or replays an earlier create_request_id.
func (s *PgStore) CreateRegistration(ctx context.Context, cmd registry.RegistrationCreateCommand) (*registry.RegistrationOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	req := cmd.Request
	var out *registry.RegistrationOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if req.CreateRequestID != "" {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('registration-create:' || $1::text || ':' || $2, 0))`, tid, req.CreateRequestID); err != nil {
				return err
			}
			r, err := scanRegistration(tx.QueryRow(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE tenant_id = $1 AND create_request_id = $2`, tid, req.CreateRequestID))
			if err == nil {
				var fp string
				if err := tx.QueryRow(ctx, `SELECT fingerprint FROM registrations WHERE registration_id = $1`, r.RegistrationID).Scan(&fp); err != nil {
					return err
				}
				if fp != cmd.Fingerprint {
					return registry.ErrCreateRequestMismatch
				}
				t, err := scanRegistrationHistory(tx.QueryRow(ctx, `SELECT `+registrationHistoryColumns+` FROM registration_history WHERE registration_id = $1 AND seq = 1`, r.RegistrationID))
				if err != nil {
					return err
				}
				out = &registry.RegistrationOutcome{Registration: *r, Transition: *t, Replayed: true}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		// The holder is a legal entity or a live party of this tenant.
		if req.HolderLegalEntityID != "" {
			if err := entityExists(ctx, tx, tid, req.HolderLegalEntityID); err != nil {
				return err
			}
		} else {
			if _, err := uuid.Parse(req.HolderPartyID); err != nil {
				return registry.ErrNotFound
			}
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM parties WHERE party_id = $1 AND tenant_id = $2`, req.HolderPartyID, tid).Scan(&status); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return registry.ErrNotFound
				}
				return err
			}
			if status == string(domain.PartyMerged) || status == string(domain.PartyInactive) {
				return fmt.Errorf("%w: the party is %s and cannot hold a new registration", registry.ErrStateConflict, status)
			}
		}
		authority, number := domain.NormalizeAuthority(req.IssuingAuthority), domain.NormalizeRegistrationNumber(req.Number)
		jurisdiction := strings.ToUpper(strings.TrimSpace(req.Jurisdiction))

		var existing string
		err := tx.QueryRow(ctx, `SELECT registration_id::text FROM registrations WHERE tenant_id = $1 AND registration_type = $2
			AND authority_normalized = $3 AND jurisdiction = $4 AND number_normalized = $5 AND status IN ('DRAFT','ACTIVE','SUSPENDED')`,
			tid, string(req.RegistrationType), authority, jurisdiction, number).Scan(&existing)
		if err == nil {
			return fmt.Errorf("%w: a live registration with this authority, type, jurisdiction and number already exists (%s)", registry.ErrConflict, existing)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if req.SupersedesID != "" {
			if _, err := uuid.Parse(req.SupersedesID); err != nil {
				return fmt.Errorf("%w: supersedes_id names no registration of this tenant", registry.ErrInvalidInput)
			}
			old, err := scanRegistration(tx.QueryRow(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE registration_id = $1 AND tenant_id = $2`, req.SupersedesID, tid))
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: supersedes_id names no registration of this tenant", registry.ErrInvalidInput)
			}
			if err != nil {
				return err
			}
			switch {
			case old.HolderLegalEntityID != req.HolderLegalEntityID || old.HolderPartyID != req.HolderPartyID:
				return fmt.Errorf("%w: a registration supersedes only one with the same holder", registry.ErrInvalidInput)
			case old.Status != domain.RegActive && old.Status != domain.RegSuspended && old.Status != domain.RegExpired:
				return fmt.Errorf("%w: the registration to supersede is %s", registry.ErrStateConflict, old.Status)
			case old.SupersededByID != "":
				return fmt.Errorf("%w: the registration to supersede already has a successor", registry.ErrStateConflict)
			}
		}
		eff, _ := domain.ParseDate(req.EffectiveFrom)
		id := uuid.NewString()
		r, err := scanRegistration(tx.QueryRow(ctx, `INSERT INTO registrations
			(registration_id, tenant_id, registration_type, issuing_authority, authority_normalized, jurisdiction, number, number_normalized,
			 holder_legal_entity_id, holder_party_id, material, issue_date, effective_from, expiry_date, source_class, evidence_ref,
			 evidence_hash, supersedes_id, created_by, create_request_id, fingerprint, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')::uuid,NULLIF($10,'')::uuid,$11,$12,$13,$14,$15,$16,NULLIF($17,''),NULLIF($18,'')::uuid,$19,
			        NULLIF($20,''),CASE WHEN $20 = '' THEN NULL ELSE $21 END,NULLIF($22,'')) RETURNING `+registrationColumns,
			id, tid, string(req.RegistrationType), strings.TrimSpace(req.IssuingAuthority), authority, jurisdiction, strings.TrimSpace(req.Number), number,
			req.HolderLegalEntityID, req.HolderPartyID, req.RegistrationType.Material(), dateOrNil(req.IssueDate), eff, dateOrNil(req.ExpiryDate),
			string(req.SourceClass), strings.TrimSpace(req.EvidenceRef), req.EvidenceHash, req.SupersedesID, cmd.Actor,
			req.CreateRequestID, cmd.Fingerprint, req.CorrelationID))
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a live registration with this authority, type, jurisdiction and number already exists", registry.ErrConflict)
			}
			return err
		}
		t, err := insertRegistrationHistory(ctx, tx, tid, id, "CreateRegistration", nil, domain.RegDraft, eff, datePtr(req.ExpiryDate), "",
			r.EvidenceRef, r.EvidenceHash, cmd.Actor, nil, 1, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.RegistrationOutcome{Registration: *r, Transition: *t}
		rec, err := events.RegistrationCreatedRecord(*r, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// VerifyRegistration moves DRAFT or SUSPENDED to ACTIVE on evidence, and completes a supersession.
func (s *PgStore) VerifyRegistration(ctx context.Context, id string, req domain.VerifyRegistrationRequest, actor string, allowUnversioned bool) (*registry.RegistrationOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.RegistrationOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		r, err := lockRegistration(ctx, tx, tid, id)
		if err != nil {
			return err
		}
		if err := checkRegistrationVersion(req.ExpectedVersion, r.RecordVersion, allowUnversioned); err != nil {
			return err
		}
		if r.Status != domain.RegDraft && r.Status != domain.RegSuspended {
			return fmt.Errorf("%w: a registration is verified from DRAFT or SUSPENDED, not %s", registry.ErrStateConflict, r.Status)
		}
		// s4.9: no single operator creates and verifies a material regulated registration.
		if r.Material && actor == r.CreatedBy {
			return fmt.Errorf("%w: a material registration is verified by someone other than the principal who created it", registry.ErrSelfApproval)
		}
		now := time.Now().UTC()
		today := asOfDate(now)
		if r.ExpiryDate != "" && r.ExpiryDate < fmtDate(today) {
			return fmt.Errorf("%w: the registration is past its expiry date %s; renew it first", registry.ErrInvalidInput, r.ExpiryDate)
		}
		next, err := scanRegistration(tx.QueryRow(ctx, `UPDATE registrations SET status = 'ACTIVE', verified_by = $3, verified_at = $4, last_grant_by = $3,
			evidence_ref = $5, evidence_hash = NULLIF($6,''), record_version = record_version + 1
			WHERE registration_id = $1 AND tenant_id = $2 RETURNING `+registrationColumns, id, tid, actor, now, strings.TrimSpace(req.EvidenceRef), req.EvidenceHash))
		if err != nil {
			return err
		}
		prev, before := string(r.Status), r.RecordVersion
		t, err := insertRegistrationHistory(ctx, tx, tid, id, "VerifyRegistration", &prev, domain.RegActive, today, datePtr(next.ExpiryDate),
			strings.TrimSpace(req.Reason), next.EvidenceRef, next.EvidenceHash, actor, &before, next.RecordVersion, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.RegistrationOutcome{Registration: *next, Transition: *t}
		rec, err := events.RegistrationStatusRecord(*next, *t, req.CorrelationID)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		if next.SupersedesID == "" {
			return nil
		}
		// The registration this one replaces ends now that the replacement is proved.
		old, err := lockRegistration(ctx, tx, tid, next.SupersedesID)
		if err != nil {
			return err
		}
		if old.Status != domain.RegActive && old.Status != domain.RegSuspended && old.Status != domain.RegExpired {
			return nil
		}
		oldNext, err := scanRegistration(tx.QueryRow(ctx, `UPDATE registrations SET status = 'SUPERSEDED', superseded_by_id = $3, record_version = record_version + 1
			WHERE registration_id = $1 AND tenant_id = $2 RETURNING `+registrationColumns, old.RegistrationID, tid, id))
		if err != nil {
			return err
		}
		oldPrev, oldBefore := string(old.Status), old.RecordVersion
		ot, err := insertRegistrationHistory(ctx, tx, tid, old.RegistrationID, "SupersedeRegistration", &oldPrev, domain.RegSuperseded, today,
			datePtr(oldNext.ExpiryDate), "superseded by "+id, next.EvidenceRef, "", actor, &oldBefore, oldNext.RecordVersion, req.CorrelationID)
		if err != nil {
			return err
		}
		out.Superseded = oldNext
		orec, err := events.RegistrationStatusRecord(*oldNext, *ot, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, orec)
	})
	return out, err
}

// RenewRegistration extends the expiry on evidence; an expired registration renewed is ACTIVE again.
func (s *PgStore) RenewRegistration(ctx context.Context, id string, req domain.RenewRegistrationRequest, actor string, allowUnversioned bool) (*registry.RegistrationOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.RegistrationOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		r, err := lockRegistration(ctx, tx, tid, id)
		if err != nil {
			return err
		}
		if err := checkRegistrationVersion(req.ExpectedVersion, r.RecordVersion, allowUnversioned); err != nil {
			return err
		}
		if r.Status != domain.RegActive && r.Status != domain.RegSuspended && r.Status != domain.RegExpired {
			return fmt.Errorf("%w: a registration is renewed from ACTIVE, SUSPENDED or EXPIRED, not %s", registry.ErrStateConflict, r.Status)
		}
		// s4.9: a material renewal needs independent approval: not the principal who last made it valid.
		if r.Material && actor == r.LastGrantBy {
			return fmt.Errorf("%w: a material registration is renewed by someone other than the principal who last verified or renewed it", registry.ErrSelfApproval)
		}
		newExp, _ := domain.ParseDate(req.NewExpiryDate)
		if r.ExpiryDate != "" {
			if old, _ := domain.ParseDate(r.ExpiryDate); !newExp.After(old) {
				return fmt.Errorf("%w: new_expiry_date must be later than the current expiry %s", registry.ErrInvalidInput, r.ExpiryDate)
			}
		}
		eff, _ := domain.ParseDate(r.EffectiveFrom)
		if req.EffectiveFrom != "" {
			n, _ := domain.ParseDate(req.EffectiveFrom)
			if n.Before(eff) {
				return fmt.Errorf("%w: effective_from cannot move earlier than %s", registry.ErrInvalidInput, r.EffectiveFrom)
			}
			eff = n
		}
		issue := datePtr(r.IssueDate)
		if req.IssueDate != "" {
			issue = datePtr(req.IssueDate)
		}
		if newExp.Before(eff) || (issue != nil && issue.After(eff)) {
			return fmt.Errorf("%w: the dates are inconsistent: issue_date <= effective_from <= new_expiry_date", registry.ErrInvalidInput)
		}
		now := time.Now().UTC()
		today := asOfDate(now)
		to := r.Status
		if r.Status == domain.RegExpired {
			if newExp.Before(today) {
				return fmt.Errorf("%w: an expired registration is renewed to a date that has not passed", registry.ErrInvalidInput)
			}
			to = domain.RegActive
		}
		next, err := scanRegistration(tx.QueryRow(ctx, `UPDATE registrations SET status = $3, expiry_date = $4, issue_date = $5, effective_from = $6,
			evidence_ref = $7, evidence_hash = NULLIF($8,''), last_grant_by = $9, record_version = record_version + 1
			WHERE registration_id = $1 AND tenant_id = $2 RETURNING `+registrationColumns,
			id, tid, string(to), newExp, issue, eff, strings.TrimSpace(req.EvidenceRef), req.EvidenceHash, actor))
		if err != nil {
			return err
		}
		prev, before := string(r.Status), r.RecordVersion
		t, err := insertRegistrationHistory(ctx, tx, tid, id, "RenewRegistration", &prev, to, today, &newExp, "", next.EvidenceRef, next.EvidenceHash,
			actor, &before, next.RecordVersion, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.RegistrationOutcome{Registration: *next, Transition: *t}
		rec, err := events.RegistrationStatusRecord(*next, *t, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// the statuses each ending command may start from
var registrationEndFrom = map[domain.RegistrationStatus][]domain.RegistrationStatus{
	domain.RegSuspended: {domain.RegActive},
	domain.RegExpired:   {domain.RegActive, domain.RegSuspended},
	domain.RegRevoked:   {domain.RegDraft, domain.RegActive, domain.RegSuspended, domain.RegExpired},
}

// ChangeRegistrationStatus suspends, expires or revokes a registration.
func (s *PgStore) ChangeRegistrationStatus(ctx context.Context, id, command string, to domain.RegistrationStatus, req domain.RegistrationEndRequest, actor string, allowUnversioned bool) (*registry.RegistrationOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.RegistrationOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		r, err := lockRegistration(ctx, tx, tid, id)
		if err != nil {
			return err
		}
		if err := checkRegistrationVersion(req.ExpectedVersion, r.RecordVersion, allowUnversioned); err != nil {
			return err
		}
		ok := false
		for _, from := range registrationEndFrom[to] {
			ok = ok || r.Status == from
		}
		if !ok {
			return fmt.Errorf("%w: a %s registration cannot be moved to %s", registry.ErrStateConflict, r.Status, to)
		}
		eff := asOfDate(time.Now().UTC())
		if req.EffectiveDate != "" {
			eff, _ = domain.ParseDate(req.EffectiveDate)
		}
		next, err := scanRegistration(tx.QueryRow(ctx, `UPDATE registrations SET status = $3, record_version = record_version + 1
			WHERE registration_id = $1 AND tenant_id = $2 RETURNING `+registrationColumns, id, tid, string(to)))
		if err != nil {
			return err
		}
		prev, before := string(r.Status), r.RecordVersion
		t, err := insertRegistrationHistory(ctx, tx, tid, id, command, &prev, to, eff, datePtr(next.ExpiryDate), strings.TrimSpace(req.Reason),
			strings.TrimSpace(req.EvidenceRef), "", actor, &before, next.RecordVersion, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.RegistrationOutcome{Registration: *next, Transition: *t}
		rec, err := events.RegistrationStatusRecord(*next, *t, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// GetRegistration returns a registration; (nil, nil) when absent.
func (s *PgStore) GetRegistration(ctx context.Context, id string) (*domain.Registration, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil
	}
	var out *domain.Registration
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		r, err := scanRegistration(tx.QueryRow(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE registration_id = $1 AND tenant_id = $2`, id, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = r
		return err
	})
	return out, err
}

// RegistrationHolder returns the holder of a registration.
func (s *PgStore) RegistrationHolder(ctx context.Context, id string) (string, string, error) {
	r, err := s.GetRegistration(ctx, id)
	if err != nil {
		return "", "", err
	}
	if r == nil {
		return "", "", registry.ErrNotFound
	}
	return r.HolderLegalEntityID, r.HolderPartyID, nil
}

// RegistrationHistory lists every recorded step of a registration, in order.
func (s *PgStore) RegistrationHistory(ctx context.Context, id string) ([]domain.RegistrationTransition, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrNotFound
	}
	out := []domain.RegistrationTransition{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM registrations WHERE registration_id = $1 AND tenant_id = $2`, id, tid).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+registrationHistoryColumns+` FROM registration_history WHERE registration_id = $1 AND tenant_id = $2 ORDER BY seq`, id, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanRegistrationHistory(rows)
			if err != nil {
				return err
			}
			out = append(out, *t)
		}
		return rows.Err()
	})
	return out, err
}

func queryRegistrations(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]domain.Registration, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Registration{}
	for rows.Next() {
		r, err := scanRegistration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// FindRegistrationsByNumber matches on the normalized number.
func (s *PgStore) FindRegistrationsByNumber(ctx context.Context, number string, typ domain.RegistrationType, authority, jurisdiction string) ([]domain.Registration, error) {
	tid := domain.TenantFromContext(ctx)
	var out []domain.Registration
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var err error
		out, err = queryRegistrations(ctx, tx, `SELECT `+registrationColumns+` FROM registrations
			WHERE tenant_id = $1 AND number_normalized = $2 AND ($3::text = '' OR registration_type = $3::text)
			  AND ($4::text = '' OR authority_normalized = $4::text) AND ($5::text = '' OR jurisdiction = $5::text)
			ORDER BY created_at, registration_id`,
			tid, domain.NormalizeRegistrationNumber(number), string(typ), domain.NormalizeAuthority(authority), strings.ToUpper(strings.TrimSpace(jurisdiction)))
		return err
	})
	return out, err
}

// ListRegistrations lists registrations, a page at a time.
func (s *PgStore) ListRegistrations(ctx context.Context, q domain.RegistrationSearchQuery) ([]domain.Registration, string, error) {
	tid := domain.TenantFromContext(ctx)
	limit := q.Limit
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var afterAt *time.Time
	afterID := ""
	if q.After != "" {
		ts, cid, ok := strings.Cut(q.After, "|")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if _, perr := uuid.Parse(cid); !ok || err != nil || perr != nil {
			return nil, "", fmt.Errorf("%w: cursor is not one this service issued", registry.ErrInvalidInput)
		}
		afterAt, afterID = &t, cid
	}
	var out []domain.Registration
	var next string
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := queryRegistrations(ctx, tx, `SELECT `+registrationColumns+` FROM registrations
			WHERE tenant_id = $1 AND ($2::text = '' OR holder_legal_entity_id::text = $2::text) AND ($3::text = '' OR holder_party_id::text = $3::text)
			  AND ($4::text = '' OR registration_type = $4::text) AND ($5::text = '' OR status = $5::text)
			  AND ($6::text = '' OR jurisdiction = $6::text)
			  AND ($7::timestamptz IS NULL OR (created_at, registration_id) > ($7, $8::uuid))
			ORDER BY created_at, registration_id LIMIT $9`,
			tid, q.HolderLegalEntityID, q.HolderPartyID, string(q.Type), string(q.Status), strings.ToUpper(strings.TrimSpace(q.Jurisdiction)),
			afterAt, nullUUID(afterID), limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			next = last.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + last.RegistrationID
			rows = rows[:limit]
		}
		out = rows
		return nil
	})
	return out, next, err
}

// ListExpiringRegistrations lists registrations expiring within a window, soonest first.
func (s *PgStore) ListExpiringRegistrations(ctx context.Context, withinDays int, asOf time.Time) ([]domain.Registration, error) {
	tid := domain.TenantFromContext(ctx)
	horizon := asOfDate(asOf).AddDate(0, 0, withinDays)
	var out []domain.Registration
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var err error
		out, err = queryRegistrations(ctx, tx, `SELECT `+registrationColumns+` FROM registrations
			WHERE tenant_id = $1 AND status IN ('ACTIVE','SUSPENDED') AND expiry_date IS NOT NULL AND expiry_date <= $2
			ORDER BY expiry_date, registration_id`, tid, horizon)
		return err
	})
	return out, err
}

// SweepRegistrationExpiries records expiries and warns once about those coming, across tenants.
func (s *PgStore) SweepRegistrationExpiries(ctx context.Context, now time.Time, warnDays int) (registry.SweepResult, error) {
	today := asOfDate(now)
	horizon := today.AddDate(0, 0, warnDays)

	// Which tenants have anything due: the one cross-tenant read, by a named capability that sees due rows only.
	var tenants []string
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return registry.SweepResult{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.registration_sweep', 'true', true)`); err != nil {
		_ = tx.Rollback(ctx)
		return registry.SweepResult{}, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT tenant_id::text FROM registrations
		WHERE status IN ('ACTIVE','SUSPENDED') AND expiry_date IS NOT NULL AND expiry_date <= $1`, horizon)
	if err != nil {
		_ = tx.Rollback(ctx)
		return registry.SweepResult{}, err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			_ = tx.Rollback(ctx)
			return registry.SweepResult{}, err
		}
		tenants = append(tenants, t)
	}
	rows.Close()
	_ = tx.Rollback(ctx)

	var res registry.SweepResult
	var firstErr error
	for _, tid := range tenants {
		tctx := domain.WithTenant(ctx, tid)
		err := s.withRLS(tctx, tid, func(tx pgx.Tx) error {
			due, err := queryRegistrations(tctx, tx, `SELECT `+registrationColumns+` FROM registrations
				WHERE tenant_id = $1 AND status IN ('ACTIVE','SUSPENDED') AND expiry_date IS NOT NULL AND expiry_date <= $2
				ORDER BY expiry_date, registration_id FOR UPDATE SKIP LOCKED`, tid, horizon)
			if err != nil {
				return err
			}
			for _, r := range due {
				exp, _ := domain.ParseDate(r.ExpiryDate)
				if exp.Before(today) {
					if err := s.sweepExpire(tctx, tx, tid, r, exp.AddDate(0, 0, 1)); err != nil {
						return err
					}
					res.Expired++
					continue
				}
				if r.Status != domain.RegActive {
					continue
				}
				done, err := s.sweepWarn(tctx, tx, tid, r, exp, today)
				if err != nil {
					return err
				}
				if done {
					res.Warned++
				}
			}
			return nil
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("tenant %s: %w", tid, err)
		}
	}
	return res, firstErr
}

func (s *PgStore) sweepExpire(ctx context.Context, tx pgx.Tx, tid string, r domain.Registration, eff time.Time) error {
	next, err := scanRegistration(tx.QueryRow(ctx, `UPDATE registrations SET status = 'EXPIRED', record_version = record_version + 1
		WHERE registration_id = $1 AND tenant_id = $2 RETURNING `+registrationColumns, r.RegistrationID, tid))
	if err != nil {
		return err
	}
	prev, before := string(r.Status), r.RecordVersion
	t, err := insertRegistrationHistory(ctx, tx, tid, r.RegistrationID, "ExpireRegistration", &prev, domain.RegExpired, eff, datePtr(next.ExpiryDate),
		"expiry date passed", sweepActor, "", sweepActor, &before, next.RecordVersion, "")
	if err != nil {
		return err
	}
	rec, err := events.RegistrationStatusRecord(*next, *t, "")
	if err != nil {
		return err
	}
	return s.enqueue(ctx, tx, rec)
}

func (s *PgStore) sweepWarn(ctx context.Context, tx pgx.Tx, tid string, r domain.Registration, exp, today time.Time) (bool, error) {
	var already *time.Time
	if err := tx.QueryRow(ctx, `SELECT expiring_notified_for FROM registrations WHERE registration_id = $1`, r.RegistrationID).Scan(&already); err != nil {
		return false, err
	}
	if already != nil && already.Equal(exp) {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE registrations SET expiring_notified_for = $2 WHERE registration_id = $1`, r.RegistrationID, exp); err != nil {
		return false, err
	}
	rec, err := events.RegistrationExpiringRecord(r, int(exp.Sub(today).Hours()/24), sweepActor)
	if err != nil {
		return false, err
	}
	return true, s.enqueue(ctx, tx, rec)
}
