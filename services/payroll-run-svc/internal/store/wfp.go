package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payroll-run-svc/internal/domain"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
)

// EventBuilder renders the outbox event for a committed object. It runs INSIDE
// the transaction, so the event exists if and only if the change does. Nil means
// the change emits nothing.
type EventBuilder[T any] func(*T) (domain.OutboxEvent, error)

func enqueue[T any](ctx context.Context, tx pgx.Tx, obj *T, mk EventBuilder[T]) error {
	if mk == nil {
		return nil
	}
	ev, err := mk(obj)
	if err != nil {
		return fmt.Errorf("build event: %w", err)
	}
	if !domain.EventCatalogue[ev.EventType] {
		return fmt.Errorf("event type %q is not in the WFP catalogue", ev.EventType)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload)
		VALUES ($1, $2, $3, $4)`, ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
	}
	return nil
}

func tenantOf(ctx context.Context) (string, error) {
	t := svcmiddleware.TenantFromContext(ctx)
	if t == "" {
		return "", domain.ErrIdentityMissing
	}
	return t, nil
}

// ── source trust profiles ────────────────────────────────────────────────────

const trustCols = `profile_id, tenant_id, source_system_id, source_kind, status, allowed_capabilities,
	certification_ref, certified_at, version, created_by, created_at, updated_at`

func scanTrust(row pgx.Row) (*domain.SourceTrustProfile, error) {
	var p domain.SourceTrustProfile
	err := row.Scan(&p.ProfileID, &p.TenantID, &p.SourceSystemID, &p.SourceKind, &p.Status, &p.AllowedCapabilities,
		&p.CertificationRef, &p.CertifiedAt, &p.Version, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTrustProfileNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// CreateSourceTrustProfile registers a source in PENDING_CERTIFICATION.
// Idempotent: the same source_system_id with the same kind and capabilities
// returns the existing profile; different content is a conflict, never an overwrite.
func (s *PgStore) CreateSourceTrustProfile(ctx context.Context, p *domain.SourceTrustProfile, mk EventBuilder[domain.SourceTrustProfile]) (created bool, err error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return false, err
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO wfp_source_trust_profiles
				(profile_id, tenant_id, source_system_id, source_kind, status, allowed_capabilities, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, source_system_id) DO NOTHING`,
			p.ProfileID, tenantID, p.SourceSystemID, p.SourceKind, domain.TrustPendingCertification, p.AllowedCapabilities, p.CreatedBy)
		if err != nil {
			return err
		}
		existing, err := scanTrust(tx.QueryRow(ctx, `SELECT `+trustCols+` FROM wfp_source_trust_profiles
			WHERE tenant_id = $1 AND source_system_id = $2`, tenantID, p.SourceSystemID))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if existing.SourceKind != p.SourceKind || !sameStrings(existing.AllowedCapabilities, p.AllowedCapabilities) {
				return domain.ErrTrustProfileConflict
			}
		} else {
			created = true
			if err := enqueue(ctx, tx, existing, mk); err != nil {
				return err
			}
		}
		*p = *existing
		return nil
	})
	return created, err
}

func (s *PgStore) GetSourceTrustProfile(ctx context.Context, id string) (*domain.SourceTrustProfile, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var p *domain.SourceTrustProfile
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		p, e = scanTrust(tx.QueryRow(ctx, `SELECT `+trustCols+` FROM wfp_source_trust_profiles
			WHERE tenant_id = $1 AND profile_id = $2`, tenantID, id))
		return e
	})
	return p, err
}

func (s *PgStore) GetSourceTrustProfileBySource(ctx context.Context, sourceSystemID string) (*domain.SourceTrustProfile, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var p *domain.SourceTrustProfile
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		p, e = scanTrust(tx.QueryRow(ctx, `SELECT `+trustCols+` FROM wfp_source_trust_profiles
			WHERE tenant_id = $1 AND source_system_id = $2`, tenantID, sourceSystemID))
		return e
	})
	return p, err
}

func (s *PgStore) ListSourceTrustProfiles(ctx context.Context, status string) ([]domain.SourceTrustProfile, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.SourceTrustProfile
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+trustCols+` FROM wfp_source_trust_profiles
			WHERE tenant_id = $1 AND ($2 = '' OR status = $2) ORDER BY source_system_id`, tenantID, status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanTrust(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

// TransitionSourceTrustProfile moves a profile along trustEdges under an
// expected_version check. Activation requires certificationRef (or an existing
// one, for reinstating a suspended source).
func (s *PgStore) TransitionSourceTrustProfile(ctx context.Context, id string, expectedVersion int, to, certificationRef string, mk EventBuilder[domain.SourceTrustProfile]) (*domain.SourceTrustProfile, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.SourceTrustProfile
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanTrust(tx.QueryRow(ctx, `SELECT `+trustCols+` FROM wfp_source_trust_profiles
			WHERE tenant_id = $1 AND profile_id = $2 FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrVersionConflict
		}
		if !domain.TrustTransitionAllowed(cur.Status, to) {
			return domain.ErrInvalidTrustTransition
		}
		ref := cur.CertificationRef
		certifiedAt := cur.CertifiedAt
		if to == domain.TrustActive {
			if certificationRef != "" {
				now := time.Now().UTC()
				ref, certifiedAt = &certificationRef, &now
			}
			if ref == nil {
				return domain.ErrCertificationRequired
			}
		}
		out, err = scanTrust(tx.QueryRow(ctx, `
			UPDATE wfp_source_trust_profiles
			SET status = $3, certification_ref = $4, certified_at = $5, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND profile_id = $2
			RETURNING `+trustCols, tenantID, id, to, ref, certifiedAt))
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, out, mk)
	})
	return out, err
}

// ── rule manifests ───────────────────────────────────────────────────────────

const manifestCols = `manifest_id, tenant_id, source_system_id, jurisdiction_id, manifest_version, manifest_digest,
	effective_from::text, effective_to::text, evidence_ref, registered_by, created_at`

func scanManifest(row pgx.Row) (*domain.PayrollRuleManifest, error) {
	var m domain.PayrollRuleManifest
	err := row.Scan(&m.ManifestID, &m.TenantID, &m.SourceSystemID, &m.JurisdictionID, &m.ManifestVersion, &m.ManifestDigest,
		&m.EffectiveFrom, &m.EffectiveTo, &m.EvidenceRef, &m.RegisteredBy, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrManifestNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// RegisterRuleManifest appends a manifest. Re-registering the identical
// (source, jurisdiction, version, digest) returns the existing row.
func (s *PgStore) RegisterRuleManifest(ctx context.Context, m *domain.PayrollRuleManifest, mk EventBuilder[domain.PayrollRuleManifest]) (created bool, err error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return false, err
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO wfp_payroll_rule_manifests
				(manifest_id, tenant_id, source_system_id, jurisdiction_id, manifest_version, manifest_digest,
				 effective_from, effective_to, evidence_ref, registered_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7::date, $8::date, $9, $10)
			ON CONFLICT (tenant_id, source_system_id, jurisdiction_id, manifest_version, manifest_digest) DO NOTHING`,
			m.ManifestID, tenantID, m.SourceSystemID, m.JurisdictionID, m.ManifestVersion, m.ManifestDigest,
			m.EffectiveFrom, m.EffectiveTo, m.EvidenceRef, m.RegisteredBy)
		if err != nil {
			return err
		}
		stored, err := scanManifest(tx.QueryRow(ctx, `SELECT `+manifestCols+` FROM wfp_payroll_rule_manifests
			WHERE tenant_id = $1 AND source_system_id = $2 AND jurisdiction_id = $3
			  AND manifest_version = $4 AND manifest_digest = $5`,
			tenantID, m.SourceSystemID, m.JurisdictionID, m.ManifestVersion, m.ManifestDigest))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			if err := enqueue(ctx, tx, stored, mk); err != nil {
				return err
			}
		}
		*m = *stored
		return nil
	})
	return created, err
}

func (s *PgStore) GetRuleManifest(ctx context.Context, id string) (*domain.PayrollRuleManifest, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var m *domain.PayrollRuleManifest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		m, e = scanManifest(tx.QueryRow(ctx, `SELECT `+manifestCols+` FROM wfp_payroll_rule_manifests
			WHERE tenant_id = $1 AND manifest_id = $2`, tenantID, id))
		return e
	})
	return m, err
}

// ResolveRuleManifest returns the manifest in force for a source and
// jurisdiction on asOf (the pay date, never "today"). With overlapping windows
// the latest effective_from wins, then the most recently registered. No match
// is ErrManifestNotFound: the caller fails closed rather than guessing.
func (s *PgStore) ResolveRuleManifest(ctx context.Context, sourceSystemID, jurisdictionID, asOf string) (*domain.PayrollRuleManifest, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var m *domain.PayrollRuleManifest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		m, e = scanManifest(tx.QueryRow(ctx, `SELECT `+manifestCols+` FROM wfp_payroll_rule_manifests
			WHERE tenant_id = $1 AND source_system_id = $2 AND jurisdiction_id = $3
			  AND effective_from <= $4::date AND (effective_to IS NULL OR effective_to > $4::date)
			ORDER BY effective_from DESC, created_at DESC LIMIT 1`, tenantID, sourceSystemID, jurisdictionID, asOf))
		return e
	})
	return m, err
}

// ── payroll run references ───────────────────────────────────────────────────

const runRefCols = `run_ref_id, tenant_id, legal_entity_id, source_system_id, source_run_id, source_run_version,
	run_kind, payroll_period_id, pay_date::text, rule_manifest_id::text, payroll_run_id::text, acceptance_state,
	supersedes_run_ref_id::text, identity_fingerprint, result_fingerprint, correlation_id, causation_id,
	version, created_by, created_at, updated_at`

func scanRunRef(row pgx.Row) (*domain.PayrollRunRef, error) {
	var r domain.PayrollRunRef
	err := row.Scan(&r.RunRefID, &r.TenantID, &r.LegalEntityID, &r.SourceSystemID, &r.SourceRunID, &r.SourceRunVersion,
		&r.RunKind, &r.PayrollPeriodID, &r.PayDate, &r.RuleManifestID, &r.PayrollRunID, &r.AcceptanceState,
		&r.SupersedesRunRefID, &r.IdentityFingerprint, &r.ResultFingerprint, &r.CorrelationID, &r.CausationID,
		&r.Version, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRunRefNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// RegisterPayrollRunRef records a source run version in RECEIVED.
//
// Replay of the same identity returns the existing ref. The same (source run,
// version) arriving with a different identity is refused: a materially changed
// run must carry a new source_run_version (WFP-NP-015). A version above 1 is
// linked to its predecessor, which must already be registered.
func (s *PgStore) RegisterPayrollRunRef(ctx context.Context, r *domain.PayrollRunRef, mk EventBuilder[domain.PayrollRunRef]) (created bool, err error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return false, err
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		created, e = registerRunRefTx(ctx, tx, tenantID, r)
		if e != nil || !created {
			return e
		}
		return enqueue(ctx, tx, r, mk)
	})
	return created, err
}

// registerRunRefTx is RegisterPayrollRunRef inside the caller's transaction, so the
// result command can register and decide in ONE transaction. created is false when
// the identical identity was already on record (r is then the stored row).
func registerRunRefTx(ctx context.Context, tx pgx.Tx, tenantID string, r *domain.PayrollRunRef) (created bool, err error) {
	existing, err := scanRunRef(tx.QueryRow(ctx, `SELECT `+runRefCols+` FROM wfp_payroll_run_refs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND source_system_id = $3
			  AND source_run_id = $4 AND source_run_version = $5`,
		tenantID, r.LegalEntityID, r.SourceSystemID, r.SourceRunID, r.SourceRunVersion))
	if err == nil {
		if existing.IdentityFingerprint != r.IdentityFingerprint {
			return false, domain.ErrRunIdentityConflict
		}
		*r = *existing
		return false, nil
	}
	if !errors.Is(err, domain.ErrRunRefNotFound) {
		return false, err
	}

	if r.SourceRunVersion > 1 {
		var prior string
		err := tx.QueryRow(ctx, `SELECT run_ref_id::text FROM wfp_payroll_run_refs
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND source_system_id = $3
				  AND source_run_id = $4 AND source_run_version = $5`,
			tenantID, r.LegalEntityID, r.SourceSystemID, r.SourceRunID, r.SourceRunVersion-1).Scan(&prior)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, domain.ErrPriorVersionMissing
		}
		if err != nil {
			return false, err
		}
		r.SupersedesRunRefID = &prior
	}

	stored, err := scanRunRef(tx.QueryRow(ctx, `
			INSERT INTO wfp_payroll_run_refs
				(run_ref_id, tenant_id, legal_entity_id, source_system_id, source_run_id, source_run_version,
				 run_kind, payroll_period_id, pay_date, rule_manifest_id, payroll_run_id, supersedes_run_ref_id,
				 identity_fingerprint, correlation_id, causation_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::date, $10::uuid, $11::uuid, $12::uuid, $13, $14, $15, $16)
			RETURNING `+runRefCols,
		r.RunRefID, tenantID, r.LegalEntityID, r.SourceSystemID, r.SourceRunID, r.SourceRunVersion,
		r.RunKind, r.PayrollPeriodID, r.PayDate, r.RuleManifestID, r.PayrollRunID, r.SupersedesRunRefID,
		r.IdentityFingerprint, r.CorrelationID, r.CausationID, r.CreatedBy))
	if err != nil {
		return false, err
	}
	*r = *stored
	return true, nil
}

func (s *PgStore) GetPayrollRunRef(ctx context.Context, id string) (*domain.PayrollRunRef, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var r *domain.PayrollRunRef
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		r, e = scanRunRef(tx.QueryRow(ctx, `SELECT `+runRefCols+` FROM wfp_payroll_run_refs
			WHERE tenant_id = $1 AND run_ref_id = $2`, tenantID, id))
		return e
	})
	return r, err
}

type RunRefFilter struct {
	LegalEntityID  string
	SourceSystemID string
	SourceRunID    string
	State          string
}

func (s *PgStore) ListPayrollRunRefs(ctx context.Context, f RunRefFilter) ([]domain.PayrollRunRef, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.PayrollRunRef
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+runRefCols+` FROM wfp_payroll_run_refs
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id = $2) AND ($3 = '' OR source_system_id = $3)
			  AND ($4 = '' OR source_run_id = $4) AND ($5 = '' OR acceptance_state = $5)
			ORDER BY source_system_id, source_run_id, source_run_version LIMIT 500`,
			tenantID, f.LegalEntityID, f.SourceSystemID, f.SourceRunID, f.State)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRunRef(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// TransitionPayrollRunRef moves the acceptance state along the edge table under
// an expected_version check. WFP-04 (Wave 4) is the caller for ACCEPTED, and the
// one that supplies resultFingerprint; this wave exposes no route to it.
func (s *PgStore) TransitionPayrollRunRef(ctx context.Context, id string, expectedVersion int, to, resultFingerprint string, mk EventBuilder[domain.PayrollRunRef]) (*domain.PayrollRunRef, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PayrollRunRef
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanRunRef(tx.QueryRow(ctx, `SELECT `+runRefCols+` FROM wfp_payroll_run_refs
			WHERE tenant_id = $1 AND run_ref_id = $2 FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrVersionConflict
		}
		if !domain.AcceptanceTransitionAllowed(cur.AcceptanceState, to) {
			return domain.ErrInvalidAcceptanceMove
		}
		var fp *string
		if resultFingerprint != "" {
			fp = &resultFingerprint
		}
		out, err = scanRunRef(tx.QueryRow(ctx, `
			UPDATE wfp_payroll_run_refs
			SET acceptance_state = $3, result_fingerprint = COALESCE($4, result_fingerprint),
			    version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND run_ref_id = $2
			RETURNING `+runRefCols, tenantID, id, to, fp))
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, out, mk)
	})
	return out, err
}

// ── outbox ───────────────────────────────────────────────────────────────────

// OutboxRecord is one claimed, unpublished event.
type OutboxRecord struct {
	OutboxID  int64
	EventType string
	Key       string
	Body      []byte
}

// withRelay runs fn with app.outbox_relay installed instead of a tenant: the
// relay drains every tenant's backlog and is admitted by the outbox policy's
// explicit second disjunct.
func (s *PgStore) withRelay(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin relay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if s.schema != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('search_path', $1, true)", s.schema); err != nil {
			return fmt.Errorf("set search_path: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return fmt.Errorf("set relay context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit relay transaction: %w", err)
	}
	return nil
}

// ClaimOutbox takes up to limit unpublished events, oldest first, locking them
// (FOR UPDATE SKIP LOCKED, so replicas never double-publish) while fn publishes.
// At-least-once: a crash mid-publish rolls the marking back and re-delivers.
func (s *PgStore) ClaimOutbox(ctx context.Context, limit int, fn func([]OutboxRecord) error) error {
	return s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT outbox_id, event_type, object_id, payload
			FROM wfp_outbox
			WHERE published_at IS NULL
			ORDER BY created_at, outbox_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("claim outbox: %w", err)
		}
		var claimed []OutboxRecord
		for rows.Next() {
			var r OutboxRecord
			if err := rows.Scan(&r.OutboxID, &r.EventType, &r.Key, &r.Body); err != nil {
				rows.Close()
				return fmt.Errorf("scan outbox row: %w", err)
			}
			claimed = append(claimed, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read outbox rows: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}
		ids := make([]int64, 0, len(claimed))
		for _, r := range claimed {
			ids = append(ids, r.OutboxID)
		}
		if err := fn(claimed); err != nil {
			if _, uerr := tx.Exec(ctx, `UPDATE wfp_outbox SET attempts = attempts + 1, last_error = $2
				WHERE outbox_id = ANY($1)`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE wfp_outbox SET published_at = now() WHERE outbox_id = ANY($1)`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) FROM wfp_outbox WHERE published_at IS NULL`).Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}
