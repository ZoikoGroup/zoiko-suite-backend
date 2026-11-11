package store

// ORG-08 addresses on Postgres (migration 000018). See registry.AddressStore for the contract.
//
// Every write takes the address row lock first, so two stewards cannot both amend or verify the same version, and
// checks expected_version under it (s19.1: last write wins is prohibited). History is rows: an amendment closes the
// prior version and adds one, a verification adds an observation, a link is ended by a date; none is deleted.

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

var _ registry.AddressStore = (*PgStore)(nil)

const addressVersionColumns = `version_no, lines, locality, COALESCE(subdivision,''), COALESCE(postal_code,''), country,
	COALESCE(formatted,''), normalized, valid_from, valid_to, verification_state, source_class, evidence_ref,
	recorded_by, recorded_at, superseded_at, COALESCE(reason,'')`

func scanAddressVersion(row pgx.Row) (*domain.AddressVersion, error) {
	var v domain.AddressVersion
	var from time.Time
	var to *time.Time
	if err := row.Scan(&v.VersionNo, &v.Lines, &v.Locality, &v.Subdivision, &v.PostalCode, &v.Country, &v.Formatted,
		&v.Normalized, &from, &to, &v.VerificationState, &v.SourceClass, &v.EvidenceRef, &v.RecordedBy,
		&v.RecordedAt, &v.SupersededAt, &v.Reason); err != nil {
		return nil, err
	}
	v.ValidFrom, v.ValidTo = fmtDate(from), fmtDatePtr(to)
	return &v, nil
}

func scanAddress(row pgx.Row) (*domain.Address, error) {
	var a domain.Address
	if err := row.Scan(&a.AddressID, &a.TenantID, &a.AddressType, &a.CurrentVersion, &a.CreatedBy, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

const addressColumns = `address_id, tenant_id, address_type, current_version, created_by, created_at`

// lockAddress reads an address and holds its row lock until commit.
func lockAddress(ctx context.Context, tx pgx.Tx, tenantID, addressID string) (*domain.Address, error) {
	if _, err := uuid.Parse(addressID); err != nil {
		return nil, registry.ErrNotFound
	}
	a, err := scanAddress(tx.QueryRow(ctx, `SELECT `+addressColumns+` FROM addresses WHERE address_id = $1 AND tenant_id = $2 FOR UPDATE`, addressID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return a, err
}

func checkAddressVersion(supplied, current int, allowUnversioned bool) error {
	if supplied == 0 {
		if !allowUnversioned {
			return registry.ErrVersionRequired
		}
		return nil
	}
	if supplied != current {
		return fmt.Errorf("%w: you supplied expected_version %d but this address is at %d — reload and re-evaluate",
			registry.ErrVersionConflict, supplied, current)
	}
	return nil
}

func latestAddressVersion(ctx context.Context, tx pgx.Tx, addressID string, versionNo int) (*domain.AddressVersion, error) {
	return scanAddressVersion(tx.QueryRow(ctx, `SELECT `+addressVersionColumns+` FROM address_versions WHERE address_id = $1 AND version_no = $2`, addressID, versionNo))
}

func insertAddressVersion(ctx context.Context, tx pgx.Tx, tid, addressID string, no int, f domain.AddressFields, reason, actor string) (*domain.AddressVersion, error) {
	t := f.Trim()
	from := time.Now().UTC()
	if t.ValidFrom != "" {
		from, _ = domain.ParseDate(t.ValidFrom)
	}
	from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	return scanAddressVersion(tx.QueryRow(ctx, `INSERT INTO address_versions
		(version_id, tenant_id, address_id, version_no, lines, locality, subdivision, postal_code, country, formatted, normalized,
		 valid_from, source_class, evidence_ref, recorded_by, reason, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),$9,NULLIF($10,''),$11,$12,$13,$14,$15,NULLIF($16,''),NULLIF($17,''))
		RETURNING `+addressVersionColumns,
		uuid.NewString(), tid, addressID, no, t.Lines, t.Locality, t.Subdivision, t.PostalCode, t.Country, t.Formatted,
		f.Normalized(), from, string(t.SourceClass), t.EvidenceRef, actor, reason, t.CorrelationID))
}

// CreateAddress records a new address, or replays an earlier create_request_id.
func (s *PgStore) CreateAddress(ctx context.Context, cmd registry.AddressCreateCommand) (*registry.AddressOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	req := cmd.Request
	var out *registry.AddressOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if req.CreateRequestID != "" {
			// Serialises two calls with one request id, so the replay check cannot be raced past.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('address-create:' || $1::text || ':' || $2, 0))`,
				tid, req.CreateRequestID); err != nil {
				return err
			}
			a, err := scanAddress(tx.QueryRow(ctx, `SELECT `+addressColumns+` FROM addresses WHERE tenant_id = $1 AND create_request_id = $2`, tid, req.CreateRequestID))
			if err == nil {
				var fp string
				if err := tx.QueryRow(ctx, `SELECT fingerprint FROM addresses WHERE address_id = $1`, a.AddressID).Scan(&fp); err != nil {
					return err
				}
				if fp != cmd.Fingerprint {
					return registry.ErrCreateRequestMismatch
				}
				v, err := latestAddressVersion(ctx, tx, a.AddressID, 1)
				if err != nil {
					return err
				}
				out = &registry.AddressOutcome{Address: *a, Version: *v, Replayed: true}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		id := uuid.NewString()
		a, err := scanAddress(tx.QueryRow(ctx, `INSERT INTO addresses (address_id, tenant_id, address_type, create_request_id, fingerprint, created_by, correlation_id)
			VALUES ($1,$2,$3,NULLIF($4,''),CASE WHEN $4 = '' THEN NULL ELSE $5 END,$6,NULLIF($7,'')) RETURNING `+addressColumns,
			id, tid, string(req.AddressType), req.CreateRequestID, cmd.Fingerprint, cmd.Actor, req.CorrelationID))
		if err != nil {
			return err
		}
		v, err := insertAddressVersion(ctx, tx, tid, id, 1, req.AddressFields, "", cmd.Actor)
		if err != nil {
			return err
		}
		out = &registry.AddressOutcome{Address: *a, Version: *v}
		rec, err := events.AddressCreatedRecord(*a, *v, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// AmendAddress adds a version. Identical content is a no-op, not a new version.
func (s *PgStore) AmendAddress(ctx context.Context, addressID string, req domain.AmendAddressRequest, actor string, allowUnversioned bool) (*registry.AddressOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.AddressOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		a, err := lockAddress(ctx, tx, tid, addressID)
		if err != nil {
			return err
		}
		if err := checkAddressVersion(req.ExpectedVersion, a.CurrentVersion, allowUnversioned); err != nil {
			return err
		}
		if a.AddressType.NeedsAuthority() && !req.SourceClass.Authoritative() {
			return fmt.Errorf("%w: %v", registry.ErrSourceUnverified, domain.ErrRegisteredNeedsAuthority)
		}
		cur, err := latestAddressVersion(ctx, tx, addressID, a.CurrentVersion)
		if err != nil {
			return err
		}
		t := req.AddressFields.Trim()
		if sameAddress(*cur, t) {
			out = &registry.AddressOutcome{Address: *a, Version: *cur, NoOp: true}
			return nil
		}
		from := time.Now().UTC()
		if t.ValidFrom != "" {
			from, _ = domain.ParseDate(t.ValidFrom)
		}
		curFrom, _ := domain.ParseDate(cur.ValidFrom)
		from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
		if !from.After(curFrom) {
			return fmt.Errorf("%w: valid_from must be after the current version began on %s", registry.ErrInvalidInput, cur.ValidFrom)
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE address_versions SET valid_to = $3, verification_state = 'SUPERSEDED', superseded_at = $4
			WHERE address_id = $1 AND version_no = $2`, addressID, cur.VersionNo, from, now); err != nil {
			return err
		}
		next, err := insertAddressVersion(ctx, tx, tid, addressID, cur.VersionNo+1, req.AddressFields, req.Reason, actor)
		if err != nil {
			return err
		}
		na, err := scanAddress(tx.QueryRow(ctx, `UPDATE addresses SET current_version = $3 WHERE address_id = $1 AND tenant_id = $2 RETURNING `+addressColumns,
			addressID, tid, next.VersionNo))
		if err != nil {
			return err
		}
		cur.VerificationState, cur.ValidTo = domain.AddressSuperseded, fmtDate(from)
		out = &registry.AddressOutcome{Address: *na, Version: *next}
		rec, err := events.AddressSupersededRecord(*na, *cur, *next, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// sameAddress reports whether the amendment says what the current version already says.
func sameAddress(cur domain.AddressVersion, t domain.AddressFields) bool {
	if len(cur.Lines) != len(t.Lines) {
		return false
	}
	for i := range cur.Lines {
		if cur.Lines[i] != t.Lines[i] {
			return false
		}
	}
	return cur.Locality == t.Locality && cur.Subdivision == t.Subdivision && cur.PostalCode == t.PostalCode &&
		cur.Country == t.Country && cur.Formatted == t.Formatted && (t.ValidFrom == "" || t.ValidFrom == cur.ValidFrom)
}

// VerifyAddress records what the verifier observed about the current version.
func (s *PgStore) VerifyAddress(ctx context.Context, addressID string, req domain.VerifyAddressRequest, actor string, allowUnversioned bool, check registry.AddressCheck) (*registry.VerifyOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.VerifyOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		a, err := lockAddress(ctx, tx, tid, addressID)
		if err != nil {
			return err
		}
		if err := checkAddressVersion(req.ExpectedVersion, a.CurrentVersion, allowUnversioned); err != nil {
			return err
		}
		v, err := latestAddressVersion(ctx, tx, addressID, a.CurrentVersion)
		if err != nil {
			return err
		}
		obs := check(*v)
		obs.VerificationID, obs.ObservedBy, obs.ObservedAt = uuid.NewString(), actor, time.Now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO address_verifications
			(verification_id, tenant_id, address_id, version_no, verifier, result, confidence, findings, normalized_seen, observed_by, observed_at, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''))`,
			obs.VerificationID, tid, addressID, v.VersionNo, obs.Verifier, string(obs.Result), obs.Confidence, obs.Findings,
			obs.NormalizedSeen, actor, obs.ObservedAt, req.CorrelationID); err != nil {
			return err
		}
		// Only a confident pass verifies; a low-confidence or failed observation is recorded and surfaced, and the
		// address is left as it was.
		becameVerified := obs.Result == domain.VerifyPassed && obs.Confidence >= registry.VerifiedThreshold &&
			v.VerificationState == domain.AddressUnverified
		if becameVerified {
			if _, err := tx.Exec(ctx, `UPDATE address_versions SET verification_state = 'VERIFIED' WHERE address_id = $1 AND version_no = $2`,
				addressID, v.VersionNo); err != nil {
				return err
			}
			v.VerificationState = domain.AddressVerified
		}
		out = &registry.VerifyOutcome{Address: *a, Version: *v, Observation: obs}
		if !becameVerified {
			return nil
		}
		rec, err := events.AddressVerifiedRecord(*a, *v, obs, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

func asOfDate(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// versionInForce is the version of an address effective on day.
func versionInForce(ctx context.Context, tx pgx.Tx, addressID string, day time.Time) (*domain.AddressVersion, error) {
	v, err := scanAddressVersion(tx.QueryRow(ctx, `SELECT `+addressVersionColumns+` FROM address_versions
		WHERE address_id = $1 AND valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2) ORDER BY version_no DESC LIMIT 1`, addressID, day))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// GetAddress returns an address with the version in force at asOf and that version's verification observations.
func (s *PgStore) GetAddress(ctx context.Context, addressID string, asOf time.Time, history bool) (*domain.AddressView, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(addressID); err != nil {
		return nil, nil
	}
	var out *domain.AddressView
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		a, err := scanAddress(tx.QueryRow(ctx, `SELECT `+addressColumns+` FROM addresses WHERE address_id = $1 AND tenant_id = $2`, addressID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		view := &domain.AddressView{Address: *a, Verifications: []domain.AddressObservation{}}
		if view.Version, err = versionInForce(ctx, tx, addressID, asOfDate(asOf)); err != nil {
			return err
		}
		if view.Version != nil {
			rows, err := tx.Query(ctx, `SELECT verification_id, version_no, verifier, result, confidence::float8, findings, normalized_seen, observed_by, observed_at
				FROM address_verifications WHERE address_id = $1 AND version_no = $2 ORDER BY observed_at DESC, verification_id`, addressID, view.Version.VersionNo)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var o domain.AddressObservation
				if err := rows.Scan(&o.VerificationID, &o.VersionNo, &o.Verifier, &o.Result, &o.Confidence, &o.Findings, &o.NormalizedSeen, &o.ObservedBy, &o.ObservedAt); err != nil {
					return err
				}
				view.Verifications = append(view.Verifications, o)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			rows.Close()
		}
		if history {
			rows, err := tx.Query(ctx, `SELECT `+addressVersionColumns+` FROM address_versions WHERE address_id = $1 ORDER BY version_no`, addressID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				v, err := scanAddressVersion(rows)
				if err != nil {
					return err
				}
				view.History = append(view.History, *v)
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		out = view
		return nil
	})
	return out, err
}

// SearchAddresses searches the current version of each address by its normalized form.
func (s *PgStore) SearchAddresses(ctx context.Context, q domain.AddressSearchQuery) ([]domain.AddressSummary, string, error) {
	tid := domain.TenantFromContext(ctx)
	limit := q.Limit
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var afterAt *time.Time
	afterID := ""
	if q.After != "" {
		ts, id, ok := strings.Cut(q.After, "|")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if _, perr := uuid.Parse(id); !ok || err != nil || perr != nil {
			return nil, "", fmt.Errorf("%w: cursor is not one this service issued", registry.ErrInvalidInput)
		}
		afterAt, afterID = &t, id
	}
	text := strings.SplitN(domain.NormalizeAddress([]string{q.Text}, "", "", "", ""), "|", 2)[0]
	postal := strings.SplitN(domain.NormalizeAddress(nil, "", "", q.PostalCode, ""), "|", 5)[3]
	var out []domain.AddressSummary
	var next string
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT a.address_id, a.address_type, v.version_no, v.locality, COALESCE(v.postal_code,''), v.country,
			v.verification_state, v.normalized, a.created_at
			FROM addresses a JOIN address_versions v ON v.address_id = a.address_id AND v.version_no = a.current_version
			WHERE a.tenant_id = $1
			  AND ($2::text = '' OR v.normalized LIKE '%' || $2::text || '%')
			  AND ($3::text = '' OR v.country = $3::text)
			  AND ($4::text = '' OR split_part(v.normalized, '|', 4) = $4::text)
			  AND ($5::text = '' OR a.address_type = $5::text)
			  AND ($6::timestamptz IS NULL OR (a.created_at, a.address_id) > ($6, $7::uuid))
			ORDER BY a.created_at, a.address_id LIMIT $8`,
			tid, text, strings.ToUpper(q.Country), postal, string(q.Type), afterAt, nullUUID(afterID), limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		var lastAt time.Time
		for rows.Next() {
			var r domain.AddressSummary
			var at time.Time
			if err := rows.Scan(&r.AddressID, &r.AddressType, &r.VersionNo, &r.Locality, &r.PostalCode, &r.Country, &r.State, &r.Normalized, &at); err != nil {
				return err
			}
			if len(out) == limit {
				next = lastAt.UTC().Format(time.RFC3339Nano) + "|" + out[len(out)-1].AddressID
				break
			}
			out = append(out, r)
			lastAt = at
		}
		return rows.Err()
	})
	return out, next, err
}

// LinkAddressToParty links an address to a party.
func (s *PgStore) LinkAddressToParty(ctx context.Context, partyID string, req domain.LinkAddressRequest, actor string, allowUnversioned bool) (*registry.AddressLinkOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.AddressLinkOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		p, err := lockParty(ctx, tx, tid, partyID)
		if err != nil {
			return err
		}
		if err := checkPartyVersion(req.ExpectedMasterVersion, p.MasterVersion, allowUnversioned); err != nil {
			return err
		}
		if err := partyWritable(p); err != nil {
			return err
		}
		if _, err := uuid.Parse(req.AddressID); err != nil {
			return registry.ErrNotFound
		}
		a, err := scanAddress(tx.QueryRow(ctx, `SELECT `+addressColumns+` FROM addresses WHERE address_id = $1 AND tenant_id = $2 FOR SHARE`, req.AddressID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if a.AddressType != req.LinkType {
			return fmt.Errorf("%w: the address was recorded as %s, so it can be linked only as %s", registry.ErrInvalidInput, a.AddressType, a.AddressType)
		}
		v, err := latestAddressVersion(ctx, tx, a.AddressID, a.CurrentVersion)
		if err != nil {
			return err
		}
		from := time.Now().UTC()
		if req.ValidFrom != "" {
			from, _ = domain.ParseDate(req.ValidFrom)
		}
		from = asOfDate(from)

		var existing domain.AddressLink
		var ef time.Time
		err = tx.QueryRow(ctx, `SELECT link_id, address_id, party_id, link_type, valid_from, evidence_ref, linked_by, linked_at FROM address_party_links
			WHERE tenant_id = $1 AND party_id = $2 AND address_id = $3 AND link_type = $4 AND valid_to IS NULL`,
			tid, partyID, a.AddressID, string(req.LinkType)).
			Scan(&existing.LinkID, &existing.AddressID, &existing.PartyID, &existing.LinkType, &ef, &existing.EvidenceRef, &existing.LinkedBy, &existing.LinkedAt)
		if err == nil {
			existing.ValidFrom = fmtDate(ef)
			out = &registry.AddressLinkOutcome{Link: existing, Party: *p, NoOp: true}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var ended *domain.AddressLink
		if req.LinkType == domain.AddressRegistered {
			// One live registered office. The change is a historical fact: the old link ends where the new begins.
			var old domain.AddressLink
			var of time.Time
			err := tx.QueryRow(ctx, `SELECT link_id, address_id, party_id, link_type, valid_from, evidence_ref, linked_by, linked_at FROM address_party_links
				WHERE tenant_id = $1 AND party_id = $2 AND link_type = 'REGISTERED' AND valid_to IS NULL FOR UPDATE`, tid, partyID).
				Scan(&old.LinkID, &old.AddressID, &old.PartyID, &old.LinkType, &of, &old.EvidenceRef, &old.LinkedBy, &old.LinkedAt)
			if err == nil {
				if !from.After(of) {
					return fmt.Errorf("%w: the new registered office must start after the current one began on %s", registry.ErrInvalidInput, fmtDate(of))
				}
				if _, err := tx.Exec(ctx, `UPDATE address_party_links SET valid_to = $2 WHERE link_id = $1`, old.LinkID, from); err != nil {
					return err
				}
				old.ValidFrom, old.ValidTo = fmtDate(of), fmtDate(from)
				ended = &old
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		np, err := bumpParty(ctx, tx, tid, partyID, "")
		if err != nil {
			return err
		}
		link := domain.AddressLink{LinkID: uuid.NewString(), AddressID: a.AddressID, PartyID: partyID, LinkType: req.LinkType,
			ValidFrom: fmtDate(from), EvidenceRef: strings.TrimSpace(req.EvidenceRef), LinkedBy: actor, LinkedAt: time.Now().UTC()}
		if _, err := tx.Exec(ctx, `INSERT INTO address_party_links (link_id, tenant_id, address_id, party_id, link_type, valid_from, evidence_ref, linked_by, linked_at, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''))`,
			link.LinkID, tid, a.AddressID, partyID, string(link.LinkType), from, link.EvidenceRef, actor, link.LinkedAt, req.CorrelationID); err != nil {
			return err
		}
		out = &registry.AddressLinkOutcome{Link: link, Party: *np, Ended: ended}
		rec, err := events.AddressLinkedRecord(*a, *v, link, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ListPartyAddresses lists a party's addresses with the version of each in force at asOf.
func (s *PgStore) ListPartyAddresses(ctx context.Context, partyID string, asOf time.Time, includeEnded bool) ([]domain.PartyAddress, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(partyID); err != nil {
		return nil, registry.ErrNotFound
	}
	out := []domain.PartyAddress{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM parties WHERE party_id = $1 AND tenant_id = $2`, partyID, tid).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrNotFound
			}
			return err
		}
		day := asOfDate(asOf)
		rows, err := tx.Query(ctx, `SELECT link_id, address_id, party_id, link_type, valid_from, valid_to, evidence_ref, linked_by, linked_at
			FROM address_party_links WHERE tenant_id = $1 AND party_id = $2
			  AND ($4 OR (valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3)))
			ORDER BY valid_from, link_id`, tid, partyID, day, includeEnded)
		if err != nil {
			return err
		}
		var links []domain.AddressLink
		for rows.Next() {
			var l domain.AddressLink
			var from time.Time
			var to *time.Time
			if err := rows.Scan(&l.LinkID, &l.AddressID, &l.PartyID, &l.LinkType, &from, &to, &l.EvidenceRef, &l.LinkedBy, &l.LinkedAt); err != nil {
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
			out = append(out, domain.PartyAddress{Link: l, Version: v})
		}
		return nil
	})
	return out, err
}
