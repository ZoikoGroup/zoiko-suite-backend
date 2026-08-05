// Package store provides the PostgreSQL implementation of the jurisdiction
// rules read and write model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers, service, or domain packages.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/classification"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// Rule lifecycle and drift values the store itself must reason about.
//
// Doctrine (domain/types.go) forbids Go enums over these VARCHAR fields, and
// nothing here switches over the full value space — a new status added by
// data migration flows through untouched. These three are named only because
// the SQL below and the overlap rule genuinely single them out: DRAFT is not
// yet in force, RETIRED is withdrawn, CURRENT is the drift baseline.
const (
	statusDraft       = "DRAFT"
	statusRetired     = "RETIRED"
	driftStateCurrent = "CURRENT"
)

// ListParams controls pagination and filtering for Store.List.
// All filter fields are optional — zero value = no filter applied.
type ListParams struct {
	// JurisdictionType filters by type e.g. "COUNTRY", "STATE_PROVINCE".
	// Empty = return all types.
	JurisdictionType string

	// ActiveOnly = true limits results to active_flag=true and non-expired rows.
	ActiveOnly bool

	// Limit is the page size. 0 defaults to 50; max enforced at 200.
	Limit int

	// Offset is the zero-based page offset.
	Offset int
}

// FindRulesParams controls point-in-time rule lookup.
type FindRulesParams struct {
	// JurisdictionID is required.
	JurisdictionID string

	// Domain filters by rule_domain e.g. "PAYROLL", "TAX".
	// Empty string = all domains. Never use a Go nil here — see handler comment.
	Domain string

	// EffectiveAt is the point-in-time for the half-open interval query.
	// Zero value is treated as time.Now() inside FindRules.
	EffectiveAt time.Time

	// Limit is the page size. 0 defaults to 50; max enforced at 100.
	Limit int

	// Offset is the zero-based page offset.
	Offset int
}

// Store is the interface consumed by the handler for validation, queries, and admin mutations.
type Store interface {
	// FindByID returns the ACTIVE, non-expired Jurisdiction with the given id.
	// This is the fail-closed validation contract other services depend on.
	FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error)

	// FindByIDAny returns the Jurisdiction regardless of active_flag or
	// effective_to. Used where an inactive jurisdiction is still a legitimate
	// answer — chiefly historical rule replay, which must keep working after
	// a jurisdiction is deactivated.
	FindByIDAny(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error)

	// List returns a paginated slice of jurisdictions matching params.
	List(ctx context.Context, params ListParams) ([]*domain.Jurisdiction, error)

	// FindAncestors walks the parent chain starting from jurisdictionID and
	// returns the ordered slice from immediate parent up to the root.
	FindAncestors(ctx context.Context, jurisdictionID string) ([]*domain.Jurisdiction, error)

	// FindRules returns jurisdiction rules active at the given point in time.
	FindRules(ctx context.Context, params FindRulesParams) ([]*domain.JurisdictionRule, error)

	// FindRulePack resolves the runtime rule pack for a jurisdiction and its
	// ancestors at a point in time — one winning rule per (domain, code).
	FindRulePack(ctx context.Context, jurisdictionID, ruleDomain string, at time.Time) (*domain.RulePack, error)

	// CreateJurisdiction inserts a new jurisdiction idempotently.
	CreateJurisdiction(ctx context.Context, params domain.CreateJurisdictionParams) (*domain.Jurisdiction, bool, error)

	// DeactivateJurisdiction sets active_flag=false and end-dates the record.
	DeactivateJurisdiction(ctx context.Context, jurisdictionID, actorID string) (*domain.Jurisdiction, error)

	// FindRuleByID looks up a rule by ID.
	FindRuleByID(ctx context.Context, ruleID string) (*domain.JurisdictionRule, error)

	// CreateRule inserts a new rule idempotently.
	CreateRule(ctx context.Context, params domain.CreateRuleParams) (*domain.JurisdictionRule, bool, error)

	// TransitionRuleStatus atomically updates rule_status if the current status
	// is in allowedPriors. The bool reports whether the status actually
	// changed — false means an idempotent replay, which must not re-emit an event.
	TransitionRuleStatus(ctx context.Context, params TransitionParams) (*domain.JurisdictionRule, bool, error)

	// RecordDrift updates legal_drift_state and appends to the drift history
	// in one transaction. The bool reports whether the state actually changed.
	RecordDrift(ctx context.Context, params domain.RecordDriftParams) (*domain.JurisdictionRule, *domain.DriftEvent, bool, error)

	// FindDriftEvents returns the append-only drift history for a rule, newest first.
	FindDriftEvents(ctx context.Context, ruleID string, limit, offset int) ([]*domain.DriftEvent, error)
}

// TransitionParams holds the inputs for a rule_status transition.
type TransitionParams struct {
	RuleID    string
	NewStatus string

	// AllowedPriors is the set of statuses the rule may currently be in.
	// Never client-supplied — see handler.ruleStatusAllowedPriors.
	AllowedPriors []string

	// EndDate, when true, closes an open-ended rule as part of the transition
	// (used for SUPERSEDED and RETIRED). A rule left with effective_to = NULL
	// after being superseded keeps matching every point-in-time query
	// alongside its own replacement.
	EndDate bool

	// EffectiveTo is the explicit end date to apply when EndDate is true.
	// Nil means "now". Ignored when the rule already has an end date.
	EffectiveTo *time.Time

	ActorID string
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// querier is the subset of pgxpool.Pool that pgx.Tx also satisfies, so the
// shared helpers below work inside and outside a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ── error classification ─────────────────────────────────────────────────────

// isInvalidTextRepresentation reports whether err is Postgres SQLSTATE 22P02,
// which is what a malformed UUID in a path parameter produces.
//
// Without this every "/v1/jurisdictions/not-a-uuid" request died in the pgx
// driver and surfaced as 503 store_unavailable — a client typo reading as a
// platform outage, and one that made the fail-closed 503 contract meaningless
// because callers saw it constantly. A syntactically impossible id cannot
// name an existing row, so it is a 404.
func isInvalidTextRepresentation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// isForeignKeyViolation reports whether err is SQLSTATE 23503.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// notFoundOr maps a scan error onto notFound for both "no rows" and
// "the id could never match a row", and to ErrStoreUnavailable otherwise.
func notFoundOr(err error, notFound error) error {
	if errors.Is(err, pgx.ErrNoRows) || isInvalidTextRepresentation(err) {
		return notFound
	}
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

// ── column lists ─────────────────────────────────────────────────────────────

// jurisdictionColumnNames is the single source of truth for the jurisdiction
// SELECT list. The column list and the scan targets are derived from it so
// they cannot drift apart — a hand-copied SELECT list that silently dropped
// two columns is a defect this codebase has already shipped once.
// Order must match jurisdictionScanTargets exactly.
var jurisdictionColumnNames = []string{
	"jurisdiction_id",
	"jurisdiction_code",
	"jurisdiction_name",
	"jurisdiction_type",
	"parent_jurisdiction_id",
	"authority_type",
	"effective_from",
	"effective_to",
	"active_flag",
	"data_classification",
	"created_at",
	"created_by_principal_id",
	"schema_version",
	"updated_at",
	"updated_by_principal_id",
}

var (
	jurisdictionColumns  = strings.Join(jurisdictionColumnNames, ", ")
	jurisdictionColumnsJ = qualify("j", jurisdictionColumnNames)
)

func jurisdictionScanTargets(j *domain.Jurisdiction) []any {
	return []any{
		&j.JurisdictionID,
		&j.JurisdictionCode,
		&j.JurisdictionName,
		&j.JurisdictionType,
		&j.ParentJurisdictionID,
		&j.AuthorityType,
		&j.EffectiveFrom,
		&j.EffectiveTo,
		&j.ActiveFlag,
		&j.DataClassification,
		&j.CreatedAt,
		&j.CreatedByPrincipalID,
		&j.SchemaVersion,
		&j.UpdatedAt,
		&j.UpdatedByPrincipalID,
	}
}

// ruleColumnNames is the single source of truth for the rule SELECT list.
// Order must match ruleScanTargets exactly.
var ruleColumnNames = []string{
	"jurisdiction_rule_id",
	"jurisdiction_id",
	"rule_domain",
	"rule_code",
	"rule_name",
	"effective_from",
	"effective_to",
	"rule_payload",
	"source_reference",
	"external_feed_reference",
	"rule_status",
	"legal_drift_state",
	"data_classification",
	"created_at",
	"created_by_principal_id",
	"schema_version",
	"updated_at",
	"updated_by_principal_id",
}

var (
	ruleColumns  = strings.Join(ruleColumnNames, ", ")
	ruleColumnsR = qualify("r", ruleColumnNames)
)

func ruleScanTargets(r *domain.JurisdictionRule) []any {
	return []any{
		&r.JurisdictionRuleID,
		&r.JurisdictionID,
		&r.RuleDomain,
		&r.RuleCode,
		&r.RuleName,
		&r.EffectiveFrom,
		&r.EffectiveTo,
		&r.RulePayload,
		&r.SourceReference,
		&r.ExternalFeedReference,
		&r.RuleStatus,
		&r.LegalDriftState,
		&r.DataClassification,
		&r.CreatedAt,
		&r.CreatedByPrincipalID,
		&r.SchemaVersion,
		&r.UpdatedAt,
		&r.UpdatedByPrincipalID,
	}
}

var driftEventColumnNames = []string{
	"drift_event_id",
	"jurisdiction_rule_id",
	"from_state",
	"to_state",
	"reason",
	"effective_at",
	"recorded_by_principal_id",
	"correlation_id",
	"schema_version",
}

var driftEventColumns = strings.Join(driftEventColumnNames, ", ")

func driftEventScanTargets(e *domain.DriftEvent) []any {
	return []any{
		&e.DriftEventID,
		&e.JurisdictionRuleID,
		&e.FromState,
		&e.ToState,
		&e.Reason,
		&e.EffectiveAt,
		&e.RecordedByPrincipalID,
		&e.CorrelationID,
		&e.SchemaVersion,
	}
}

// qualify prefixes every column with a table alias, for queries that join and
// would otherwise be ambiguous on jurisdiction_id.
func qualify(alias string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = alias + "." + c
	}
	return strings.Join(out, ", ")
}

// ── scanners ─────────────────────────────────────────────────────────────────

func scanJurisdiction(row pgx.Row) (*domain.Jurisdiction, error) {
	j := &domain.Jurisdiction{}
	return j, row.Scan(jurisdictionScanTargets(j)...)
}

func scanJurisdictionWithDepth(row pgx.Row) (*domain.Jurisdiction, int, error) {
	j := &domain.Jurisdiction{}
	var depth int
	err := row.Scan(append(jurisdictionScanTargets(j), &depth)...)
	return j, depth, err
}

func scanJurisdictionRule(row pgx.Row) (*domain.JurisdictionRule, error) {
	r := &domain.JurisdictionRule{}
	return r, row.Scan(ruleScanTargets(r)...)
}

func scanDriftEvent(row pgx.Row) (*domain.DriftEvent, error) {
	e := &domain.DriftEvent{}
	return e, row.Scan(driftEventScanTargets(e)...)
}

// ── jurisdictions: reads ─────────────────────────────────────────────────────

// FindByID looks up an active, non-expired jurisdiction by its UUID primary key.
func (s *PgStore) FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error) {
	query := `
		SELECT ` + jurisdictionColumns + `
		FROM jurisdictions
		WHERE jurisdiction_id    = $1
		  AND active_flag        = TRUE
		  AND (effective_to IS NULL OR effective_to > NOW())`

	j, err := scanJurisdiction(s.pool.QueryRow(ctx, query, jurisdictionID))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !isInvalidTextRepresentation(err) {
			s.log.Error("pg FindByID failed",
				zap.String("jurisdiction_id", jurisdictionID),
				zap.Error(err),
			)
		}
		return nil, notFoundOr(err, domain.ErrJurisdictionNotFound)
	}
	return j, nil
}

// FindByIDAny looks up a jurisdiction regardless of active_flag / effective_to.
func (s *PgStore) FindByIDAny(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error) {
	query := `SELECT ` + jurisdictionColumns + ` FROM jurisdictions WHERE jurisdiction_id = $1`

	j, err := scanJurisdiction(s.pool.QueryRow(ctx, query, jurisdictionID))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !isInvalidTextRepresentation(err) {
			s.log.Error("pg FindByIDAny failed",
				zap.String("jurisdiction_id", jurisdictionID),
				zap.Error(err),
			)
		}
		return nil, notFoundOr(err, domain.ErrJurisdictionNotFound)
	}
	return j, nil
}

// List returns a paginated, optionally-filtered slice of jurisdictions.
func (s *PgStore) List(ctx context.Context, params ListParams) ([]*domain.Jurisdiction, error) {
	limit := clampLimit(params.Limit, 50, 200)
	offset := params.Offset
	if offset < 0 {
		offset = 0
	}

	args := []any{}
	conditions := []string{}
	argIdx := 1

	if params.JurisdictionType != "" {
		conditions = append(conditions, fmt.Sprintf("jurisdiction_type = $%d", argIdx))
		args = append(args, params.JurisdictionType)
		argIdx++
	}
	if params.ActiveOnly {
		conditions = append(conditions,
			"active_flag = TRUE",
			"(effective_to IS NULL OR effective_to > NOW())",
		)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM   jurisdictions
		%s
		ORDER BY jurisdiction_code ASC, jurisdiction_id ASC
		LIMIT  $%d OFFSET $%d`,
		jurisdictionColumns, where, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg List failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.Jurisdiction
	for rows.Next() {
		j, scanErr := scanJurisdiction(rows)
		if scanErr != nil {
			s.log.Error("pg List scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, j)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg List rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// maxAncestorDepth bounds hierarchy traversal. A country → state → tax
// authority chain is three deep; 20 is headroom, not an expected depth.
const maxAncestorDepth = 20

// findChain returns the jurisdiction and its ancestors in one round trip,
// self first (depth 0) then outward to the root.
//
// This replaces an N+1 loop that issued one query per generation and, on a
// hierarchy containing a cycle, silently returned the same jurisdictions 20
// times instead of reporting the corruption. The recursive CTE carries the
// visited set with it, so a cycle terminates the walk at the repeat.
func (s *PgStore) findChain(ctx context.Context, jurisdictionID string) ([]*domain.Jurisdiction, error) {
	query := fmt.Sprintf(`
		WITH RECURSIVE chain AS (
		        SELECT jurisdiction_id, parent_jurisdiction_id, 0 AS depth,
		               ARRAY[jurisdiction_id] AS seen
		        FROM   jurisdictions
		        WHERE  jurisdiction_id = $1
		    UNION ALL
		        SELECT p.jurisdiction_id, p.parent_jurisdiction_id, c.depth + 1,
		               c.seen || p.jurisdiction_id
		        FROM   jurisdictions p
		        JOIN   chain c ON p.jurisdiction_id = c.parent_jurisdiction_id
		        WHERE  c.depth < $2
		          AND  NOT p.jurisdiction_id = ANY(c.seen)
		)
		SELECT %s, c.depth
		FROM   jurisdictions j
		JOIN   chain c ON c.jurisdiction_id = j.jurisdiction_id
		ORDER BY c.depth ASC`,
		jurisdictionColumnsJ,
	)

	rows, err := s.pool.Query(ctx, query, jurisdictionID, maxAncestorDepth)
	if err != nil {
		if isInvalidTextRepresentation(err) {
			return nil, domain.ErrJurisdictionNotFound
		}
		s.log.Error("pg findChain failed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var chain []*domain.Jurisdiction
	for rows.Next() {
		j, _, scanErr := scanJurisdictionWithDepth(rows)
		if scanErr != nil {
			s.log.Error("pg findChain scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		chain = append(chain, j)
	}
	if err := rows.Err(); err != nil {
		if isInvalidTextRepresentation(err) {
			return nil, domain.ErrJurisdictionNotFound
		}
		s.log.Error("pg findChain rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if len(chain) == 0 {
		return nil, domain.ErrJurisdictionNotFound
	}
	if len(chain) > maxAncestorDepth {
		s.log.Warn("jurisdiction hierarchy hit the depth ceiling — possible cycle or misconfiguration",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.Int("depth", len(chain)),
		)
	}
	return chain, nil
}

// FindAncestors returns the ancestor chain of jurisdictionID, nearest first.
// The jurisdiction itself is not included.
func (s *PgStore) FindAncestors(ctx context.Context, jurisdictionID string) ([]*domain.Jurisdiction, error) {
	chain, err := s.findChain(ctx, jurisdictionID)
	if err != nil {
		return nil, err
	}
	return chain[1:], nil
}

// ── jurisdictions: writes ────────────────────────────────────────────────────

// CreateJurisdiction inserts a new jurisdiction or returns existing on dedup match.
func (s *PgStore) CreateJurisdiction(ctx context.Context, params domain.CreateJurisdictionParams) (*domain.Jurisdiction, bool, error) {
	if params.JurisdictionID == "" {
		params.JurisdictionID = uuid.New().String()
	}
	if params.SchemaVersion == "" {
		params.SchemaVersion = "1.0"
	}
	if params.DataClassification == "" {
		params.DataClassification = classification.Public.String()
	}

	// Validate the parent before inserting. The self-referential foreign key
	// would otherwise raise 23503, which the generic error path reported as
	// 503 store_unavailable — an outage signal for what is a client mistake.
	if params.ParentJurisdictionID != nil {
		parentID := *params.ParentJurisdictionID
		if parentID == params.JurisdictionID {
			return nil, false, domain.ErrCyclicHierarchy
		}
		if _, err := s.FindByIDAny(ctx, parentID); err != nil {
			if errors.Is(err, domain.ErrJurisdictionNotFound) {
				return nil, false, domain.ErrParentNotFound
			}
			return nil, false, err
		}
	}

	query := `
		INSERT INTO jurisdictions (
			jurisdiction_id, jurisdiction_code, jurisdiction_name, jurisdiction_type,
			parent_jurisdiction_id, authority_type, effective_from, effective_to,
			active_flag, data_classification, created_by_principal_id, schema_version
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (jurisdiction_code, jurisdiction_type, COALESCE(parent_jurisdiction_id, '00000000-0000-0000-0000-000000000000'::UUID))
		DO NOTHING
		RETURNING ` + jurisdictionColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.JurisdictionID, params.JurisdictionCode, params.JurisdictionName, params.JurisdictionType,
		params.ParentJurisdictionID, params.AuthorityType, params.EffectiveFrom, params.EffectiveTo,
		params.ActiveFlag, params.DataClassification, params.CreatedByPrincipalID, params.SchemaVersion,
	)

	j, err := scanJurisdiction(row)
	if err == nil {
		return j, true, nil
	}
	if isForeignKeyViolation(err) {
		return nil, false, domain.ErrParentNotFound
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateJurisdiction failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict occurred on (jurisdiction_code, jurisdiction_type, parent_jurisdiction_id). Lookup existing record.
	lookupQuery := `
		SELECT ` + jurisdictionColumns + `
		FROM jurisdictions
		WHERE jurisdiction_code = $1
		  AND jurisdiction_type = $2
		  AND COALESCE(parent_jurisdiction_id, '00000000-0000-0000-0000-000000000000'::UUID) = COALESCE($3::uuid, '00000000-0000-0000-0000-000000000000'::UUID);`

	row = s.pool.QueryRow(ctx, lookupQuery, params.JurisdictionCode, params.JurisdictionType, params.ParentJurisdictionID)
	j, err = scanJurisdiction(row)
	if err != nil {
		s.log.Error("pg CreateJurisdiction lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if j.JurisdictionName != params.JurisdictionName || j.AuthorityType != params.AuthorityType {
		s.log.Warn("jurisdiction dedup match but attribute mismatch (409 conflict)",
			zap.String("existing_id", j.JurisdictionID),
			zap.String("req_id", params.JurisdictionID),
		)
		return nil, false, domain.ErrConflict
	}

	return j, false, nil
}

// DeactivateJurisdiction sets active_flag=false and end-dates an existing
// jurisdiction.
//
// The domain model states deactivation is "active_flag + effective_to" and
// that there is no soft-delete. Only active_flag was being written, so a
// deactivated jurisdiction still had an open-ended effective period and kept
// satisfying every effective_to-based query in this service and in callers
// that read the record directly.
func (s *PgStore) DeactivateJurisdiction(ctx context.Context, jurisdictionID, actorID string) (*domain.Jurisdiction, error) {
	query := `
		UPDATE jurisdictions
		SET active_flag             = FALSE,
		    effective_to            = COALESCE(effective_to, NOW()),
		    updated_at              = NOW(),
		    updated_by_principal_id = $2
		WHERE jurisdiction_id = $1
		RETURNING ` + jurisdictionColumns + `;`

	j, err := scanJurisdiction(s.pool.QueryRow(ctx, query, jurisdictionID, actorID))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !isInvalidTextRepresentation(err) {
			s.log.Error("pg DeactivateJurisdiction failed", zap.String("id", jurisdictionID), zap.Error(err))
		}
		return nil, notFoundOr(err, domain.ErrJurisdictionNotFound)
	}
	return j, nil
}

// ── rules: reads ─────────────────────────────────────────────────────────────

// FindRuleByID looks up a rule by ID without active status checks.
func (s *PgStore) FindRuleByID(ctx context.Context, ruleID string) (*domain.JurisdictionRule, error) {
	query := `
		SELECT ` + ruleColumns + `
		FROM jurisdiction_rules
		WHERE jurisdiction_rule_id = $1;`

	r, err := scanJurisdictionRule(s.pool.QueryRow(ctx, query, ruleID))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !isInvalidTextRepresentation(err) {
			s.log.Error("pg FindRuleByID failed", zap.String("id", ruleID), zap.Error(err))
		}
		return nil, notFoundOr(err, domain.ErrRuleNotFound)
	}
	return r, nil
}

// FindRules returns rules for a jurisdiction active at a point in time.
//
// Existence is checked with FindByIDAny, not FindByID: a historical query
// against a jurisdiction that has since been deactivated must still answer,
// or the "historical actions must always be explainable against the rule set
// active at time of execution" constraint (03-microservices.md §8.2) fails
// the moment a jurisdiction is retired.
func (s *PgStore) FindRules(ctx context.Context, params FindRulesParams) ([]*domain.JurisdictionRule, error) {
	if _, err := s.FindByIDAny(ctx, params.JurisdictionID); err != nil {
		return nil, err
	}

	at := params.EffectiveAt
	if at.IsZero() {
		at = time.Now().UTC()
	}

	limit := clampLimit(params.Limit, 50, 100)
	offset := params.Offset
	if offset < 0 {
		offset = 0
	}

	query := `
		SELECT ` + ruleColumns + `
		FROM   jurisdiction_rules
		WHERE  jurisdiction_id = $1
		  AND  ($2 = '' OR rule_domain = $2)
		  AND  rule_status    != 'DRAFT'
		  AND  effective_from  <= $3
		  AND  (effective_to IS NULL OR effective_to > $3)
		ORDER BY rule_domain ASC, effective_from ASC, jurisdiction_rule_id ASC
		LIMIT  $4 OFFSET $5`

	rows, err := s.pool.Query(ctx, query, params.JurisdictionID, params.Domain, at, limit, offset)
	if err != nil {
		s.log.Error("pg FindRules failed",
			zap.String("jurisdiction_id", params.JurisdictionID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.JurisdictionRule
	for rows.Next() {
		rule, scanErr := scanJurisdictionRule(rows)
		if scanErr != nil {
			s.log.Error("pg FindRules scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, rule)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg FindRules rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// FindRulePack resolves the runtime rule pack for a jurisdiction at a point in
// time — the "resolve jurisdiction set" + "fetch runtime rule pack" inbound
// APIs of 03-microservices.md §8.2.
//
// Callers previously had to fetch the ancestor chain and then issue one
// /rules request per generation, and merge the results themselves — which
// meant every consumer reimplemented (and could disagree about) which rule
// wins when a country and one of its states both define the same rule_code.
// Resolution happens here, once: nearest jurisdiction wins, ties broken by
// the later effective_from. DRAFT and RETIRED rules never enter a pack.
func (s *PgStore) FindRulePack(ctx context.Context, jurisdictionID, ruleDomain string, at time.Time) (*domain.RulePack, error) {
	chain, err := s.findChain(ctx, jurisdictionID)
	if err != nil {
		return nil, err
	}

	// The pack is a runtime artifact — an inactive or expired jurisdiction
	// has no runtime rules. Fail closed rather than serve a stale pack.
	self := chain[0]
	if !self.ActiveFlag || (self.EffectiveTo != nil && !self.EffectiveTo.After(time.Now().UTC())) {
		return nil, domain.ErrJurisdictionNotFound
	}

	if at.IsZero() {
		at = time.Now().UTC()
	}

	resolvedFrom := make([]string, 0, len(chain))
	for _, j := range chain {
		resolvedFrom = append(resolvedFrom, j.JurisdictionID)
	}

	query := fmt.Sprintf(`
		WITH RECURSIVE chain AS (
		        SELECT jurisdiction_id, parent_jurisdiction_id, 0 AS depth,
		               ARRAY[jurisdiction_id] AS seen
		        FROM   jurisdictions
		        WHERE  jurisdiction_id = $1
		    UNION ALL
		        SELECT p.jurisdiction_id, p.parent_jurisdiction_id, c.depth + 1,
		               c.seen || p.jurisdiction_id
		        FROM   jurisdictions p
		        JOIN   chain c ON p.jurisdiction_id = c.parent_jurisdiction_id
		        WHERE  c.depth < $2
		          AND  NOT p.jurisdiction_id = ANY(c.seen)
		)
		SELECT DISTINCT ON (r.rule_domain, r.rule_code) %s
		FROM   jurisdiction_rules r
		JOIN   chain c ON c.jurisdiction_id = r.jurisdiction_id
		WHERE  ($3 = '' OR r.rule_domain = $3)
		  AND  r.rule_status NOT IN ('DRAFT', 'RETIRED')
		  AND  r.effective_from <= $4
		  AND  (r.effective_to IS NULL OR r.effective_to > $4)
		ORDER BY r.rule_domain ASC, r.rule_code ASC, c.depth ASC, r.effective_from DESC`,
		ruleColumnsR,
	)

	rows, err := s.pool.Query(ctx, query, jurisdictionID, maxAncestorDepth, ruleDomain, at)
	if err != nil {
		s.log.Error("pg FindRulePack failed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	pack := &domain.RulePack{
		JurisdictionID: jurisdictionID,
		EffectiveAt:    at,
		ResolvedFrom:   resolvedFrom,
		Rules:          []*domain.JurisdictionRule{},
	}
	for rows.Next() {
		rule, scanErr := scanJurisdictionRule(rows)
		if scanErr != nil {
			s.log.Error("pg FindRulePack scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		pack.Rules = append(pack.Rules, rule)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg FindRulePack rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return pack, nil
}

// ── rules: writes ────────────────────────────────────────────────────────────

// CreateRule inserts a new rule idempotently.
func (s *PgStore) CreateRule(ctx context.Context, params domain.CreateRuleParams) (*domain.JurisdictionRule, bool, error) {
	if params.JurisdictionRuleID == "" {
		params.JurisdictionRuleID = uuid.New().String()
	}
	if params.SchemaVersion == "" {
		params.SchemaVersion = "1.0"
	}
	if params.LegalDriftState == "" {
		params.LegalDriftState = driftStateCurrent
	}
	if params.DataClassification == "" {
		params.DataClassification = classification.Internal.String()
	}
	if len(params.RulePayload) == 0 {
		params.RulePayload = []byte(`{}`)
	}

	// A rule for a jurisdiction that does not exist (or has been deactivated)
	// used to hit the foreign key and surface as 503 store_unavailable.
	if _, err := s.FindByID(ctx, params.JurisdictionID); err != nil {
		return nil, false, err
	}

	// Two live rules with the same code and overlapping periods make "the
	// effective rule at date X" ambiguous. DRAFT rules are exempt so a
	// replacement can be drafted while the incumbent is still in force; the
	// same check runs again on the transition into ACTIVE.
	if isLiveRuleStatus(params.RuleStatus) {
		overlaps, err := s.hasOverlappingRule(ctx, s.pool, params.JurisdictionID, params.RuleDomain, params.RuleCode, params.EffectiveFrom, params.EffectiveTo)
		if err != nil {
			return nil, false, err
		}
		if overlaps {
			return nil, false, domain.ErrOverlappingRule
		}
	}

	query := `
		INSERT INTO jurisdiction_rules (
			jurisdiction_rule_id, jurisdiction_id, rule_domain, rule_code, rule_name,
			effective_from, effective_to, rule_payload, source_reference, rule_status,
			external_feed_reference, legal_drift_state, data_classification,
			created_by_principal_id, schema_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (jurisdiction_id, rule_code, effective_from)
		DO NOTHING
		RETURNING ` + ruleColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.JurisdictionRuleID, params.JurisdictionID, params.RuleDomain, params.RuleCode, params.RuleName,
		params.EffectiveFrom, params.EffectiveTo, params.RulePayload, params.SourceReference, params.RuleStatus,
		params.ExternalFeedReference, params.LegalDriftState, params.DataClassification,
		params.CreatedByPrincipalID, params.SchemaVersion,
	)

	r, err := scanJurisdictionRule(row)
	if err == nil {
		return r, true, nil
	}
	if isForeignKeyViolation(err) {
		return nil, false, domain.ErrJurisdictionNotFound
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateRule failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict occurred on (jurisdiction_id, rule_code, effective_from). Lookup existing record.
	lookupQuery := `
		SELECT ` + ruleColumns + `
		FROM jurisdiction_rules
		WHERE jurisdiction_id = $1
		  AND rule_code = $2
		  AND effective_from = $3;`

	row = s.pool.QueryRow(ctx, lookupQuery, params.JurisdictionID, params.RuleCode, params.EffectiveFrom)
	r, err = scanJurisdictionRule(row)
	if err != nil {
		s.log.Error("pg CreateRule lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if !jsonEquivalent(r.RulePayload, params.RulePayload) || r.RuleName != params.RuleName {
		s.log.Warn("rule dedup match but payload/name mismatch (409 conflict)",
			zap.String("existing_id", r.JurisdictionRuleID),
			zap.String("req_id", params.JurisdictionRuleID),
		)
		return nil, false, domain.ErrConflict
	}

	return r, false, nil
}

// TransitionRuleStatus updates rule_status atomically with a state machine
// check, returning whether the status actually changed.
//
// Two behaviours worth stating explicitly:
//
//   - An idempotent replay (rule already in the target status) returns the
//     rule with changed=false rather than failing the allowedPriors guard.
//   - A transition into SUPERSEDED or RETIRED closes an open-ended rule
//     (params.EndDate). Without that a superseded rule keeps matching every
//     point-in-time query forever, side by side with its own replacement.
func (s *PgStore) TransitionRuleStatus(ctx context.Context, params TransitionParams) (*domain.JurisdictionRule, bool, error) {
	current, err := s.FindRuleByID(ctx, params.RuleID)
	if err != nil {
		return nil, false, err
	}
	if current.RuleStatus == params.NewStatus {
		s.log.Debug("rule status transition idempotent no-op",
			zap.String("rule_id", params.RuleID),
			zap.String("status", params.NewStatus),
		)
		return current, false, nil
	}

	// Activating a rule whose period overlaps another live rule with the same
	// code would create the ambiguity CreateRule deliberately let through for
	// DRAFT records. This is where that debt comes due.
	if isLiveRuleStatus(params.NewStatus) {
		overlaps, overlapErr := s.hasOverlappingRule(ctx, s.pool, current.JurisdictionID, current.RuleDomain, current.RuleCode, current.EffectiveFrom, current.EffectiveTo)
		if overlapErr != nil {
			return nil, false, overlapErr
		}
		if overlaps {
			return nil, false, domain.ErrOverlappingRule
		}
	}

	var endDate *time.Time
	if params.EndDate {
		endDate = params.EffectiveTo
		if endDate == nil {
			now := time.Now().UTC()
			endDate = &now
		}
		if !endDate.After(current.EffectiveFrom) {
			return nil, false, domain.ErrInvalidEffectivePeriod
		}
	}

	query := `
		UPDATE jurisdiction_rules
		SET rule_status             = $1,
		    effective_to            = CASE WHEN $5::timestamptz IS NOT NULL AND effective_to IS NULL
		                                   THEN $5::timestamptz ELSE effective_to END,
		    updated_at              = NOW(),
		    updated_by_principal_id = $2
		WHERE jurisdiction_rule_id = $3 AND rule_status = ANY($4::text[])
		RETURNING ` + ruleColumns + `;`

	r, err := scanJurisdictionRule(s.pool.QueryRow(ctx, query, params.NewStatus, params.ActorID, params.RuleID, params.AllowedPriors, endDate))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrInvalidTransition
		}
		s.log.Error("pg TransitionRuleStatus failed", zap.String("id", params.RuleID), zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, true, nil
}

// RecordDrift moves a rule's legal_drift_state and appends the transition to
// jurisdiction_rule_drift_events, atomically.
//
// The drift_events table shipped with the initial schema and nothing ever
// wrote to it — legal drift was a column with no history and no way to set
// it, despite "legal drift indicators" being owned by this service and
// legal.drift.detected being one of its published events.
func (s *PgStore) RecordDrift(ctx context.Context, params domain.RecordDriftParams) (*domain.JurisdictionRule, *domain.DriftEvent, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg RecordDrift: begin failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lockQuery := `
		SELECT ` + ruleColumns + `
		FROM jurisdiction_rules
		WHERE jurisdiction_rule_id = $1
		FOR UPDATE;`

	current, err := scanJurisdictionRule(tx.QueryRow(ctx, lockQuery, params.JurisdictionRuleID))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !isInvalidTextRepresentation(err) {
			s.log.Error("pg RecordDrift: lock read failed", zap.Error(err))
		}
		return nil, nil, false, notFoundOr(err, domain.ErrRuleNotFound)
	}

	if current.LegalDriftState == params.ToState {
		// Idempotent replay. Committing an empty transaction is cheaper than
		// rolling back and re-reading, and leaves no history entry behind.
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return current, nil, false, nil
	}

	updateQuery := `
		UPDATE jurisdiction_rules
		SET legal_drift_state       = $1,
		    updated_at              = NOW(),
		    updated_by_principal_id = $2
		WHERE jurisdiction_rule_id = $3
		RETURNING ` + ruleColumns + `;`

	updated, err := scanJurisdictionRule(tx.QueryRow(ctx, updateQuery, params.ToState, params.RecordedByPrincipalID, params.JurisdictionRuleID))
	if err != nil {
		s.log.Error("pg RecordDrift: update failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	insertQuery := `
		INSERT INTO jurisdiction_rule_drift_events (
			jurisdiction_rule_id, from_state, to_state, reason,
			recorded_by_principal_id, correlation_id
		) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + driftEventColumns + `;`

	var correlationID *string
	if params.CorrelationID != "" {
		correlationID = &params.CorrelationID
	}

	event, err := scanDriftEvent(tx.QueryRow(ctx, insertQuery,
		params.JurisdictionRuleID, current.LegalDriftState, params.ToState, params.Reason,
		params.RecordedByPrincipalID, correlationID,
	))
	if err != nil {
		s.log.Error("pg RecordDrift: history insert failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg RecordDrift: commit failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, event, true, nil
}

// FindDriftEvents returns the append-only drift history for a rule, newest first.
func (s *PgStore) FindDriftEvents(ctx context.Context, ruleID string, limit, offset int) ([]*domain.DriftEvent, error) {
	if _, err := s.FindRuleByID(ctx, ruleID); err != nil {
		return nil, err
	}

	limit = clampLimit(limit, 50, 200)
	if offset < 0 {
		offset = 0
	}

	query := `
		SELECT ` + driftEventColumns + `
		FROM   jurisdiction_rule_drift_events
		WHERE  jurisdiction_rule_id = $1
		ORDER BY effective_at DESC, drift_event_id DESC
		LIMIT  $2 OFFSET $3;`

	rows, err := s.pool.Query(ctx, query, ruleID, limit, offset)
	if err != nil {
		s.log.Error("pg FindDriftEvents failed", zap.String("rule_id", ruleID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.DriftEvent
	for rows.Next() {
		e, scanErr := scanDriftEvent(rows)
		if scanErr != nil {
			s.log.Error("pg FindDriftEvents scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, e)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg FindDriftEvents rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// hasOverlappingRule reports whether a live rule other than the one keyed by
// (jurisdictionID, ruleCode, from) already covers any part of [from, to).
//
// The dedup-key row is excluded via effective_from — the unique index on
// (jurisdiction_id, rule_code, effective_from) guarantees at most one row
// shares it, so "same effective_from" means "the row this write is
// idempotent against", never a genuine second rule.
func (s *PgStore) hasOverlappingRule(ctx context.Context, q querier, jurisdictionID, ruleDomain, ruleCode string, from time.Time, to *time.Time) (bool, error) {
	const query = `
		SELECT 1
		FROM   jurisdiction_rules
		WHERE  jurisdiction_id = $1
		  AND  rule_domain     = $2
		  AND  rule_code       = $3
		  AND  effective_from <> $4
		  AND  rule_status NOT IN ('DRAFT', 'RETIRED')
		  AND  $4 < COALESCE(effective_to, 'infinity'::timestamptz)
		  AND  COALESCE($5::timestamptz, 'infinity'::timestamptz) > effective_from
		LIMIT 1;`

	var one int
	err := q.QueryRow(ctx, query, jurisdictionID, ruleDomain, ruleCode, from, to).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		s.log.Error("pg hasOverlappingRule failed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.String("rule_code", ruleCode),
			zap.Error(err),
		)
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return true, nil
}

// isLiveRuleStatus reports whether a rule in this status participates in
// point-in-time resolution, and so must not overlap another such rule.
// DRAFT is not yet in force; RETIRED is withdrawn.
func isLiveRuleStatus(status string) bool {
	return status != "" && status != statusDraft && status != statusRetired
}

// jsonEquivalent compares two JSON documents by value rather than by bytes.
//
// The stored payload comes back from JSONB with normalised key order and
// whitespace, so a byte comparison against the caller's original text
// reported a conflict for an identical replay — turning a retried POST into
// a spurious 409. Falls back to byte equality if either side is not valid JSON.
func jsonEquivalent(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}
