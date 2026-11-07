package store

// MDM-01 party contact points on Postgres (migration 000028). See registry.ContactPointStore for the contract.
//
// Every status change takes the contact point row lock and checks expected_version under it. The principal who
// proposed a contact point is refused as its verifier here, and again by a table constraint. The address value is read
// back only by ListPartyContactPoints and is never passed to an event or a log line.

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

var _ registry.ContactPointStore = (*PgStore)(nil)

const contactPointColumns = `contact_point_id, tenant_id, party_id, channel, value, value_digest, purpose, status, source_class,
	valid_from, valid_to, COALESCE(evidence_ref,''), created_by, COALESCE(verified_by,''), verified_at, COALESCE(retired_by,''),
	retired_at, record_version, created_at`

func scanContactPoint(row pgx.Row) (*domain.ContactPoint, error) {
	var c domain.ContactPoint
	if err := row.Scan(&c.ContactPointID, &c.TenantID, &c.PartyID, &c.Channel, &c.Value, &c.ValueDigest, &c.Purpose, &c.Status,
		&c.SourceClass, &c.ValidFrom, &c.ValidTo, &c.EvidenceRef, &c.CreatedBy, &c.VerifiedBy, &c.VerifiedAt, &c.RetiredBy,
		&c.RetiredAt, &c.RecordVersion, &c.CreatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func lockContactPoint(ctx context.Context, tx pgx.Tx, tid, id string) (*domain.ContactPoint, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrNotFound
	}
	c, err := scanContactPoint(tx.QueryRow(ctx, `SELECT `+contactPointColumns+` FROM party_contact_points WHERE contact_point_id = $1 AND tenant_id = $2 FOR UPDATE`, id, tid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return c, err
}

func checkContactVersion(supplied, current int64, allowUnversioned bool) error {
	switch {
	case supplied == 0 && !allowUnversioned:
		return registry.ErrVersionRequired
	case supplied != 0 && supplied != current:
		return fmt.Errorf("%w: you supplied expected_version %d but this contact point is at %d — reload and re-evaluate",
			registry.ErrVersionConflict, supplied, current)
	}
	return nil
}

// CreateContactPoint records a PROPOSED contact point.
func (s *PgStore) CreateContactPoint(ctx context.Context, cmd registry.ContactPointCreateCommand) (*registry.ContactPointOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	req := cmd.Request
	var out *registry.ContactPointOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		p, err := lockParty(ctx, tx, tid, cmd.PartyID)
		if err != nil {
			return err
		}
		if err := partyWritable(p); err != nil {
			return err
		}
		digest := domain.ContactDigest(req.Value)
		var one int
		err = tx.QueryRow(ctx, `SELECT 1 FROM party_contact_points WHERE tenant_id = $1 AND party_id = $2 AND channel = $3 AND value_digest = $4 AND status <> 'RETIRED'`,
			tid, cmd.PartyID, string(req.Channel), digest).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: this party already has a live contact point with that address", registry.ErrConflict)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var from any
		if req.ValidFrom != nil {
			from = req.ValidFrom.UTC()
		}
		c, err := scanContactPoint(tx.QueryRow(ctx, `INSERT INTO party_contact_points
			(contact_point_id, tenant_id, party_id, channel, value, value_digest, purpose, source_class, valid_from, evidence_ref, created_by, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,COALESCE($9::timestamptz, clock_timestamp()),NULLIF($10,''),$11,NULLIF($12,'')) RETURNING `+contactPointColumns,
			uuid.NewString(), tid, cmd.PartyID, string(req.Channel), req.Value, digest, string(req.Purpose), string(req.SourceClass),
			from, req.EvidenceRef, cmd.Actor, req.CorrelationID))
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: this party already has a live contact point with that address", registry.ErrConflict)
			}
			return err
		}
		out = &registry.ContactPointOutcome{ContactPoint: c.WithoutValue()}
		rec, err := events.ContactPointRecord(events.EventContactPointProposed, *c, cmd.Actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// VerifyContactPoint moves PROPOSED to VERIFIED.
func (s *PgStore) VerifyContactPoint(ctx context.Context, id string, req domain.VerifyContactPointRequest, actor string, allowUnversioned bool) (*registry.ContactPointOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.ContactPointOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		c, err := lockContactPoint(ctx, tx, tid, id)
		if err != nil {
			return err
		}
		if err := checkContactVersion(req.ExpectedVersion, c.RecordVersion, allowUnversioned); err != nil {
			return err
		}
		if !c.Status.CanMoveTo(domain.ContactVerified) {
			return fmt.Errorf("%w: a %s contact point cannot be verified", registry.ErrStateConflict, c.Status)
		}
		// Maker-checker: the principal who proposed it does not verify it.
		if actor == c.CreatedBy {
			return fmt.Errorf("%w: a contact point is verified by someone other than the principal who proposed it", registry.ErrSelfApproval)
		}
		p, err := scanParty(tx.QueryRow(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id = $1 AND tenant_id = $2`, c.PartyID, tid))
		if err != nil {
			return err
		}
		if err := partyWritable(p); err != nil {
			return err
		}
		next, err := scanContactPoint(tx.QueryRow(ctx, `UPDATE party_contact_points SET status = 'VERIFIED', verified_by = $3, verified_at = clock_timestamp(),
			evidence_ref = $4, record_version = record_version + 1, updated_at = clock_timestamp()
			WHERE contact_point_id = $1 AND tenant_id = $2 RETURNING `+contactPointColumns, id, tid, actor, strings.TrimSpace(req.EvidenceRef)))
		if err != nil {
			return err
		}
		out = &registry.ContactPointOutcome{ContactPoint: next.WithoutValue()}
		rec, err := events.ContactPointRecord(events.EventContactPointVerified, *next, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// RetireContactPoint ends a contact point, finally.
func (s *PgStore) RetireContactPoint(ctx context.Context, id string, req domain.RetireContactPointRequest, actor string, allowUnversioned bool) (*registry.ContactPointOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.ContactPointOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		c, err := lockContactPoint(ctx, tx, tid, id)
		if err != nil {
			return err
		}
		if err := checkContactVersion(req.ExpectedVersion, c.RecordVersion, allowUnversioned); err != nil {
			return err
		}
		if !c.Status.CanMoveTo(domain.ContactRetired) {
			return fmt.Errorf("%w: a %s contact point cannot be retired", registry.ErrStateConflict, c.Status)
		}
		// valid_to stays after valid_from even for a contact point whose start was set in the future.
		next, err := scanContactPoint(tx.QueryRow(ctx, `UPDATE party_contact_points SET status = 'RETIRED', retired_by = $3,
			retired_at = clock_timestamp(), valid_to = GREATEST(clock_timestamp(), valid_from + interval '1 microsecond'),
			retire_reason = $4, record_version = record_version + 1, updated_at = clock_timestamp()
			WHERE contact_point_id = $1 AND tenant_id = $2 RETURNING `+contactPointColumns, id, tid, actor, strings.TrimSpace(req.Reason)))
		if err != nil {
			return err
		}
		out = &registry.ContactPointOutcome{ContactPoint: next.WithoutValue()}
		rec, err := events.ContactPointRecord(events.EventContactPointRetired, *next, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ListPartyContactPoints lists the contact points of a party in force at q.AsOf, values included.
func (s *PgStore) ListPartyContactPoints(ctx context.Context, partyID string, q domain.ContactPointQuery) ([]domain.ContactPoint, error) {
	tid := domain.TenantFromContext(ctx)
	asOf := q.AsOf
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	var out []domain.ContactPoint
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if _, err := uuid.Parse(partyID); err != nil {
			return registry.ErrNotFound
		}
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM parties WHERE party_id = $1 AND tenant_id = $2`, partyID, tid).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+contactPointColumns+` FROM party_contact_points
			WHERE tenant_id = $1 AND party_id = $2 AND ($3::text = '' OR channel = $3) AND ($4::text = '' OR status = $4)
			  AND valid_from <= $5 AND (valid_to IS NULL OR $5 < valid_to)
			ORDER BY created_at, contact_point_id`, tid, partyID, string(q.Channel), string(q.Status), asOf)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanContactPoint(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}
