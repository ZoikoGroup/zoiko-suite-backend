// Package store provides the PostgreSQL implementation of purchase-request-svc's
// persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction — the Row-Level Security policy is real and correctly written.
// But every method ALSO filters explicitly by tenant_id in its own SQL,
// rather than relying on RLS alone: this pool connects as a Postgres
// superuser (DB_USER=postgres, same as every other service in this
// platform), and Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy. This was found via genuine CI failures in
// general-ledger-svc and tenant-entity-registry-svc, so this service is
// built with the explicit filter from day one rather than discovering the
// same gap a third time.
//
// Every state change writes, in ONE transaction: the row (version bumped), an
// append-only purchase_request_history entry, and the outbox events describing
// it. The Kafka relay (internal/outbox) delivers the events afterwards, so a
// broker outage can neither lose an event nor fail a business command.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
	"zoiko.io/purchase-request-svc/internal/outbox"
)

const sourceService = "purchase-request-svc"

// mapPgError translates driver-level failures that are really caller mistakes
// into domain errors, so they stop being reported as outages.
//
// 22P02 (invalid_text_representation) is the one that matters here: this
// service's request_id, tenant_id and legal_entity_id are all uuid columns, so
// a mistyped id fails inside the driver before any row is examined. Left
// unmapped it surfaced as 503 store_unavailable — indistinguishable from the
// database being unreachable, which sends whoever is reading the logs looking
// for an outage that never happened.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "22P02":
		return domain.ErrInvalidIdentifier
	case "P0001":
		// RAISE EXCEPTION from the immutability triggers: a lifecycle rule the
		// application did not catch first. Surface it as a refused transition,
		// never as an outage.
		return fmt.Errorf("%w: %s", domain.ErrInvalidTransition, pgErr.Message)
	}
	return err
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// Pool exposes the pool for the outbox relay and idempotency store.
func (s *PgStore) Pool() *pgxpool.Pool { return s.pool }

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

// ── column plumbing ──────────────────────────────────────────────────────────

const hdrCols = `request_id, tenant_id, legal_entity_id, requested_by_principal_id, description, amount,
	currency_code, status, version, business_purpose, cost_center, project_ref, budget_ref,
	preferred_supplier_ref, to_char(required_date,'YYYY-MM-DD'), attachment_refs, expires_at,
	budget_decision, budget_basis, submitted_by_principal_id, last_amended_by_principal_id,
	approved_by_principal_id, rejected_by_principal_id, rejection_reason, cancelled_by_principal_id,
	cancellation_reason, approval_invalidated_count, converted_purchase_order_id::text,
	converted_by_principal_id, correlation_id, created_at, updated_at, submitted_at, approved_at,
	rejected_at, cancelled_at, converted_at`

func scanHeader(row pgx.Row) (*domain.PurchaseRequest, error) {
	var r domain.PurchaseRequest
	var status, budget string
	var attach []byte
	err := row.Scan(&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.RequestedByPrincipalID, &r.Description, &r.Amount,
		&r.CurrencyCode, &status, &r.Version, &r.BusinessPurpose, &r.CostCenter, &r.ProjectRef, &r.BudgetRef,
		&r.PreferredSupplierRef, &r.RequiredDate, &attach, &r.ExpiresAt,
		&budget, &r.BudgetBasis, &r.SubmittedByPrincipalID, &r.LastAmendedByPrincipalID,
		&r.ApprovedByPrincipalID, &r.RejectedByPrincipalID, &r.RejectionReason, &r.CancelledByPrincipalID,
		&r.CancellationReason, &r.ApprovalInvalidatedCount, &r.ConvertedPurchaseOrderID,
		&r.ConvertedByPrincipalID, &r.CorrelationID, &r.CreatedAt, &r.UpdatedAt, &r.SubmittedAt, &r.ApprovedAt,
		&r.RejectedAt, &r.CancelledAt, &r.ConvertedAt)
	if err != nil {
		return nil, err
	}
	r.Status = domain.RequestStatus(status)
	r.BudgetDecision = domain.BudgetDecision(budget)
	r.AttachmentRefs = decodeRefs(attach)
	r.Lines = []domain.RequestLine{}
	return &r, nil
}

func decodeRefs(b []byte) []string {
	out := []string{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func refsJSON(v []string) []byte {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return b
}

const lineCols = `line_id, line_number, item_ref, description, category, quantity, unit_of_measure, amount,
	currency_code, to_char(required_date,'YYYY-MM-DD'), cost_center, project_ref, budget_ref,
	preferred_supplier_ref, attachment_refs`

func (s *PgStore) loadLines(ctx context.Context, tx pgx.Tx, tenantID, requestID string) ([]domain.RequestLine, error) {
	rows, err := tx.Query(ctx, `SELECT `+lineCols+` FROM purchase_request_lines
		WHERE request_id = $1 AND tenant_id = $2 ORDER BY line_number`, requestID, tenantID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()
	out := []domain.RequestLine{}
	for rows.Next() {
		var l domain.RequestLine
		var attach []byte
		if err := rows.Scan(&l.LineID, &l.LineNumber, &l.ItemRef, &l.Description, &l.Category, &l.Quantity, &l.UnitOfMeasure,
			&l.Amount, &l.CurrencyCode, &l.RequiredDate, &l.CostCenter, &l.ProjectRef, &l.BudgetRef,
			&l.PreferredSupplierRef, &attach); err != nil {
			return nil, err
		}
		l.AttachmentRefs = decodeRefs(attach)
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *PgStore) loadRequest(ctx context.Context, tx pgx.Tx, tenantID, requestID string, forUpdate bool) (*domain.PurchaseRequest, error) {
	q := `SELECT ` + hdrCols + ` FROM purchase_requests WHERE request_id = $1 AND tenant_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	r, err := scanHeader(tx.QueryRow(ctx, q, requestID, tenantID))
	if err != nil {
		return nil, mapPgError(err)
	}
	r.Lines, err = s.loadLines(ctx, tx, tenantID, requestID)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func insertLines(ctx context.Context, tx pgx.Tx, r *domain.PurchaseRequest) error {
	for i := range r.Lines {
		l := &r.Lines[i]
		if l.LineID == "" {
			l.LineID = uuid.NewString()
		}
		l.LineNumber = i + 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO purchase_request_lines (line_id, request_id, tenant_id, line_number, item_ref, description,
				category, quantity, unit_of_measure, amount, currency_code, required_date, cost_center, project_ref,
				budget_ref, preferred_supplier_ref, attachment_refs)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::date,$13,$14,$15,$16,$17)`,
			l.LineID, r.RequestID, r.TenantID, l.LineNumber, l.ItemRef, l.Description, l.Category, l.Quantity,
			l.UnitOfMeasure, l.Amount, l.CurrencyCode, l.RequiredDate, l.CostCenter, l.ProjectRef, l.BudgetRef,
			l.PreferredSupplierRef, refsJSON(l.AttachmentRefs)); err != nil {
			return mapPgError(err)
		}
	}
	return nil
}

// ── history + outbox (always inside the caller's tx) ─────────────────────────

// Meta identifies who is acting and under which correlation.
type Meta struct {
	Actor         string
	CorrelationID string
	Reason        string
	Details       map[string]any
}

func (s *PgStore) recordHistory(ctx context.Context, tx pgx.Tx, r *domain.PurchaseRequest, action string, from *domain.RequestStatus, m Meta) error {
	details := map[string]any{
		"amount":           r.Amount,
		"currency_code":    r.CurrencyCode,
		"lines":            r.Lines,
		"business_purpose": r.BusinessPurpose,
		"cost_center":      r.CostCenter,
		"project_ref":      r.ProjectRef,
		"budget_ref":       r.BudgetRef,
		"budget_decision":  r.BudgetDecision,
		"budget_basis":     r.BudgetBasis,
	}
	for k, v := range m.Details {
		details[k] = v
	}
	var fromStr *string
	if from != nil {
		f := string(*from)
		fromStr = &f
	}
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO purchase_request_history (history_id, request_id, tenant_id, version, action, from_status, to_status, actor, reason, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		uuid.NewString(), r.RequestID, r.TenantID, r.Version, action, fromStr, string(r.Status), m.Actor, m.Reason, b)
	return err
}

// emit inserts one outbox event (spec name or alias) for r.
func (s *PgStore) emit(ctx context.Context, tx pgx.Tx, eventType string, r *domain.PurchaseRequest, m Meta, payload any) error {
	id := uuid.NewString()
	ev := outbox.Event{
		OutboxEventID: id, AggregateType: "purchase_request", AggregateID: r.RequestID, EventType: eventType,
		TenantID: r.TenantID, LegalEntityID: r.LegalEntityID, ActorID: m.Actor, CorrelationID: m.CorrelationID,
	}
	ev.Payload = payload
	env := outbox.NewEnvelope(id, sourceService, eventType, ev, r.Version)
	ev.Payload = env
	_, err := outbox.Insert(ctx, tx, ev)
	return err
}

// summary is the payload of spec-named events.
func summary(r *domain.PurchaseRequest, m Meta) map[string]any {
	p := map[string]any{
		"request_id":      r.RequestID,
		"tenant_id":       r.TenantID,
		"legal_entity_id": r.LegalEntityID,
		"status":          r.Status,
		"version":         r.Version,
		"amount":          r.Amount,
		"currency_code":   r.CurrencyCode,
		"requested_by":    r.RequestedByPrincipalID,
		"correlation_id":  m.CorrelationID,
	}
	if r.ConvertedPurchaseOrderID != nil {
		p["purchase_order_id"] = *r.ConvertedPurchaseOrderID
	}
	if m.Reason != "" {
		p["reason"] = m.Reason
	}
	return p
}

// ── create ───────────────────────────────────────────────────────────────────

// CreateRequest inserts a requisition in DRAFT with its lines.
//
// Idempotent on (tenant_id, correlation_id): a retried call (e.g. a client
// timeout on a POST that actually succeeded server-side) hits the partial
// unique index and resolves to the ORIGINAL request — mutating *req in place to
// reflect it — rather than creating a duplicate. Returns created=false when the
// row already existed.
func (s *PgStore) CreateRequest(ctx context.Context, req *domain.PurchaseRequest) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrTenantScopeMissing
	}
	// The row is filed under the verified scope, not under req.TenantID.
	req.TenantID = tenantID
	req.Version = 1
	req.Status = domain.RequestStatusDraft
	req.BudgetDecision = domain.BudgetNotChecked
	req.RecomputeAmount()

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			INSERT INTO purchase_requests (
				request_id, tenant_id, legal_entity_id, requested_by_principal_id, description, amount,
				currency_code, status, version, business_purpose, cost_center, project_ref, budget_ref,
				preferred_supplier_ref, required_date, attachment_refs, expires_at, correlation_id,
				created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,'DRAFT',1,$8,$9,$10,$11,$12,$13::date,$14,$15,$16,$17,$17)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, req.RequestID, req.TenantID, req.LegalEntityID, req.RequestedByPrincipalID, req.Description, req.Amount,
			req.CurrencyCode, req.BusinessPurpose, req.CostCenter, req.ProjectRef, req.BudgetRef,
			req.PreferredSupplierRef, req.RequiredDate, refsJSON(req.AttachmentRefs), req.ExpiresAt, req.CorrelationID, now)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			existing, err := scanHeader(tx.QueryRow(ctx, `SELECT `+hdrCols+` FROM purchase_requests
				WHERE tenant_id = $1 AND correlation_id = $2`, req.TenantID, req.CorrelationID))
			if err != nil {
				return err
			}
			existing.Lines, err = s.loadLines(ctx, tx, existing.TenantID, existing.RequestID)
			if err != nil {
				return err
			}
			*req = *existing
			created = false
			return nil
		}
		if err := insertLines(ctx, tx, req); err != nil {
			return err
		}
		req.CreatedAt, req.UpdatedAt = now, now
		m := Meta{Actor: req.RequestedByPrincipalID, CorrelationID: req.CorrelationID}
		if err := s.recordHistory(ctx, tx, req, "CREATED", nil, m); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, "PurchaseRequisitionCreated", req, m, summary(req, m)); err != nil {
			return err
		}
		// Alias: the pre-AP-02 event name other consumers already read.
		if err := s.emit(ctx, tx, "purchase.request.created", req, m, map[string]any{
			"request_id": req.RequestID, "tenant_id": req.TenantID, "legal_entity_id": req.LegalEntityID, "amount": req.Amount,
		}); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

// ── reads ────────────────────────────────────────────────────────────────────

// GetRequest returns (nil, nil) if not found — including when the caller's
// tenant scope doesn't match the request's tenant (explicit filter, not
// RLS-only — see package doc).
func (s *PgStore) GetRequest(ctx context.Context, requestID string) (*domain.PurchaseRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}
	var r *domain.PurchaseRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		r, err = s.loadRequest(ctx, tx, tenantID, requestID, false)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	// A malformed request_id or tenant scope cannot name an existing row, so it
	// is absent — not an outage. Reported identically to a well-formed id that
	// happens not to exist, and to another tenant's request.
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ListRequests returns purchase requests matching the given filter
// (tenant_id is required; the others are optional). Lines are included.
func (s *PgStore) ListRequests(ctx context.Context, filter domain.ListRequestsFilter) ([]domain.PurchaseRequest, error) {
	var out []domain.PurchaseRequest
	status := ""
	if filter.Status != "" {
		status = string(domain.NormalizeStatus(filter.Status))
	}
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+hdrCols+`
			FROM purchase_requests
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR status = $3)
			ORDER BY created_at DESC
		`, filter.TenantID, filter.LegalEntityID, status)
		if err != nil {
			return mapPgError(err)
		}
		var heads []*domain.PurchaseRequest
		for rows.Next() {
			r, err := scanHeader(rows)
			if err != nil {
				rows.Close()
				return err
			}
			heads = append(heads, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, h := range heads {
			h.Lines, err = s.loadLines(ctx, tx, h.TenantID, h.RequestID)
			if err != nil {
				return err
			}
			out = append(out, *h)
		}
		return nil
	})
	return out, err
}

// GetHistory returns a requisition's append-only history, oldest first.
func (s *PgStore) GetHistory(ctx context.Context, requestID string) ([]domain.HistoryEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}
	out := []domain.HistoryEntry{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT history_id, request_id, version, action, from_status, to_status, actor, reason, details, created_at
			FROM purchase_request_history
			WHERE request_id = $1 AND tenant_id = $2 ORDER BY created_at, version`, requestID, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var h domain.HistoryEntry
			var det []byte
			if err := rows.Scan(&h.HistoryID, &h.RequestID, &h.Version, &h.Action, &h.FromStatus, &h.ToStatus, &h.Actor, &h.Reason, &det, &h.CreatedAt); err != nil {
				return err
			}
			h.Details = map[string]any{}
			_ = json.Unmarshal(det, &h.Details)
			out = append(out, h)
		}
		return rows.Err()
	})
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return []domain.HistoryEntry{}, nil
	}
	return out, err
}

// ── state transitions ────────────────────────────────────────────────────────

// Transition describes one lifecycle move.
type Transition struct {
	TenantID        string
	RequestID       string
	ExpectedVersion *int
	From            []domain.RequestStatus
	To              domain.RequestStatus
	Action          string // history action, e.g. "SUBMITTED"
	Meta            Meta
	// Guard runs against the row while it is locked (FOR UPDATE), so a check
	// such as segregation of duties cannot be raced by a concurrent amend.
	Guard func(cur *domain.PurchaseRequest) error
	// Budget, when non-nil, records the budget check outcome (submission).
	Budget *BudgetOutcome
	// PurchaseOrderID is set when converting.
	PurchaseOrderID string
	// Events to emit once the row has moved (primary spec event + aliases).
	Events []EventSpec
}

// BudgetOutcome is the recorded budget/policy response.
type BudgetOutcome struct {
	Decision domain.BudgetDecision
	Basis    string
}

// EventSpec names an outbox event; Payload nil means the standard summary.
type EventSpec struct {
	Type    string
	Payload func(r *domain.PurchaseRequest, m Meta) any
}

// Apply performs the transition atomically: lock, version/state checks, guard,
// UPDATE (version+1), history row and outbox events in one transaction.
func (s *PgStore) Apply(ctx context.Context, t Transition) (*domain.PurchaseRequest, error) {
	var out *domain.PurchaseRequest
	err := s.withRLS(ctx, t.TenantID, func(tx pgx.Tx) error {
		cur, err := s.loadRequest(ctx, tx, t.TenantID, t.RequestID, true)
		if err != nil {
			return err
		}
		if t.ExpectedVersion != nil && cur.Version != *t.ExpectedVersion {
			return domain.ErrStaleVersion
		}
		legalFrom := false
		for _, f := range t.From {
			if cur.Status == f {
				legalFrom = true
			}
		}
		if !legalFrom || !domain.CanTransition(cur.Status, t.To) {
			return domain.ErrInvalidTransition
		}
		if t.Guard != nil {
			if err := t.Guard(cur); err != nil {
				return err
			}
		}
		from := cur.Status
		now := time.Now().UTC()
		actor := t.Meta.Actor

		var budgetDecision, budgetBasis any
		if t.Budget != nil {
			budgetDecision, budgetBasis = string(t.Budget.Decision), t.Budget.Basis
		}
		var poID any
		if t.PurchaseOrderID != "" {
			poID = t.PurchaseOrderID
		}
		reason := t.Meta.Reason
		row := tx.QueryRow(ctx, `
			UPDATE purchase_requests SET
				status = $3::varchar, version = version + 1, updated_at = $4,
				budget_decision = COALESCE($5, budget_decision), budget_basis = COALESCE($6, budget_basis),
				submitted_by_principal_id = CASE WHEN $3::varchar = 'PENDING_APPROVAL' THEN $7 ELSE submitted_by_principal_id END,
				submitted_at = CASE WHEN $3::varchar = 'PENDING_APPROVAL' THEN $4 ELSE submitted_at END,
				approved_by_principal_id = CASE WHEN $3::varchar = 'APPROVED' THEN $7 ELSE approved_by_principal_id END,
				approved_at = CASE WHEN $3::varchar = 'APPROVED' THEN $4 ELSE approved_at END,
				rejected_by_principal_id = CASE WHEN $3::varchar = 'REJECTED' THEN $7 ELSE rejected_by_principal_id END,
				rejected_at = CASE WHEN $3::varchar = 'REJECTED' THEN $4 ELSE rejected_at END,
				rejection_reason = CASE WHEN $3::varchar = 'REJECTED' THEN $8 ELSE rejection_reason END,
				cancelled_by_principal_id = CASE WHEN $3::varchar = 'CANCELLED' THEN $7 ELSE cancelled_by_principal_id END,
				cancelled_at = CASE WHEN $3::varchar = 'CANCELLED' THEN $4 ELSE cancelled_at END,
				cancellation_reason = CASE WHEN $3::varchar = 'CANCELLED' THEN $8 ELSE cancellation_reason END,
				converted_purchase_order_id = CASE WHEN $3::varchar = 'CONVERTED' THEN $9::uuid ELSE converted_purchase_order_id END,
				converted_by_principal_id = CASE WHEN $3::varchar = 'CONVERTED' THEN $7 ELSE converted_by_principal_id END,
				converted_at = CASE WHEN $3::varchar = 'CONVERTED' THEN $4 ELSE converted_at END
			WHERE request_id = $1 AND tenant_id = $2
			RETURNING `+hdrCols,
			t.RequestID, t.TenantID, string(t.To), now, budgetDecision, budgetBasis, actor, reason, poID)
		upd, err := scanHeader(row)
		if err != nil {
			return mapPgError(err)
		}
		upd.Lines = cur.Lines
		if err := s.recordHistory(ctx, tx, upd, t.Action, &from, t.Meta); err != nil {
			return err
		}
		for _, ev := range t.Events {
			var payload any
			if ev.Payload != nil {
				payload = ev.Payload(upd, t.Meta)
			} else {
				payload = summary(upd, t.Meta)
			}
			if err := s.emit(ctx, tx, ev.Type, upd, t.Meta, payload); err != nil {
				return err
			}
		}
		out = upd
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, domain.ErrRequestNotFound
	}
	return out, err
}

// Amendment describes a content change.
type Amendment struct {
	TenantID        string
	RequestID       string
	ExpectedVersion *int
	Meta            Meta
	// Apply mutates a copy of the locked current state and validates it. It
	// must not change Status/Version. Return an error to abort.
	Apply func(r *domain.PurchaseRequest) error
}

// AmendRequest applies an amendment.
//
// A requisition in PENDING_APPROVAL or APPROVED is dropped back to DRAFT in the
// same UPDATE that changes its content: the approval (and the budget decision)
// is invalidated and must be earned again, and an "approval invalidated" event
// is emitted. Terminal states refuse. Returns whether an approval was
// invalidated.
func (s *PgStore) AmendRequest(ctx context.Context, a Amendment) (*domain.PurchaseRequest, bool, error) {
	var out *domain.PurchaseRequest
	invalidated := false
	err := s.withRLS(ctx, a.TenantID, func(tx pgx.Tx) error {
		cur, err := s.loadRequest(ctx, tx, a.TenantID, a.RequestID, true)
		if err != nil {
			return err
		}
		if a.ExpectedVersion != nil && cur.Version != *a.ExpectedVersion {
			return domain.ErrStaleVersion
		}
		switch cur.Status {
		case domain.RequestStatusDraft, domain.RequestStatusPending, domain.RequestStatusApproved:
		default:
			return domain.ErrInvalidTransition
		}
		invalidated = cur.Status == domain.RequestStatusApproved
		from := cur.Status
		work := *cur
		work.Lines = append([]domain.RequestLine(nil), cur.Lines...)
		if err := a.Apply(&work); err != nil {
			return err
		}
		work.RecomputeAmount()

		row := tx.QueryRow(ctx, `
			UPDATE purchase_requests SET
				status = 'DRAFT', version = version + 1, updated_at = now(),
				description = $3, amount = $4, currency_code = $5, business_purpose = $6, cost_center = $7,
				project_ref = $8, budget_ref = $9, preferred_supplier_ref = $10, required_date = $11::date,
				attachment_refs = $12, expires_at = $13, last_amended_by_principal_id = $14,
				budget_decision = 'NOT_CHECKED', budget_basis = '',
				approved_by_principal_id = NULL, approved_at = NULL,
				submitted_by_principal_id = NULL, submitted_at = NULL,
				approval_invalidated_count = approval_invalidated_count + $15
			WHERE request_id = $1 AND tenant_id = $2
			RETURNING `+hdrCols,
			a.RequestID, a.TenantID, work.Description, work.Amount, work.CurrencyCode, work.BusinessPurpose,
			work.CostCenter, work.ProjectRef, work.BudgetRef, work.PreferredSupplierRef, work.RequiredDate,
			refsJSON(work.AttachmentRefs), work.ExpiresAt, a.Meta.Actor, boolInt(from == domain.RequestStatusApproved))
		upd, err := scanHeader(row)
		if err != nil {
			return mapPgError(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM purchase_request_lines WHERE request_id = $1 AND tenant_id = $2`, a.RequestID, a.TenantID); err != nil {
			return mapPgError(err)
		}
		upd.Lines = work.Lines
		if err := insertLines(ctx, tx, upd); err != nil {
			return err
		}
		m := a.Meta
		if invalidated {
			m.Details = map[string]any{"approval_invalidated": true, "previous_status": string(from)}
		}
		if err := s.recordHistory(ctx, tx, upd, "AMENDED", &from, m); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, "PurchaseRequisitionAmended", upd, m, summary(upd, m)); err != nil {
			return err
		}
		if invalidated || from == domain.RequestStatusPending {
			if err := s.emit(ctx, tx, "PurchaseRequisitionApprovalInvalidated", upd, m, summary(upd, m)); err != nil {
				return err
			}
		}
		out = upd
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, false, domain.ErrRequestNotFound
	}
	return out, invalidated, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// QueueWorkerExpiry is the app.queue_worker value migration 000007 accepts for
// the expiry sweeper's cross-tenant read.
const QueueWorkerExpiry = "requisition-expiry"

// ExpireDue moves every PENDING_APPROVAL/APPROVED requisition whose expires_at
// has passed to EXPIRED (history + event). It is a system job: it finds work
// across tenants and applies each expiry inside the owning tenant's scope.
// Returns how many expired.
//
// Discovery runs in a transaction that sets app.queue_worker, which migration
// 000007's SELECT-only policy honours: under a NOBYPASSRLS role the bare pool
// would see no rows at all. The marker grants no write -- each expiry below is
// an ordinary tenant-scoped transition.
func (s *PgStore) ExpireDue(ctx context.Context, now time.Time) (int, error) {
	type key struct{ tenant, id string }
	var due []key
	err := func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.queue_worker', $1, true)", QueueWorkerExpiry); err != nil {
			return fmt.Errorf("set_config app.queue_worker: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT tenant_id::text, request_id::text FROM purchase_requests
			WHERE expires_at IS NOT NULL AND expires_at <= $1 AND status IN ('PENDING_APPROVAL','APPROVED')
			LIMIT 200`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.tenant, &k.id); err != nil {
				return err
			}
			due = append(due, k)
		}
		return rows.Err()
	}()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range due {
		_, err := s.Apply(ctx, Transition{
			TenantID: k.tenant, RequestID: k.id,
			From: []domain.RequestStatus{domain.RequestStatusPending, domain.RequestStatusApproved},
			To:   domain.RequestStatusExpired, Action: "EXPIRED",
			Meta:   Meta{Actor: "system:expiry", Reason: "requisition validity window elapsed"},
			Events: []EventSpec{{Type: "PurchaseRequisitionExpired"}},
		})
		if err != nil {
			s.log.Warn("expire requisition failed", zap.String("request_id", k.id), zap.Error(err))
			continue
		}
		n++
	}
	return n, nil
}
