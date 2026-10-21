package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/employee-master-svc/internal/domain"
	svcmiddleware "zoiko.io/employee-master-svc/internal/middleware"
)

// EventBuilder renders the outbox events for a committed change. It runs INSIDE
// the transaction, so an event exists if and only if the change does.
type EventBuilder func(p *domain.WorkerFinancialProfile, superseded []domain.WorkerFinancialProfile) ([]domain.OutboxEvent, error)

// SyncResult tells the caller what a sync did.
type SyncResult struct {
	Profile    *domain.WorkerFinancialProfile
	Created    bool // false: an identical source_version was already on record
	Superseded []domain.WorkerFinancialProfile
}

func tenantOf(ctx context.Context) (string, error) {
	t := svcmiddleware.TenantFromContext(ctx)
	if t == "" {
		return "", domain.ErrIdentityMissing
	}
	return t, nil
}

func enqueue(ctx context.Context, tx pgx.Tx, events []domain.OutboxEvent) error {
	for _, ev := range events {
		if !domain.EventCatalogue[ev.EventType] {
			return fmt.Errorf("event type %q is not in the WFP-01 catalogue", ev.EventType)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
			ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload); err != nil {
			return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
		}
	}
	return nil
}

const profileCols = `profile_id, tenant_id, worker_id::text, assignment_id, legal_entity_id, payroll_jurisdiction_id,
	pay_currency, employment_classification, cost_center, dimensions, payee_ref, payee_ref_status, payee_ref_changed,
	effective_from::text, effective_to::text, source_system_id, source_version, source_fingerprint, state,
	quarantine_reason, supersedes_profile_id::text, version, correlation_id, created_by, created_at, updated_at`

func scanProfile(row pgx.Row) (*domain.WorkerFinancialProfile, error) {
	var p domain.WorkerFinancialProfile
	var dims []byte
	err := row.Scan(&p.ProfileID, &p.TenantID, &p.WorkerID, &p.AssignmentID, &p.LegalEntityID, &p.PayrollJurisdictionID,
		&p.PayCurrency, &p.EmploymentClassification, &p.CostCenter, &dims, &p.PayeeRef, &p.PayeeRefStatus, &p.PayeeRefChanged,
		&p.EffectiveFrom, &p.EffectiveTo, &p.SourceSystemID, &p.SourceVersion, &p.SourceFingerprint, &p.State,
		&p.QuarantineReason, &p.SupersedesProfileID, &p.Version, &p.CorrelationID, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Dimensions = map[string]string{}
	if len(dims) > 0 {
		if err := json.Unmarshal(dims, &p.Dimensions); err != nil {
			return nil, fmt.Errorf("decode dimensions: %w", err)
		}
	}
	return &p, nil
}

// lockAssignment serializes writers on one worker assignment, so two racing
// source versions cannot both pass the stale-version and overlap checks.
func lockAssignment(ctx context.Context, tx pgx.Tx, tenantID, workerID, assignmentID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenantID+"|"+workerID+"|"+assignmentID)
	return err
}

func workerFacts(ctx context.Context, tx pgx.Tx, tenantID, workerID string) (domain.WorkerFacts, error) {
	var w domain.WorkerFacts
	err := tx.QueryRow(ctx, `SELECT legal_entity_id, status, termination_date::text FROM employees
		WHERE tenant_id = $1 AND employee_id = $2`, tenantID, workerID).Scan(&w.LegalEntityID, &w.Status, &w.TerminationDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.WorkerFacts{}, nil
	}
	if err != nil {
		return domain.WorkerFacts{}, err
	}
	w.Found = true
	return w, nil
}

// SyncWorkerFinancialProfile records a source delivery.
//
//   - Replay of an unchanged source_version returns the stored row.
//   - The same source_version with different content is ErrSourceVersionConflict.
//   - A lower source_version than one already received is ErrStaleSourceVersion
//     and leaves current state untouched.
//   - A worker/entity mapping failure (or preReason, from checks this service
//     cannot make itself, such as the jurisdiction registry) QUARANTINES the
//     profile: it is stored with its reason, affects nothing, and can be
//     revalidated. Nothing is ever defaulted.
//   - Otherwise it becomes ACTIVE; overlapping ACTIVE profiles are end-dated or
//     superseded in the same transaction.
func (s *PgStore) SyncWorkerFinancialProfile(ctx context.Context, p *domain.WorkerFinancialProfile, preReason string, mk EventBuilder) (*SyncResult, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res *SyncResult
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := lockAssignment(ctx, tx, tenantID, p.WorkerID, p.AssignmentID); err != nil {
			return err
		}

		existing, err := scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM wfp_worker_financial_profiles
			WHERE tenant_id = $1 AND source_system_id = $2 AND worker_id = $3::uuid AND assignment_id = $4 AND source_version = $5`,
			tenantID, p.SourceSystemID, p.WorkerID, p.AssignmentID, p.SourceVersion))
		if err == nil {
			if existing.SourceFingerprint != p.SourceFingerprint {
				return domain.ErrSourceVersionConflict
			}
			res = &SyncResult{Profile: existing}
			return nil
		}
		if !errors.Is(err, domain.ErrProfileNotFound) {
			return err
		}

		var maxVersion *int
		if err := tx.QueryRow(ctx, `SELECT max(source_version) FROM wfp_worker_financial_profiles
			WHERE tenant_id = $1 AND source_system_id = $2 AND worker_id = $3::uuid AND assignment_id = $4`,
			tenantID, p.SourceSystemID, p.WorkerID, p.AssignmentID).Scan(&maxVersion); err != nil {
			return err
		}
		if maxVersion != nil && *maxVersion > p.SourceVersion {
			return domain.ErrStaleSourceVersion
		}

		reason := preReason
		if reason == "" {
			facts, err := workerFacts(ctx, tx, tenantID, p.WorkerID)
			if err != nil {
				return err
			}
			reason = domain.MappingReason(facts, p.LegalEntityID, p.EffectiveFrom)
		}

		p.TenantID = tenantID
		var superseded []domain.WorkerFinancialProfile
		if reason != "" {
			p.State = domain.ProfileQuarantined
			p.QuarantineReason = &reason
		} else {
			p.State = domain.ProfileActive
			p.QuarantineReason = nil
			superseded, err = applyActive(ctx, tx, p)
			if err != nil {
				return err
			}
		}

		stored, err := insertProfile(ctx, tx, p)
		if err != nil {
			return err
		}
		if mk != nil {
			evs, err := mk(stored, superseded)
			if err != nil {
				return fmt.Errorf("build events: %w", err)
			}
			if err := enqueue(ctx, tx, evs); err != nil {
				return err
			}
		}
		res = &SyncResult{Profile: stored, Created: true, Superseded: superseded}
		return nil
	})
	return res, err
}

func insertProfile(ctx context.Context, tx pgx.Tx, p *domain.WorkerFinancialProfile) (*domain.WorkerFinancialProfile, error) {
	dims, err := json.Marshal(nonNilDims(p.Dimensions))
	if err != nil {
		return nil, err
	}
	payeeStatus := domain.PayeeNone
	if p.PayeeRef != nil {
		payeeStatus = domain.PayeeUnverified
	}
	return scanProfile(tx.QueryRow(ctx, `
		INSERT INTO wfp_worker_financial_profiles
			(profile_id, tenant_id, worker_id, assignment_id, legal_entity_id, payroll_jurisdiction_id, pay_currency,
			 employment_classification, cost_center, dimensions, payee_ref, payee_ref_status, payee_ref_changed,
			 effective_from, effective_to, source_system_id, source_version, source_fingerprint, state,
			 quarantine_reason, supersedes_profile_id, correlation_id, created_by)
		VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13, $14::date, $15::date,
		        $16, $17, $18, $19, $20, $21::uuid, $22, $23)
		RETURNING `+profileCols,
		p.ProfileID, p.TenantID, p.WorkerID, p.AssignmentID, p.LegalEntityID, p.PayrollJurisdictionID, p.PayCurrency,
		p.EmploymentClassification, p.CostCenter, string(dims), p.PayeeRef, payeeStatus, p.PayeeRefChanged,
		p.EffectiveFrom, p.EffectiveTo, p.SourceSystemID, p.SourceVersion, p.SourceFingerprint, p.State,
		p.QuarantineReason, p.SupersedesProfileID, p.CorrelationID, p.CreatedBy))
}

func nonNilDims(d map[string]string) map[string]string {
	if d == nil {
		return map[string]string{}
	}
	return d
}

// applyActive makes room for p (about to become ACTIVE) and fills in
// p.SupersedesProfileID and p.PayeeRefChanged. Existing ACTIVE profiles that
// overlap its window are:
//   - SUPERSEDED, when they start on or after p's start (p replaces them), or
//   - end-dated at p's start, when they began earlier (history stays valid for
//     the dates before p).
//
// A window that would split an existing profile is refused rather than guessed at.
func applyActive(ctx context.Context, tx pgx.Tx, p *domain.WorkerFinancialProfile) ([]domain.WorkerFinancialProfile, error) {
	rows, err := tx.Query(ctx, `SELECT `+profileCols+` FROM wfp_worker_financial_profiles
		WHERE tenant_id = $1 AND worker_id = $2::uuid AND assignment_id = $3 AND state = 'ACTIVE'
		  AND profile_id <> $4::uuid
		ORDER BY effective_from FOR UPDATE`, p.TenantID, p.WorkerID, p.AssignmentID, p.ProfileID)
	if err != nil {
		return nil, err
	}
	var all []domain.WorkerFinancialProfile
	for rows.Next() {
		prior, err := scanProfile(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, *prior)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Payee change is measured against the latest profile in force before p.
	var latest *domain.WorkerFinancialProfile
	for i := range all {
		if all[i].EffectiveFrom <= p.EffectiveFrom {
			latest = &all[i]
		}
	}
	if latest != nil {
		p.PayeeRefChanged = !sameStrPtr(latest.PayeeRef, p.PayeeRef)
		id := latest.ProfileID
		p.SupersedesProfileID = &id
	}

	var superseded []domain.WorkerFinancialProfile
	for _, prior := range all {
		if !overlaps(prior, p.EffectiveFrom, p.EffectiveTo) {
			continue
		}
		extendsPast := p.EffectiveTo != nil && (prior.EffectiveTo == nil || *prior.EffectiveTo > *p.EffectiveTo)
		if extendsPast {
			return nil, domain.ErrPartialOverlap
		}
		if prior.EffectiveFrom >= p.EffectiveFrom {
			up, err := scanProfile(tx.QueryRow(ctx, `UPDATE wfp_worker_financial_profiles
				SET state = 'SUPERSEDED', version = version + 1, updated_at = now()
				WHERE tenant_id = $1 AND profile_id = $2::uuid RETURNING `+profileCols, p.TenantID, prior.ProfileID))
			if err != nil {
				return nil, err
			}
			superseded = append(superseded, *up)
		} else {
			if _, err := tx.Exec(ctx, `UPDATE wfp_worker_financial_profiles
				SET effective_to = $3::date, version = version + 1, updated_at = now()
				WHERE tenant_id = $1 AND profile_id = $2::uuid`, p.TenantID, prior.ProfileID, p.EffectiveFrom); err != nil {
				return nil, err
			}
		}
	}
	return superseded, nil
}

func overlaps(prior domain.WorkerFinancialProfile, from string, to *string) bool {
	// [prior.from, prior.to) ∩ [from, to) ≠ ∅
	if prior.EffectiveTo != nil && *prior.EffectiveTo <= from {
		return false
	}
	if to != nil && *to <= prior.EffectiveFrom {
		return false
	}
	return true
}

func sameStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ── revalidate / deactivate ──────────────────────────────────────────────────

// RevalidateWorkerFinancialProfile re-runs the mapping checks on a QUARANTINED
// profile (for example after the missing worker has been created). If it now
// passes it becomes ACTIVE through the same supersession rules as a fresh sync;
// otherwise it stays quarantined with the current reason.
func (s *PgStore) RevalidateWorkerFinancialProfile(ctx context.Context, id string, expectedVersion int, preReason string, mk EventBuilder) (*SyncResult, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res *SyncResult
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM wfp_worker_financial_profiles
			WHERE tenant_id = $1 AND profile_id = $2::uuid`, tenantID, id))
		if err != nil {
			return err
		}
		if err := lockAssignment(ctx, tx, tenantID, cur.WorkerID, cur.AssignmentID); err != nil {
			return err
		}
		cur, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM wfp_worker_financial_profiles
			WHERE tenant_id = $1 AND profile_id = $2::uuid FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrProfileVersionConflict
		}
		if cur.State != domain.ProfileQuarantined {
			return domain.ErrProfileNotQuarantined
		}

		reason := preReason
		if reason == "" {
			facts, err := workerFacts(ctx, tx, tenantID, cur.WorkerID)
			if err != nil {
				return err
			}
			reason = domain.MappingReason(facts, cur.LegalEntityID, cur.EffectiveFrom)
		}
		if reason != "" {
			out, err := scanProfile(tx.QueryRow(ctx, `UPDATE wfp_worker_financial_profiles
				SET quarantine_reason = $3, version = version + 1, updated_at = now()
				WHERE tenant_id = $1 AND profile_id = $2::uuid RETURNING `+profileCols, tenantID, id, reason))
			if err != nil {
				return err
			}
			res = &SyncResult{Profile: out}
			return nil
		}

		// A newer source version received since this one was quarantined wins.
		var newer bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM wfp_worker_financial_profiles
			WHERE tenant_id = $1 AND source_system_id = $2 AND worker_id = $3::uuid AND assignment_id = $4
			  AND source_version > $5 AND state <> 'QUARANTINED')`,
			tenantID, cur.SourceSystemID, cur.WorkerID, cur.AssignmentID, cur.SourceVersion).Scan(&newer); err != nil {
			return err
		}
		if newer {
			return domain.ErrStaleSourceVersion
		}

		superseded, err := applyActive(ctx, tx, cur)
		if err != nil {
			return err
		}
		out, err := scanProfile(tx.QueryRow(ctx, `UPDATE wfp_worker_financial_profiles
			SET state = 'ACTIVE', quarantine_reason = NULL, payee_ref_changed = $3, supersedes_profile_id = $4::uuid,
			    version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND profile_id = $2::uuid RETURNING `+profileCols,
			tenantID, id, cur.PayeeRefChanged, cur.SupersedesProfileID))
		if err != nil {
			return err
		}
		if mk != nil {
			evs, err := mk(out, superseded)
			if err != nil {
				return fmt.Errorf("build events: %w", err)
			}
			if err := enqueue(ctx, tx, evs); err != nil {
				return err
			}
		}
		res = &SyncResult{Profile: out, Created: true, Superseded: superseded}
		return nil
	})
	return res, err
}

// DeactivateWorkerFinancialProfile ends an ACTIVE profile (DEACTIVATED is
// terminal and excluded from as-of resolution). Content is never edited.
func (s *PgStore) DeactivateWorkerFinancialProfile(ctx context.Context, id string, expectedVersion int, mk EventBuilder) (*domain.WorkerFinancialProfile, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.WorkerFinancialProfile
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM wfp_worker_financial_profiles
			WHERE tenant_id = $1 AND profile_id = $2::uuid FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrProfileVersionConflict
		}
		if cur.State != domain.ProfileActive {
			return domain.ErrProfileNotActive
		}
		out, err = scanProfile(tx.QueryRow(ctx, `UPDATE wfp_worker_financial_profiles
			SET state = 'DEACTIVATED', version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND profile_id = $2::uuid RETURNING `+profileCols, tenantID, id))
		if err != nil {
			return err
		}
		if mk != nil {
			evs, err := mk(out, nil)
			if err != nil {
				return fmt.Errorf("build events: %w", err)
			}
			return enqueue(ctx, tx, evs)
		}
		return nil
	})
	return out, err
}

// ── queries ──────────────────────────────────────────────────────────────────

func (s *PgStore) GetWorkerFinancialProfile(ctx context.Context, id string) (*domain.WorkerFinancialProfile, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var p *domain.WorkerFinancialProfile
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		p, e = scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM wfp_worker_financial_profiles
			WHERE tenant_id = $1 AND profile_id = $2::uuid`, tenantID, id))
		return e
	})
	return p, err
}

// ListProfileVersions returns every version (any state) for a worker, oldest first.
func (s *PgStore) ListProfileVersions(ctx context.Context, workerID, assignmentID string) ([]domain.WorkerFinancialProfile, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	return s.queryProfiles(ctx, tenantID, `
		WHERE tenant_id = $1 AND worker_id = $2::uuid AND ($3 = '' OR assignment_id = $3)
		ORDER BY assignment_id, source_version, created_at`, workerID, assignmentID)
}

// GetAsOfProfiles returns the ACTIVE profile(s) covering asOf, one per
// assignment. DEACTIVATED, QUARANTINED and SUPERSEDED profiles never answer.
func (s *PgStore) GetAsOfProfiles(ctx context.Context, workerID, assignmentID, asOf string) ([]domain.WorkerFinancialProfile, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	return s.queryProfiles(ctx, tenantID, `
		WHERE tenant_id = $1 AND worker_id = $2::uuid AND ($3 = '' OR assignment_id = $3) AND state = 'ACTIVE'
		  AND effective_from <= $4::date AND (effective_to IS NULL OR effective_to > $4::date)
		ORDER BY assignment_id`, workerID, assignmentID, asOf)
}

func (s *PgStore) queryProfiles(ctx context.Context, tenantID, where string, args ...any) ([]domain.WorkerFinancialProfile, error) {
	var out []domain.WorkerFinancialProfile
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+profileCols+` FROM wfp_worker_financial_profiles `+where,
			append([]any{tenantID}, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProfile(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

// WorkerFactsFor exposes the worker lookup for ValidateWorkerFinancialContext.
func (s *PgStore) WorkerFactsFor(ctx context.Context, workerID string) (domain.WorkerFacts, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return domain.WorkerFacts{}, err
	}
	var w domain.WorkerFacts
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		w, e = workerFacts(ctx, tx, tenantID, workerID)
		return e
	})
	return w, err
}

// ── outbox ───────────────────────────────────────────────────────────────────

type OutboxRecord struct {
	OutboxID  int64
	EventType string
	Key       string
	Body      []byte
}

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
			SELECT outbox_id, event_type, object_id, payload FROM wfp_outbox
			WHERE published_at IS NULL ORDER BY created_at, outbox_id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
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
