package store

// ORG-08 establishments on Postgres (migration 000019). See registry.EstablishmentStore for the contract.
//
// Every status change takes the establishment row lock, checks expected_version under it and appends a transition,
// so "what was active on this date" is read from the transitions, never from the current status. A material
// establishment is activated by a principal other than its creator: the store refuses it, and so does the table.

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

var _ registry.EstablishmentStore = (*PgStore)(nil)

const establishmentColumns = `establishment_id, tenant_id, legal_entity_id, establishment_type, name, address_id, status, material,
	valid_from, valid_to, source_class, evidence_ref, record_version, created_by, created_at, COALESCE(activated_by,'')`

func scanEstablishment(row pgx.Row) (*domain.Establishment, error) {
	var e domain.Establishment
	var from time.Time
	var to *time.Time
	if err := row.Scan(&e.EstablishmentID, &e.TenantID, &e.LegalEntityID, &e.EstablishmentType, &e.Name, &e.AddressID,
		&e.Status, &e.Material, &from, &to, &e.SourceClass, &e.EvidenceRef, &e.RecordVersion, &e.CreatedBy, &e.CreatedAt,
		&e.ActivatedBy); err != nil {
		return nil, err
	}
	e.ValidFrom, e.ValidTo = fmtDate(from), fmtDatePtr(to)
	return &e, nil
}

const transitionColumns = `seq, from_status, to_status, effective_date, command, COALESCE(reason,''), evidence_ref, actor,
	COALESCE(version_before,0), version_after, occurred_at`

func scanTransition(row pgx.Row) (*domain.EstablishmentTransition, error) {
	var t domain.EstablishmentTransition
	var from *string
	var eff time.Time
	if err := row.Scan(&t.Seq, &from, &t.To, &eff, &t.Command, &t.Reason, &t.EvidenceRef, &t.Actor, &t.VersionBefore, &t.VersionAfter, &t.OccurredAt); err != nil {
		return nil, err
	}
	if from != nil {
		t.From = domain.EstablishmentStatus(*from)
	}
	t.EffectiveDate = fmtDate(eff)
	return &t, nil
}

func lockEstablishment(ctx context.Context, tx pgx.Tx, tid, id string) (*domain.Establishment, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrNotFound
	}
	e, err := scanEstablishment(tx.QueryRow(ctx, `SELECT `+establishmentColumns+` FROM establishments WHERE establishment_id = $1 AND tenant_id = $2 FOR UPDATE`, id, tid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return e, err
}

func insertTransition(ctx context.Context, tx pgx.Tx, tid, estID string, seq int, from *string, to domain.EstablishmentStatus,
	eff time.Time, command, reason, evidence, actor string, before *int64, after int64, corr string) (*domain.EstablishmentTransition, error) {
	return scanTransition(tx.QueryRow(ctx, `INSERT INTO establishment_transitions
		(transition_id, tenant_id, establishment_id, seq, from_status, to_status, effective_date, command, reason, evidence_ref, actor, version_before, version_after, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13,NULLIF($14,'')) RETURNING `+transitionColumns,
		uuid.NewString(), tid, estID, seq, from, string(to), eff, command, reason, evidence, actor, before, after, corr))
}

// CreateEstablishment records a PROPOSED establishment, or replays an earlier create_request_id.
func (s *PgStore) CreateEstablishment(ctx context.Context, cmd registry.EstablishmentCreateCommand) (*registry.EstablishmentOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	req := cmd.Request
	var out *registry.EstablishmentOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if req.CreateRequestID != "" {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('establishment-create:' || $1::text || ':' || $2, 0))`, tid, req.CreateRequestID); err != nil {
				return err
			}
			e, err := scanEstablishment(tx.QueryRow(ctx, `SELECT `+establishmentColumns+` FROM establishments WHERE tenant_id = $1 AND create_request_id = $2`, tid, req.CreateRequestID))
			if err == nil {
				var fp string
				if err := tx.QueryRow(ctx, `SELECT fingerprint FROM establishments WHERE establishment_id = $1`, e.EstablishmentID).Scan(&fp); err != nil {
					return err
				}
				if fp != cmd.Fingerprint {
					return registry.ErrCreateRequestMismatch
				}
				t, err := scanTransition(tx.QueryRow(ctx, `SELECT `+transitionColumns+` FROM establishment_transitions WHERE establishment_id = $1 AND seq = 1`, e.EstablishmentID))
				if err != nil {
					return err
				}
				out = &registry.EstablishmentOutcome{Establishment: *e, Transition: *t, Replayed: true}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var one int
		if _, err := uuid.Parse(req.LegalEntityID); err != nil {
			return registry.ErrNotFound
		}
		if err := tx.QueryRow(ctx, `SELECT 1 FROM legal_entities WHERE legal_entity_id = $1 AND tenant_id = $2`, req.LegalEntityID, tid).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrNotFound
			}
			return err
		}
		if _, err := uuid.Parse(req.AddressID); err != nil {
			return fmt.Errorf("%w: address_id names no address of this tenant", registry.ErrInvalidInput)
		}
		if err := tx.QueryRow(ctx, `SELECT 1 FROM addresses WHERE address_id = $1 AND tenant_id = $2`, req.AddressID, tid).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: address_id names no address of this tenant", registry.ErrInvalidInput)
			}
			return err
		}
		from := time.Now().UTC()
		if req.ValidFrom != "" {
			from, _ = domain.ParseDate(req.ValidFrom)
		}
		from = asOfDate(from)
		id := uuid.NewString()
		e, err := scanEstablishment(tx.QueryRow(ctx, `INSERT INTO establishments
			(establishment_id, tenant_id, legal_entity_id, establishment_type, name, address_id, material, valid_from, source_class,
			 evidence_ref, create_request_id, fingerprint, created_by, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),CASE WHEN $11 = '' THEN NULL ELSE $12 END,$13,NULLIF($14,'')) RETURNING `+establishmentColumns,
			id, tid, req.LegalEntityID, string(req.EstablishmentType), strings.TrimSpace(req.Name), req.AddressID, req.EstablishmentType.Material(),
			from, string(req.SourceClass), strings.TrimSpace(req.EvidenceRef), req.CreateRequestID, cmd.Fingerprint, cmd.Actor, req.CorrelationID))
		if err != nil {
			return err
		}
		t, err := insertTransition(ctx, tx, tid, id, 1, nil, domain.EstProposed, from, "CreateEstablishment", "", e.EvidenceRef, cmd.Actor, nil, 1, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.EstablishmentOutcome{Establishment: *e, Transition: *t}
		rec, err := events.EstablishmentCreatedRecord(*e, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ChangeEstablishmentStatus moves an establishment and records the transition.
func (s *PgStore) ChangeEstablishmentStatus(ctx context.Context, establishmentID string, req domain.ChangeEstablishmentStatusRequest, actor string, allowUnversioned bool) (*registry.EstablishmentOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.EstablishmentOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		e, err := lockEstablishment(ctx, tx, tid, establishmentID)
		if err != nil {
			return err
		}
		switch {
		case req.ExpectedVersion == 0 && !allowUnversioned:
			return registry.ErrVersionRequired
		case req.ExpectedVersion != 0 && req.ExpectedVersion != e.RecordVersion:
			return fmt.Errorf("%w: you supplied expected_version %d but this establishment is at %d — reload and re-evaluate",
				registry.ErrVersionConflict, req.ExpectedVersion, e.RecordVersion)
		}
		if !e.Status.CanMoveTo(req.To) {
			return fmt.Errorf("%w: an establishment cannot move from %s to %s", registry.ErrStateConflict, e.Status, req.To)
		}
		// s4.8: no single operator may create and legally activate a material establishment.
		if req.To == domain.EstActive && e.Material && actor == e.CreatedBy {
			return fmt.Errorf("%w: a material establishment is activated by someone other than the principal who proposed it", registry.ErrSelfApproval)
		}
		eff := time.Now().UTC()
		if req.EffectiveDate != "" {
			eff, _ = domain.ParseDate(req.EffectiveDate)
		}
		eff = asOfDate(eff)
		from, _ := domain.ParseDate(e.ValidFrom)
		if eff.Before(from) {
			return fmt.Errorf("%w: effective_date is before the establishment began on %s", registry.ErrInvalidInput, e.ValidFrom)
		}
		var lastSeq int
		var lastEff time.Time
		if err := tx.QueryRow(ctx, `SELECT seq, effective_date FROM establishment_transitions WHERE establishment_id = $1 ORDER BY seq DESC LIMIT 1`, establishmentID).Scan(&lastSeq, &lastEff); err != nil {
			return err
		}
		if eff.Before(lastEff) {
			return fmt.Errorf("%w: effective_date is before the last change, on %s", registry.ErrInvalidInput, fmtDate(lastEff))
		}
		var validTo any
		if req.To == domain.EstClosed {
			validTo = eff
		}
		next, err := scanEstablishment(tx.QueryRow(ctx, `UPDATE establishments SET status = $3::text, record_version = record_version + 1,
			valid_to = COALESCE($4::date, valid_to), activated_by = CASE WHEN $3::text = 'ACTIVE' THEN $5::text ELSE activated_by END
			WHERE establishment_id = $1 AND tenant_id = $2 RETURNING `+establishmentColumns,
			establishmentID, tid, string(req.To), validTo, actor))
		if err != nil {
			return err
		}
		prev := string(e.Status)
		before := e.RecordVersion
		cmdName := map[domain.EstablishmentStatus]string{domain.EstActive: "ActivateEstablishment", domain.EstInactive: "DeactivateEstablishment", domain.EstClosed: "CloseEstablishment"}[req.To]
		t, err := insertTransition(ctx, tx, tid, establishmentID, lastSeq+1, &prev, req.To, eff, cmdName, strings.TrimSpace(req.Reason),
			strings.TrimSpace(req.EvidenceRef), actor, &before, next.RecordVersion, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.EstablishmentOutcome{Establishment: *next, Transition: *t}
		rec, err := events.EstablishmentStatusRecord(*next, *t, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// EstablishmentEntity returns the legal entity an establishment belongs to.
func (s *PgStore) EstablishmentEntity(ctx context.Context, establishmentID string) (string, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(establishmentID); err != nil {
		return "", registry.ErrNotFound
	}
	var id string
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM establishments WHERE establishment_id = $1 AND tenant_id = $2`, establishmentID, tid).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		return err
	})
	return id, err
}

func loadTransitions(ctx context.Context, tx pgx.Tx, estID string) ([]domain.EstablishmentTransition, error) {
	rows, err := tx.Query(ctx, `SELECT `+transitionColumns+` FROM establishment_transitions WHERE establishment_id = $1 ORDER BY seq`, estID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EstablishmentTransition
	for rows.Next() {
		t, err := scanTransition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetEstablishment returns an establishment with the address version in force at asOf.
func (s *PgStore) GetEstablishment(ctx context.Context, establishmentID string, asOf time.Time, history bool) (*domain.EstablishmentView, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(establishmentID); err != nil {
		return nil, nil
	}
	var out *domain.EstablishmentView
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		e, err := scanEstablishment(tx.QueryRow(ctx, `SELECT `+establishmentColumns+` FROM establishments WHERE establishment_id = $1 AND tenant_id = $2`, establishmentID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		view := &domain.EstablishmentView{Establishment: *e}
		if view.Address, err = versionInForce(ctx, tx, e.AddressID, asOfDate(asOf)); err != nil {
			return err
		}
		if history {
			if view.History, err = loadTransitions(ctx, tx, establishmentID); err != nil {
				return err
			}
		}
		out = view
		return nil
	})
	return out, err
}

func entityExists(ctx context.Context, tx pgx.Tx, tid, entityID string) error {
	if _, err := uuid.Parse(entityID); err != nil {
		return registry.ErrNotFound
	}
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM legal_entities WHERE legal_entity_id = $1 AND tenant_id = $2`, entityID, tid).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		return err
	}
	return nil
}

// ListEntityEstablishments lists an entity's establishments, optionally by current status.
func (s *PgStore) ListEntityEstablishments(ctx context.Context, legalEntityID string, status domain.EstablishmentStatus) ([]domain.Establishment, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.Establishment{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := entityExists(ctx, tx, tid, legalEntityID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+establishmentColumns+` FROM establishments
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3::text = '' OR status = $3::text) ORDER BY created_at, establishment_id`,
			tid, legalEntityID, string(status))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEstablishment(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

// EstablishmentsAsOf lists the establishments ACTIVE on a date, from the recorded transitions.
func (s *PgStore) EstablishmentsAsOf(ctx context.Context, legalEntityID string, asOf time.Time) ([]domain.EstablishmentView, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.EstablishmentView{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := entityExists(ctx, tx, tid, legalEntityID); err != nil {
			return err
		}
		day := asOfDate(asOf)
		rows, err := tx.Query(ctx, `SELECT `+establishmentColumns+` FROM establishments e
			WHERE e.tenant_id = $1 AND e.legal_entity_id = $2
			  AND (SELECT t.to_status FROM establishment_transitions t WHERE t.establishment_id = e.establishment_id AND t.effective_date <= $3
			       ORDER BY t.effective_date DESC, t.seq DESC LIMIT 1) = 'ACTIVE'
			ORDER BY e.created_at, e.establishment_id`, tid, legalEntityID, day)
		if err != nil {
			return err
		}
		var ests []domain.Establishment
		for rows.Next() {
			e, err := scanEstablishment(rows)
			if err != nil {
				rows.Close()
				return err
			}
			e.Status = domain.EstActive // what it was on that date
			ests = append(ests, *e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, e := range ests {
			v, err := versionInForce(ctx, tx, e.AddressID, day)
			if err != nil {
				return err
			}
			out = append(out, domain.EstablishmentView{Establishment: e, Address: v})
		}
		return nil
	})
	return out, err
}

// ListEntityAddresses lists the addresses a legal entity has or had.
func (s *PgStore) ListEntityAddresses(ctx context.Context, legalEntityID string, asOf time.Time, includeEnded bool) ([]domain.EntityAddress, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.EntityAddress{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := entityExists(ctx, tx, tid, legalEntityID); err != nil {
			return err
		}
		day := asOfDate(asOf)
		rows, err := tx.Query(ctx, `SELECT link_id, address_id, legal_entity_id, link_type, valid_from, valid_to, evidence_ref, linked_by, linked_at
			FROM entity_address_links WHERE tenant_id = $1 AND legal_entity_id = $2
			  AND ($4 OR (valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3))) ORDER BY valid_from, link_id`,
			tid, legalEntityID, day, includeEnded)
		if err != nil {
			return err
		}
		var links []domain.AddressLink
		for rows.Next() {
			var l domain.AddressLink
			var from time.Time
			var to *time.Time
			if err := rows.Scan(&l.LinkID, &l.AddressID, &l.LegalEntityID, &l.LinkType, &from, &to, &l.EvidenceRef, &l.LinkedBy, &l.LinkedAt); err != nil {
				rows.Close()
				return err
			}
			l.ValidFrom, l.ValidTo = fmtDate(from), fmtDatePtr(to)
			links = append(links, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, l := range links {
			v, err := versionInForce(ctx, tx, l.AddressID, day)
			if err != nil {
				return err
			}
			out = append(out, domain.EntityAddress{Link: l, Version: v})
		}
		return nil
	})
	return out, err
}
