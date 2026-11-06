package store

// MDM-04 reference releases on Postgres (migration 000026). See registry.ReferenceStore for the contract.
//
// A set changes only by releases. A release is staged with all its entries at once, and published later by someone else, in
// effective-date order. The state of a code is derived from the published releases: on a date, or under one exact release.
// Every command takes the set's row lock, so staging and publishing a set's releases are serialized.

import (
	"context"
	"encoding/json"
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

var _ registry.ReferenceStore = (*PgStore)(nil)

const refSetSelect = `SELECT s.set_id, s.tenant_id, s.set_code, s.name, COALESCE(s.description,''), s.source_authority, s.allow_custom, s.created_by, s.created_at,
	COALESCE((SELECT r.release_version FROM reference_releases r WHERE r.tenant_id = s.tenant_id AND r.set_id = s.set_id AND r.status = 'PUBLISHED'
		AND r.effective_from <= CURRENT_DATE ORDER BY r.publish_seq DESC LIMIT 1), '')
	FROM reference_sets s`

func scanRefSet(row pgx.Row) (*domain.ReferenceSet, error) {
	var s domain.ReferenceSet
	if err := row.Scan(&s.SetID, &s.TenantID, &s.SetCode, &s.Name, &s.Description, &s.SourceAuthority, &s.AllowCustom, &s.CreatedBy, &s.CreatedAt, &s.CurrentRelease); err != nil {
		return nil, err
	}
	return &s, nil
}

func lockRefSet(ctx context.Context, tx pgx.Tx, tid, code string) (*domain.ReferenceSet, error) {
	s, err := scanRefSet(tx.QueryRow(ctx, refSetSelect+` WHERE s.tenant_id = $1 AND s.set_code = $2 FOR UPDATE OF s`, tid, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return s, err
}

const refReleaseSelect = `SELECT r.release_id, r.release_version, r.status, r.source_published_at, r.effective_from, r.source_authority, r.compatibility, COALESCE(r.checksum,''),
	r.evidence_ref, r.staged_by, r.staged_at, COALESCE(r.released_by,''), r.released_at, COALESCE(r.release_evidence,''), COALESCE(r.publish_seq,0) FROM reference_releases r`

func scanRefRelease(row pgx.Row, setCode string) (*domain.ReferenceRelease, error) {
	var r domain.ReferenceRelease
	var eff time.Time
	if err := row.Scan(&r.ReleaseID, &r.ReleaseVersion, &r.Status, &r.SourcePublishedAt, &eff, &r.SourceAuthority, &r.Compatibility, &r.Checksum,
		&r.EvidenceRef, &r.StagedBy, &r.StagedAt, &r.ReleasedBy, &r.ReleasedAt, &r.ReleaseEvidence, &r.PublishSeq); err != nil {
		return nil, err
	}
	r.SetCode, r.EffectiveFrom = setCode, fmtDate(eff)
	return &r, nil
}

// withCounts adds the number of entries and their change types to a release.
func withCounts(ctx context.Context, tx pgx.Tx, r *domain.ReferenceRelease) error {
	rows, err := tx.Query(ctx, `SELECT change_type, count(*) FROM reference_entries WHERE release_id = $1 GROUP BY change_type`, r.ReleaseID)
	if err != nil {
		return err
	}
	defer rows.Close()
	r.Changes = map[string]int{}
	for rows.Next() {
		var ct string
		var n int
		if err := rows.Scan(&ct, &n); err != nil {
			return err
		}
		r.Changes[ct] = n
		r.EntryCount += n
	}
	return rows.Err()
}

// publishedState is how each code of the set stands after every published release, in release order.
func publishedState(ctx context.Context, tx pgx.Tx, tid, setID string) (map[string]domain.CodeState, error) {
	rows, err := tx.Query(ctx, `SELECT e.code, e.change_type, e.origin FROM reference_entries e JOIN reference_releases r ON r.tenant_id = e.tenant_id AND r.release_id = e.release_id
		WHERE e.tenant_id = $1 AND e.set_id = $2 AND r.status = 'PUBLISHED' ORDER BY r.publish_seq`, tid, setID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	st := map[string]domain.CodeState{}
	for rows.Next() {
		var code, ct, origin string
		if err := rows.Scan(&code, &ct, &origin); err != nil {
			return nil, err
		}
		switch domain.ReferenceChange(ct) {
		case domain.ChangeAdd:
			st[code] = domain.CodeState{Exists: true, Active: true, Origin: origin}
		case domain.ChangeRetire, domain.ChangeSupersede:
			st[code] = domain.CodeState{Exists: true, Active: false, Origin: origin}
		}
	}
	return st, rows.Err()
}

// latestEffective is the effective date of the newest published release, when any.
func latestEffective(ctx context.Context, tx pgx.Tx, tid, setID string) (*time.Time, error) {
	var d *time.Time
	err := tx.QueryRow(ctx, `SELECT max(effective_from) FROM reference_releases WHERE tenant_id = $1 AND set_id = $2 AND status = 'PUBLISHED'`, tid, setID).Scan(&d)
	return d, err
}

// ── sets ────────────────────────────────────────────────────────────────────

// CreateReferenceSet defines a reference set.
func (s *PgStore) CreateReferenceSet(ctx context.Context, req domain.CreateReferenceSetRequest, actor string) (*domain.ReferenceSet, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ReferenceSet
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		id := uuid.NewString()
		_, err := tx.Exec(ctx, `INSERT INTO reference_sets (set_id, tenant_id, set_code, name, description, source_authority, allow_custom, created_by, correlation_id)
			VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,NULLIF($9,''))`,
			id, tid, req.SetCode, strings.TrimSpace(req.Name), strings.TrimSpace(req.Description), strings.TrimSpace(req.SourceAuthority), req.AllowCustom, actor, req.CorrelationID)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a reference set %s already exists", registry.ErrConflict, req.SetCode)
			}
			return err
		}
		out, err = scanRefSet(tx.QueryRow(ctx, refSetSelect+` WHERE s.tenant_id = $1 AND s.set_id = $2`, tid, id))
		if err != nil {
			return err
		}
		rec, err := events.ReferenceSetCreatedRecord(*out, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ListReferenceSets lists the reference sets of the tenant.
func (s *PgStore) ListReferenceSets(ctx context.Context) ([]domain.ReferenceSet, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.ReferenceSet{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, refSetSelect+` WHERE s.tenant_id = $1 ORDER BY s.set_code`, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			set, err := scanRefSet(rows)
			if err != nil {
				return err
			}
			out = append(out, *set)
		}
		return rows.Err()
	})
	return out, err
}

// GetReferenceSet returns a set, or nil.
func (s *PgStore) GetReferenceSet(ctx context.Context, setCode string) (*domain.ReferenceSet, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ReferenceSet
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		set, err := scanRefSet(tx.QueryRow(ctx, refSetSelect+` WHERE s.tenant_id = $1 AND s.set_code = $2`, tid, setCode))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = set
		return err
	})
	return out, err
}

// ── releases ────────────────────────────────────────────────────────────────

// StageReferenceRelease records a release and its entries.
func (s *PgStore) StageReferenceRelease(ctx context.Context, setCode string, req domain.StageReferenceReleaseRequest, actor string) (*domain.ReferenceRelease, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ReferenceRelease
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		set, err := lockRefSet(ctx, tx, tid, setCode)
		if err != nil {
			return err
		}
		eff, _ := domain.ParseDate(req.EffectiveFrom)
		if latest, err := latestEffective(ctx, tx, tid, set.SetID); err != nil {
			return err
		} else if latest != nil && !eff.After(*latest) {
			return fmt.Errorf("%w: a release takes effect after every release already published; the latest took effect on %s", registry.ErrStateConflict, fmtDate(*latest))
		}
		prior, err := publishedState(ctx, tx, tid, set.SetID)
		if err != nil {
			return err
		}
		if err := domain.CheckEntries(prior, req.Entries, set.AllowCustom); err != nil {
			return fmt.Errorf("%w: %v", registry.ErrInvalidInput, err)
		}
		authority := strings.TrimSpace(req.SourceAuthority)
		if authority == "" {
			authority = set.SourceAuthority
		}
		published, _ := time.Parse(time.RFC3339, req.SourcePublishedAt)
		id := uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO reference_releases (release_id, tenant_id, set_id, release_version, status, source_published_at, effective_from, source_authority,
			compatibility, checksum, evidence_ref, staged_by, correlation_id) VALUES ($1,$2,$3,$4,'STAGED',$5,$6,$7,$8,NULLIF($9,''),$10,$11,NULLIF($12,''))`,
			id, tid, set.SetID, req.ReleaseVersion, published, eff, authority, req.Compatibility, req.Checksum, strings.TrimSpace(req.EvidenceRef), actor, req.CorrelationID)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: release %s of %s already exists", registry.ErrConflict, req.ReleaseVersion, setCode)
			}
			return err
		}
		for _, e := range req.Entries {
			origin := e.OriginOrDefault()
			// Retiring or superseding a code does not change what kind of code it is.
			if e.ChangeType == domain.ChangeRetire || e.ChangeType == domain.ChangeSupersede {
				origin = prior[e.Code].Origin
			}
			meta := e.Metadata
			if meta == nil {
				meta = map[string]any{}
			}
			mb, err := json.Marshal(meta)
			if err != nil {
				return fmt.Errorf("%w: %s metadata is not valid JSON", registry.ErrInvalidInput, e.Code)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO reference_entries (entry_id, tenant_id, set_id, release_id, code, change_type, label, origin, metadata, replaced_by_code, reason)
				VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9::jsonb,NULLIF($10,''),NULLIF($11,''))`,
				uuid.NewString(), tid, set.SetID, id, e.Code, string(e.ChangeType), strings.TrimSpace(e.Label), origin, string(mb), e.ReplacedByCode, strings.TrimSpace(e.Reason)); err != nil {
				return err
			}
		}
		out, err = scanRefRelease(tx.QueryRow(ctx, refReleaseSelect+` WHERE r.release_id = $1`, id), setCode)
		if err != nil {
			return err
		}
		if err := withCounts(ctx, tx, out); err != nil {
			return err
		}
		rec, err := events.ReferenceReleaseRecord(events.EventReferenceReleaseStaged, tid, set.SetID, *out, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// PublishReferenceRelease puts a staged release in force.
func (s *PgStore) PublishReferenceRelease(ctx context.Context, setCode, version string, req domain.PublishReferenceReleaseRequest, actor string) (*domain.ReferenceRelease, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ReferenceRelease
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		set, err := lockRefSet(ctx, tx, tid, setCode)
		if err != nil {
			return err
		}
		rel, err := scanRefRelease(tx.QueryRow(ctx, refReleaseSelect+` WHERE r.tenant_id = $1 AND r.set_id = $2 AND r.release_version = $3 FOR UPDATE OF r`, tid, set.SetID, version), setCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if rel.Status != domain.ReleaseStaged {
			return fmt.Errorf("%w: release %s is already %s", registry.ErrStateConflict, version, rel.Status)
		}
		// The release gate: whoever staged a release does not publish it.
		if rel.StagedBy == actor {
			return fmt.Errorf("%w: a release is published by someone other than the principal who staged it", registry.ErrSelfApproval)
		}
		eff, _ := domain.ParseDate(rel.EffectiveFrom)
		if latest, err := latestEffective(ctx, tx, tid, set.SetID); err != nil {
			return err
		} else if latest != nil && !eff.After(*latest) {
			return fmt.Errorf("%w: another release took effect on %s since this was staged; stage it again", registry.ErrStateConflict, fmtDate(*latest))
		}
		prior, err := publishedState(ctx, tx, tid, set.SetID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT code, change_type, origin, COALESCE(replaced_by_code,'') FROM reference_entries WHERE release_id = $1`, rel.ReleaseID)
		if err != nil {
			return err
		}
		var entries []domain.ReferenceEntryInput
		for rows.Next() {
			var e domain.ReferenceEntryInput
			var ct string
			if err := rows.Scan(&e.Code, &ct, &e.Origin, &e.ReplacedByCode); err != nil {
				rows.Close()
				return err
			}
			e.ChangeType = domain.ReferenceChange(ct)
			entries = append(entries, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := domain.CheckEntries(prior, entries, set.AllowCustom); err != nil {
			return fmt.Errorf("%w: the set changed since this release was staged: %v", registry.ErrStateConflict, err)
		}
		var seq int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(publish_seq),0) + 1 FROM reference_releases WHERE set_id = $1`, set.SetID).Scan(&seq); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE reference_releases SET status = 'PUBLISHED', released_by = $2, released_at = now(), release_evidence = $3, publish_seq = $4 WHERE release_id = $1`,
			rel.ReleaseID, actor, strings.TrimSpace(req.EvidenceRef), seq); err != nil {
			return err
		}
		out, err = scanRefRelease(tx.QueryRow(ctx, refReleaseSelect+` WHERE r.release_id = $1`, rel.ReleaseID), setCode)
		if err != nil {
			return err
		}
		if err := withCounts(ctx, tx, out); err != nil {
			return err
		}
		rec, err := events.ReferenceReleaseRecord(events.EventReferenceReleasePublished, tid, set.SetID, *out, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ListReferenceReleases lists a set's releases, oldest effective date first.
func (s *PgStore) ListReferenceReleases(ctx context.Context, setCode string) ([]domain.ReferenceRelease, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.ReferenceRelease{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		set, err := scanRefSet(tx.QueryRow(ctx, refSetSelect+` WHERE s.tenant_id = $1 AND s.set_code = $2`, tid, setCode))
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, refReleaseSelect+` WHERE r.tenant_id = $1 AND r.set_id = $2 ORDER BY r.effective_from, r.staged_at`, tid, set.SetID)
		if err != nil {
			return err
		}
		for rows.Next() {
			r, err := scanRefRelease(rows, setCode)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, *r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if err := withCounts(ctx, tx, &out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ── resolution ──────────────────────────────────────────────────────────────

// ResolveReference returns a code as it stood on a date, or under one exact release.
func (s *PgStore) ResolveReference(ctx context.Context, q domain.ReferenceQuery) (*domain.ReferenceResolution, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ReferenceResolution
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		set, err := scanRefSet(tx.QueryRow(ctx, refSetSelect+` WHERE s.tenant_id = $1 AND s.set_code = $2`, tid, q.SetCode))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: reference set %s", registry.ErrNotFound, q.SetCode)
		}
		if err != nil {
			return err
		}
		day := asOfDate(q.AsOf)
		// A pinned release is read whole, whatever the date: the release is what controls the interpretation.
		var pinSeq *int
		if q.Release != "" {
			var seq *int
			err := tx.QueryRow(ctx, `SELECT publish_seq FROM reference_releases WHERE tenant_id = $1 AND set_id = $2 AND release_version = $3 AND status = 'PUBLISHED'`,
				tid, set.SetID, q.Release).Scan(&seq)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && seq == nil) {
				return fmt.Errorf("%w: release %s of %s is not published", registry.ErrNotFound, q.Release, q.SetCode)
			}
			if err != nil {
				return err
			}
			pinSeq = seq
		}
		rows, err := tx.Query(ctx, `SELECT r.release_version, r.effective_from, r.source_published_at, r.source_authority, r.compatibility, COALESCE(r.checksum,''), r.evidence_ref, r.publish_seq,
			e.change_type, COALESCE(e.label,''), e.origin, e.metadata, COALESCE(e.replaced_by_code,''), COALESCE(e.reason,'')
			FROM reference_entries e JOIN reference_releases r ON r.tenant_id = e.tenant_id AND r.release_id = e.release_id
			WHERE e.tenant_id = $1 AND e.set_id = $2 AND e.code = $3 AND r.status = 'PUBLISHED'
			  AND ($4::int IS NOT NULL AND r.publish_seq <= $4::int OR $4::int IS NULL AND r.effective_from <= $5::date)
			ORDER BY r.publish_seq`, tid, set.SetID, q.Code, pinSeq, day)
		if err != nil {
			return err
		}
		defer rows.Close()
		var history []domain.ReferenceHistoryItem
		for rows.Next() {
			var h domain.ReferenceHistoryItem
			var eff time.Time
			var ct string
			if err := rows.Scan(&h.Release.ReleaseVersion, &eff, &h.Release.SourcePublishedAt, &h.Release.SourceAuthority, &h.Release.Compatibility, &h.Release.Checksum,
				&h.Release.EvidenceRef, &h.Release.PublishSeq, &ct, &h.Label, &h.Origin, &h.Metadata, &h.ReplacedByCode, &h.Reason); err != nil {
				return err
			}
			h.Release.EffectiveFrom, h.ChangeType = fmtDate(eff), domain.ReferenceChange(ct)
			history = append(history, h)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		res := domain.ResolveReference(history)
		if res == nil {
			return fmt.Errorf("%w: %s is not a code of %s on %s", registry.ErrNotFound, q.Code, q.SetCode, fmtDate(day))
		}
		res.SetCode, res.Code, res.AsOf, res.PinnedRelease = q.SetCode, q.Code, fmtDate(day), q.Release
		if !q.History {
			res.History = nil
		}
		// The codes this one took over from, under the same date or release.
		sr, err := tx.Query(ctx, `SELECT DISTINCT e.code FROM reference_entries e JOIN reference_releases r ON r.tenant_id = e.tenant_id AND r.release_id = e.release_id
			WHERE e.tenant_id = $1 AND e.set_id = $2 AND e.replaced_by_code = $3 AND r.status = 'PUBLISHED'
			  AND ($4::int IS NOT NULL AND r.publish_seq <= $4::int OR $4::int IS NULL AND r.effective_from <= $5::date) ORDER BY e.code`, tid, set.SetID, q.Code, pinSeq, day)
		if err != nil {
			return err
		}
		defer sr.Close()
		for sr.Next() {
			var c string
			if err := sr.Scan(&c); err != nil {
				return err
			}
			res.Supersedes = append(res.Supersedes, c)
		}
		if err := sr.Err(); err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}
