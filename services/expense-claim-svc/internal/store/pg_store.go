// Package store implements expense-claim-svc's persistence.
//
// Every state change runs in ONE transaction that (1) locks the claim row
// and checks expected_version and the transition table, (2) updates the
// claim (version + 1), (3) appends the audit-trail row, (4) writes the
// outbox rows (spec event name + legacy alias) and (5) stores the
// Idempotency-Key result. Every query carries an explicit tenant predicate
// in addition to row-level security.
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

	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/middleware"
	"zoiko.io/expense-claim-svc/internal/outbox"
)

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// isInvalidUUID reports Postgres's "invalid input syntax for type uuid".
func isInvalidUUID(err error) bool { return pgCode(err) == "22P02" }

// isUniqueViolation reports a unique-constraint violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool { return pgCode(err) == "23505" }

// isClaimNotEditable reports the custom SQLSTATE ZK001 raised by the
// expense_lines trigger when the parent claim is no longer DRAFT/RETURNED.
func isClaimNotEditable(err error) bool { return pgCode(err) == "ZK001" }

// isIllegalTransition reports ZK002, raised by the claims trigger for a
// state change outside the transition table.
func isIllegalTransition(err error) bool { return pgCode(err) == "ZK002" }

type nullString struct{ dest *string }

func (n *nullString) Scan(src interface{}) error {
	if src == nil {
		*n.dest = ""
		return nil
	}
	s, _ := src.(string)
	*n.dest = s
	return nil
}

// Store is the interface the handler and the payable relay depend on.
type Store interface {
	// GetIdempotency returns the stored result of an earlier command with
	// this Idempotency-Key in the caller's tenant, or nil.
	GetIdempotency(ctx context.Context, key string) (*domain.IdemRecord, error)

	CreateClaim(ctx context.Context, tenantID string, req domain.CreateExpenseClaimRequest, principalID, correlationID string, idem *domain.IdemKey) (*domain.ExpenseClaim, error)
	FindClaim(ctx context.Context, claimID string) (*domain.ExpenseClaim, error)

	AddExpenseLine(ctx context.Context, claimID string, req domain.AddExpenseLineRequest, principalID string, idem *domain.IdemKey) (*domain.ExpenseLine, error)
	VoidExpenseLine(ctx context.Context, claimID, lineID, reason, principalID, correlationID string, idem *domain.IdemKey) (*domain.ExpenseLine, error)
	ListLines(ctx context.Context, claimID string) ([]domain.ExpenseLine, error)
	SetLineTaxDetermination(ctx context.Context, lineID, determinationID string, taxableAmount, calculatedTaxAmount float64) error

	// SubmitClaim moves DRAFT/RETURNED → SUBMITTED and writes the immutable
	// submission snapshot (version N+1). On an already SUBMITTED claim it is
	// a no-op that returns the claim (routing is being resumed).
	SubmitClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error)
	// RouteForApproval moves SUBMITTED → PENDING_APPROVAL, recording the
	// policy assessment.
	RouteForApproval(ctx context.Context, p domain.RoutingParams) (*domain.ExpenseClaim, error)
	// ApproveClaim moves PENDING_APPROVAL → APPROVED and, in the same
	// transaction, writes the durable AP-08 hand-off record and the
	// accounting fact.
	ApproveClaim(ctx context.Context, p domain.ApproveParams) (*domain.ExpenseClaim, error)
	RejectClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error)
	ReturnClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error)
	CancelClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error)
	RecordPolicyException(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error)
	// CloseClaim moves REIMBURSABLE → CLOSED.
	CloseClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error)

	IsReceiptInUse(ctx context.Context, documentID string) (inUse bool, claimID string, lineID string, err error)
	ListClaimEvents(ctx context.Context, claimID string) ([]domain.ExpenseClaimEvent, error)
	ListSubmissions(ctx context.Context, claimID string) ([]domain.ExpenseClaimSubmission, error)

	// ── payable relay (cross-tenant; each call below re-enters the request's own tenant) ──

	// FindPayableRequest returns the claim's hand-off record in the caller's tenant (nil if none).
	FindPayableRequest(ctx context.Context, claimID string) (*domain.PayableRequest, error)
	ListDuePayableRequests(ctx context.Context, limit int) ([]domain.PayableRequest, error)
	CompletePayableRequest(ctx context.Context, req domain.PayableRequest, payableID, payeeRef, destinationID string) error
	BlockPayableRequest(ctx context.Context, req domain.PayableRequest, reason string, nextAttempt time.Time) error
	RecordPayableFailure(ctx context.Context, req domain.PayableRequest, errText string, nextAttempt time.Time) error
	ListSettlementCandidates(ctx context.Context, limit int) ([]domain.SettlementCandidate, error)

	// ListPostingRequests returns the claim's ACC-04 posting requests with their real status.
	ListPostingRequests(ctx context.Context, claimID string) ([]domain.PostingRequest, error)
	// DispatchPostings locks due PENDING requests (FOR UPDATE SKIP LOCKED), asks fn for each
	// outcome and records it, all in one transaction.
	DispatchPostings(ctx context.Context, limit int, fn func(domain.PostingRequest) domain.PostingOutcome) (int, error)
	// RequeuePostings puts the claim's FAILED/QUARANTINED posting requests back
	// to PENDING (after an operator fixed the cause); POSTED is never touched.
	RequeuePostings(ctx context.Context, claimID string) (int64, error)
	MarkSettlementChecked(ctx context.Context, tenantID, claimID string) error
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// Pool exposes the pool to the outbox relay.
func (s *PgStore) Pool() *pgxpool.Pool { return s.pool }

func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", middleware.TenantFromContext(ctx)); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// withSystem runs fn as the background relay: no tenant, but admitted by the
// app.system_relay disjunct of the tenant policies.
func (s *PgStore) withSystem(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', '', true), set_config('app.system_relay', 'true', true)"); err != nil {
		return fmt.Errorf("set_config app.system_relay: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func tenantOf(ctx context.Context) string { return middleware.TenantFromContext(ctx) }

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PgStore) unavailable(op string, err error) error {
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

// ── history, outbox, idempotency ─────────────────────────────────────────────

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, claimID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO expense_claim_events (event_id, tenant_id, claim_id, event_type, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), tenantID, claimID, eventType, detail, actorPrincipalID,
	)
	return err
}

// enqueue writes one outbox row per published name of a history event type
// (spec name + legacy alias), in the caller's transaction.
func (s *PgStore) enqueue(ctx context.Context, tx pgx.Tx, histType string, c *domain.ExpenseClaim, actor, corr string, payload any) error {
	var corrPtr *string
	if corr != "" {
		corrPtr = &corr
	}
	for _, t := range domain.OutboxTypes(histType) {
		id := uuid.NewString()
		env := outbox.NewVariantBEnvelope(id, t, c.ClaimID, c.TenantID, &actor, corrPtr, payload)
		if err := outbox.Insert(ctx, tx, outbox.Event{
			OutboxEventID: id, AggregateType: "expense_claim", AggregateID: c.ClaimID, EventType: t,
			TenantID: c.TenantID, LegalEntityID: c.LegalEntityID, ActorID: &actor, CorrelationID: corrPtr, Payload: env,
		}); err != nil {
			return err
		}
	}
	return nil
}

// emit appends the audit-trail row and publishes the event through the outbox.
func (s *PgStore) emit(ctx context.Context, tx pgx.Tx, c *domain.ExpenseClaim, histType, detail, actor, corr string, extra map[string]any) error {
	if err := s.recordEvent(ctx, tx, c.TenantID, c.ClaimID, histType, detail, actor); err != nil {
		return err
	}
	payload := map[string]any{"claim": c, "detail": detail}
	for k, v := range extra {
		payload[k] = v
	}
	return s.enqueue(ctx, tx, histType, c, actor, corr, payload)
}

func (s *PgStore) GetIdempotency(ctx context.Context, key string) (*domain.IdemRecord, error) {
	var rec domain.IdemRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT operation, request_hash, status_code, response::text
			FROM expense_claim_idempotency WHERE tenant_id::text = $1 AND idempotency_key = $2`,
			tenantOf(ctx), key).Scan(&rec.Operation, &rec.RequestHash, &rec.StatusCode, &rec.Body)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.unavailable("GetIdempotency", err)
	}
	return &rec, nil
}

// saveIdem stores the command result in the command's own transaction.
func (s *PgStore) saveIdem(ctx context.Context, tx pgx.Tx, idem *domain.IdemKey, status int, body any) error {
	if idem == nil {
		return nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO expense_claim_idempotency (tenant_id, idempotency_key, operation, request_hash, status_code, response)
		VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		tenantOf(ctx), idem.Key, idem.Operation, idem.RequestHash, status, string(b))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrIdempotencyConflict
	}
	return nil
}

// ── claims ───────────────────────────────────────────────────────────────────

const claimColumns = `
	claim_id, tenant_id, legal_entity_id, claimant_principal_id, currency, business_purpose,
	project_cost_center, payment_preference_ref, status, version, submitted_version, rejection_reason, return_reason,
	has_policy_exception, policy_exception_reason, policy_assessment_result, policy_version_id,
	approved_by_principal_id, approved_at, payable_state, payable_id, payable_blocked_reason,
	payee_destination_id, closed_at, close_reason, created_at, updated_at`

func scanClaim(row pgx.Row) (*domain.ExpenseClaim, error) {
	c := &domain.ExpenseClaim{}
	err := row.Scan(&c.ClaimID, &c.TenantID, &c.LegalEntityID, &c.ClaimantPrincipalID, &c.Currency, &nullString{&c.BusinessPurpose},
		&nullString{&c.ProjectCostCenter}, &nullString{&c.PaymentPreferenceRef}, &c.Status, &c.Version, &c.SubmittedVersion,
		&nullString{&c.RejectionReason}, &nullString{&c.ReturnReason}, &c.HasPolicyException, &nullString{&c.PolicyExceptionReason},
		&c.PolicyAssessmentResult, &nullString{&c.PolicyVersionID}, &c.ApprovedByPrincipalID, &c.ApprovedAt,
		&c.PayableState, &nullString{&c.PayableID}, &nullString{&c.PayableBlockedReason}, &nullString{&c.PayeeDestinationID},
		&c.ClosedAt, &nullString{&c.CloseReason}, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *PgStore) CreateClaim(ctx context.Context, tenantID string, req domain.CreateExpenseClaimRequest, principalID, correlationID string, idem *domain.IdemKey) (*domain.ExpenseClaim, error) {
	id := uuid.New().String()
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `
			INSERT INTO expense_claims (claim_id, tenant_id, legal_entity_id, claimant_principal_id, currency, business_purpose,
				project_cost_center, payment_preference_ref, status, policy_assessment_result)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'DRAFT', 'NOT_ASSESSED')
			RETURNING `+claimColumns,
			id, tenantID, req.LegalEntityID, req.ClaimantPrincipalID, req.Currency,
			req.BusinessPurpose, req.ProjectCostCenter, req.PaymentPreferenceRef,
		))
		if err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventClaimCreated, "", principalID, correlationID, nil); err != nil {
			return err
		}
		return s.saveIdem(ctx, tx, idem, 201, c)
	})
	if errors.Is(err, domain.ErrIdempotencyConflict) {
		return nil, err
	}
	if err != nil {
		return nil, s.unavailable("CreateClaim", err)
	}
	return c, nil
}

func (s *PgStore) FindClaim(ctx context.Context, claimID string) (*domain.ExpenseClaim, error) {
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `SELECT `+claimColumns+` FROM expense_claims WHERE claim_id = $1 AND tenant_id::text = $2`, claimID, tenantOf(ctx)))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrClaimNotFound
	}
	if err != nil {
		return nil, s.unavailable("FindClaim", err)
	}
	return c, nil
}

// lockClaim reads the claim FOR UPDATE and checks expected_version.
func (s *PgStore) lockClaim(ctx context.Context, tx pgx.Tx, claimID string, expected *int) (*domain.ExpenseClaim, error) {
	c, err := scanClaim(tx.QueryRow(ctx, `SELECT `+claimColumns+` FROM expense_claims WHERE claim_id = $1 AND tenant_id::text = $2 FOR UPDATE`, claimID, tenantOf(ctx)))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrClaimNotFound
	}
	if err != nil {
		return nil, err
	}
	if expected != nil && *expected != c.Version {
		return nil, domain.ErrStaleVersion
	}
	return c, nil
}

// updateClaim applies a SET clause (args start at $2) and bumps the version.
func updateClaim(ctx context.Context, tx pgx.Tx, claimID, set string, args ...any) (*domain.ExpenseClaim, error) {
	all := append([]any{claimID}, args...)
	return scanClaim(tx.QueryRow(ctx, `
		UPDATE expense_claims SET `+set+`, version = version + 1, updated_at = NOW()
		WHERE claim_id = $1 RETURNING `+claimColumns, all...))
}

type command struct {
	p        domain.CommandParams
	allowed  func(domain.ClaimStatus) bool
	apply    func(tx pgx.Tx, c *domain.ExpenseClaim) (*domain.ExpenseClaim, error)
	histType string
	detail   string
	// extra builds additional outbox payload fields from the updated claim.
	extra func(tx pgx.Tx, updated *domain.ExpenseClaim) (map[string]any, error)
	// after runs further in-transaction writes (e.g. the payable request).
	after func(tx pgx.Tx, updated *domain.ExpenseClaim) error
}

func (s *PgStore) runCommand(ctx context.Context, op string, cmd command) (*domain.ExpenseClaim, error) {
	var updated *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		c, err := s.lockClaim(ctx, tx, cmd.p.ClaimID, cmd.p.ExpectedVersion)
		if err != nil {
			return err
		}
		if !cmd.allowed(c.Status) {
			return domain.ErrInvalidTransition
		}
		if updated, err = cmd.apply(tx, c); err != nil {
			return err
		}
		var extra map[string]any
		if cmd.extra != nil {
			if extra, err = cmd.extra(tx, updated); err != nil {
				return err
			}
		}
		if err := s.emit(ctx, tx, updated, cmd.histType, cmd.detail, cmd.p.PrincipalID, cmd.p.CorrelationID, extra); err != nil {
			return err
		}
		if cmd.after != nil {
			if err := cmd.after(tx, updated); err != nil {
				return err
			}
		}
		return s.saveIdem(ctx, tx, cmd.p.Idem, 200, updated)
	})
	return s.mapCommandErr(op, updated, err)
}

func (s *PgStore) mapCommandErr(op string, c *domain.ExpenseClaim, err error) (*domain.ExpenseClaim, error) {
	switch {
	case err == nil:
		return c, nil
	case errors.Is(err, domain.ErrClaimNotFound), errors.Is(err, domain.ErrStaleVersion), errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrIdempotencyConflict), errors.Is(err, domain.ErrNoLines):
		return nil, err
	case isIllegalTransition(err), isClaimNotEditable(err):
		return nil, domain.ErrInvalidTransition
	default:
		return nil, s.unavailable(op, err)
	}
}

// ── expense lines ────────────────────────────────────────────────────────────

const lineColumns = `
	line_id, tenant_id, claim_id, merchant, expense_date, amount, currency, category, project_cost_center,
	receipt_document_id, claim_tax_recovery, jurisdiction, tax_category, tax_determination_id,
	taxable_amount, calculated_tax_amount, voided_at, void_reason, created_at`

func scanLine(row pgx.Row) (*domain.ExpenseLine, error) {
	l := &domain.ExpenseLine{}
	err := row.Scan(&l.LineID, &l.TenantID, &l.ClaimID, &l.Merchant, &l.ExpenseDate, &l.Amount, &l.Currency,
		&nullString{&l.Category}, &nullString{&l.ProjectCostCenter}, &nullString{&l.ReceiptDocumentID}, &l.ClaimTaxRecovery,
		&nullString{&l.Jurisdiction}, &nullString{&l.TaxCategory}, &nullString{&l.TaxDeterminationID},
		&l.TaxableAmount, &l.CalculatedTaxAmount, &l.VoidedAt, &nullString{&l.VoidReason}, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// AddExpenseLine relies on the migration's BEFORE INSERT trigger to reject a
// line added to a claim that is no longer DRAFT/RETURNED, and on the
// partial unique index to reject a receipt document already attached to
// another live line — both genuine database invariants.
func (s *PgStore) AddExpenseLine(ctx context.Context, claimID string, req domain.AddExpenseLineRequest, principalID string, idem *domain.IdemKey) (*domain.ExpenseLine, error) {
	id := uuid.New().String()
	var l *domain.ExpenseLine
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		var claimCurrency string
		if err := tx.QueryRow(ctx, `SELECT tenant_id, currency FROM expense_claims WHERE claim_id = $1 AND tenant_id::text = $2`, claimID, tenantOf(ctx)).Scan(&tenantID, &claimCurrency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrClaimNotFound
			}
			return err
		}
		if req.Currency != claimCurrency {
			return domain.ErrCurrencyMismatch
		}
		var err error
		l, err = scanLine(tx.QueryRow(ctx, `
			INSERT INTO expense_lines (line_id, tenant_id, claim_id, merchant, expense_date, amount, currency, category,
				project_cost_center, receipt_document_id, claim_tax_recovery, jurisdiction, tax_category)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			RETURNING `+lineColumns,
			id, tenantID, claimID, req.Merchant, req.ExpenseDate, req.Amount, req.Currency, req.Category,
			req.ProjectCostCenter, strPtrOrNil(req.ReceiptDocumentID), req.ClaimTaxRecovery,
			req.Jurisdiction, req.TaxCategory,
		))
		if err != nil {
			return err
		}
		return s.saveIdem(ctx, tx, idem, 201, l)
	})
	switch {
	case err == nil:
		return l, nil
	case errors.Is(err, domain.ErrClaimNotFound), errors.Is(err, domain.ErrCurrencyMismatch), errors.Is(err, domain.ErrIdempotencyConflict):
		return nil, err
	case isUniqueViolation(err):
		return nil, domain.ErrDuplicateReceipt
	case isClaimNotEditable(err):
		return nil, domain.ErrInvalidTransition
	case isInvalidUUID(err):
		// A receipt_document_id that is not a UUID cannot name a document: the
		// caller's mistake, not an outage.
		return nil, domain.ErrDocumentNotFound
	default:
		return nil, s.unavailable("AddExpenseLine", err)
	}
}

// VoidExpenseLine marks a line void (kept as evidence, receipt freed). Only
// while the claim is DRAFT/RETURNED.
func (s *PgStore) VoidExpenseLine(ctx context.Context, claimID, lineID, reason, principalID, correlationID string, idem *domain.IdemKey) (*domain.ExpenseLine, error) {
	var l *domain.ExpenseLine
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		c, err := s.lockClaim(ctx, tx, claimID, nil)
		if err != nil {
			return err
		}
		if !domain.CanAddLine(c.Status) {
			return domain.ErrInvalidTransition
		}
		l, err = scanLine(tx.QueryRow(ctx, `
			UPDATE expense_lines SET voided_at = NOW(), void_reason = $3
			WHERE line_id = $1 AND claim_id = $2 AND tenant_id::text = $4 AND voided_at IS NULL
			RETURNING `+lineColumns, lineID, claimID, reason, tenantOf(ctx)))
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return domain.ErrLineNotFound
		}
		if err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventLineVoided, reason, principalID, correlationID, map[string]any{"line": l}); err != nil {
			return err
		}
		return s.saveIdem(ctx, tx, idem, 200, l)
	})
	switch {
	case err == nil:
		return l, nil
	case errors.Is(err, domain.ErrClaimNotFound), errors.Is(err, domain.ErrLineNotFound), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrIdempotencyConflict):
		return nil, err
	case isClaimNotEditable(err):
		return nil, domain.ErrInvalidTransition
	default:
		return nil, s.unavailable("VoidExpenseLine", err)
	}
}

func (s *PgStore) ListLines(ctx context.Context, claimID string) ([]domain.ExpenseLine, error) {
	var out []domain.ExpenseLine
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = listLines(ctx, tx, claimID, tenantOf(ctx), false)
		return err
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.unavailable("ListLines", err)
	}
	return out, nil
}

func listLines(ctx context.Context, tx pgx.Tx, claimID, tenantID string, activeOnly bool) ([]domain.ExpenseLine, error) {
	q := `SELECT ` + lineColumns + ` FROM expense_lines WHERE claim_id = $1 AND tenant_id::text = $2`
	if activeOnly {
		q += ` AND voided_at IS NULL`
	}
	rows, err := tx.Query(ctx, q+` ORDER BY created_at ASC, line_id ASC`, claimID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ExpenseLine
	for rows.Next() {
		l, err := scanLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

func (s *PgStore) SetLineTaxDetermination(ctx context.Context, lineID, determinationID string, taxableAmount, calculatedTaxAmount float64) error {
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE expense_lines SET tax_determination_id = $2, taxable_amount = $3, calculated_tax_amount = $4
			WHERE line_id = $1 AND tenant_id::text = $5`,
			lineID, determinationID, taxableAmount, calculatedTaxAmount, tenantOf(ctx),
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrLineNotFound
		}
		return nil
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrLineNotFound):
		return domain.ErrLineNotFound
	case isClaimNotEditable(err):
		return domain.ErrInvalidTransition
	default:
		return s.unavailable("SetLineTaxDetermination", err)
	}
}

func (s *PgStore) IsReceiptInUse(ctx context.Context, documentID string) (bool, string, string, error) {
	var claimID, lineID string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT claim_id, line_id FROM expense_lines
			WHERE receipt_document_id = $1 AND voided_at IS NULL AND tenant_id::text = $2 LIMIT 1`, documentID, tenantOf(ctx)).Scan(&claimID, &lineID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return false, "", "", nil
	}
	if err != nil {
		return false, "", "", s.unavailable("IsReceiptInUse", err)
	}
	return true, claimID, lineID, nil
}

// ── claim lifecycle ──────────────────────────────────────────────────────────

func (s *PgStore) SubmitClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	var snapshotHash string
	var result *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		c, err := s.lockClaim(ctx, tx, p.ClaimID, p.ExpectedVersion)
		if err != nil {
			return err
		}
		if c.Status == domain.StatusSubmitted { // resume routing: nothing to write
			result = c
			return s.saveIdem(ctx, tx, p.Idem, 200, c)
		}
		if !domain.CanTransition(c.Status, domain.StatusSubmitted) {
			return domain.ErrInvalidTransition
		}
		lines, err := listLines(ctx, tx, p.ClaimID, tenantOf(ctx), true)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return domain.ErrNoLines
		}
		versionNo := c.SubmittedVersion + 1
		snap := domain.BuildSnapshot(c, lines, versionNo, p.PrincipalID, time.Now())
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		canonical, hash, err := domain.SnapshotHash(raw)
		if err != nil {
			return err
		}
		snapshotHash = hash
		if _, err := tx.Exec(ctx, `
			INSERT INTO expense_claim_submissions (submission_id, tenant_id, claim_id, version_no, snapshot, snapshot_hash, submitted_by)
			VALUES ($1, $2::uuid, $3, $4, $5::jsonb, $6, $7)`,
			uuid.NewString(), tenantOf(ctx), p.ClaimID, versionNo, string(canonical), hash, p.PrincipalID); err != nil {
			return err
		}
		updated, err := updateClaim(ctx, tx, p.ClaimID, `status = 'SUBMITTED', submitted_version = $2`, versionNo)
		if err != nil {
			return err
		}
		result = updated
		detail := fmt.Sprintf("version %d sha256:%s", versionNo, hash)
		if err := s.emit(ctx, tx, updated, domain.EventClaimSubmitted, detail, p.PrincipalID, p.CorrelationID,
			map[string]any{"submission_version": versionNo, "snapshot_hash": snapshotHash, "total_amount": snap.TotalAmount}); err != nil {
			return err
		}
		return s.saveIdem(ctx, tx, p.Idem, 200, updated)
	})
	return s.mapCommandErr("SubmitClaim", result, err)
}

func (s *PgStore) RouteForApproval(ctx context.Context, p domain.RoutingParams) (*domain.ExpenseClaim, error) {
	var result *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		c, err := s.lockClaim(ctx, tx, p.ClaimID, nil)
		if err != nil {
			return err
		}
		if c.Status == domain.StatusPendingApproval { // already routed
			result = c
			return s.saveIdem(ctx, tx, p.Idem, 200, c)
		}
		if c.Status != domain.StatusSubmitted {
			return domain.ErrInvalidTransition
		}
		updated, err := updateClaim(ctx, tx, p.ClaimID, `status = 'PENDING_APPROVAL', policy_assessment_result = $2, policy_version_id = $3`,
			string(p.PolicyResult), p.PolicyVersionID)
		if err != nil {
			return err
		}
		result = updated
		if err := s.emit(ctx, tx, updated, domain.EventClaimRouted, string(p.PolicyResult), p.PrincipalID, p.CorrelationID, nil); err != nil {
			return err
		}
		return s.saveIdem(ctx, tx, p.Idem, 200, updated)
	})
	return s.mapCommandErr("RouteForApproval", result, err)
}

func (s *PgStore) ApproveClaim(ctx context.Context, p domain.ApproveParams) (*domain.ExpenseClaim, error) {
	return s.runCommand(ctx, "ApproveClaim", command{
		p:       p.CommandParams,
		allowed: domain.CanDecide,
		apply: func(tx pgx.Tx, _ *domain.ExpenseClaim) (*domain.ExpenseClaim, error) {
			return updateClaim(ctx, tx, p.ClaimID, `status = 'APPROVED', approved_by_principal_id = $2, approved_at = NOW(), payable_state = 'PENDING'`, p.PrincipalID)
		},
		histType: domain.EventClaimApproved,
		extra: func(tx pgx.Tx, updated *domain.ExpenseClaim) (map[string]any, error) {
			lines, err := listLines(ctx, tx, p.ClaimID, tenantOf(ctx), true)
			if err != nil {
				return nil, err
			}
			if len(lines) == 0 {
				return nil, domain.ErrNoLines
			}
			return map[string]any{"submission_version": updated.SubmittedVersion}, nil
		},
		after: func(tx pgx.Tx, updated *domain.ExpenseClaim) error {
			lines, err := listLines(ctx, tx, p.ClaimID, tenantOf(ctx), true)
			if err != nil {
				return err
			}
			var total float64
			for _, l := range lines {
				total += l.Amount
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO payable_requests (tenant_id, legal_entity_id, claim_id, claimant_principal_id, payment_preference_ref,
					requested_by, correlation_id, amount, currency, due_date)
				VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				ON CONFLICT (claim_id) DO NOTHING`,
				tenantOf(ctx), updated.LegalEntityID, updated.ClaimID, updated.ClaimantPrincipalID, updated.PaymentPreferenceRef,
				p.PrincipalID, p.CorrelationID, total, updated.Currency, p.DueDate); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, updated, domain.EventClaimPayableRequested, fmt.Sprintf("%.2f %s due %s", total, updated.Currency, p.DueDate.UTC().Format("2006-01-02")),
				p.PrincipalID, p.CorrelationID, map[string]any{"amount": total, "currency": updated.Currency, "due_date": p.DueDate.UTC()}); err != nil {
				return err
			}
			posting := domain.BuildApprovalPosting(updated, lines, p.Posting, p.CorrelationID, time.Now())
			postingJSON, err := json.Marshal(posting)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO accounting_posting_requests (tenant_id, legal_entity_id, aggregate_id, source_event_id, request_payload)
				VALUES ($1::uuid, $2, $3, $4, $5::jsonb)
				ON CONFLICT (tenant_id, source_event_id) DO NOTHING`,
				tenantOf(ctx), updated.LegalEntityID, updated.ClaimID, posting.SourceEventID, string(postingJSON)); err != nil {
				return err
			}
			acc, err := domain.NewAccountingRequested(uuid.NewString(), updated, lines, time.Now())
			if err != nil {
				return err
			}
			return s.enqueue(ctx, tx, domain.EventAccountingRequested, updated, p.PrincipalID, p.CorrelationID, acc)
		},
	})
}

func (s *PgStore) RejectClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.runCommand(ctx, "RejectClaim", command{
		p: p, allowed: domain.CanDecide, histType: domain.EventClaimRejected, detail: p.Reason,
		apply: func(tx pgx.Tx, _ *domain.ExpenseClaim) (*domain.ExpenseClaim, error) {
			return updateClaim(ctx, tx, p.ClaimID, `status = 'REJECTED', rejection_reason = $2`, p.Reason)
		},
	})
}

func (s *PgStore) ReturnClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.runCommand(ctx, "ReturnClaim", command{
		p: p, allowed: domain.CanDecide, histType: domain.EventClaimReturned, detail: p.Reason,
		apply: func(tx pgx.Tx, _ *domain.ExpenseClaim) (*domain.ExpenseClaim, error) {
			return updateClaim(ctx, tx, p.ClaimID, `status = 'RETURNED', return_reason = $2`, p.Reason)
		},
	})
}

func (s *PgStore) CancelClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.runCommand(ctx, "CancelClaim", command{
		p: p, allowed: domain.CanCancel, histType: domain.EventClaimCancelled, detail: p.Reason,
		apply: func(tx pgx.Tx, _ *domain.ExpenseClaim) (*domain.ExpenseClaim, error) {
			return updateClaim(ctx, tx, p.ClaimID, `status = 'CANCELLED'`)
		},
	})
}

func (s *PgStore) RecordPolicyException(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.runCommand(ctx, "RecordPolicyException", command{
		p: p, allowed: domain.CanDecide, histType: domain.EventPolicyExceptionRecorded, detail: p.Reason,
		apply: func(tx pgx.Tx, _ *domain.ExpenseClaim) (*domain.ExpenseClaim, error) {
			return updateClaim(ctx, tx, p.ClaimID, `has_policy_exception = TRUE, policy_exception_reason = $2`, p.Reason)
		},
	})
}

func (s *PgStore) CloseClaim(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error) {
	return s.runCommand(ctx, "CloseClaim", command{
		p: p, allowed: domain.CanClose, histType: domain.EventClaimClosed, detail: p.Reason,
		apply: func(tx pgx.Tx, _ *domain.ExpenseClaim) (*domain.ExpenseClaim, error) {
			return updateClaim(ctx, tx, p.ClaimID, `status = 'CLOSED', closed_at = NOW(), close_reason = $2`, p.Reason)
		},
	})
}

// ── events & submissions ─────────────────────────────────────────────────────

func (s *PgStore) ListClaimEvents(ctx context.Context, claimID string) ([]domain.ExpenseClaimEvent, error) {
	var out []domain.ExpenseClaimEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, claim_id, event_type, detail, actor_principal_id, created_at
			FROM expense_claim_events WHERE claim_id = $1 AND tenant_id::text = $2 ORDER BY created_at ASC, event_id ASC`, claimID, tenantOf(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ExpenseClaimEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.ClaimID, &e.EventType, &nullString{&e.Detail}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.unavailable("ListClaimEvents", err)
	}
	return out, nil
}

func (s *PgStore) ListSubmissions(ctx context.Context, claimID string) ([]domain.ExpenseClaimSubmission, error) {
	var out []domain.ExpenseClaimSubmission
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT submission_id, claim_id, version_no, snapshot::text, snapshot_hash, submitted_by, submitted_at
			FROM expense_claim_submissions WHERE claim_id = $1 AND tenant_id::text = $2 ORDER BY version_no ASC`, claimID, tenantOf(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sub domain.ExpenseClaimSubmission
			var snap string
			if err := rows.Scan(&sub.SubmissionID, &sub.ClaimID, &sub.VersionNo, &snap, &sub.SnapshotHash, &sub.SubmittedBy, &sub.SubmittedAt); err != nil {
				return err
			}
			sub.Snapshot = json.RawMessage(snap)
			out = append(out, sub)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.unavailable("ListSubmissions", err)
	}
	return out, nil
}

// ── payable relay ────────────────────────────────────────────────────────────

func (s *PgStore) FindPayableRequest(ctx context.Context, claimID string) (*domain.PayableRequest, error) {
	var r domain.PayableRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT request_id, tenant_id::text, legal_entity_id::text, claim_id::text, claimant_principal_id, payment_preference_ref,
			       requested_by, correlation_id, amount::float8, currency, due_date, state, blocked_reason, attempts
			FROM payable_requests WHERE claim_id = $1 AND tenant_id::text = $2`, claimID, tenantOf(ctx)).
			Scan(&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.ClaimID, &r.ClaimantPrincipalID, &r.PaymentPreferenceRef,
				&r.RequestedBy, &r.CorrelationID, &r.Amount, &r.Currency, &r.DueDate, &r.State, &r.BlockedReason, &r.Attempts)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.unavailable("FindPayableRequest", err)
	}
	return &r, nil
}

func (s *PgStore) ListDuePayableRequests(ctx context.Context, limit int) ([]domain.PayableRequest, error) {
	var out []domain.PayableRequest
	err := s.withSystem(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT request_id, tenant_id::text, legal_entity_id::text, claim_id::text, claimant_principal_id, payment_preference_ref,
			       requested_by, correlation_id, amount::float8, currency, due_date, state, blocked_reason, attempts
			FROM payable_requests
			WHERE state IN ('PENDING', 'BLOCKED') AND next_attempt_at <= now()
			ORDER BY next_attempt_at ASC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.PayableRequest
			if err := rows.Scan(&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.ClaimID, &r.ClaimantPrincipalID, &r.PaymentPreferenceRef,
				&r.RequestedBy, &r.CorrelationID, &r.Amount, &r.Currency, &r.DueDate, &r.State, &r.BlockedReason, &r.Attempts); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.unavailable("ListDuePayableRequests", err)
	}
	return out, nil
}

// CompletePayableRequest records the AP-08 payable id and moves the claim
// APPROVED → REIMBURSABLE. Idempotent: a request already CREATED is left alone.
func (s *PgStore) CompletePayableRequest(ctx context.Context, req domain.PayableRequest, payableID, payeeRef, destinationID string) error {
	ctx = middleware.WithTenant(ctx, req.TenantID)
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM payable_requests WHERE request_id = $1 AND tenant_id::text = $2 FOR UPDATE`, req.RequestID, req.TenantID).Scan(&state); err != nil {
			return err
		}
		if state == string(domain.PayableCreated) {
			return nil
		}
		c, err := s.lockClaim(ctx, tx, req.ClaimID, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payable_requests SET state = 'CREATED', blocked_reason = '', payable_id = $2, payee_ref = $3, payee_destination_id = $4,
				attempts = attempts + 1, last_error = '', updated_at = NOW()
			WHERE request_id = $1`, req.RequestID, payableID, payeeRef, destinationID); err != nil {
			return err
		}
		if c.Status != domain.StatusApproved {
			return nil // e.g. a legacy row; the request is still recorded
		}
		updated, err := updateClaim(ctx, tx, req.ClaimID, `status = 'REIMBURSABLE', payable_state = 'CREATED', payable_id = $2, payable_blocked_reason = '', payee_destination_id = $3`,
			payableID, destinationID)
		if err != nil {
			return err
		}
		return s.emit(ctx, tx, updated, domain.EventClaimPayableCreated, payableID, "system:payable-relay", req.CorrelationID,
			map[string]any{"payable_id": payableID, "payee_ref": payeeRef, "payee_destination_id": destinationID})
	})
	if err != nil {
		return s.unavailable("CompletePayableRequest", err)
	}
	return nil
}

// BlockPayableRequest records that the claim is approved but not payable
// (stable reason) and schedules the next re-check. The audit/outbox event is
// written only when the blocked reason is new.
func (s *PgStore) BlockPayableRequest(ctx context.Context, req domain.PayableRequest, reason string, nextAttempt time.Time) error {
	ctx = middleware.WithTenant(ctx, req.TenantID)
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var prevState, prevReason string
		if err := tx.QueryRow(ctx, `SELECT state, blocked_reason FROM payable_requests WHERE request_id = $1 AND tenant_id::text = $2 FOR UPDATE`, req.RequestID, req.TenantID).Scan(&prevState, &prevReason); err != nil {
			return err
		}
		if prevState == string(domain.PayableCreated) {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payable_requests SET state = 'BLOCKED', blocked_reason = $2, attempts = attempts + 1, next_attempt_at = $3, updated_at = NOW()
			WHERE request_id = $1`, req.RequestID, reason, nextAttempt); err != nil {
			return err
		}
		if prevState == string(domain.PayableBlocked) && prevReason == reason {
			return nil
		}
		updated, err := updateClaim(ctx, tx, req.ClaimID, `payable_state = 'BLOCKED', payable_blocked_reason = $2`, reason)
		if err != nil {
			return err
		}
		return s.emit(ctx, tx, updated, domain.EventClaimPayableBlocked, reason, "system:payable-relay", req.CorrelationID,
			map[string]any{"blocked_reason": reason})
	})
	if err != nil {
		return s.unavailable("BlockPayableRequest", err)
	}
	return nil
}

// RecordPayableFailure notes a transient AP-08/ORG-10/employee-master failure
// and backs off. The first failure also emits the legacy
// EXPENSE_CLAIM_PAYABLE_CREATE_FAILED visibility event.
func (s *PgStore) RecordPayableFailure(ctx context.Context, req domain.PayableRequest, errText string, nextAttempt time.Time) error {
	ctx = middleware.WithTenant(ctx, req.TenantID)
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var attempts int
		if err := tx.QueryRow(ctx, `
			UPDATE payable_requests SET attempts = attempts + 1, last_error = $2, next_attempt_at = $3, updated_at = NOW()
			WHERE request_id = $1 AND tenant_id::text = $4 AND state <> 'CREATED' RETURNING attempts`,
			req.RequestID, errText, nextAttempt, req.TenantID).Scan(&attempts); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if attempts != 1 {
			return nil
		}
		c, err := scanClaim(tx.QueryRow(ctx, `SELECT `+claimColumns+` FROM expense_claims WHERE claim_id = $1 AND tenant_id::text = $2`, req.ClaimID, req.TenantID))
		if err != nil {
			return err
		}
		return s.emit(ctx, tx, c, domain.EventClaimPayableCreateFailed, errText, "system:payable-relay", req.CorrelationID, map[string]any{"error": errText})
	})
	if err != nil {
		return s.unavailable("RecordPayableFailure", err)
	}
	return nil
}

func (s *PgStore) ListSettlementCandidates(ctx context.Context, limit int) ([]domain.SettlementCandidate, error) {
	var out []domain.SettlementCandidate
	err := s.withSystem(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.tenant_id::text, c.claim_id::text, c.legal_entity_id::text, c.payable_id, c.approved_by_principal_id
			FROM expense_claims c JOIN payable_requests r ON r.claim_id = c.claim_id
			WHERE c.status = 'REIMBURSABLE' AND r.state = 'CREATED' AND c.payable_id <> ''
			ORDER BY r.settlement_checked_at ASC NULLS FIRST LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.SettlementCandidate
			var approver *string
			if err := rows.Scan(&c.TenantID, &c.ClaimID, &c.LegalEntityID, &c.PayableID, &approver); err != nil {
				return err
			}
			if approver != nil {
				c.PrincipalID = *approver
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.unavailable("ListSettlementCandidates", err)
	}
	return out, nil
}

func (s *PgStore) MarkSettlementChecked(ctx context.Context, tenantID, claimID string) error {
	ctx = middleware.WithTenant(ctx, tenantID)
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payable_requests SET settlement_checked_at = now() WHERE claim_id = $1 AND tenant_id::text = $2`, claimID, tenantID)
		return err
	})
	if err != nil {
		return s.unavailable("MarkSettlementChecked", err)
	}
	return nil
}

const postingColumns = `request_id, tenant_id::text, legal_entity_id::text, aggregate_id, source_event_id, request_payload::text,
	status, attempts, last_error, posting_execution_id, journal_id, posted_at, created_at`

func scanPosting(row pgx.Row) (*domain.PostingRequest, error) {
	var p domain.PostingRequest
	var payload string
	if err := row.Scan(&p.RequestID, &p.TenantID, &p.LegalEntityID, &p.AggregateID, &p.SourceEventID, &payload,
		&p.Status, &p.Attempts, &p.LastError, &p.PostingExecutionID, &p.JournalID, &p.PostedAt, &p.CreatedAt); err != nil {
		return nil, err
	}
	p.Payload = []byte(payload)
	return &p, nil
}

func (s *PgStore) ListPostingRequests(ctx context.Context, claimID string) ([]domain.PostingRequest, error) {
	var out []domain.PostingRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+postingColumns+` FROM accounting_posting_requests
			WHERE aggregate_id = $1 AND tenant_id::text = $2 ORDER BY created_at ASC`, claimID, tenantOf(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPosting(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.unavailable("ListPostingRequests", err)
	}
	return out, nil
}

// MaxPostingAttempts is how many transient failures a posting request survives
// before it shows as FAILED.
const MaxPostingAttempts = 10

// RequeuePostings is the operator's recovery for a posting that was refused or
// exhausted its retries. Migration 000007 lets FAILED/QUARANTINED move back to
// PENDING (and nowhere else); POSTED stays final.
func (s *PgStore) RequeuePostings(ctx context.Context, claimID string) (int64, error) {
	var n int64
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE accounting_posting_requests SET status = 'PENDING', attempts = 0, next_attempt_at = now(), updated_at = now()
			WHERE aggregate_id = $1 AND tenant_id::text = $2 AND status IN ('FAILED', 'QUARANTINED')`, claimID, tenantOf(ctx))
		n = tag.RowsAffected()
		return err
	})
	if isInvalidUUID(err) {
		return 0, domain.ErrClaimNotFound
	}
	if err != nil {
		return 0, s.unavailable("RequeuePostings", err)
	}
	return n, nil
}

func (s *PgStore) DispatchPostings(ctx context.Context, limit int, fn func(domain.PostingRequest) domain.PostingOutcome) (int, error) {
	n := 0
	err := s.withSystem(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+postingColumns+` FROM accounting_posting_requests
			WHERE status = 'PENDING' AND next_attempt_at <= now()
			ORDER BY next_attempt_at ASC LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		var batch []domain.PostingRequest
		for rows.Next() {
			p, err := scanPosting(rows)
			if err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, *p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range batch {
			o := fn(p)
			// A request that keeps failing transiently is not retried forever:
			// after MaxPostingAttempts it shows as FAILED (visible, requeueable).
			if o.Status == domain.PostingPending && p.Attempts+1 >= MaxPostingAttempts {
				o.Status = domain.PostingFailed
			}
			if o.Status == domain.PostingPending {
				_, err = tx.Exec(ctx, `UPDATE accounting_posting_requests SET attempts = attempts + 1, last_error = $2, next_attempt_at = $3, updated_at = now() WHERE request_id = $1`,
					p.RequestID, o.Error, o.NextAttempt)
			} else {
				_, err = tx.Exec(ctx, `UPDATE accounting_posting_requests SET status = $2, attempts = attempts + 1, last_error = $3,
					posting_execution_id = $4, journal_id = $5, posted_at = CASE WHEN $2 = 'POSTED' THEN now() ELSE NULL END, updated_at = now()
					WHERE request_id = $1`, p.RequestID, o.Status, o.Error, o.ExecutionID, o.JournalID)
			}
			if err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, s.unavailable("DispatchPostings", err)
	}
	return n, nil
}
