package store

// REF-03 exchange rates on Postgres (migration 000022). See registry.FXStore for the contract.
//
// The table holds the rules the service also enforces: a rate is positive and exact, an observation's content never
// changes, a manual or outlying rate is approved by someone other than its ingester, a set holds one rate per pair of
// its own type and date, a certified set is immutable, and nothing is deleted.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var _ registry.FXStore = (*PgStore)(nil)

func checkRecordVersion(supplied, current int64, allowUnversioned bool, noun string) error {
	if supplied == 0 {
		if !allowUnversioned {
			return registry.ErrVersionRequired
		}
		return nil
	}
	if supplied != current {
		return fmt.Errorf("%w: you supplied expected_version %d but this %s is at %d — reload and re-evaluate",
			registry.ErrVersionConflict, supplied, noun, current)
	}
	return nil
}

// ── sources ─────────────────────────────────────────────────────────────────

const fxSourceColumns = `source_id, tenant_id, source_code, name, source_class, timezone, status, record_version, created_by, created_at,
	COALESCE(withdrawn_by,''), withdrawn_at`

func scanFXSource(row pgx.Row) (*domain.FXSource, error) {
	var s domain.FXSource
	if err := row.Scan(&s.SourceID, &s.TenantID, &s.SourceCode, &s.Name, &s.SourceClass, &s.Timezone, &s.Status, &s.RecordVersion,
		&s.CreatedBy, &s.CreatedAt, &s.WithdrawnBy, &s.WithdrawnAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateFXSource registers and approves a source.
func (s *PgStore) CreateFXSource(ctx context.Context, req domain.CreateFXSourceRequest, actor string) (*domain.FXSource, error) {
	tid := domain.TenantFromContext(ctx)
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	var out *domain.FXSource
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		src, err := scanFXSource(tx.QueryRow(ctx, `INSERT INTO fx_sources (source_id, tenant_id, source_code, name, source_class, timezone, created_by, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')) RETURNING `+fxSourceColumns,
			uuid.NewString(), tid, strings.TrimSpace(req.SourceCode), strings.TrimSpace(req.Name), string(req.SourceClass), tz, actor, req.CorrelationID))
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a source with code %s already exists", registry.ErrConflict, req.SourceCode)
			}
			return err
		}
		out = src
		return nil
	})
	return out, err
}

// WithdrawFXSource withdraws a source.
func (s *PgStore) WithdrawFXSource(ctx context.Context, sourceID string, req domain.WithdrawFXSourceRequest, actor string, allowUnversioned bool) (*domain.FXSource, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(sourceID); err != nil {
		return nil, registry.ErrNotFound
	}
	var out *domain.FXSource
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := scanFXSource(tx.QueryRow(ctx, `SELECT `+fxSourceColumns+` FROM fx_sources WHERE source_id = $1 AND tenant_id = $2 FOR UPDATE`, sourceID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, cur.RecordVersion, allowUnversioned, "source"); err != nil {
			return err
		}
		if cur.Status != "ACTIVE" {
			return fmt.Errorf("%w: the source is already %s", registry.ErrStateConflict, cur.Status)
		}
		out, err = scanFXSource(tx.QueryRow(ctx, `UPDATE fx_sources SET status = 'WITHDRAWN', withdrawn_by = $3, withdrawn_at = now(), withdraw_reason = $4,
			record_version = record_version + 1 WHERE source_id = $1 AND tenant_id = $2 RETURNING `+fxSourceColumns, sourceID, tid, actor, strings.TrimSpace(req.Reason)))
		return err
	})
	return out, err
}

// ListFXSources lists the tenant's sources.
func (s *PgStore) ListFXSources(ctx context.Context) ([]domain.FXSource, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.FXSource{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+fxSourceColumns+` FROM fx_sources WHERE tenant_id = $1 ORDER BY source_code`, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			src, err := scanFXSource(rows)
			if err != nil {
				return err
			}
			out = append(out, *src)
		}
		return rows.Err()
	})
	return out, err
}

// ── observations ────────────────────────────────────────────────────────────

const fxObservationColumns = `observation_id, tenant_id, source_id, base_currency, quote_currency, rate_type, rate::text, market_date, observed_at,
	COALESCE(provider_observation_id,''), payload_ref, COALESCE(payload_hash,''), status, manual, outlier, needs_independent, checks,
	COALESCE(reason,''), COALESCE(evidence_ref,''), ingested_by, ingested_at, COALESCE(superseded_by_id::text,''), superseded_at`

func scanFXObservation(row pgx.Row) (*domain.FXObservation, error) {
	var o domain.FXObservation
	var md time.Time
	var checks []byte
	if err := row.Scan(&o.ObservationID, &o.TenantID, &o.SourceID, &o.BaseCurrency, &o.QuoteCurrency, &o.RateType, &o.Rate, &md, &o.ObservedAt,
		&o.ProviderObservationID, &o.PayloadRef, &o.PayloadHash, &o.Status, &o.Manual, &o.Outlier, &o.NeedsIndependent, &checks,
		&o.Reason, &o.EvidenceRef, &o.IngestedBy, &o.IngestedAt, &o.SupersededByID, &o.SupersededAt); err != nil {
		return nil, err
	}
	o.MarketDate, o.Rate = fmtDate(md), domain.CanonicalRate(o.Rate)
	if err := json.Unmarshal(checks, &o.Checks); err != nil {
		return nil, err
	}
	if o.Checks == nil {
		o.Checks = []domain.FXCheck{}
	}
	return &o, nil
}

// ingestTx records one observation inside tx. replayed is true when the same observation was recorded before.
func (s *PgStore) ingestTx(ctx context.Context, tx pgx.Tx, tid string, req domain.IngestFXObservationRequest, actor string) (*domain.FXObservation, bool, error) {
	if _, err := uuid.Parse(req.SourceID); err != nil {
		return nil, false, fmt.Errorf("%w: source_id names no source of this tenant", registry.ErrInvalidInput)
	}
	// Serialises two ingestions of one observation so the duplicate check cannot be raced past.
	key := req.ProviderObservationID
	if key == "" {
		key = req.BaseCurrency + req.QuoteCurrency + string(req.RateType) + req.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('fx-ingest:' || $1::text || ':' || $2 || ':' || $3, 0))`, tid, req.SourceID, key); err != nil {
		return nil, false, err
	}

	var class, status string
	if err := tx.QueryRow(ctx, `SELECT source_class, status FROM fx_sources WHERE source_id = $1 AND tenant_id = $2 FOR SHARE`, req.SourceID, tid).Scan(&class, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("%w: source_id names no source of this tenant", registry.ErrInvalidInput)
		}
		return nil, false, err
	}
	if status != "ACTIVE" {
		return nil, false, fmt.Errorf("%w: the source is %s and takes no new rates", registry.ErrStateConflict, status)
	}
	manual := domain.FXSourceClass(class) == domain.FXSourceManualExc
	if manual && (strings.TrimSpace(req.Reason) == "" || strings.TrimSpace(req.EvidenceRef) == "") {
		return nil, false, fmt.Errorf("%w: a manual exceptional rate carries its reason and source evidence", registry.ErrInvalidInput)
	}
	rate, _ := domain.ParseRate(req.Rate)
	canon := domain.FormatRate(rate)
	md, _ := domain.ParseDate(req.MarketDate)

	// Duplicate check: the provider's own observation id, or the same source, pair, type and instant.
	existing, err := scanFXObservation(tx.QueryRow(ctx, `SELECT `+fxObservationColumns+` FROM fx_observations WHERE tenant_id = $1 AND source_id = $2
		AND ((provider_observation_id = NULLIF($3,'') AND $3 <> '')
		     OR (base_currency = $4 AND quote_currency = $5 AND rate_type = $6 AND observed_at = $7)) LIMIT 1`,
		tid, req.SourceID, req.ProviderObservationID, req.BaseCurrency, req.QuoteCurrency, string(req.RateType), req.ObservedAt.UTC()))
	if err == nil {
		if domain.CanonicalRate(existing.Rate) != canon || existing.MarketDate != fmtDate(md) || existing.PayloadHash != req.PayloadHash {
			return nil, false, fmt.Errorf("%w: an observation with this key already exists with a different rate, market date or payload", registry.ErrConflict)
		}
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}

	// Outlier check against the last accepted rate of the pair and type, in exact arithmetic.
	checks := []domain.FXCheck{{Check: "source_approved", Result: "PASS"}, {Check: "duplicate", Result: "PASS"}}
	outlier := false
	var prev string
	err = tx.QueryRow(ctx, `SELECT rate::text FROM fx_observations WHERE tenant_id = $1 AND base_currency = $2 AND quote_currency = $3 AND rate_type = $4
		AND status IN ('VALIDATED','APPROVED') AND market_date <= $5 ORDER BY market_date DESC, observed_at DESC LIMIT 1`,
		tid, req.BaseCurrency, req.QuoteCurrency, string(req.RateType), md).Scan(&prev)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		checks = append(checks, domain.FXCheck{Check: "outlier", Result: "PASS", Detail: "no prior accepted rate to compare with"})
	case err != nil:
		return nil, false, err
	default:
		p, _ := domain.ParseRate(domain.CanonicalRate(prev))
		if p != nil && domain.OutlierExceeds(p, rate) {
			outlier = true
			checks = append(checks, domain.FXCheck{Check: "outlier", Result: "FLAGGED",
				Detail: "moved more than " + domain.FXOutlierDeviation + " from the last accepted rate " + domain.CanonicalRate(prev)})
		} else {
			checks = append(checks, domain.FXCheck{Check: "outlier", Result: "PASS", Detail: "within " + domain.FXOutlierDeviation + " of " + domain.CanonicalRate(prev)})
		}
	}
	if manual {
		checks = append(checks, domain.FXCheck{Check: "manual_exception", Result: "FLAGGED", Detail: "a manual exceptional rate needs independent approval"})
	}
	st := domain.FXValidated
	if manual || outlier {
		st = domain.FXObserved
	}
	cj, _ := json.Marshal(checks)
	o, err := scanFXObservation(tx.QueryRow(ctx, `INSERT INTO fx_observations
		(observation_id, tenant_id, source_id, base_currency, quote_currency, rate_type, rate, market_date, observed_at, provider_observation_id,
		 payload_ref, payload_hash, status, manual, outlier, needs_independent, checks, reason, evidence_ref, ingested_by, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,NULLIF($10,''),$11,NULLIF($12,''),$13,$14,$15,$16,$17,NULLIF($18,''),NULLIF($19,''),$20,NULLIF($21,''))
		RETURNING `+fxObservationColumns,
		uuid.NewString(), tid, req.SourceID, req.BaseCurrency, req.QuoteCurrency, string(req.RateType), canon, md, req.ObservedAt.UTC(),
		req.ProviderObservationID, strings.TrimSpace(req.PayloadRef), req.PayloadHash, st, manual, outlier, manual || outlier, cj,
		strings.TrimSpace(req.Reason), strings.TrimSpace(req.EvidenceRef), actor, req.CorrelationID))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, false, fmt.Errorf("%w: this observation was recorded meanwhile", registry.ErrConflict)
		}
		return nil, false, err
	}
	return o, false, nil
}

// IngestFXObservation records one observation.
func (s *PgStore) IngestFXObservation(ctx context.Context, req domain.IngestFXObservationRequest, actor string) (*registry.FXIngestOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.FXIngestOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		o, replayed, err := s.ingestTx(ctx, tx, tid, req, actor)
		if err != nil {
			return err
		}
		out = &registry.FXIngestOutcome{Observation: *o, Replayed: replayed}
		if replayed {
			return nil
		}
		rec, err := events.FXRateObservedRecord(*o, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// SupersedeFXObservation replaces an observation that is not yet in a rate set.
func (s *PgStore) SupersedeFXObservation(ctx context.Context, observationID string, req domain.SupersedeFXObservationRequest, actor string) (*registry.FXIngestOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(observationID); err != nil {
		return nil, registry.ErrNotFound
	}
	var out *registry.FXIngestOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		old, err := scanFXObservation(tx.QueryRow(ctx, `SELECT `+fxObservationColumns+` FROM fx_observations WHERE observation_id = $1 AND tenant_id = $2 FOR UPDATE`, observationID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if old.Status != domain.FXObserved && old.Status != domain.FXValidated {
			return fmt.Errorf("%w: the observation is %s; only an observation not yet in a rate set is replaced", registry.ErrStateConflict, old.Status)
		}
		// An observation in a rate set is APPROVED, so the status check above is also the "not in a set" check: a rate in a
		// set is corrected by a superseding set, never replaced in place.
		if old.BaseCurrency != req.BaseCurrency || old.QuoteCurrency != req.QuoteCurrency || old.RateType != req.RateType || old.MarketDate != req.MarketDate {
			return fmt.Errorf("%w: a replacement is for the same pair, rate type and market date", registry.ErrInvalidInput)
		}
		next, replayed, err := s.ingestTx(ctx, tx, tid, req.IngestFXObservationRequest, actor)
		if err != nil {
			return err
		}
		if replayed || next.ObservationID == old.ObservationID {
			return fmt.Errorf("%w: the replacement must be a different observation", registry.ErrInvalidInput)
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE fx_observations SET status = 'SUPERSEDED', superseded_by_id = $2, superseded_at = $3, supersede_reason = $4
			WHERE observation_id = $1`, old.ObservationID, next.ObservationID, now, strings.TrimSpace(req.Reason)); err != nil {
			return err
		}
		old.Status, old.SupersededByID, old.SupersededAt = domain.FXSuperseded, next.ObservationID, &now
		out = &registry.FXIngestOutcome{Observation: *next, Superseded: old}
		rec, err := events.FXRateObservedRecord(*next, req.CorrelationID)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		srec, err := events.FXRateSupersededRecord(*old, next.ObservationID, req.Reason, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, srec)
	})
	return out, err
}

// ── rate sets ───────────────────────────────────────────────────────────────

const fxSetColumns = `set_id, tenant_id, set_name, rate_type, market_date, set_version, status, COALESCE(supersedes_set_id::text,''),
	COALESCE(superseded_by_set_id::text,''), COALESCE(override_reason,''), evidence_ref, approved_by, approved_at, COALESCE(closed_by,''), closed_at, superseded_at`

func scanFXSet(row pgx.Row) (*domain.FXRateSet, error) {
	var s domain.FXRateSet
	var md time.Time
	if err := row.Scan(&s.SetID, &s.TenantID, &s.SetName, &s.RateType, &md, &s.SetVersion, &s.Status, &s.SupersedesSetID, &s.SupersededBySetID,
		&s.OverrideReason, &s.EvidenceRef, &s.ApprovedBy, &s.ApprovedAt, &s.ClosedBy, &s.ClosedAt, &s.SupersededAt); err != nil {
		return nil, err
	}
	s.MarketDate = fmtDate(md)
	return &s, nil
}

func loadFXSetMembers(ctx context.Context, tx pgx.Tx, tid string, set *domain.FXRateSet) error {
	rows, err := tx.Query(ctx, `SELECT `+fxObservationColumns+` FROM fx_observations WHERE tenant_id = $1
		AND observation_id IN (SELECT observation_id FROM fx_rate_set_members WHERE tenant_id = $1 AND set_id = $2)
		ORDER BY base_currency, quote_currency`, tid, set.SetID)
	if err != nil {
		return err
	}
	defer rows.Close()
	set.Members = []domain.FXObservation{}
	for rows.Next() {
		o, err := scanFXObservation(rows)
		if err != nil {
			return err
		}
		set.Members = append(set.Members, *o)
	}
	return rows.Err()
}

// GetFXRateSet returns a set with its members; (nil, nil) when absent.
func (s *PgStore) GetFXRateSet(ctx context.Context, setID string) (*domain.FXRateSet, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(setID); err != nil {
		return nil, nil
	}
	var out *domain.FXRateSet
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		set, err := scanFXSet(tx.QueryRow(ctx, `SELECT `+fxSetColumns+` FROM fx_rate_sets WHERE set_id = $1 AND tenant_id = $2`, setID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := loadFXSetMembers(ctx, tx, tid, set); err != nil {
			return err
		}
		out = set
		return nil
	})
	return out, err
}

// ApproveFXRateSet creates an approved set.
func (s *PgStore) ApproveFXRateSet(ctx context.Context, req domain.ApproveFXRateSetRequest, actor string) (*domain.FXRateSet, error) {
	tid := domain.TenantFromContext(ctx)
	name := strings.TrimSpace(req.SetName)
	if name == "" {
		name = "DEFAULT"
	}
	md, _ := domain.ParseDate(req.MarketDate)
	var out *domain.FXRateSet
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		version, supersedes := 1, ""
		var old *domain.FXRateSet
		if req.SupersedesSetID != "" {
			if _, err := uuid.Parse(req.SupersedesSetID); err != nil {
				return fmt.Errorf("%w: supersedes_set_id names no rate set of this tenant", registry.ErrInvalidInput)
			}
			o, err := scanFXSet(tx.QueryRow(ctx, `SELECT `+fxSetColumns+` FROM fx_rate_sets WHERE set_id = $1 AND tenant_id = $2 FOR UPDATE`, req.SupersedesSetID, tid))
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: supersedes_set_id names no rate set of this tenant", registry.ErrInvalidInput)
			}
			if err != nil {
				return err
			}
			if o.Status == domain.FXSetSuperseded {
				return fmt.Errorf("%w: that rate set was already superseded", registry.ErrStateConflict)
			}
			if o.SetName != name || o.RateType != req.RateType || o.MarketDate != fmtDate(md) {
				return fmt.Errorf("%w: a superseding set has the same name, rate type and market date as the set it corrects", registry.ErrInvalidInput)
			}
			if err := loadFXSetMembers(ctx, tx, tid, o); err != nil {
				return err
			}
			old, version, supersedes = o, o.SetVersion+1, o.SetID
		}

		// Lock the observations in a fixed order.
		ids := append([]string(nil), req.ObservationIDs...)
		sort.Strings(ids)
		for _, id := range ids {
			if _, err := uuid.Parse(id); err != nil {
				return fmt.Errorf("%w: observation %s does not exist", registry.ErrInvalidInput, id)
			}
		}
		rows, err := tx.Query(ctx, `SELECT `+fxObservationColumns+` FROM fx_observations WHERE tenant_id = $1 AND observation_id = ANY($2::uuid[])
			ORDER BY observation_id FOR UPDATE`, tid, ids)
		if err != nil {
			return err
		}
		var obs []domain.FXObservation
		for rows.Next() {
			o, err := scanFXObservation(rows)
			if err != nil {
				rows.Close()
				return err
			}
			obs = append(obs, *o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(obs) != len(ids) {
			return fmt.Errorf("%w: an observation named does not exist in this tenant", registry.ErrInvalidInput)
		}
		inOld := map[string]bool{}
		if old != nil {
			for _, m := range old.Members {
				inOld[m.ObservationID] = true
			}
		}
		pairs := map[string]bool{}
		needsOverride := false
		for _, o := range obs {
			switch {
			case o.RateType != req.RateType || o.MarketDate != fmtDate(md):
				return fmt.Errorf("%w: observation %s is %s for %s, not %s for %s", registry.ErrInvalidInput, o.ObservationID, o.RateType, o.MarketDate, req.RateType, fmtDate(md))
			case o.Status == domain.FXSuperseded:
				return fmt.Errorf("%w: observation %s is superseded", registry.ErrStateConflict, o.ObservationID)
			case o.Status == domain.FXApproved && !inOld[o.ObservationID]:
				return fmt.Errorf("%w: observation %s is already approved in another rate set", registry.ErrStateConflict, o.ObservationID)
			}
			if pairs[o.BaseCurrency+o.QuoteCurrency] {
				return fmt.Errorf("%w: a rate set holds one rate per pair, and %s/%s is named twice", registry.ErrInvalidInput, o.BaseCurrency, o.QuoteCurrency)
			}
			pairs[o.BaseCurrency+o.QuoteCurrency] = true
			if o.Status == domain.FXObserved {
				needsOverride = true
			}
			// s4.13: a rate source operator cannot approve their own manual or outlying rate.
			if o.NeedsIndependent && o.IngestedBy == actor {
				return fmt.Errorf("%w: a manual or outlying rate is approved by someone other than the principal who ingested it", registry.ErrSelfApproval)
			}
		}
		if needsOverride && strings.TrimSpace(req.OverrideReason) == "" {
			return fmt.Errorf("%w: the set includes a manual or outlying rate; override_reason is required", registry.ErrInvalidInput)
		}

		setID := uuid.NewString()
		if old != nil {
			if _, err := tx.Exec(ctx, `UPDATE fx_rate_sets SET status = 'SUPERSEDED', superseded_by_set_id = $2, superseded_at = now() WHERE set_id = $1`, old.SetID, setID); err != nil {
				return err
			}
		}
		set, err := scanFXSet(tx.QueryRow(ctx, `INSERT INTO fx_rate_sets (set_id, tenant_id, set_name, rate_type, market_date, set_version, supersedes_set_id,
			override_reason, evidence_ref, approved_by, correlation_id) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,NULLIF($8,''),$9,$10,NULLIF($11,''))
			RETURNING `+fxSetColumns, setID, tid, name, string(req.RateType), md, version, supersedes, strings.TrimSpace(req.OverrideReason),
			strings.TrimSpace(req.EvidenceRef), actor, req.CorrelationID))
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a live rate set %q exists for %s %s; supersede it to correct it", registry.ErrConflict, name, req.RateType, fmtDate(md))
			}
			return err
		}
		for _, o := range obs {
			if _, err := tx.Exec(ctx, `INSERT INTO fx_rate_set_members (tenant_id, set_id, observation_id, base_currency, quote_currency) VALUES ($1,$2,$3,$4,$5)`,
				tid, setID, o.ObservationID, o.BaseCurrency, o.QuoteCurrency); err != nil {
				return err
			}
			if o.Status != domain.FXApproved {
				if _, err := tx.Exec(ctx, `UPDATE fx_observations SET status = 'APPROVED' WHERE observation_id = $1`, o.ObservationID); err != nil {
					return err
				}
			}
		}
		if err := loadFXSetMembers(ctx, tx, tid, set); err != nil {
			return err
		}
		out = set

		// A correction: what the old set held that the new one does not stops being a rate in force.
		var superseded []domain.FXObservation
		if old != nil {
			now := time.Now().UTC()
			inNew := map[string]bool{}
			for _, o := range obs {
				inNew[o.ObservationID] = true
			}
			for _, m := range old.Members {
				if inNew[m.ObservationID] {
					continue
				}
				if _, err := tx.Exec(ctx, `UPDATE fx_observations SET status = 'SUPERSEDED', superseded_at = $2, supersede_reason = $3 WHERE observation_id = $1`,
					m.ObservationID, now, "rate set superseded by "+setID); err != nil {
					return err
				}
				m.Status, m.SupersededAt = domain.FXSuperseded, &now
				superseded = append(superseded, m)
			}
		}

		rec, err := events.FXRateApprovedRecord(*set, req.CorrelationID)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		if old != nil {
			orec, err := events.FXRateSetSupersededRecord(*old, setID, actor, req.CorrelationID)
			if err != nil {
				return err
			}
			if err := s.enqueue(ctx, tx, orec); err != nil {
				return err
			}
		}
		for _, m := range superseded {
			srec, err := events.FXRateSupersededRecord(m, "", "rate set superseded by "+setID, actor, req.CorrelationID)
			if err != nil {
				return err
			}
			if err := s.enqueue(ctx, tx, srec); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// CloseFXRateSet certifies a set.
func (s *PgStore) CloseFXRateSet(ctx context.Context, setID string, req domain.CloseFXRateSetRequest, actor string) (*domain.FXRateSet, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(setID); err != nil {
		return nil, registry.ErrNotFound
	}
	var out *domain.FXRateSet
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := scanFXSet(tx.QueryRow(ctx, `SELECT `+fxSetColumns+` FROM fx_rate_sets WHERE set_id = $1 AND tenant_id = $2 FOR UPDATE`, setID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Status != domain.FXSetApproved {
			return fmt.Errorf("%w: the rate set is %s; only an APPROVED set is closed", registry.ErrStateConflict, cur.Status)
		}
		set, err := scanFXSet(tx.QueryRow(ctx, `UPDATE fx_rate_sets SET status = 'CLOSED', closed_by = $2, closed_at = now() WHERE set_id = $1 RETURNING `+fxSetColumns, setID, actor))
		if err != nil {
			return err
		}
		if err := loadFXSetMembers(ctx, tx, tid, set); err != nil {
			return err
		}
		out = set
		rec, err := events.FXRateSetClosedRecord(*set, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ── reads ───────────────────────────────────────────────────────────────────

// GetApprovedFXComponent returns the approved rate of a pair, from the rate set live now or at knownAt.
func (s *PgStore) GetApprovedFXComponent(ctx context.Context, base, quote string, typ domain.FXRateType, marketDate time.Time, setName string, knownAt time.Time) (*domain.FXComponent, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.FXComponent
	var known *time.Time
	if !knownAt.IsZero() {
		k := knownAt.UTC()
		known = &k
	}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var c domain.FXComponent
		var md time.Time
		err := tx.QueryRow(ctx, `SELECT o.observation_id::text, o.rate::text, o.source_id::text, s.set_id::text, s.set_version, o.market_date
			FROM fx_rate_set_members m
			JOIN fx_rate_sets s ON s.tenant_id = m.tenant_id AND s.set_id = m.set_id
			JOIN fx_observations o ON o.tenant_id = m.tenant_id AND o.observation_id = m.observation_id
			WHERE m.tenant_id = $1 AND m.base_currency = $2 AND m.quote_currency = $3 AND s.rate_type = $4 AND s.market_date = $5 AND s.set_name = $6
			  AND (($7::timestamptz IS NULL AND s.status IN ('APPROVED','CLOSED'))
			       OR ($7::timestamptz IS NOT NULL AND s.approved_at <= $7::timestamptz AND (s.superseded_at IS NULL OR s.superseded_at > $7::timestamptz)))
			ORDER BY s.set_version DESC LIMIT 1`,
			tid, base, quote, string(typ), asOfDate(marketDate), setName, known).
			Scan(&c.ObservationID, &c.Rate, &c.SourceID, &c.SetID, &c.SetVersion, &md)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		c.BaseCurrency, c.QuoteCurrency, c.Rate, c.MarketDate = base, quote, domain.CanonicalRate(c.Rate), fmtDate(md)
		out = &c
		return nil
	})
	return out, err
}

// ListFXRatesBySource lists a source's observations by market date, newest first.
func (s *PgStore) ListFXRatesBySource(ctx context.Context, sourceID string, from, to time.Time, limit int, after string) ([]domain.FXObservation, string, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(sourceID); err != nil {
		return nil, "", registry.ErrNotFound
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var afterDate *time.Time
	afterID := ""
	if after != "" {
		ds, id, ok := strings.Cut(after, "|")
		d, err := domain.ParseDate(ds)
		if _, perr := uuid.Parse(id); !ok || err != nil || perr != nil {
			return nil, "", fmt.Errorf("%w: cursor is not one this service issued", registry.ErrInvalidInput)
		}
		afterDate, afterID = &d, id
	}
	var fromP, toP *time.Time
	if !from.IsZero() {
		f := asOfDate(from)
		fromP = &f
	}
	if !to.IsZero() {
		t := asOfDate(to)
		toP = &t
	}
	out := []domain.FXObservation{}
	var next string
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM fx_sources WHERE source_id = $1 AND tenant_id = $2`, sourceID, tid).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+fxObservationColumns+` FROM fx_observations WHERE tenant_id = $1 AND source_id = $2
			AND ($3::date IS NULL OR market_date >= $3::date) AND ($4::date IS NULL OR market_date <= $4::date)
			AND ($5::date IS NULL OR (market_date, observation_id) < ($5::date, $6::uuid))
			ORDER BY market_date DESC, observation_id DESC LIMIT $7`, tid, sourceID, fromP, toP, afterDate, nullUUID(afterID), limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanFXObservation(rows)
			if err != nil {
				return err
			}
			if len(out) == limit {
				last := out[len(out)-1]
				next = last.MarketDate + "|" + last.ObservationID
				break
			}
			out = append(out, *o)
		}
		return rows.Err()
	})
	return out, next, err
}
