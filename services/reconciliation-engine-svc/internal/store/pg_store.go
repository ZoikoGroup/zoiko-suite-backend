// Package store is the PgStore persistence layer for
// reconciliation-engine-svc (DATA-07, ZS-SVC-N-001 §4). Every method
// runs inside one transaction that first declares app.tenant_id for
// RLS, then performs the write — tenant scoping is enforced by the
// database, never an application-level WHERE clause the caller could
// get wrong. Populations and their items are frozen the moment
// StartRun creates them; match results, exceptions and certifications
// are append-only evidence, never edited after the fact.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/outbox"
)

// Store is the DATA-07 persistence contract.
type Store interface {
	DefineReconciliation(ctx context.Context, tenantID string, req domain.DefineReconciliationRequest, actor string) (*domain.ReconciliationDefinition, error)
	GetDefinition(ctx context.Context, tenantID, definitionID string) (*domain.ReconciliationDefinition, error)
	CheckIdempotency(ctx context.Context, tenantID string, claim domain.IdempotencyClaim) error
	StartRun(ctx context.Context, tenantID string, req domain.StartRunRequest, expectedDefinitionVersion int64,
		pops []domain.FrozenPopulation, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)
	Match(ctx context.Context, tenantID, runID string, req domain.MatchRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)
	RaiseException(ctx context.Context, tenantID, runID string, req domain.RaiseExceptionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationException, error)
	Reperform(ctx context.Context, tenantID, runID string, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)
	Certify(ctx context.Context, tenantID, runID string, actor string, claim domain.IdempotencyClaim) (*domain.Certification, error)
	SupersedeRun(ctx context.Context, tenantID, runID string, req domain.SupersedeRunRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)

	GetRun(ctx context.Context, tenantID, runID string) (*domain.ReconciliationRun, error)
	GetPopulationSnapshots(ctx context.Context, tenantID, runID string) ([]domain.PopulationSnapshot, error)
	GetMatchResults(ctx context.Context, tenantID, runID string) ([]domain.MatchResult, error)
	GetExceptions(ctx context.Context, tenantID, runID string) ([]domain.ReconciliationException, error)
	GetCertification(ctx context.Context, tenantID, runID string) (*domain.Certification, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit(ctx)
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	return fmt.Errorf("reconciliation-engine-svc: %s", pgErr.Message)
}

func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES (current_setting('app.tenant_id', true), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = current_setting('app.tenant_id', true) AND owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ── ReconciliationDefinition ────────────────────────────────────────────────

const definitionColumns = `definition_id, tenant_id, legal_entity_id::text, name, populations, match_key_fields, balance_dimensions,
	amount_scale, tolerance_policy, materiality_amount_minor_units, version, created_at, created_by, updated_at`

func scanDefinition(row pgx.Row) (*domain.ReconciliationDefinition, error) {
	var d domain.ReconciliationDefinition
	var entity *string
	var scale *int
	var pops, tol []byte
	if err := row.Scan(&d.DefinitionID, &d.TenantID, &entity, &d.Name, &pops, &d.MatchKeyFields, &d.BalanceDimensions, &scale,
		&tol, &d.MaterialityAmountMinorUnits, &d.Version, &d.CreatedAt, &d.CreatedBy, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if len(tol) > 0 {
		if err := json.Unmarshal(tol, &d.Tolerance); err != nil {
			return nil, fmt.Errorf("decode definition tolerance: %w", err)
		}
	}
	if entity != nil {
		d.LegalEntityID = *entity
	}
	if scale != nil {
		d.AmountScale = *scale
	}
	if len(pops) > 0 {
		if err := json.Unmarshal(pops, &d.Populations); err != nil {
			return nil, fmt.Errorf("decode definition populations: %w", err)
		}
	}
	return &d, nil
}

// DefineReconciliation upserts a definition by (tenant, legal entity, name).
// The version is bumped server-side only when a rule column changes (see the
// versioning trigger); a run copies the current version's values onto itself
// and never reads this table again, so an edit here never reaches back into
// an already-started run.
func (s *PgStore) DefineReconciliation(ctx context.Context, tenantID string, req domain.DefineReconciliationRequest, actor string) (*domain.ReconciliationDefinition, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	pops, err := json.Marshal(req.Populations)
	if err != nil {
		return nil, fmt.Errorf("encode populations: %w", err)
	}
	tol, err := json.Marshal(req.Tolerance)
	if err != nil {
		return nil, fmt.Errorf("encode tolerance: %w", err)
	}
	dims := req.BalanceDimensions
	if dims == nil {
		dims = []string{}
	}
	var out *domain.ReconciliationDefinition
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		id := newID(domain.PrefixDefinition)
		got, err := scanDefinition(tx.QueryRow(ctx, `
			INSERT INTO reconciliation_definitions (definition_id, tenant_id, legal_entity_id, name, populations, match_key_fields,
				balance_dimensions, amount_scale, tolerance_policy, materiality_amount_minor_units, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (tenant_id, legal_entity_id, name) DO UPDATE SET
				populations = EXCLUDED.populations,
				match_key_fields = EXCLUDED.match_key_fields,
				balance_dimensions = EXCLUDED.balance_dimensions,
				amount_scale = EXCLUDED.amount_scale,
				tolerance_policy = EXCLUDED.tolerance_policy,
				materiality_amount_minor_units = EXCLUDED.materiality_amount_minor_units
				-- version and updated_at are owned by trigger_definition_versioning:
				-- bumped only when a rule column changes.
			RETURNING `+definitionColumns,
			id, tenantID, req.LegalEntityID, req.Name, pops, req.MatchKeyFields, dims, *req.AmountScale,
			tol, req.MaterialityAmountMinorUnits, actor))
		if err != nil {
			return fmt.Errorf("upsert reconciliation definition: %w", err)
		}
		out = got
		return nil
	})
	return out, err
}

func loadDefinition(ctx context.Context, tx pgx.Tx, tenantID, definitionID string, lock bool) (*domain.ReconciliationDefinition, error) {
	q := `SELECT ` + definitionColumns + ` FROM reconciliation_definitions WHERE tenant_id = $1 AND definition_id = $2`
	if lock {
		q += ` FOR SHARE`
	}
	d, err := scanDefinition(tx.QueryRow(ctx, q, tenantID, definitionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDefinitionNotFound
	}
	return d, err
}

func (s *PgStore) GetDefinition(ctx context.Context, tenantID, definitionID string) (*domain.ReconciliationDefinition, error) {
	var out *domain.ReconciliationDefinition
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		d, err := loadDefinition(ctx, tx, tenantID, definitionID, false)
		out = d
		return err
	})
	return out, err
}

// ── ReconciliationRun ────────────────────────────────────────────────────────

const runColumns = `run_id, tenant_id, legal_entity_id::text, definition_id, definition_version, status, period_id, scope,
	amount_scale, match_key_fields, balance_dimensions, tolerance_policy, materiality_amount_minor_units, supersedes_run_id, superseded_by_run_id,
	superseded_reason, superseded_by, superseded_at, started_at, started_by, certified_at, certified_by`

func scanRun(row pgx.Row) (*domain.ReconciliationRun, error) {
	var r domain.ReconciliationRun
	var certifiedBy, entity, period *string
	var scale *int
	var scope, tol []byte
	if err := row.Scan(&r.RunID, &r.TenantID, &entity, &r.DefinitionID, &r.DefinitionVersion, &r.Status, &period, &scope,
		&scale, &r.MatchKeyFields, &r.BalanceDimensions, &tol, &r.MaterialityAmountMinorUnits, &r.SupersedesRunID, &r.SupersededByRunID,
		&r.SupersededReason, &r.SupersededBy, &r.SupersededAt, &r.StartedAt, &r.StartedBy, &r.CertifiedAt, &certifiedBy); err != nil {
		return nil, err
	}
	if certifiedBy != nil {
		r.CertifiedBy = *certifiedBy
	}
	if entity != nil {
		r.LegalEntityID = *entity
	}
	if period != nil {
		r.PeriodID = *period
	}
	if scale != nil {
		r.AmountScale = *scale
	}
	if len(tol) > 0 {
		r.Tolerance = &domain.TolerancePolicy{}
		if err := json.Unmarshal(tol, r.Tolerance); err != nil {
			return nil, fmt.Errorf("decode run tolerance: %w", err)
		}
	}
	if len(scope) > 0 {
		if err := json.Unmarshal(scope, &r.Scope); err != nil {
			return nil, fmt.Errorf("decode run scope: %w", err)
		}
		if len(r.Scope) == 0 {
			r.Scope = nil
		}
	}
	return &r, nil
}

func loadRunForUpdate(ctx context.Context, tx pgx.Tx, tenantID, runID string) (*domain.ReconciliationRun, error) {
	r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM reconciliation_runs WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`,
		tenantID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRunNotFound
	}
	return r, err
}

func (s *PgStore) GetRun(ctx context.Context, tenantID, runID string) (*domain.ReconciliationRun, error) {
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM reconciliation_runs WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		out = r
		return err
	})
	return out, err
}

// CheckIdempotency reports, without claiming anything, whether claim's key
// was already used: an IdempotentReplayError for the same request, or
// ErrIdempotencyKeyReused for a different one. StartRun calls it before
// reading any source, so a retry of a run that already started replays it
// instead of re-reading populations that may since have moved.
func (s *PgStore) CheckIdempotency(ctx context.Context, tenantID string, c domain.IdempotencyClaim) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var reqHash, resourceID string
		err := tx.QueryRow(ctx, `
			SELECT request_sha256, resource_id FROM idempotency_keys
			WHERE tenant_id = $1 AND owner_scope = $2 AND principal_id = $3 AND idempotency_key = $4`,
			tenantID, c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read idempotency claim: %w", err)
		}
		if reqHash != c.RequestSHA256 {
			return domain.ErrIdempotencyKeyReused
		}
		return &domain.IdempotentReplayError{ResourceID: resourceID}
	})
}

// StartRun freezes the two populations the caller read from their sources —
// the "frozen populations" core control — and copies the definition's rules
// onto the run itself, so nothing about a definition edited later can ever
// reach back into this run. The populations were read for definition version
// expectedVersion; if the definition changed meanwhile, the populations may
// not be the ones it now names, and the run is refused (ErrDefinitionChanged).
func (s *PgStore) StartRun(ctx context.Context, tenantID string, req domain.StartRunRequest, expectedVersion int64,
	pops []domain.FrozenPopulation, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		def, err := loadDefinition(ctx, tx, tenantID, req.DefinitionID, true)
		if err != nil {
			return err
		}
		if def.Legacy() {
			return domain.ErrLegacyRecord
		}
		if def.Version != expectedVersion {
			return domain.ErrDefinitionChanged
		}
		if err := checkPopulationsMatchDefinition(def, pops); err != nil {
			return err
		}

		runID := newID(domain.PrefixRun)
		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		scope, err := json.Marshal(req.Scope)
		if err != nil {
			return fmt.Errorf("encode run scope: %w", err)
		}
		if req.Scope == nil {
			scope = []byte("{}")
		}
		var period *string
		if req.PeriodID != "" {
			period = &req.PeriodID
		}
		tol, err := json.Marshal(def.Tolerance)
		if err != nil {
			return fmt.Errorf("encode run tolerance: %w", err)
		}
		dims := def.BalanceDimensions
		if dims == nil {
			dims = []string{}
		}
		run, err := scanRun(tx.QueryRow(ctx, `
			INSERT INTO reconciliation_runs (run_id, tenant_id, legal_entity_id, definition_id, definition_version, status,
				period_id, scope, amount_scale, match_key_fields, balance_dimensions, tolerance_policy,
				materiality_amount_minor_units, started_by)
			VALUES ($1, $2, $3, $4, $5, 'Planned', $6, $7, $8, $9, $10, $11, $12, $13)
			RETURNING `+runColumns,
			runID, tenantID, def.LegalEntityID, def.DefinitionID, def.Version, period, scope, def.AmountScale,
			def.MatchKeyFields, dims, tol, def.MaterialityAmountMinorUnits, actor))
		if err != nil {
			return fmt.Errorf("create reconciliation run: %w", err)
		}

		for _, pop := range pops {
			if err := freezePopulation(ctx, tx, tenantID, runID, def.AmountScale, pop); err != nil {
				return err
			}
		}

		out = run
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: run.RunID,
			EventType: "CONTROL.ReconciliationStarted", TenantID: &tenantID, Payload: run})
	})
	return out, err
}

// checkPopulationsMatchDefinition refuses populations that are not exactly the
// definition's two sides, each read from the system its spec names.
func checkPopulationsMatchDefinition(def *domain.ReconciliationDefinition, pops []domain.FrozenPopulation) error {
	if len(pops) != len(def.Populations) {
		return fmt.Errorf("%w: %d populations supplied, the definition names %d", domain.ErrInvalidArgument, len(pops), len(def.Populations))
	}
	want := map[string]string{}
	for _, sp := range def.Populations {
		want[sp.Side] = sp.System
	}
	for _, p := range pops {
		sys, ok := want[p.Side]
		if !ok || sys != p.SourceSystem {
			return fmt.Errorf("%w: population %q/%q is not one the definition names", domain.ErrInvalidArgument, p.Side, p.SourceSystem)
		}
		delete(want, p.Side)
	}
	return nil
}

// freezePopulation writes one side's snapshot and items. The snapshot's
// control totals are computed here from the items themselves, with overflow
// checks (A14): a total that does not fit an int64 refuses the run instead of
// wrapping. The row count must equal the source's declared row count (the
// table's CHECK enforces the same).
func freezePopulation(ctx context.Context, tx pgx.Tx, tenantID, runID string, scale int, pop domain.FrozenPopulation) error {
	if strings.TrimSpace(pop.Watermark) == "" {
		return fmt.Errorf("%w: population %q has no source watermark", domain.ErrInvalidArgument, pop.Side)
	}
	if pop.DeclaredTotals == nil || pop.DeclaredRowCount != int64(len(pop.Items)) {
		return fmt.Errorf("%w: population %q does not carry declared totals for %d rows", domain.ErrInvalidArgument, pop.Side, len(pop.Items))
	}
	// The overall total is computed only so that its overflow is refused (A14);
	// it adds currencies together, so it is never stored (A2).
	perCcy, _, err := domain.ControlTotals(pop.Items)
	if err != nil {
		return fmt.Errorf("population %q: %w", pop.Side, err)
	}
	controlJSON, err := json.Marshal(perCcy)
	if err != nil {
		return fmt.Errorf("encode control totals: %w", err)
	}
	declaredJSON, err := json.Marshal(pop.DeclaredTotals)
	if err != nil {
		return fmt.Errorf("encode declared totals: %w", err)
	}

	snapshotID := newID(domain.PrefixSnapshot)
	if _, err := tx.Exec(ctx, `
		INSERT INTO population_snapshots (snapshot_id, tenant_id, run_id, side, source_system, spec_ref, source_watermark,
			item_count, amount_scale, control_totals, declared_row_count, declared_totals, content_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		snapshotID, tenantID, runID, pop.Side, pop.SourceSystem, pop.SpecRef, pop.Watermark,
		len(pop.Items), scale, controlJSON, pop.DeclaredRowCount, declaredJSON, domain.HashPopulation(pop.Items)); err != nil {
		return fmt.Errorf("freeze population snapshot: %w", err)
	}

	for _, it := range pop.Items {
		dims, err := json.Marshal(it.Dimensions)
		if err != nil {
			return fmt.Errorf("marshal item dimensions: %w", err)
		}
		if it.Dimensions == nil {
			dims = []byte("{}")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO population_items (item_id, tenant_id, snapshot_id, source_record_id, ref_id, amount_minor_units, currency, occurred_at, dimensions)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			newID(domain.PrefixItem), tenantID, snapshotID, it.SourceRecordID, it.RefID, it.AmountMinorUnits, it.Currency, it.OccurredAt, dims); err != nil {
			return fmt.Errorf("freeze population item: %w", err)
		}
	}
	return nil
}

// ── Match ─────────────────────────────────────────────────────────────────────

const matchItemSelect = `SELECT pi.item_id, ps.side, COALESCE(pi.source_record_id, ''), pi.ref_id, pi.amount_minor_units,
		COALESCE(pi.currency, ''), pi.occurred_at, pi.dimensions
	FROM population_items pi JOIN population_snapshots ps ON ps.snapshot_id = pi.snapshot_id`

func scanMatchItems(rows pgx.Rows) ([]domain.MatchItem, error) {
	defer rows.Close()
	var out []domain.MatchItem
	for rows.Next() {
		var it domain.MatchItem
		var dims []byte
		if err := rows.Scan(&it.ItemID, &it.Side, &it.SourceRecordID, &it.RefID, &it.Amount, &it.Currency, &it.OccurredAt, &dims); err != nil {
			return nil, err
		}
		if len(dims) > 0 {
			if err := json.Unmarshal(dims, &it.Dimensions); err != nil {
				return nil, fmt.Errorf("decode item dimensions: %w", err)
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func splitBySide(items []domain.MatchItem, sideA string) (a, b []domain.MatchItem) {
	for _, it := range items {
		if it.Side == sideA {
			a = append(a, it)
		} else {
			b = append(b, it)
		}
	}
	return a, b
}

// runSides returns the run's two population sides, ordered by label. They
// come from the frozen snapshots, not from the unprocessed items: an EMPTY
// population is still a side (B2; ZS-SVC-Z-001 NP-27).
func runSides(ctx context.Context, tx pgx.Tx, runID string) (string, string, error) {
	rows, err := tx.Query(ctx, `SELECT side FROM population_snapshots WHERE run_id = $1 ORDER BY side`, runID)
	if err != nil {
		return "", "", fmt.Errorf("load population sides: %w", err)
	}
	defer rows.Close()
	var sides []string
	for rows.Next() {
		var side string
		if err := rows.Scan(&side); err != nil {
			return "", "", err
		}
		sides = append(sides, side)
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	sort.Strings(sides)
	if len(sides) != 2 {
		return "", "", fmt.Errorf("reconciliation run %s has %d populations; exactly two are required", runID, len(sides))
	}
	return sides[0], sides[1], nil
}

func insertItemException(ctx context.Context, tx pgx.Tx, tenantID, runID, actor string, e domain.ItemException) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return fmt.Errorf("encode exception detail: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_exceptions (exception_id, tenant_id, run_id, kind, item_id, side, ref_id, currency, reason,
			amount_minor_units, detail, raised_by)
		VALUES ($1, $2, $3, 'ITEM', $4, $5, $6, $7, $8, $9, $10, $11)`,
		newID(domain.PrefixException), tenantID, runID, e.Item.ItemID, e.Item.Side, e.Item.RefID, e.Item.Currency, e.Reason,
		e.Item.Amount, detail, actor); err != nil {
		return fmt.Errorf("raise auto exception: %w", err)
	}
	return nil
}

func insertBalanceException(ctx context.Context, tx pgx.Tx, tenantID, runID, actor, sideA, sideB string, v domain.BalanceVariance) error {
	scope := "population"
	if v.Dimension != "" {
		scope = fmt.Sprintf("%s=%q", v.Dimension, v.DimensionValue)
	}
	reason := fmt.Sprintf("%s %s total differs between %s (%d) and %s (%d) by %d, tolerance %d",
		v.Currency, scope, sideA, v.TotalA, sideB, v.TotalB, v.Difference, v.Allowed)
	diff, allowed := v.Difference, v.Allowed
	detail, err := json.Marshal(domain.ExceptionDetail{Rule: domain.RuleBalance, Currency: v.Currency, DifferenceMinor: &diff,
		AllowedMinor: &allowed, SideTotals: map[string]int64{sideA: v.TotalA, sideB: v.TotalB}})
	if err != nil {
		return fmt.Errorf("encode balance detail: %w", err)
	}
	// Raised once per run, currency and bucket however often matching is re-run.
	if _, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_exceptions (exception_id, tenant_id, run_id, kind, currency, dimension, dimension_value, reason,
			amount_minor_units, detail, raised_by)
		VALUES ($1, $2, $3, 'BALANCE', $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (run_id, currency, dimension, dimension_value) WHERE kind = 'BALANCE' DO NOTHING`,
		newID(domain.PrefixException), tenantID, runID, v.Currency, v.Dimension, v.DimensionValue, reason, v.Difference, detail, actor); err != nil {
		return fmt.Errorf("raise balance exception: %w", err)
	}
	return nil
}

// runAutoMatch pairs the run's still-unprocessed items (neither matched nor
// excepted) on the run's pinned match key, under its pinned tolerance policy
// (domain.AutoMatch), then checks per-currency balance for the whole
// population and every pinned balance dimension (domain.CheckBalances) over
// ALL of the run's items. Matching by key — never by aggregate totals — is
// what makes "equal totals with different composition still surface
// exceptions" true; the balance check is what stops a balanced aggregate
// hiding an unbalanced sub-dimension.
func runAutoMatch(ctx context.Context, tx pgx.Tx, tenantID, actor string, run *domain.ReconciliationRun) error {
	if run.Tolerance == nil || len(run.MatchKeyFields) == 0 {
		return domain.ErrLegacyRecord
	}
	sideA, sideB, err := runSides(ctx, tx, run.RunID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, matchItemSelect+`
		WHERE ps.run_id = $1
			AND NOT EXISTS (SELECT 1 FROM match_results mr WHERE mr.item_a_id = pi.item_id OR mr.item_b_id = pi.item_id)
			AND NOT EXISTS (SELECT 1 FROM reconciliation_exceptions re WHERE re.item_id = pi.item_id)`, run.RunID)
	if err != nil {
		return fmt.Errorf("load unprocessed items: %w", err)
	}
	pending, err := scanMatchItems(rows)
	if err != nil {
		return err
	}
	a, b := splitBySide(pending, sideA)
	pairs, excs := domain.AutoMatch(sideA, sideB, a, b, run.MatchKeyFields, *run.Tolerance)
	for _, p := range pairs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO match_results (match_result_id, tenant_id, run_id, item_a_id, item_b_id, ref_id, manual, matched_by)
			VALUES ($1, $2, $3, $4, $5, $6, FALSE, $7)`,
			newID(domain.PrefixMatchResult), tenantID, run.RunID, p.A.ItemID, p.B.ItemID, p.A.RefID, actor); err != nil {
			return fmt.Errorf("insert auto match: %w", err)
		}
	}
	for _, e := range excs {
		if err := insertItemException(ctx, tx, tenantID, run.RunID, actor, e); err != nil {
			return err
		}
	}

	rows, err = tx.Query(ctx, matchItemSelect+` WHERE ps.run_id = $1`, run.RunID)
	if err != nil {
		return fmt.Errorf("load run items: %w", err)
	}
	all, err := scanMatchItems(rows)
	if err != nil {
		return err
	}
	allA, allB := splitBySide(all, sideA)
	variances, err := domain.CheckBalances(sideA, sideB, allA, allB, run.BalanceDimensions, *run.Tolerance)
	if err != nil {
		return err
	}
	for _, v := range variances {
		if err := insertBalanceException(ctx, tx, tenantID, run.RunID, actor, sideA, sideB, v); err != nil {
			return err
		}
	}
	return nil
}

func openExceptionCount(ctx context.Context, tx pgx.Tx, runID string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_exceptions WHERE run_id = $1 AND status = 'Open'`, runID).Scan(&n)
	return n, err
}

func openRunStatuses() map[domain.RunStatus]bool {
	return map[domain.RunStatus]bool{
		domain.RunPlanned: true, domain.RunRunning: true, domain.RunExceptionsOpen: true, domain.RunReperformed: true,
	}
}

// Match runs the automatic matching pass over any still-unprocessed
// items, then applies every caller-supplied manual match — each of
// which must carry a non-empty reason and is stamped with the calling
// principal as its authority (domain.MatchRequest.Validate already
// enforces the reason is present; the authority is this handler's own
// authenticated actor, never a caller-suppliable field). A manual
// match resolves any exception it was raised against.
func (s *PgStore) Match(ctx context.Context, tenantID, runID string, req domain.MatchRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if !openRunStatuses()[run.Status] {
			return domain.ErrRunNotOpen
		}

		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		if run.Status == domain.RunPlanned {
			if _, err := tx.Exec(ctx, `UPDATE reconciliation_runs SET status = 'Running' WHERE run_id = $1`, runID); err != nil {
				return fmt.Errorf("start matching: %w", err)
			}
		}

		if err := runAutoMatch(ctx, tx, tenantID, actor, run); err != nil {
			return err
		}

		for _, m := range req.ManualMatches {
			if err := applyManualMatch(ctx, tx, tenantID, runID, actor, *run.Tolerance, m); err != nil {
				return err
			}
		}

		openCount, err := openExceptionCount(ctx, tx, runID)
		if err != nil {
			return err
		}
		target := domain.RunRunning
		eventType := "CONTROL.ReconciliationCompleted"
		if openCount > 0 {
			target = domain.RunExceptionsOpen
			eventType = "CONTROL.ExceptionRaised"
		}

		updated, err := scanRun(tx.QueryRow(ctx, `UPDATE reconciliation_runs SET status = $2 WHERE run_id = $1 RETURNING `+runColumns, runID, target))
		if err != nil {
			return fmt.Errorf("finalize match status: %w", err)
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: runID,
			EventType: eventType, TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

func applyManualMatch(ctx context.Context, tx pgx.Tx, tenantID, runID, actor string, pol domain.TolerancePolicy, m domain.ManualMatchInput) error {
	var pair [2]domain.MatchItem
	for i, itemID := range []string{m.ItemAID, m.ItemBID} {
		rows, err := tx.Query(ctx, matchItemSelect+` WHERE ps.run_id = $1 AND pi.item_id = $2`, runID, itemID)
		if err != nil {
			return fmt.Errorf("check manual match item: %w", err)
		}
		got, err := scanMatchItems(rows)
		if err != nil {
			return fmt.Errorf("check manual match item: %w", err)
		}
		if len(got) != 1 {
			return domain.ErrItemNotFound
		}
		pair[i] = got[0]
	}
	if err := domain.ValidateManualMatch(pair[0], pair[1], pol); err != nil {
		return err
	}
	var alreadyMatched bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM match_results WHERE item_a_id IN ($1, $2) OR item_b_id IN ($1, $2))`,
		m.ItemAID, m.ItemBID).Scan(&alreadyMatched); err != nil {
		return fmt.Errorf("check existing match: %w", err)
	}
	if alreadyMatched {
		return fmt.Errorf("item is already matched")
	}

	refID := pair[0].RefID

	mrID := newID(domain.PrefixMatchResult)
	if _, err := tx.Exec(ctx, `
		INSERT INTO match_results (match_result_id, tenant_id, run_id, item_a_id, item_b_id, ref_id, manual, reason, matched_by)
		VALUES ($1, $2, $3, $4, $5, $6, TRUE, $7, $8)`,
		mrID, tenantID, runID, m.ItemAID, m.ItemBID, refID, m.Reason, actor); err != nil {
		return fmt.Errorf("insert manual match: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE reconciliation_exceptions SET status = 'Resolved', resolved_by_match_id = $1
		WHERE item_id IN ($2, $3) AND status = 'Open'`, mrID, m.ItemAID, m.ItemBID); err != nil {
		return fmt.Errorf("resolve exceptions for manual match: %w", err)
	}
	return nil
}

// ── RaiseException ───────────────────────────────────────────────────────────

const exceptionColumns = `exception_id, tenant_id, run_id, kind, COALESCE(item_id, ''), COALESCE(side, ''), COALESCE(ref_id, ''),
	COALESCE(currency, ''), dimension, dimension_value, reason, amount_minor_units, detail, status,
	raised_at, raised_by, resolved_at, resolved_by_match_id`

func scanException(row pgx.Row) (*domain.ReconciliationException, error) {
	var e domain.ReconciliationException
	var detail []byte
	if err := row.Scan(&e.ExceptionID, &e.TenantID, &e.RunID, &e.Kind, &e.ItemID, &e.Side, &e.RefID, &e.Currency, &e.Dimension,
		&e.DimensionValue, &e.Reason, &e.AmountMinorUnits, &detail, &e.Status, &e.RaisedAt, &e.RaisedBy, &e.ResolvedAt,
		&e.ResolvedByMatchID); err != nil {
		return nil, err
	}
	if len(detail) > 0 {
		e.Detail = json.RawMessage(detail)
	}
	return &e, nil
}

func (s *PgStore) RaiseException(ctx context.Context, tenantID, runID string, req domain.RaiseExceptionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationException, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationException
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if !openRunStatuses()[run.Status] {
			return domain.ErrRunNotOpen
		}

		rows, err := tx.Query(ctx, matchItemSelect+` WHERE ps.run_id = $1 AND pi.item_id = $2`, runID, req.ItemID)
		if err != nil {
			return fmt.Errorf("load item for exception: %w", err)
		}
		items, err := scanMatchItems(rows)
		if err != nil {
			return fmt.Errorf("load item for exception: %w", err)
		}
		if len(items) != 1 {
			return domain.ErrItemNotFound
		}
		it := items[0]

		claim.ResourceID = runID + "|" + req.ItemID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		exID := newID(domain.PrefixException)
		got, err := scanException(tx.QueryRow(ctx, `
			INSERT INTO reconciliation_exceptions (exception_id, tenant_id, run_id, kind, item_id, side, ref_id, currency, reason,
				amount_minor_units, detail, raised_by)
			VALUES ($1, $2, $3, 'ITEM', $4, $5, $6, $7, $8, $9, '{"rule":"RAISED_MANUALLY"}', $10)
			RETURNING `+exceptionColumns,
			exID, tenantID, runID, it.ItemID, it.Side, it.RefID, it.Currency, req.Reason, it.Amount, actor))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("an exception has already been raised for this item")
			}
			return fmt.Errorf("raise exception: %w", err)
		}

		if _, err := tx.Exec(ctx, `UPDATE reconciliation_runs SET status = 'ExceptionsOpen' WHERE run_id = $1`, runID); err != nil {
			return fmt.Errorf("mark run exceptions open: %w", err)
		}

		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_exception", AggregateID: got.ExceptionID,
			EventType: "CONTROL.ExceptionRaised", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetExceptions(ctx context.Context, tenantID, runID string) ([]domain.ReconciliationException, error) {
	var out []domain.ReconciliationException
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+exceptionColumns+` FROM reconciliation_exceptions WHERE tenant_id = $1 AND run_id = $2 ORDER BY raised_at`,
			tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanException(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

// ── Reperform ────────────────────────────────────────────────────────────────

// Reperform explicitly checkpoints a re-evaluation of an ExceptionsOpen
// run — it re-runs the automatic pass over anything still unprocessed
// (normally a no-op once every item has a match or an exception) and
// moves the run to Reperformed, the doc's own distinct lifecycle stage
// between ExceptionsOpen and Certified/Failed. Certify accepts a run
// from either Running or Reperformed, but only once zero exceptions
// remain Open.
func (s *PgStore) Reperform(ctx context.Context, tenantID, runID string, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if run.Status != domain.RunExceptionsOpen && run.Status != domain.RunRunning {
			return domain.ErrRunNotOpen
		}

		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		if err := runAutoMatch(ctx, tx, tenantID, actor, run); err != nil {
			return err
		}

		updated, err := scanRun(tx.QueryRow(ctx, `UPDATE reconciliation_runs SET status = 'Reperformed' WHERE run_id = $1 RETURNING `+runColumns, runID))
		if err != nil {
			return fmt.Errorf("mark run reperformed: %w", err)
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: runID,
			EventType: "CONTROL.ReconciliationCompleted", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// ── Certify ──────────────────────────────────────────────────────────────────

const certificationColumns = `certification_id, tenant_id, run_id, definition_version, matched_count, exception_count,
	total_asserted_minor_units, control_totals, manifest_sha256, sealed_at, sealed_by`

func scanCertification(row pgx.Row) (*domain.Certification, error) {
	var c domain.Certification
	var totals []byte
	if err := row.Scan(&c.CertificationID, &c.TenantID, &c.RunID, &c.DefinitionVersion, &c.MatchedCount, &c.ExceptionCount,
		&c.TotalAssertedMinor, &totals, &c.ManifestSHA256, &c.SealedAt, &c.SealedBy); err != nil {
		return nil, err
	}
	if len(totals) > 0 {
		if err := json.Unmarshal(totals, &c.ControlTotals); err != nil {
			return nil, fmt.Errorf("decode certification control totals: %w", err)
		}
	}
	return &c, nil
}

type certificationManifest struct {
	RunID                       string                            `json:"run_id"`
	DefinitionVersion           int64                             `json:"definition_version"`
	AmountScale                 int                               `json:"amount_scale"`
	Tolerance                   *domain.TolerancePolicy           `json:"tolerance"`
	MatchKeyFields              []string                          `json:"match_key_fields"`
	BalanceDimensions           []string                          `json:"balance_dimensions"`
	MaterialityAmountMinorUnits int64                             `json:"materiality_amount_minor_units"`
	MatchedCount                int64                             `json:"matched_count"`
	ExceptionCount              int64                             `json:"exception_count"`
	ControlTotals               map[string][]domain.CurrencyTotal `json:"control_totals"`
	SealedAt                    time.Time                         `json:"sealed_at"`
}

// sideControlTotals returns each side's per-currency control totals. They are
// never added together across sides or currencies (A2).
func sideControlTotals(ctx context.Context, tx pgx.Tx, runID string) (map[string][]domain.CurrencyTotal, error) {
	rows, err := tx.Query(ctx, `SELECT side, control_totals FROM population_snapshots WHERE run_id = $1 ORDER BY side`, runID)
	if err != nil {
		return nil, fmt.Errorf("load control totals: %w", err)
	}
	defer rows.Close()
	out := map[string][]domain.CurrencyTotal{}
	for rows.Next() {
		var side string
		var raw []byte
		if err := rows.Scan(&side, &raw); err != nil {
			return nil, err
		}
		var t []domain.CurrencyTotal
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &t); err != nil {
				return nil, fmt.Errorf("decode control totals: %w", err)
			}
		}
		if t == nil {
			t = []domain.CurrencyTotal{}
		}
		out[side] = t
	}
	return out, rows.Err()
}

// Certify seals an immutable Certification — only legal from Running or
// Reperformed, and only once zero exceptions remain Open (the doc's
// own "certified runs immutable" core control starts here; the
// reconciliation_runs trigger then enforces it for the rest of the
// run's life). This pass does not model a separate "expected
// assertions" mismatch concept, so Certify never auto-transitions a run
// to Failed on its own — it either certifies or refuses with a clear
// reason, leaving Failed reachable only through an explicit future
// extension rather than an invented, ungoverned auto-fail path.
func (s *PgStore) Certify(ctx context.Context, tenantID, runID string, actor string, claim domain.IdempotencyClaim) (*domain.Certification, error) {
	var out *domain.Certification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if run.Status != domain.RunRunning && run.Status != domain.RunReperformed && run.Status != domain.RunExceptionsOpen {
			return domain.ErrRunNotOpen
		}
		openCount, err := openExceptionCount(ctx, tx, runID)
		if err != nil {
			return err
		}
		if openCount > 0 {
			return domain.ErrOpenExceptionsRemain
		}

		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		var matchedCount, exceptionCount int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM match_results WHERE run_id = $1`, runID).Scan(&matchedCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_exceptions WHERE run_id = $1`, runID).Scan(&exceptionCount); err != nil {
			return err
		}
		totals, err := sideControlTotals(ctx, tx, runID)
		if err != nil {
			return err
		}
		totalsJSON, err := json.Marshal(totals)
		if err != nil {
			return fmt.Errorf("encode control totals: %w", err)
		}

		sealedAt := time.Now().UTC()
		manifest, err := json.Marshal(certificationManifest{
			RunID: runID, DefinitionVersion: run.DefinitionVersion, AmountScale: run.AmountScale, Tolerance: run.Tolerance,
			MatchKeyFields: run.MatchKeyFields, BalanceDimensions: run.BalanceDimensions,
			MaterialityAmountMinorUnits: run.MaterialityAmountMinorUnits,
			MatchedCount:                matchedCount, ExceptionCount: exceptionCount, ControlTotals: totals, SealedAt: sealedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal certification manifest: %w", err)
		}

		certID := newID(domain.PrefixCertification)
		cert, err := scanCertification(tx.QueryRow(ctx, `
			INSERT INTO certifications (certification_id, tenant_id, run_id, definition_version, matched_count,
				exception_count, control_totals, manifest_sha256, sealed_at, sealed_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+certificationColumns,
			certID, tenantID, runID, run.DefinitionVersion, matchedCount, exceptionCount, totalsJSON, sha256Hex(manifest), sealedAt, actor))
		if err != nil {
			return fmt.Errorf("seal certification: %w", err)
		}

		if _, err := tx.Exec(ctx, `UPDATE reconciliation_runs SET status = 'Certified', certified_by = $2 WHERE run_id = $1`, runID, actor); err != nil {
			return fmt.Errorf("certify run: %w", err)
		}

		out = cert
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "certification", AggregateID: cert.CertificationID,
			EventType: "CONTROL.Certified", TenantID: &tenantID, Payload: cert})
	})
	return out, err
}

func (s *PgStore) GetCertification(ctx context.Context, tenantID, runID string) (*domain.Certification, error) {
	var out *domain.Certification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		c, err := scanCertification(tx.QueryRow(ctx, `SELECT `+certificationColumns+` FROM certifications WHERE tenant_id = $1 AND run_id = $2`,
			tenantID, runID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		out = c
		return err
	})
	return out, err
}

// ── SupersedeRun ─────────────────────────────────────────────────────────────

// SupersedeRun marks a Certified or Failed run as superseded by a
// replacement run, without altering its own sealed evidence — the same
// append-only correction pattern used for lineage edges and ingestion
// runs elsewhere in DATA-0x. If remediation needs to happen, it happens
// in the owning domain; this only records that this run's result has
// been superseded, by what, and by whom.
//
// The replacement must reconcile the same thing (same definition) and must
// itself have reached a terminal state (Certified or Failed): a result is
// never superseded by one that does not exist yet (A13). A replacement
// supersedes at most one run. Both runs are locked in run_id order so two
// concurrent supersessions cannot deadlock on each other.
func (s *PgStore) SupersedeRun(ctx context.Context, tenantID, runID string, req domain.SupersedeRunRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.NewRunID == runID {
		return nil, domain.ErrSupersedeSelf
	}
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		locked := map[string]*domain.ReconciliationRun{}
		ids := []string{runID, req.NewRunID}
		sort.Strings(ids)
		for _, id := range ids {
			r, err := loadRunForUpdate(ctx, tx, tenantID, id)
			if err != nil {
				return err
			}
			locked[id] = r
		}
		run, replacement := locked[runID], locked[req.NewRunID]
		if run.Status != domain.RunCertified && run.Status != domain.RunFailed {
			return domain.ErrRunNotOpen
		}
		if replacement.DefinitionID != run.DefinitionID {
			return domain.ErrSupersedeDifferentDefinition
		}
		if replacement.Status != domain.RunCertified && replacement.Status != domain.RunFailed {
			return domain.ErrReplacementNotTerminal
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reconciliation_runs WHERE tenant_id = $1 AND superseded_by_run_id = $2)`,
			tenantID, req.NewRunID).Scan(&used); err != nil {
			return fmt.Errorf("check replacement run: %w", err)
		}
		if used {
			return domain.ErrReplacementAlreadyUsed
		}

		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		updated, err := scanRun(tx.QueryRow(ctx, `
			UPDATE reconciliation_runs SET status = 'Superseded', superseded_by_run_id = $2, superseded_reason = $3, superseded_by = $4
			WHERE run_id = $1 RETURNING `+runColumns,
			runID, req.NewRunID, req.Reason, actor))
		if err != nil {
			return fmt.Errorf("supersede run: %w", err)
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: runID,
			EventType: "CONTROL.ReconciliationSuperseded", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// ── Read surfaces ────────────────────────────────────────────────────────────

const snapshotColumns = `snapshot_id, tenant_id, run_id, side, source_system, spec_ref, source_watermark, item_count, amount_scale,
	total_amount_minor_units, control_totals, declared_row_count, declared_totals, content_hash, created_at`

func scanSnapshot(row pgx.Row) (*domain.PopulationSnapshot, error) {
	var p domain.PopulationSnapshot
	var specRef, watermark *string
	var control, declared []byte
	if err := row.Scan(&p.SnapshotID, &p.TenantID, &p.RunID, &p.Side, &p.SourceSystem, &specRef, &watermark, &p.ItemCount,
		&p.AmountScale, &p.TotalAmountMinorUnits, &control, &p.DeclaredRowCount, &declared, &p.ContentHash, &p.CreatedAt); err != nil {
		return nil, err
	}
	if specRef != nil {
		p.SpecRef = *specRef
	}
	if watermark != nil {
		p.SourceWatermark = *watermark
	}
	if len(control) > 0 {
		if err := json.Unmarshal(control, &p.ControlTotals); err != nil {
			return nil, fmt.Errorf("decode control totals: %w", err)
		}
	}
	if len(declared) > 0 {
		if err := json.Unmarshal(declared, &p.DeclaredTotals); err != nil {
			return nil, fmt.Errorf("decode declared totals: %w", err)
		}
	}
	return &p, nil
}

func (s *PgStore) GetPopulationSnapshots(ctx context.Context, tenantID, runID string) ([]domain.PopulationSnapshot, error) {
	var out []domain.PopulationSnapshot
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+snapshotColumns+` FROM population_snapshots WHERE tenant_id = $1 AND run_id = $2 ORDER BY side`,
			tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanSnapshot(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

const matchResultColumns = `match_result_id, tenant_id, run_id, item_a_id, item_b_id, ref_id, manual, reason, matched_by, matched_at`

func scanMatchResult(row pgx.Row) (*domain.MatchResult, error) {
	var m domain.MatchResult
	var reason *string
	if err := row.Scan(&m.MatchResultID, &m.TenantID, &m.RunID, &m.ItemAID, &m.ItemBID, &m.RefID, &m.Manual, &reason,
		&m.MatchedBy, &m.MatchedAt); err != nil {
		return nil, err
	}
	if reason != nil {
		m.Reason = *reason
	}
	return &m, nil
}

func (s *PgStore) GetMatchResults(ctx context.Context, tenantID, runID string) ([]domain.MatchResult, error) {
	var out []domain.MatchResult
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+matchResultColumns+` FROM match_results WHERE tenant_id = $1 AND run_id = $2 ORDER BY matched_at`,
			tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMatchResult(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}
