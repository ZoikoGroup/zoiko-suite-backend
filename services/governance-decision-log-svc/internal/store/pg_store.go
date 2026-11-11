// Package store provides the append-only persistence layer for governance
// decisions.
//
// Architectural constraints (doctrine.md, mirrored from
// audit-event-store-svc/internal/store/store.go):
//   - No UPDATE or DELETE on any stored decision — ever.
//   - Idempotency is guaranteed by a single atomic database statement:
//     INSERT INTO governance_decisions … ON CONFLICT (tenant_id, decision_id) DO NOTHING
//     A prior SELECT-EXISTS check is explicitly prohibited: two concurrent
//     callers can both pass a SELECT EXISTS check before either inserts,
//     producing a duplicate row. The ON CONFLICT clause makes the entire
//     upsert atomic at the database level.
//   - A decision, its Idempotency-Key and its outbox event commit together or
//     not at all (RecordDecision). Writing them in three transactions is what
//     let an event, or a key, be lost after the decision committed.
//   - The hash chain (G-07) is computed by the database's BEFORE INSERT trigger
//     (000016 gdl_chain_link), never by this code, so nothing a caller sends can
//     choose a decision's place in the chain or its hashes.
package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/events"
)

// ListParams narrows a query across the five required filters
// (03-microservices.md §8.7): actor, entity, action, rule basis, time
// range — and the register's facets. All fields are optional and compose with
// AND semantics.
type ListParams struct {
	TenantID      string
	ActorID       string
	LegalEntityID string
	ActionType    string
	RuleBasis     string
	From          time.Time
	To            time.Time

	// Register facets (000016).
	Domain             string
	Category           string
	Priority           string
	Jurisdiction       string
	CausationID        string
	WorkflowInstanceID string
	// CorrelationID is the "audit" query dimension of Doc 03 §8.7 (decision
	// D2-2.F11): every decision made under one audited business trace.
	CorrelationID string
	// Query is a free-text search over the id, display reference, title,
	// action type, actor, subject, rule basis and correlation id.
	Query string

	Limit  int
	Offset int
	// Cursor continues keyset pagination (API-4); when set, Offset is ignored.
	Cursor *domain.PaginationCursor
}

// ListResult is one page of decisions.
type ListResult struct {
	Records []*domain.GovernanceDecision
	// Total is every decision matching the filters, not just this page.
	Total int64
	// NextCursor continues after the last record; nil on the last page.
	NextCursor *domain.PaginationCursor
	Limit      int
	Offset     int
}

// DecisionRecord is everything one accepted POST /v1/decisions writes.
type DecisionRecord struct {
	Decision domain.GovernanceDecision
	// IdempotencyKey and BodyHash are recorded only when the decision is
	// newly inserted. Empty IdempotencyKey records no key.
	IdempotencyKey string
	BodyHash       []byte
	// Event is the complete event envelope (events.DecisionRecorded), queued
	// in the outbox only when the decision is newly inserted. Nil queues none.
	Event []byte
	// EventFor, when set, builds the envelope from the STORED row instead —
	// so the event carries what only the database knows (domain when it was
	// classified, chain_seq, curr_hash). Takes precedence over Event.
	EventFor func(stored *domain.GovernanceDecision) ([]byte, error)
}

// ReplayEvent is the outbox row queued with a newly recorded replay manifest.
type ReplayEvent struct {
	// Envelope is the complete event (events.DecisionReplayed).
	Envelope      []byte
	LegalEntityID string
	CorrelationID string
}

// SummaryParams scopes GET /v1/decisions/summary.
type SummaryParams struct {
	TenantID      string
	LegalEntityID string
	From, To      time.Time
}

// ReplayListParams scopes the tenant-wide replay history.
type ReplayListParams struct {
	TenantID       string
	MismatchesOnly bool
	Limit, Offset  int
}

// SealRequest is one POST /v1/evidence/packages/seal.
type SealRequest struct {
	TenantID          string
	PrincipalID       string
	ThroughDecisionID string
	Label, Reason     *string
	IdempotencyKey    string
	RequestHash       []byte
}

// Store is the persistence interface for governance decisions.
type Store interface {
	// RecordDecision writes rec in one transaction. When the tenant already
	// holds rec.Decision.DecisionID it writes nothing and returns the STORED
	// row with created=false, so the caller can compare it with what it was
	// sent instead of echoing an unstored body. ErrIdempotencyKeyInUse means a
	// concurrent request committed the same key first; nothing was written.
	RecordDecision(ctx context.Context, rec DecisionRecord) (stored *domain.GovernanceDecision, created bool, err error)

	// FindByID retrieves a single decision by its DecisionID, scoped to
	// tenantID. A decision belonging to a different tenant is indistinguishable
	// from a nonexistent one — returns domain.ErrDecisionNotFound in both cases.
	FindByID(ctx context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error)

	// List returns one page of decisions matching params, newest decided_at
	// first, with the total match count.
	List(ctx context.Context, params ListParams) (*ListResult, error)

	// Summary aggregates the register over a window.
	Summary(ctx context.Context, params SummaryParams) (*domain.DecisionSummary, error)

	// CreateReplayManifest is idempotent on (tenant, decision, m.IdempotencyKey):
	// when a manifest already exists for that key it writes nothing and returns
	// the stored manifest with created=false. A non-nil event is queued in the
	// outbox in the same transaction, only when the manifest is new.
	CreateReplayManifest(ctx context.Context, m *domain.ReplayManifest, event *ReplayEvent) (stored *domain.ReplayManifest, created bool, err error)
	ListReplayManifestsByDecision(ctx context.Context, tenantID, decisionID string) ([]*domain.ReplayManifest, error)
	ListReplayManifests(ctx context.Context, params ReplayListParams) ([]*domain.ReplayManifest, int64, error)

	// CheckIdempotencyKey returns the decision_id and body hash recorded for
	// a key, or ("", nil, nil) when the key is unused in this tenant.
	CheckIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (string, []byte, error)

	// ── G-07 Evidence ledger ───────────────────────────────────────────────
	DecisionEvidence(ctx context.Context, tenantID, decisionID string) (*domain.DecisionEvidence, error)
	VerifyChain(ctx context.Context, tenantID string) (*domain.ChainVerification, error)
	ExportChain(ctx context.Context, tenantID string) (*ChainExport, error)
	SealPackage(ctx context.Context, req SealRequest) (pkg *domain.EvidencePackage, created bool, err error)
	ListPackages(ctx context.Context, tenantID string) ([]*domain.EvidencePackage, error)
	GetPackage(ctx context.Context, tenantID, packageID string) (*domain.EvidencePackage, error)
	VerifyPackage(ctx context.Context, tenantID, packageID string) (*domain.PackageCheck, error)
}

// PgStore implements Store against PostgreSQL via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New constructs a PgStore.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withRLS runs fn inside a transaction with app.tenant_id set via
// set_config, so the tenant_isolation_policy RLS policy on
// governance_decisions (migration 000002) actually scopes every statement
// fn issues. Mirrors the withRLS pattern used throughout the platform (e.g.
// employee-master-svc).
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// unavailable wraps a genuine store failure, unless it is already one of the
// domain errors a handler answers differently.
func (s *PgStore) unavailable(op string, err error, fields ...zap.Field) error {
	for _, known := range []error{
		domain.ErrDecisionNotFound, domain.ErrIdempotencyKeyInUse, domain.ErrPackageNotFound,
		domain.ErrNothingToSeal, domain.ErrChainBroken, domain.ErrUnknownDomain, domain.ErrInvalidField,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	s.log.Error("pg "+op+" failed", append(fields, zap.Error(err))...)
	return fmt.Errorf("%w: %s: %v", domain.ErrStoreUnavailable, op, err)
}

// decisionColumns is the SELECT list shared by every decision read, over the
// table aliased g. Order must match scanDecision exactly.
const decisionColumns = `
	g.decision_id, g.tenant_id, g.legal_entity_id, g.actor_id, g.action_type,
	g.outcome, g.rule_basis, g.policy_version_id, g.evaluation_context, g.correlation_id,
	g.workflow_instance_id, g.causation_id, g.action_subject_type, g.action_subject_id, g.decided_at,
	g.jurisdiction_context, g.stored_at,
	g.title, g.summary, g.domain, g.priority, g.tags, g.related_obligation_id,
	g.chain_seq, g.prev_hash, g.curr_hash,
	COALESCE((SELECT c.category FROM decision_outcome_categories c WHERE c.outcome = upper(btrim(g.outcome))), 'OTHER'),
	(SELECT count(*) FROM governance_decisions f WHERE f.tenant_id = g.tenant_id AND f.causation_id = g.decision_id)`

// categoryExpr is a decision's status category, for filters and aggregates.
const categoryExpr = `COALESCE((SELECT c.category FROM decision_outcome_categories c WHERE c.outcome = upper(btrim(g.outcome))), 'OTHER')`

// scanDecision scans one row produced by a decisionColumns SELECT.
func scanDecision(row pgx.Row) (*domain.GovernanceDecision, error) {
	var d domain.GovernanceDecision
	var prevHash, currHash []byte
	var followUps int64
	err := row.Scan(
		&d.DecisionID,
		&d.TenantID,
		&d.LegalEntityID,
		&d.ActorID,
		&d.ActionType,
		&d.Outcome,
		&d.RuleBasis,
		&d.PolicyVersionID,
		&d.EvaluationContext,
		&d.CorrelationID,
		&d.WorkflowInstanceID,
		&d.CausationID,
		&d.ActionSubjectType,
		&d.ActionSubjectID,
		&d.DecidedAt,
		&d.JurisdictionContext,
		&d.RecordedAt,
		&d.Title,
		&d.Summary,
		&d.Domain,
		&d.Priority,
		&d.Tags,
		&d.RelatedObligationID,
		&d.ChainSeq,
		&prevHash,
		&currHash,
		&d.StatusCategory,
		&followUps,
	)
	if err != nil {
		return &d, err
	}
	d.PrevHash = hex.EncodeToString(prevHash)
	d.CurrHash = hex.EncodeToString(currHash)
	d.FollowUpCount = int(followUps)
	d.DisplayRef = domain.DisplayRef(d.RecordedAt, d.ChainSeq)
	d.RetentionClass, d.RecordClass = domain.RetentionClass, domain.RecordClass
	return &d, nil
}

// RecordDecision — see Store.
//
// The insert is ON CONFLICT (tenant_id, decision_id) DO NOTHING, the key since
// 000013. RETURNING yields no row when the conflict fired, and the stored row
// is then read back inside the same transaction. The BEFORE INSERT triggers
// classify the domain and link the chain (000016).
func (s *PgStore) RecordDecision(ctx context.Context, rec DecisionRecord) (*domain.GovernanceDecision, bool, error) {
	d := rec.Decision
	const insertQ = `
INSERT INTO governance_decisions AS g
    (decision_id, tenant_id, legal_entity_id, actor_id, action_type,
     outcome, rule_basis, policy_version_id, evaluation_context, correlation_id,
     workflow_instance_id, causation_id, action_subject_type, action_subject_id, decided_at,
     jurisdiction_context, title, summary, domain, priority, tags, related_obligation_id)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
ON CONFLICT (tenant_id, decision_id) DO NOTHING
RETURNING ` + decisionColumns

	var tags any
	if len(d.Tags) > 0 {
		tags = d.Tags
	}
	var domainCode any
	if d.Domain != "" {
		domainCode = d.Domain
	}

	var stored *domain.GovernanceDecision
	created := false
	err := s.withRLS(ctx, d.TenantID, func(tx pgx.Tx) error {
		row, scanErr := scanDecision(tx.QueryRow(ctx, insertQ,
			d.DecisionID,
			d.TenantID,
			d.LegalEntityID,
			d.ActorID,
			d.ActionType,
			d.Outcome,
			d.RuleBasis,
			nullableString(d.PolicyVersionID),
			nullableJSON(d.EvaluationContext),
			d.CorrelationID,
			nullableString(d.WorkflowInstanceID),
			nullableString(d.CausationID),
			nullableString(d.ActionSubjectType),
			nullableString(d.ActionSubjectID),
			d.DecidedAt,
			nullableString(d.JurisdictionContext),
			nullableString(d.Title),
			nullableString(d.Summary),
			domainCode,
			nullableString(d.Priority),
			tags,
			nullableString(d.RelatedObligationID),
		))
		if errors.Is(scanErr, pgx.ErrNoRows) {
			// The tenant already holds this decision_id. Write nothing more:
			// whether this is a true replay is the caller's call to make
			// against the stored row.
			existing, findErr := scanDecision(tx.QueryRow(ctx,
				`SELECT `+decisionColumns+` FROM governance_decisions g WHERE g.tenant_id = $1 AND g.decision_id = $2`,
				d.TenantID, d.DecisionID))
			stored = existing
			return findErr
		}
		if scanErr != nil {
			return classifyWriteError(scanErr)
		}
		stored, created = row, true

		if rec.IdempotencyKey != "" {
			tag, keyErr := tx.Exec(ctx,
				`INSERT INTO idempotency_keys (idempotency_key, tenant_id, body_hash, decision_id)
				 VALUES ($1, $2, $3, $4)
				 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
				rec.IdempotencyKey, d.TenantID, rec.BodyHash, d.DecisionID)
			if keyErr != nil {
				return keyErr
			}
			if tag.RowsAffected() == 0 {
				return domain.ErrIdempotencyKeyInUse
			}
		}

		event := rec.Event
		if rec.EventFor != nil {
			built, buildErr := rec.EventFor(row)
			if buildErr != nil {
				return fmt.Errorf("build decision event: %w", buildErr)
			}
			event = built
		}
		if event != nil {
			if evErr := queueEvents(ctx, tx, event, d.TenantID, d.LegalEntityID, d.ActorID, d.CorrelationID, rec.IdempotencyKey, d.DecisionID); evErr != nil {
				return evErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, s.unavailable("RecordDecision", err, zap.String("decision_id", d.DecisionID))
	}
	return stored, created, nil
}

// classifyWriteError turns the constraint violations a caller can cause into
// domain errors (400), so they are not answered as an outage (503) a caller
// retries forever.
func classifyWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "domain"):
		return domain.ErrUnknownDomain
	case pgErr.Code == "23514", pgErr.Code == "22001":
		return fmt.Errorf("%w: %s", domain.ErrInvalidField, pgErr.Message)
	}
	return err
}

// FindByID retrieves a single decision row, scoped to tenantID. The explicit
// tenant_id = $2 filter is deliberate defense-in-depth alongside the RLS
// policy — see withRLS — rather than relying on RLS alone.
func (s *PgStore) FindByID(ctx context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error) {
	const q = `SELECT ` + decisionColumns + `
FROM governance_decisions g
WHERE g.decision_id = $1 AND g.tenant_id = $2`

	var d *domain.GovernanceDecision
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanDecision(tx.QueryRow(ctx, q, decisionID, tenantID))
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return domain.ErrDecisionNotFound
		}
		return scanErr
	})
	if err != nil {
		return nil, s.unavailable("FindByID", err, zap.String("decision_id", decisionID))
	}
	return d, nil
}

// displayRefPattern is the human reference a register search may name.
var displayRefPattern = regexp.MustCompile(`(?i)^GD-(\d{4})-(\d+)$`)

// whereClause builds the filter of a List (without paging), over alias g.
func whereClause(p ListParams) (string, []any) {
	args := []any{}
	conditions := []string{}
	add := func(cond string, vals ...any) {
		idx := make([]any, len(vals))
		for i, v := range vals {
			args = append(args, v)
			idx[i] = len(args)
		}
		conditions = append(conditions, fmt.Sprintf(cond, idx...))
	}

	// tenant_id is always the first condition — every List call is
	// tenant-scoped, backstopped by the RLS policy set in withRLS.
	add("g.tenant_id = $%d", p.TenantID)
	if p.ActorID != "" {
		add("g.actor_id = $%d", p.ActorID)
	}
	if p.LegalEntityID != "" {
		add("g.legal_entity_id = $%d", p.LegalEntityID)
	}
	if p.ActionType != "" {
		add("g.action_type = $%d", p.ActionType)
	}
	if p.RuleBasis != "" {
		add("g.rule_basis = $%d", p.RuleBasis)
	}
	if !p.From.IsZero() {
		add("g.decided_at >= $%d", p.From)
	}
	if !p.To.IsZero() {
		add("g.decided_at <= $%d", p.To)
	}
	if p.Domain != "" {
		add("g.domain = $%d", p.Domain)
	}
	if p.Category != "" {
		add(categoryExpr+" = $%d", p.Category)
	}
	if p.Priority == "UNSET" {
		conditions = append(conditions, "g.priority IS NULL")
	} else if p.Priority != "" {
		add("g.priority = $%d", p.Priority)
	}
	if p.Jurisdiction != "" {
		add("g.jurisdiction_context = $%d", p.Jurisdiction)
	}
	if p.CausationID != "" {
		add("g.causation_id = $%d", p.CausationID)
	}
	if p.WorkflowInstanceID != "" {
		add("g.workflow_instance_id = $%d", p.WorkflowInstanceID)
	}
	if p.CorrelationID != "" {
		add("g.correlation_id = $%d", p.CorrelationID)
	}
	if q := strings.TrimSpace(p.Query); q != "" {
		if m := displayRefPattern.FindStringSubmatch(q); m != nil {
			year, _ := strconv.Atoi(m[1])
			seq, _ := strconv.ParseInt(m[2], 10, 64)
			add("(g.chain_seq = $%d AND EXTRACT(YEAR FROM g.stored_at AT TIME ZONE 'UTC') = $%d)", seq, year)
		} else {
			like := "%" + escapeLike(q) + "%"
			add(`(g.decision_id ILIKE $%[1]d OR g.title ILIKE $%[1]d OR g.action_type ILIKE $%[1]d
				OR g.actor_id ILIKE $%[1]d OR g.action_subject_id ILIKE $%[1]d OR g.rule_basis ILIKE $%[1]d
				OR g.correlation_id ILIKE $%[1]d OR g.related_obligation_id ILIKE $%[1]d
				OR g.summary ILIKE $%[1]d)`, like)
		}
	}
	return "WHERE " + strings.Join(conditions, " AND "), args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// List — see Store. Newest decided_at first, decision_id breaking ties, so a
// page boundary is stable. One extra row is read to know whether a next page
// exists; the cursor always continues after the last row returned, whether
// this page was reached by offset or by cursor.
func (s *PgStore) List(ctx context.Context, params ListParams) (*ListResult, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := params.Offset
	if offset < 0 || params.Cursor != nil {
		offset = 0
	}

	where, args := whereClause(params)
	countQ := `SELECT count(*) FROM governance_decisions g ` + where

	pageWhere := where
	pageArgs := append([]any{}, args...)
	if params.Cursor != nil {
		cursorAt, err := params.Cursor.ParseDecidedAt()
		if err != nil {
			return nil, fmt.Errorf("%w: cursor", domain.ErrInvalidField)
		}
		pageArgs = append(pageArgs, cursorAt, params.Cursor.DecisionID)
		pageWhere += fmt.Sprintf(" AND (g.decided_at, g.decision_id) < ($%d, $%d)", len(pageArgs)-1, len(pageArgs))
	}
	pageArgs = append(pageArgs, limit+1, offset)
	pageQ := fmt.Sprintf(`SELECT %s FROM governance_decisions g %s
		ORDER BY g.decided_at DESC, g.decision_id DESC
		LIMIT $%d OFFSET $%d`, decisionColumns, pageWhere, len(pageArgs)-1, len(pageArgs))

	res := &ListResult{Limit: limit, Offset: offset}
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, countQ, args...).Scan(&res.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, pageQ, pageArgs...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, scanErr := scanDecision(rows)
			if scanErr != nil {
				return scanErr
			}
			res.Records = append(res.Records, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.unavailable("List", err)
	}

	if len(res.Records) > limit {
		res.Records = res.Records[:limit]
		last := res.Records[limit-1]
		res.NextCursor = &domain.PaginationCursor{
			DecidedAt:  last.DecidedAt.UTC().Format(time.RFC3339Nano),
			DecisionID: last.DecisionID,
		}
	}
	return res, nil
}

// nullableJSON converts an empty/nil RawMessage to nil so Postgres stores
// SQL NULL instead of an empty JSONB value.
func nullableJSON(raw []byte) interface{} {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// nullableString converts a nil or empty string pointer to nil so Postgres stores
// SQL NULL instead of an empty string.
func nullableString(s *string) interface{} {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// ── replay manifests (backlog item 34) ────────────────────────────────────────

const replayManifestColumns = `
	replay_manifest_id, decision_id, policy_version_id,
	replayed_outcome, original_outcome, outcomes_match, replay_notes,
	replayed_at, replayed_by_principal_id, tenant_id,
	COALESCE(idempotency_key, ''), request_hash`

func scanReplayManifest(row pgx.Row) (*domain.ReplayManifest, error) {
	m := &domain.ReplayManifest{}
	err := row.Scan(
		&m.ReplayManifestID, &m.DecisionID, &m.PolicyVersionID,
		&m.ReplayedOutcome, &m.OriginalOutcome, &m.OutcomesMatch, &m.ReplayNotes,
		&m.ReplayedAt, &m.ReplayedByPrincipalID, &m.TenantID,
		&m.IdempotencyKey, &m.RequestHash,
	)
	return m, err
}

// CreateReplayManifest — see Store. The manifest inherits the tenant_id from
// the parent decision, so the insert runs under that tenant's RLS scope.
//
// The unique index from 000014 is the arbiter: of two concurrent replays with
// one key, exactly one inserts, and the other reads back the winner's manifest.
func (s *PgStore) CreateReplayManifest(ctx context.Context, m *domain.ReplayManifest, event *ReplayEvent) (*domain.ReplayManifest, bool, error) {
	const insertQ = `
		INSERT INTO replay_manifests (
			replay_manifest_id, decision_id, policy_version_id,
			replayed_outcome, original_outcome, outcomes_match, replay_notes,
			replayed_at, replayed_by_principal_id, tenant_id,
			idempotency_key, request_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), $12)
		ON CONFLICT (tenant_id, decision_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING ` + replayManifestColumns

	var stored *domain.ReplayManifest
	created := false
	err := s.withRLS(ctx, m.TenantID, func(tx pgx.Tx) error {
		row, scanErr := scanReplayManifest(tx.QueryRow(ctx, insertQ,
			m.ReplayManifestID, m.DecisionID, m.PolicyVersionID,
			m.ReplayedOutcome, m.OriginalOutcome, m.OutcomesMatch, m.ReplayNotes,
			m.ReplayedAt, m.ReplayedByPrincipalID, m.TenantID,
			m.IdempotencyKey, m.RequestHash,
		))
		if errors.Is(scanErr, pgx.ErrNoRows) {
			existing, findErr := scanReplayManifest(tx.QueryRow(ctx,
				`SELECT `+replayManifestColumns+` FROM replay_manifests
				 WHERE tenant_id = $1 AND decision_id = $2 AND idempotency_key = $3`,
				m.TenantID, m.DecisionID, m.IdempotencyKey))
			stored = existing
			return findErr
		}
		if scanErr != nil {
			return scanErr
		}
		stored, created = row, true

		if event != nil {
			if evErr := queueEvents(ctx, tx, event.Envelope, m.TenantID, event.LegalEntityID, m.ReplayedByPrincipalID,
				event.CorrelationID, m.IdempotencyKey, m.DecisionID); evErr != nil {
				return evErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, s.unavailable("CreateReplayManifest", err, zap.String("decision_id", m.DecisionID))
	}
	return stored, created, nil
}

func (s *PgStore) ListReplayManifestsByDecision(ctx context.Context, tenantID, decisionID string) ([]*domain.ReplayManifest, error) {
	// Explicit tenant_id filter for defense-in-depth alongside RLS
	// (mirrors FindByID pattern for governance_decisions)
	const q = `SELECT ` + replayManifestColumns + `
FROM replay_manifests
WHERE decision_id = $1 AND tenant_id = $2
ORDER BY replayed_at DESC`

	var out []*domain.ReplayManifest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, q, decisionID, tenantID)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()

		for rows.Next() {
			m, scanErr := scanReplayManifest(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.unavailable("ListReplayManifestsByDecision", err, zap.String("decision_id", decisionID))
	}
	return out, nil
}

// ListReplayManifests is the tenant's whole replay history, newest first, each
// manifest with the reference and name of the decision it replayed.
func (s *PgStore) ListReplayManifests(ctx context.Context, p ReplayListParams) ([]*domain.ReplayManifest, int64, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}
	filter := "r.tenant_id = $1"
	if p.MismatchesOnly {
		filter += " AND NOT r.outcomes_match"
	}
	q := `SELECT r.replay_manifest_id, r.decision_id, r.policy_version_id,
		r.replayed_outcome, r.original_outcome, r.outcomes_match, r.replay_notes,
		r.replayed_at, r.replayed_by_principal_id, r.tenant_id,
		COALESCE(r.idempotency_key, ''), r.request_hash,
		g.stored_at, g.chain_seq, g.title, g.action_type
		FROM replay_manifests r
		JOIN governance_decisions g ON g.tenant_id = r.tenant_id AND g.decision_id = r.decision_id
		WHERE ` + filter + `
		ORDER BY r.replayed_at DESC, r.replay_manifest_id
		LIMIT $2 OFFSET $3`

	var out []*domain.ReplayManifest
	var total int64
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM replay_manifests r WHERE `+filter, p.TenantID).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, q, p.TenantID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m := &domain.ReplayManifest{}
			var storedAt time.Time
			var seq int64
			if err := rows.Scan(
				&m.ReplayManifestID, &m.DecisionID, &m.PolicyVersionID,
				&m.ReplayedOutcome, &m.OriginalOutcome, &m.OutcomesMatch, &m.ReplayNotes,
				&m.ReplayedAt, &m.ReplayedByPrincipalID, &m.TenantID,
				&m.IdempotencyKey, &m.RequestHash,
				&storedAt, &seq, &m.DecisionTitle, &m.DecisionActionType,
			); err != nil {
				return err
			}
			m.DecisionDisplayRef = domain.DisplayRef(storedAt, seq)
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, s.unavailable("ListReplayManifests", err)
	}
	return out, total, nil
}

// CheckIdempotencyKey retrieves the stored decision_id and body_hash for a
// given idempotency key. Returns ("", nil, nil) if the key doesn't exist.
func (s *PgStore) CheckIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (string, []byte, error) {
	var decisionID string
	var bodyHash []byte
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT decision_id, body_hash FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenantID, idempotencyKey,
		).Scan(&decisionID, &bodyHash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, s.unavailable("CheckIdempotencyKey", err, zap.String("idempotency_key", idempotencyKey))
	}
	return decisionID, bodyHash, nil
}

var _ Store = (*PgStore)(nil)

// queueEvents writes an event to the outbox in the caller's transaction,
// followed by its canonical-name alias (decision C3, rule R9): the outbox row's
// event_type is the envelope's own, never a literal that could drift from it.
func queueEvents(ctx context.Context, tx pgx.Tx, envelope []byte, tenantID, legalEntityID, actorID, correlationID, idempotencyKey, aggregateID string) error {
	queued, err := events.WithCanonicalAlias(envelope)
	if err != nil {
		return err
	}
	for _, q := range queued {
		if _, err := tx.Exec(ctx,
			`INSERT INTO outbox (event_type, payload, tenant_id, legal_entity_id, actor_id, correlation_id, idempotency_key, aggregate_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			q.Type, q.Envelope, tenantID, legalEntityID, actorID, correlationID, idempotencyKey, aggregateID); err != nil {
			return err
		}
	}
	return nil
}
