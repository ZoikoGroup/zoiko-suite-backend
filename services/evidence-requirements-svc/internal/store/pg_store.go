// Package store provides the PostgreSQL implementation of
// evidence-requirements-svc's persistence layer.
//
// Two things in here are deliberate reactions to live platform defects, and
// changing either of them will reintroduce a real bug:
//
//  1. Tenant scope is set with SELECT set_config('app.tenant_id', $1, true),
//     NOT with SET LOCAL app.tenant_id = $1. Postgres does not accept bind
//     parameters in SET, so the latter raises a syntax error; because the
//     error is returned and checked, every enclosing write aborts. Twelve
//     services currently ship that form (all ten Phase 5 services plus
//     offboarding-severance-svc and workforce-compliance-svc) and their DB
//     layers cannot write at all.
//
//  2. Every method ALSO filters explicitly by tenant_id in its own SQL
//     rather than relying on the Row-Level Security policy alone. The
//     policies in the migration are real and correctly written, but this
//     pool connects as a Postgres superuser (DB_USER=postgres, same as every
//     other service here), and superusers unconditionally bypass RLS
//     regardless of policy. RLS is defense-in-depth; the explicit filter is
//     the actual guarantee. general-ledger-svc and tenant-entity-registry-svc
//     both learned this through genuine CI failures.
//
// internal/store/tenant_isolation_test.go proves both hold.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

// newCursorSigner returns a cursor signer for the given key.
func newCursorSigner(key string) (*apicontract.CursorSigner, error) {
	if key == "" {
		key = "evidence-requirements-svc-local-dev-cursor-key-16-bytes-min"
	}
	if len(key) < 16 {
		return nil, errors.New("cursor signing key must be at least 16 bytes")
	}
	return apicontract.NewCursorSigner([]byte(key))
}

// parseAndVerifyCursor decodes and verifies a cursor token.
func parseAndVerifyCursor(signer *apicontract.CursorSigner, cursor, tenantID, route, filter, sortKey string) (apicontract.Cursor, error) {
	if cursor == "" {
		return apicontract.Cursor{}, nil
	}
	c, err := signer.Verify(cursor, tenantID, route, filter, sortKey)
	if err != nil {
		return apicontract.Cursor{}, err
	}
	return c, nil
}

// buildFilterKey builds the canonical filter key for cursor scope verification.
func buildFilterKey(filter map[string]string) string {
	keys := make([]string, 0, len(filter))
	for k := range filter {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys)-1; i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(filter[k])
	}
	return b.String()
}

// buildSortKey builds the canonical sort key for cursor scope verification.
func buildSortKey(sort []apicontract.SortTerm) string {
	parts := make([]string, 0, len(sort))
	for _, t := range sort {
		if t.Desc {
			parts = append(parts, "-"+t.Field)
		} else {
			parts = append(parts, t.Field)
		}
	}
	return strings.Join(parts, ",")
}

// buildQueryString builds a query string from a filter map.
func buildQueryString(filters map[string]string) string {
	var parts []string
	for k, v := range filters {
		parts = append(parts, k+"="+url.QueryEscape(v))
	}
	return strings.Join(parts, "&")
}

// parsePageFromFilter builds an apicontract.Page from a filter map.
func parsePageFromFilter(filters map[string]string, allowedFilters, allowedSort []string, defaultSort string) (apicontract.Page, error) {
	mockReq := &http.Request{
		URL: &url.URL{
			RawQuery: buildQueryString(filters),
		},
	}
	page, prob := apicontract.ParsePage(mockReq, allowedFilters, allowedSort, defaultSort)
	if prob != nil {
		return page, prob
	}
	return page, nil
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func tenantFromCtxOrFallback(ctx context.Context, fallback string) string {
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		return t
	}
	return fallback
}

// requirementColumns is the shared SELECT list, kept in one place so the
// scan order below can never drift from it.
const requirementColumns = `
	evidence_requirement_id, tenant_id, legal_entity_id, domain_code, action_type,
	evidence_type, requirement_payload, effective_from, effective_to,
	created_at, created_by_principal_id, correlation_id, idempotency_key`

func scanRequirement(row pgx.Row) (*domain.EvidenceRequirement, error) {
	var r domain.EvidenceRequirement
	var payload []byte
	if err := row.Scan(
		&r.EvidenceRequirementID, &r.TenantID, &r.LegalEntityID, &r.DomainCode, &r.ActionType,
		&r.EvidenceType, &payload, &r.EffectiveFrom, &r.EffectiveTo,
		&r.CreatedAt, &r.CreatedByPrincipalID, &r.CorrelationID, &r.IdempotencyKey,
	); err != nil {
		return nil, err
	}
	r.RequirementPayload = json.RawMessage(payload)
	return &r, nil
}

// mapPgError translates driver-level failures that are really caller mistakes
// into domain errors, so they stop being reported as outages.
//
// 22P02 (invalid_text_representation) is the one that matters here:
// evidence_requirement_id, evaluation_id, tenant_id and legal_entity_id are all
// uuid columns, so a mistyped id fails inside the driver before any row is
// examined. Left unmapped it surfaced as 503 store_unavailable —
// indistinguishable from the database being unreachable.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code == "22P02" {
		return domain.ErrInvalidIdentifier
	}
	return err
}

// CreateRequirement inserts an evidence requirement. Idempotent on
// (tenant_id, idempotency_key): if a row already exists for that pair, r is
// overwritten with the EXISTING row's values and created=false is returned.
//
// The unique constraint is real and enforced by the database — most
// Finance/Workforce/Legal services on this platform store correlation_id
// without constraining it, so their retries mint duplicates.
func (s *PgStore) CreateRequirement(ctx context.Context, r *domain.EvidenceRequirement) (created bool, err error) {
	tenantID := tenantFromCtxOrFallback(ctx, r.TenantID)

	payload := r.RequirementPayload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	// An absent key means "not an idempotent write", not "share the empty
	// key with every other absent-key row". The latter made a second insert
	// in the same tenant collide with the first at the DB level.
	idempotencyKey := r.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = r.EvidenceRequirementID
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			INSERT INTO evidence_requirements (
				evidence_requirement_id, tenant_id, legal_entity_id, domain_code, action_type,
				evidence_type, requirement_payload, effective_from, effective_to,
				created_at, created_by_principal_id, correlation_id, idempotency_key
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULL,$9,$10,$11,$12)
			ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
		`, r.EvidenceRequirementID, r.TenantID, r.LegalEntityID, r.DomainCode, r.ActionType,
			r.EvidenceType, []byte(payload), r.EffectiveFrom,
			now, r.CreatedByPrincipalID, r.CorrelationID, idempotencyKey)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			r.RequirementPayload = payload
			r.CreatedAt = now
			r.EffectiveTo = nil
			return nil
		}

		// Conflict: a requirement for this (tenant_id, idempotency_key)
		// already exists — this is a retry. Return the existing row as-is so
		// the handler can respond idempotently.
		existing, err := scanRequirement(tx.QueryRow(ctx,
			`SELECT `+requirementColumns+`
			 FROM evidence_requirements WHERE tenant_id = $1 AND idempotency_key = $2`,
			r.TenantID, idempotencyKey))
		if err != nil {
			return err
		}
		*r = *existing
		created = false
		return nil
	})
	return created, err
}

// GetRequirement returns (nil, nil) if not found — including when the
// caller's tenant scope doesn't match the requirement's tenant (explicit
// filter, not RLS-only — see package doc).
func (s *PgStore) GetRequirement(ctx context.Context, requirementID string) (*domain.EvidenceRequirement, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var out *domain.EvidenceRequirement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := scanRequirement(tx.QueryRow(ctx,
			`SELECT `+requirementColumns+`
			 FROM evidence_requirements
			 WHERE evidence_requirement_id = $1 AND tenant_id = $2`,
			requirementID, tenantID))
		if err != nil {
			return mapPgError(err)
		}
		out = r
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	// A malformed id cannot name an existing requirement, so it is absent — not
	// an outage, and reported identically to another tenant's requirement.
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListRequirements returns catalog rows matching filter. TenantID is
// required; the rest are optional. A zero AsOf means no effective-date
// filter at all — retired requirements are included, which is what an
// auditor reviewing history needs.
func (s *PgStore) ListRequirements(ctx context.Context, filter domain.ListRequirementsFilter) ([]domain.EvidenceRequirement, error) {
	var asOf *time.Time
	if !filter.AsOf.IsZero() {
		t := filter.AsOf
		asOf = &t
	}

	var out []domain.EvidenceRequirement
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+requirementColumns+`
			FROM evidence_requirements
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR domain_code = $3)
			  AND ($4 = '' OR action_type = $4)
			  AND ($5::timestamptz IS NULL OR (
			        effective_from <= $5 AND (effective_to IS NULL OR effective_to > $5)))
			ORDER BY domain_code, action_type, evidence_type, effective_from DESC
		`, filter.TenantID, filter.LegalEntityID, filter.DomainCode, filter.ActionType, asOf)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRequirement(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// EffectiveRequirements returns the requirements in force for one action at
// asOf: those scoped to legalEntityID, plus those with a NULL
// legal_entity_id (tenant-wide). This is the read the evaluator depends on.
func (s *PgStore) EffectiveRequirements(ctx context.Context, tenantID, legalEntityID, domainCode, actionType string, asOf time.Time) ([]domain.EvidenceRequirement, error) {
	var out []domain.EvidenceRequirement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+requirementColumns+`
			FROM evidence_requirements
			WHERE tenant_id = $1
			  AND domain_code = $2
			  AND action_type = $3
			  AND (legal_entity_id IS NULL OR legal_entity_id::text = $4)
			  AND effective_from <= $5
			  AND (effective_to IS NULL OR effective_to > $5)
			ORDER BY evidence_type
		`, tenantID, domainCode, actionType, legalEntityID, asOf)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRequirement(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// EndDateRequirement retires a requirement by setting effective_to. It never
// deletes and never sets a flag (doctrine: no soft-delete on material
// objects; effective end-dating only).
//
// The UPDATE carries its own guard — WHERE effective_to IS NULL AND
// tenant_id = $N — so the state check, the transition, and the tenant scope
// are one atomic statement. Returns domain.ErrAlreadyRetired if the row
// exists but is already retired, and domain.ErrRequirementNotFound if it
// does not exist within this tenant. Never a silent no-op:
// invoice-approval-svc's non-atomic read-then-write is the anti-pattern
// here, and it can double-apply under concurrency.
func (s *PgStore) EndDateRequirement(ctx context.Context, tenantID, requirementID string, effectiveTo time.Time, reason, actorPrincipalID string) (*domain.EvidenceRequirement, error) {
	var out *domain.EvidenceRequirement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := scanRequirement(tx.QueryRow(ctx, `
			UPDATE evidence_requirements
			SET effective_to = $1, retired_by_principal_id = $2, retired_reason = $3
			WHERE evidence_requirement_id = $4 AND tenant_id = $5 AND effective_to IS NULL
			RETURNING `+requirementColumns,
			effectiveTo, actorPrincipalID, reason, requirementID, tenantID))
		if err == nil {
			out = r
			return nil
		}
		// A malformed id names no requirement, so it is a not-found rather than
		// an "already retired" claim about a row that does not exist. Checked
		// before ErrNoRows because 22P02 is not a no-rows result.
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return domain.ErrRequirementNotFound
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// Zero rows updated. Distinguish "already retired" from "not found
		// in this tenant" so the caller gets a truthful error instead of a
		// generic one. Same transaction, so the answer is consistent with
		// the UPDATE that just ran.
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM evidence_requirements
				WHERE evidence_requirement_id = $1 AND tenant_id = $2)
		`, requirementID, tenantID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return domain.ErrAlreadyRetired
		}
		return domain.ErrRequirementNotFound
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordEvaluation appends an evaluation record. Idempotent on
// (tenant_id, idempotency_key): a replayed evaluation returns the ORIGINAL
// determination with created=false, and the handler must not republish its
// Kafka event in that case.
// If outboxEvent is non-nil and the evaluation is newly created (not a replay),
// the outbox event is written atomically in the same transaction.
func (s *PgStore) RecordEvaluation(ctx context.Context, e *domain.EvidenceEvaluation, outboxEvent *domain.OutboxEvent) (created bool, err error) {
	tenantID := tenantFromCtxOrFallback(ctx, e.TenantID)

	// Convert requirement_ids slice to PostgreSQL array format
	reqIDs := e.RequirementIDs
	if reqIDs == nil {
		reqIDs = []string{}
	}

	// As with requirements, an absent key is not a shared empty key; the
	// evaluation's own uuid keeps independent determinations insertable.
	idempotencyKey := e.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = e.EvaluationID
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			INSERT INTO evidence_evaluations (
				evaluation_id, tenant_id, legal_entity_id, domain_code, action_type,
				outcome, unmet_payload, present_artifacts_payload,
				evaluated_at, evaluated_for_principal_id, correlation_id, idempotency_key, requirement_ids
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
		`, e.EvaluationID, e.TenantID, e.LegalEntityID, e.DomainCode, e.ActionType,
			string(e.Outcome), []byte(e.UnmetPayload), []byte(e.PresentArtifactsPayload),
			now, e.EvaluatedForPrincipalID, e.CorrelationID, idempotencyKey, reqIDs)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			e.EvaluatedAt = now
			// Write outbox event atomically in the same transaction
			if outboxEvent != nil {
				if _, err := tx.Exec(ctx, `
					INSERT INTO outbox (outbox_id, aggregate_type, aggregate_id, event_type, payload)
					VALUES (gen_random_uuid(), $1, $2, $3, $4)
				`, outboxEvent.AggregateType, outboxEvent.AggregateID, outboxEvent.EventType, outboxEvent.Payload); err != nil {
					return err
				}
			}
			return nil
		}

		existing, err := scanEvaluation(tx.QueryRow(ctx,
			`SELECT `+evaluationColumns+`
			 FROM evidence_evaluations WHERE tenant_id = $1 AND idempotency_key = $2`,
			e.TenantID, idempotencyKey))
		if err != nil {
			return err
		}
		*e = *existing
		created = false
		return nil
	})
	return created, err
}

const evaluationColumns = `
	evaluation_id, tenant_id, legal_entity_id, domain_code, action_type,
	outcome, unmet_payload, present_artifacts_payload,
	evaluated_at, evaluated_for_principal_id, correlation_id, idempotency_key, requirement_ids`

func scanEvaluation(row pgx.Row) (*domain.EvidenceEvaluation, error) {
	var e domain.EvidenceEvaluation
	var outcome string
	var unmet, present []byte
	var reqIDs []string
	if err := row.Scan(
		&e.EvaluationID, &e.TenantID, &e.LegalEntityID, &e.DomainCode, &e.ActionType,
		&outcome, &unmet, &present,
		&e.EvaluatedAt, &e.EvaluatedForPrincipalID, &e.CorrelationID, &e.IdempotencyKey, &reqIDs,
	); err != nil {
		return nil, err
	}
	e.Outcome = domain.Outcome(outcome)
	e.UnmetPayload = json.RawMessage(unmet)
	e.PresentArtifactsPayload = json.RawMessage(present)
	e.RequirementIDs = reqIDs
	return &e, nil
}

// GetEvaluation returns (nil, nil) if not found within the caller's tenant
// scope. Past determinations are retrievable because the determination is
// itself evidence (03-microservices.md §17.6).
func (s *PgStore) GetEvaluation(ctx context.Context, evaluationID string) (*domain.EvidenceEvaluation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var out *domain.EvidenceEvaluation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		e, err := scanEvaluation(tx.QueryRow(ctx,
			`SELECT `+evaluationColumns+`
			 FROM evidence_evaluations
			 WHERE evaluation_id = $1 AND tenant_id = $2`,
			evaluationID, tenantID))
		if err != nil {
			return mapPgError(err)
		}
		out = e
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WriteOutbox writes an event to the outbox table within the current transaction.
// The outbox table has no tenant_id, so we don't need RLS context for it.
func (s *PgStore) WriteOutbox(ctx context.Context, aggregateType, aggregateID, eventType string, payload []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO outbox (outbox_id, aggregate_type, aggregate_id, event_type, payload)
		VALUES (gen_random_uuid(), $1, $2, $3, $4)
	`, aggregateType, aggregateID, eventType, payload)
	return err
}

// ListRequirementsCursor returns a cursor-paginated page of requirements.
// Uses API-4 cursor semantics: filter/sort encoded in the cursor token.
func (s *PgStore) ListRequirementsCursor(ctx context.Context, filter domain.ListRequirementsFilter) (domain.CursorPage[domain.EvidenceRequirement], error) {
	var out domain.CursorPage[domain.EvidenceRequirement]
	out.Items = []domain.EvidenceRequirement{}
	var cursor apicontract.Cursor

	allowedFilters := []string{"legal_entity_id", "domain_code", "action_type", "as_of"}
	allowedSort := []string{"domain_code", "action_type", "evidence_type", "effective_from", "created_at"}

	pageFilters := map[string]string{}
	if filter.LegalEntityID != "" {
		pageFilters["filter[legal_entity_id]"] = filter.LegalEntityID
	}
	if filter.DomainCode != "" {
		pageFilters["filter[domain_code]"] = filter.DomainCode
	}
	if filter.ActionType != "" {
		pageFilters["filter[action_type]"] = filter.ActionType
	}
	if !filter.AsOf.IsZero() {
		pageFilters["filter[as_of]"] = filter.AsOf.Format(time.RFC3339)
	}

	page, err := parsePageFromFilter(pageFilters, allowedFilters, allowedSort, "domain_code,action_type,evidence_type,effective_from")
	if err != nil {
		return out, err
	}

	// Override with values from the filter struct if provided.
	if filter.PageSize > 0 {
		page.PageSize = filter.PageSize
	}
	if filter.Cursor != "" {
		page.Cursor = filter.Cursor
	}
	if filter.Sort != "" {
		page.Sort = nil
		for _, part := range strings.Split(filter.Sort, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			term := apicontract.SortTerm{Field: strings.TrimPrefix(part, "-"), Desc: strings.HasPrefix(part, "-")}
			page.Sort = append(page.Sort, term)
		}
	}

	var signer *apicontract.CursorSigner
	signer, err = apicontract.NewCursorSigner([]byte("evidence-requirements-svc-local-dev-cursor-key-16-bytes-min"))
	if err != nil {
		return out, err
	}

	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return out, fmt.Errorf("tenant not in context")
	}

	route := apicontract.ScopedRoute("/v1/evidence-requirements")
	filterKey := buildFilterKey(page.Filters)
	sortKey := buildSortKey(page.Sort)

	var cur apicontract.Cursor
	if signer != nil && page.Cursor != "" {
		cur, err = signer.Verify(page.Cursor, tenantID, route, filterKey, sortKey)
	} else if page.Cursor != "" {
		err = errors.New("cursor signer unavailable")
	}
	if err != nil {
		return out, err
	}
	cursor = cur
	if err != nil {
		return out, err
	}

	// Build the SQL query with cursor support.
	var asOf *time.Time
	if !filter.AsOf.IsZero() {
		t := filter.AsOf
		asOf = &t
	}

	// Build WHERE clause dynamically based on filters.
	where := "WHERE tenant_id = $1"
	args := []any{tenantID}
	argIdx := 2

	if filter.LegalEntityID != "" {
		where += fmt.Sprintf(" AND legal_entity_id::text = $%d", argIdx)
		args = append(args, filter.LegalEntityID)
		argIdx++
	}
	if filter.DomainCode != "" {
		where += fmt.Sprintf(" AND domain_code = $%d", argIdx)
		args = append(args, filter.DomainCode)
		argIdx++
	}
	if filter.ActionType != "" {
		where += fmt.Sprintf(" AND action_type = $%d", argIdx)
		args = append(args, filter.ActionType)
		argIdx++
	}
	if asOf != nil {
		where += fmt.Sprintf(" AND ($%d::timestamptz IS NULL OR (effective_from <= $%d AND (effective_to IS NULL OR effective_to > $%d)))", argIdx, argIdx, argIdx)
		args = append(args, *asOf)
		argIdx++
	}

	// Cursor-based pagination: if we have a cursor, add the last_id condition.
	if cursor.LastID != "" {
		where += fmt.Sprintf(" AND evidence_requirement_id < $%d", argIdx)
		args = append(args, cursor.LastID)
		argIdx++
	}

	// Build ORDER BY from sort terms.
	orderBy := "ORDER BY "
	sortParts := []string{}
	for _, t := range page.Sort {
		dir := "ASC"
		if t.Desc {
			dir = "DESC"
		}
		sortParts = append(sortParts, t.Field+" "+dir)
	}
	if len(sortParts) == 0 {
		sortParts = append(sortParts, "domain_code ASC", "action_type ASC", "evidence_type ASC", "effective_from DESC")
	}
	orderBy += strings.Join(sortParts, ", ")

	// Add LIMIT (page_size + 1 to detect if there's a next page).
	limit := page.PageSize + 1
	where += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	query := fmt.Sprintf("SELECT %s FROM evidence_requirements %s %s", requirementColumns, where, orderBy)

	var items []domain.EvidenceRequirement
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRequirement(rows)
			if err != nil {
				return err
			}
			items = append(items, *r)
		}
		return rows.Err()
	})
	if err != nil {
		return out, err
	}

	// Determine if there's a next page.
	hasNext := len(items) > page.PageSize
	if hasNext {
		items = items[:page.PageSize]
	}

	// Build next cursor if there's a next page.
	if hasNext && len(items) > 0 {
		last := items[len(items)-1]
		nextCursor, err := signer.Sign(apicontract.Cursor{
			TenantID: tenantID,
			Route:    route,
			Filter:   filterKey,
			Sort:     sortKey,
			Offset:   cursor.Offset + len(items),
			LastID:   last.EvidenceRequirementID,
		})
		if err != nil {
			return out, err
		}
		out.NextCursor = nextCursor
	}

	out.Items = items
	return out, nil
}

// ListEvaluationsCursor returns a cursor-paginated page of evaluations.
func (s *PgStore) ListEvaluationsCursor(ctx context.Context, filter domain.ListEvaluationsFilter) (domain.CursorPage[domain.EvidenceEvaluation], error) {
	var out domain.CursorPage[domain.EvidenceEvaluation]
	out.Items = []domain.EvidenceEvaluation{}
	var cursor apicontract.Cursor

	allowedFilters := []string{"legal_entity_id", "domain_code", "action_type", "outcome", "since"}
	allowedSort := []string{"evaluated_at", "outcome", "domain_code", "action_type", "legal_entity_id"}

	pageFilters := map[string]string{}
	if filter.LegalEntityID != "" {
		pageFilters["filter[legal_entity_id]"] = filter.LegalEntityID
	}
	if filter.DomainCode != "" {
		pageFilters["filter[domain_code]"] = filter.DomainCode
	}
	if filter.ActionType != "" {
		pageFilters["filter[action_type]"] = filter.ActionType
	}
	if filter.Outcome != "" {
		pageFilters["filter[outcome]"] = filter.Outcome
	}
	if filter.Since != nil {
		pageFilters["filter[since]"] = filter.Since.Format(time.RFC3339)
	}

	mockReq := &http.Request{
		URL: &url.URL{
			RawQuery: buildQueryString(pageFilters),
		},
	}

	page, parseErr := apicontract.ParsePage(mockReq, allowedFilters, allowedSort, "-evaluated_at")
	if parseErr != nil {
		return out, parseErr
	}

	if filter.PageSize > 0 {
		page.PageSize = filter.PageSize
	}
	if filter.Cursor != "" {
		page.Cursor = filter.Cursor
	}
	if filter.Sort != "" {
		page.Sort = nil
		for _, part := range strings.Split(filter.Sort, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			term := apicontract.SortTerm{Field: strings.TrimPrefix(part, "-"), Desc: strings.HasPrefix(part, "-")}
			page.Sort = append(page.Sort, term)
		}
	}

	var signer *apicontract.CursorSigner
	var err error
	signer, err = apicontract.NewCursorSigner([]byte("evidence-requirements-svc-local-dev-cursor-key-16-bytes-min"))
	if err != nil {
		return out, err
	}

	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return out, fmt.Errorf("tenant not in context")
	}

	route := apicontract.ScopedRoute("/v1/evidence/evaluations")
	filterKey := buildFilterKey(page.Filters)
	sortKey := buildSortKey(page.Sort)

	var cur apicontract.Cursor
	if signer != nil && page.Cursor != "" {
		cur, err = signer.Verify(page.Cursor, tenantID, route, filterKey, sortKey)
	} else if page.Cursor != "" {
		err = errors.New("cursor signer unavailable")
	}
	if err != nil {
		return out, err
	}
	cursor = cur
	if err != nil {
		return out, err
	}

	where := "WHERE tenant_id = $1"
	args := []any{tenantID}
	argIdx := 2

	if filter.LegalEntityID != "" {
		where += fmt.Sprintf(" AND legal_entity_id::text = $%d", argIdx)
		args = append(args, filter.LegalEntityID)
		argIdx++
	}
	if filter.DomainCode != "" {
		where += fmt.Sprintf(" AND domain_code = $%d", argIdx)
		args = append(args, filter.DomainCode)
		argIdx++
	}
	if filter.ActionType != "" {
		where += fmt.Sprintf(" AND action_type = $%d", argIdx)
		args = append(args, filter.ActionType)
		argIdx++
	}
	if filter.Outcome != "" {
		where += fmt.Sprintf(" AND outcome = $%d", argIdx)
		args = append(args, filter.Outcome)
		argIdx++
	}
	if filter.Since != nil {
		where += fmt.Sprintf(" AND evaluated_at >= $%d", argIdx)
		args = append(args, *filter.Since)
		argIdx++
	}

	if cursor.LastID != "" {
		where += fmt.Sprintf(" AND evaluation_id < $%d", argIdx)
		args = append(args, cursor.LastID)
		argIdx++
	}

	orderBy := "ORDER BY "
	sortParts := []string{}
	for _, t := range page.Sort {
		dir := "ASC"
		if t.Desc {
			dir = "DESC"
		}
		sortParts = append(sortParts, t.Field+" "+dir)
	}
	if len(sortParts) == 0 {
		sortParts = append(sortParts, "evaluated_at DESC")
	}
	orderBy += strings.Join(sortParts, ", ")

	limit := page.PageSize + 1
	where += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	query := fmt.Sprintf("SELECT %s FROM evidence_evaluations %s %s", evaluationColumns, where, orderBy)

	var items []domain.EvidenceEvaluation
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEvaluation(rows)
			if err != nil {
				return err
			}
			items = append(items, *e)
		}
		return rows.Err()
	})
	if err != nil {
		return out, err
	}

	hasNext := len(items) > page.PageSize
	if hasNext {
		items = items[:page.PageSize]
	}

	if hasNext && len(items) > 0 {
		last := items[len(items)-1]
		nextCursor, err := signer.Sign(apicontract.Cursor{
			TenantID: tenantID,
			Route:    route,
			Filter:   filterKey,
			Sort:     sortKey,
			Offset:   cursor.Offset + len(items),
			LastID:   last.EvaluationID,
		})
		if err != nil {
			return out, err
		}
		out.NextCursor = nextCursor
	}

	out.Items = items
	return out, nil
}

// GetSummary returns aggregate counts for the evidence dashboard.
func (s *PgStore) GetSummary(ctx context.Context, tenantID string) (*domain.EvidenceSummary, error) {
	var out domain.EvidenceSummary
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Active requirements (effective_to IS NULL or in future)
		err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM evidence_requirements
			WHERE tenant_id = $1 AND (effective_to IS NULL OR effective_to > now())
		`, tenantID).Scan(&out.ActiveRequirements)
		if err != nil {
			return err
		}

		// Evaluations SATISFIED in last 30 days
		err = tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM evidence_evaluations
			WHERE tenant_id = $1 AND outcome = 'SATISFIED' AND evaluated_at >= now() - interval '30 days'
		`, tenantID).Scan(&out.EvaluationsSatisfied30d)
		if err != nil {
			return err
		}

		// Evaluations MISSING in last 30 days
		err = tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM evidence_evaluations
			WHERE tenant_id = $1 AND outcome = 'MISSING' AND evaluated_at >= now() - interval '30 days'
		`, tenantID).Scan(&out.EvaluationsMissing30d)
		if err != nil {
			return err
		}

		// Actions with NO_REQUIREMENTS_DEFINED in last 30 days
		err = tx.QueryRow(ctx, `
			SELECT COUNT(DISTINCT domain_code || ':' || action_type) FROM evidence_evaluations
			WHERE tenant_id = $1 AND outcome = 'NO_REQUIREMENTS_DEFINED' AND evaluated_at >= now() - interval '30 days'
		`, tenantID).Scan(&out.ActionsWithNoRequirements30d)
		if err != nil {
			return err
		}

		// Open PBC requests
		err = tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM evidence_requests
			WHERE tenant_id = $1 AND status NOT IN ('SATISFIED', 'CLOSED')
		`, tenantID).Scan(&out.OpenPBCRequests)
		if err != nil {
			return err
		}

		// Overdue PBC requests
		err = tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM evidence_requests
			WHERE tenant_id = $1 AND status NOT IN ('SATISFIED', 'CLOSED') AND due_at < now()
		`, tenantID).Scan(&out.OverduePBCRequests)
		if err != nil {
			return err
		}

		// Unpublished outbox events older than 5 minutes
		err = tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM outbox
			WHERE published_at IS NULL AND created_at < now() - interval '5 minutes'
		`).Scan(&out.UnpublishedOutboxOlderThan5Minutes)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
