package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// PaymentChange is what a committed WFP-07 command changed; the builder turns it into
// outbox events inside the same transaction. Kind: CREATED, APPROVED, INVALIDATED,
// SUBMITTED, CANCELLED, SUPERSEDED.
type PaymentChange struct {
	Kind        string
	Instruction *domain.PaymentInstruction
	Reason      string
}

type PaymentEventBuilder func(PaymentChange) ([]domain.OutboxEvent, error)

func enqueuePayment(ctx context.Context, tx pgx.Tx, mk PaymentEventBuilder, ch PaymentChange) error {
	if mk == nil {
		return nil
	}
	evs, err := mk(ch)
	if err != nil {
		return fmt.Errorf("build events: %w", err)
	}
	for _, ev := range evs {
		if !domain.EventCatalogue[ev.EventType] {
			return fmt.Errorf("event type %q is not in the WFP catalogue", ev.EventType)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
			ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload); err != nil {
			return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
		}
	}
	return nil
}

func mapPaymentGuard(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23000":
		switch {
		case strings.Contains(pgErr.Message, "separation of duties"):
			return domain.ErrSoDPreparerOrReceiver
		case strings.Contains(pgErr.Message, "no longer ACCEPTED"), strings.Contains(pgErr.Message, "only from an ACCEPTED"):
			return domain.ErrInstructionResultNotActive
		case strings.Contains(pgErr.Message, "illegal payment instruction transition"), strings.Contains(pgErr.Message, "only a VALIDATED"),
			strings.Contains(pgErr.Message, "is frozen"), strings.Contains(pgErr.Message, "quorum"):
			return domain.ErrInstructionState
		case strings.Contains(pgErr.Message, "does not match the instruction"):
			return domain.ErrApprovalInvalidated
		}
	}
	return err
}

const instrCols = `i.instruction_id::text, i.tenant_id, i.legal_entity_id, i.instruction_type, i.run_ref_id::text, i.result_id::text,
	i.worker_id::text, i.liability_id::text, i.payee_ref, i.destination_id, i.destination_version, i.destination_fingerprint,
	i.amount::text, i.currency, i.value_date::text, i.payer_account_ref, i.payment_method, i.payment_reference, i.fingerprint, i.state,
	i.policy_result, i.policy_version_id, i.required_signatures,
	(SELECT count(DISTINCT a.approver) FROM wfp_payment_approvals a WHERE a.instruction_id = i.instruction_id AND a.invalidated_at IS NULL AND a.fingerprint = i.fingerprint)::int,
	i.approved_fingerprint, i.approved_at, i.invalidation_reason, i.invalidated_at, i.cancel_reason, i.attempt_id, i.banking_status,
	i.banking_error, i.submitted_by, i.submitted_at, i.handed_off_at, i.idempotency_key, i.version, i.correlation_id, i.created_by,
	i.created_at, i.updated_at, i.payment_id, i.returned_at, i.return_reason`

func scanInstruction(row pgx.Row) (*domain.PaymentInstruction, error) {
	var p domain.PaymentInstruction
	err := row.Scan(&p.InstructionID, &p.TenantID, &p.LegalEntityID, &p.InstructionType, &p.RunRefID, &p.ResultID,
		&p.WorkerID, &p.LiabilityID, &p.PayeeRef, &p.DestinationID, &p.DestinationVersion, &p.DestinationFingerprint,
		&p.Amount, &p.Currency, &p.ValueDate, &p.PayerAccountRef, &p.PaymentMethod, &p.PaymentReference, &p.Fingerprint, &p.State,
		&p.PolicyResult, &p.PolicyVersionID, &p.RequiredSignatures, &p.SignaturesCollected,
		&p.ApprovedFingerprint, &p.ApprovedAt, &p.InvalidationReason, &p.InvalidatedAt, &p.CancelReason, &p.AttemptID, &p.BankingStatus,
		&p.BankingError, &p.SubmittedBy, &p.SubmittedAt, &p.HandedOffAt, &p.IdempotencyKey, &p.Version, &p.CorrelationID, &p.CreatedBy,
		&p.CreatedAt, &p.UpdatedAt, &p.PaymentID, &p.ReturnedAt, &p.ReturnReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInstructionNotFound
	}
	return &p, err
}

func lockInstructionTx(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.PaymentInstruction, error) {
	return scanInstruction(tx.QueryRow(ctx, `SELECT `+instrCols+` FROM wfp_payment_instructions i
		WHERE i.tenant_id = $1 AND i.instruction_id = $2::uuid FOR UPDATE OF i`, tenantID, id))
}

func updateInstructionTx(ctx context.Context, tx pgx.Tx, tenantID, id, setSQL string, args ...any) (*domain.PaymentInstruction, error) {
	all := append([]any{tenantID, id}, args...)
	row := tx.QueryRow(ctx, `WITH u AS (UPDATE wfp_payment_instructions SET `+setSQL+`, version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND instruction_id = $2::uuid RETURNING *)
		SELECT `+instrCols+` FROM u i`, all...)
	p, err := scanInstruction(row)
	return p, mapPaymentGuard(err)
}

// ActiveLiabilityInstructionAmount is what live LIABILITY instructions already ask Banking to
// pay for a liability (so a second instruction cannot pay the same money twice).
func activeLiabilityAmountTx(ctx context.Context, tx pgx.Tx, tenantID, liabilityID string) (*big.Rat, error) {
	var s string
	err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::text FROM wfp_payment_instructions
		WHERE tenant_id = $1 AND liability_id = $2::uuid AND state NOT IN ('CANCELLED', 'SUPERSEDED', 'RETURNED')`, tenantID, liabilityID).Scan(&s)
	if err != nil {
		return nil, err
	}
	return domain.ParseDecimal(s)
}

// ReservedForLiability returns the amount live instructions already cover for a liability.
func (s *PgStore) ReservedForLiability(ctx context.Context, liabilityID string) (string, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return "", err
	}
	var out string
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		r, e := activeLiabilityAmountTx(ctx, tx, tenantID, liabilityID)
		if e != nil {
			return e
		}
		out = domain.FormatDecimal(r)
		return nil
	})
	return out, err
}

// CreatePaymentInstruction stores a DRAFT whose fingerprint was computed by the caller from
// server-resolved facts. For a liability the bound (live instructions + this one <= what is
// outstanding) is re-checked under the liability's row lock. A repeat of the same request
// (idempotency key, or the same worker's net pay) returns the existing instruction.
func (s *PgStore) CreatePaymentInstruction(ctx context.Context, in *domain.PaymentInstruction, mk PaymentEventBuilder) (*domain.PaymentInstruction, bool, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, false, err
	}
	var out *domain.PaymentInstruction
	created := true
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if in.InstructionType == domain.PayTypeLiab {
			l, err := lockLiabilityTx(ctx, tx, tenantID, *in.LiabilityID)
			if err != nil {
				return err
			}
			if l.RunRefID != in.RunRefID || l.ResultID != in.ResultID {
				return domain.ErrSubjectNotPayable
			}
			switch l.State {
			case domain.LiabRecognized, domain.LiabDue, domain.LiabScheduled, domain.LiabPartiallySettled:
			default:
				return domain.ErrSubjectNotPayable
			}
			if l.ExceptionState != domain.LiabExcNone {
				return domain.ErrSubjectNotPayable
			}
			if l.Currency != in.Currency {
				return domain.ErrSubjectNotPayable
			}
			owed, err := domain.ParseDecimal(l.OutstandingAmount)
			if err != nil {
				return err
			}
			reserved, err := activeLiabilityAmountTx(ctx, tx, tenantID, l.LiabilityID)
			if err != nil {
				return err
			}
			amt, err := domain.ParseDecimal(in.Amount)
			if err != nil {
				return err
			}
			if new(big.Rat).Add(reserved, amt).Cmp(owed) > 0 {
				return domain.ErrExceedsOutstanding
			}
		}
		if in.InstructionType == domain.PayTypeNetPay && in.WorkerID != nil {
			// WFP-CTRL-021: a replacement net-pay instruction (a correction result of the same entity and
			// period) waits until the original Banking attempt is resolved; blind duplication is how
			// an unknown outcome becomes a double payment.
			var open int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM wfp_payment_instructions i
				JOIN wfp_payroll_run_refs r ON r.run_ref_id = i.run_ref_id
				JOIN wfp_payroll_run_refs me ON me.run_ref_id = $3::uuid
				WHERE i.tenant_id = $1 AND i.worker_id = $2::uuid AND i.instruction_type = 'NET_PAY'
				  AND i.state IN ('SUBMITTED_TO_BANKING', 'HANDED_OFF') AND i.result_id <> $4::uuid
				  AND r.legal_entity_id = me.legal_entity_id AND r.payroll_period_id = me.payroll_period_id
				  AND COALESCE((SELECT o.outcome FROM wfp_instruction_banking_observations o WHERE o.instruction_id = i.instruction_id
				                ORDER BY o.observed_at DESC, o.observation_id DESC LIMIT 1), 'NONE') IN ('NONE', 'IN_FLIGHT', 'UNRESOLVED')`,
				tenantID, *in.WorkerID, in.RunRefID, in.ResultID).Scan(&open); err != nil {
				return err
			}
			if open > 0 {
				return domain.ErrOutcomeUnresolved
			}
		}
		_, err := tx.Exec(ctx, `SAVEPOINT create_instruction`)
		if err != nil {
			return err
		}
		out, err = scanInstructionInsert(ctx, tx, tenantID, in)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if _, rerr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT create_instruction`); rerr != nil {
				return rerr
			}
			existing, ferr := findExistingInstructionTx(ctx, tx, tenantID, in)
			if ferr != nil {
				return ferr
			}
			out, created = existing, false
			return nil
		}
		if err != nil {
			return mapPaymentGuard(err)
		}
		return enqueuePayment(ctx, tx, mk, PaymentChange{Kind: "CREATED", Instruction: out})
	})
	return out, created, err
}

func scanInstructionInsert(ctx context.Context, tx pgx.Tx, tenantID string, in *domain.PaymentInstruction) (*domain.PaymentInstruction, error) {
	row := tx.QueryRow(ctx, `
		WITH u AS (
			INSERT INTO wfp_payment_instructions
				(instruction_id, tenant_id, legal_entity_id, instruction_type, run_ref_id, result_id, worker_id, liability_id, payee_ref,
				 destination_id, destination_version, destination_fingerprint, amount, currency, value_date, payer_account_ref, payment_method,
				 payment_reference, fingerprint, idempotency_key, correlation_id, created_by)
			VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6::uuid, $7::uuid, $8::uuid, $9, $10, $11, $12, $13::numeric, $14, $15::date, $16, $17,
			        $18, $19, $20, $21, $22)
			RETURNING *)
		SELECT `+instrCols+` FROM u i`,
		in.InstructionID, tenantID, in.LegalEntityID, in.InstructionType, in.RunRefID, in.ResultID, in.WorkerID, in.LiabilityID, in.PayeeRef,
		in.DestinationID, in.DestinationVersion, in.DestinationFingerprint, in.Amount, in.Currency, in.ValueDate, in.PayerAccountRef, in.PaymentMethod,
		in.PaymentReference, in.Fingerprint, in.IdempotencyKey, in.CorrelationID, in.CreatedBy)
	return scanInstruction(row)
}

func findExistingInstructionTx(ctx context.Context, tx pgx.Tx, tenantID string, in *domain.PaymentInstruction) (*domain.PaymentInstruction, error) {
	if in.IdempotencyKey != nil {
		p, err := scanInstruction(tx.QueryRow(ctx, `SELECT `+instrCols+` FROM wfp_payment_instructions i
			WHERE i.tenant_id = $1 AND i.idempotency_key = $2`, tenantID, *in.IdempotencyKey))
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, domain.ErrInstructionNotFound) {
			return nil, err
		}
	}
	return scanInstruction(tx.QueryRow(ctx, `SELECT `+instrCols+` FROM wfp_payment_instructions i
		WHERE i.tenant_id = $1 AND i.result_id = $2::uuid AND i.worker_id = $3::uuid AND i.instruction_type = 'NET_PAY'
		  AND i.state NOT IN ('CANCELLED', 'SUPERSEDED', 'RETURNED')`, tenantID, in.ResultID, in.WorkerID))
}

func (s *PgStore) GetPaymentInstruction(ctx context.Context, id string) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var p *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		if p, e = scanInstruction(tx.QueryRow(ctx, `SELECT `+instrCols+` FROM wfp_payment_instructions i
			WHERE i.tenant_id = $1 AND i.instruction_id = $2::uuid`, tenantID, id)); e != nil {
			return e
		}
		rows, e := tx.Query(ctx, `SELECT approval_id::text, approver, fingerprint, created_at, invalidated_at
			FROM wfp_payment_approvals WHERE tenant_id = $1 AND instruction_id = $2::uuid ORDER BY created_at`, tenantID, id)
		if e != nil {
			return e
		}
		defer rows.Close()
		p.Approvals = []domain.PaymentApproval{}
		for rows.Next() {
			var a domain.PaymentApproval
			if e := rows.Scan(&a.ApprovalID, &a.Approver, &a.Fingerprint, &a.CreatedAt, &a.InvalidatedAt); e != nil {
				return e
			}
			p.Approvals = append(p.Approvals, a)
		}
		return rows.Err()
	})
	return p, err
}

type InstructionFilter struct {
	RunRefID      string
	LegalEntityID string
	State         string
	LiabilityID   string
	PendingOnly   bool
	Limit         int
}

func (s *PgStore) ListPaymentInstructions(ctx context.Context, f InstructionFilter) ([]domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	out := []domain.PaymentInstruction{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+instrCols+` FROM wfp_payment_instructions i
			WHERE i.tenant_id = $1 AND ($2 = '' OR i.run_ref_id::text = $2) AND ($3 = '' OR i.legal_entity_id = $3)
			  AND ($4 = '' OR i.state = $4) AND ($5 = '' OR i.liability_id::text = $5)
			  AND (NOT $6 OR i.state IN ('DRAFT', 'VALIDATED', 'APPROVED', 'SUBMITTED_TO_BANKING'))
			ORDER BY i.created_at, i.instruction_id LIMIT $7`,
			tenantID, f.RunRefID, f.LegalEntityID, f.State, f.LiabilityID, f.PendingOnly, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanInstruction(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

// ValidationFacts are the server-resolved facts a validation pins.
type ValidationFacts struct {
	Payee              domain.PayeeDestination
	PolicyResult       string
	PolicyVersionID    string
	RequiredSignatures int
}

// ValidatePaymentInstruction freezes a DRAFT with the facts as they are now (a payee
// destination that changed since creation is re-pinned here, with a new fingerprint), the
// approval-threshold assessment and the signature quorum it requires.
func (s *PgStore) ValidatePaymentInstruction(ctx context.Context, id string, expectedVersion int, f ValidationFacts) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrInstructionVersionConflict
		}
		if cur.State != domain.PayDraft {
			return domain.ErrInstructionState
		}
		cur.PayeeRef, cur.DestinationID, cur.DestinationVersion, cur.DestinationFingerprint = f.Payee.PayeeRef, f.Payee.DestinationID, f.Payee.Version, f.Payee.Fingerprint
		fp := domain.ComputeInstructionFingerprint(cur)
		out, err = updateInstructionTx(ctx, tx, tenantID, id, `state = 'VALIDATED', payee_ref = $3, destination_id = $4, destination_version = $5,
			destination_fingerprint = $6, fingerprint = $7, policy_result = $8, policy_version_id = $9, required_signatures = $10,
			invalidation_reason = NULL`,
			f.Payee.PayeeRef, f.Payee.DestinationID, f.Payee.Version, f.Payee.Fingerprint, fp, f.PolicyResult, nullIfEmpty(f.PolicyVersionID), f.RequiredSignatures)
		return err
	})
	return out, err
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ApprovePaymentInstruction records one approval of the fingerprint the approver saw. The
// instruction becomes APPROVED when the quorum of distinct approvers is met. The database
// refuses the preparer and the principal who received the result.
func (s *PgStore) ApprovePaymentInstruction(ctx context.Context, id, approver, fingerprint string, mk PaymentEventBuilder) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.State != domain.PayValidated {
			return domain.ErrInstructionState
		}
		if cur.Fingerprint != fingerprint {
			return domain.ErrApprovalInvalidated
		}
		_, err = tx.Exec(ctx, `INSERT INTO wfp_payment_approvals (approval_id, tenant_id, instruction_id, approver, fingerprint)
			VALUES ($1::uuid, $2, $3::uuid, $4, $5)`, newUUID(), tenantID, id, approver, fingerprint)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrAlreadyApproved
		}
		if err != nil {
			return mapPaymentGuard(err)
		}
		cur, err = scanInstruction(tx.QueryRow(ctx, `SELECT `+instrCols+` FROM wfp_payment_instructions i WHERE i.tenant_id = $1 AND i.instruction_id = $2::uuid`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.SignaturesCollected < cur.RequiredSignatures {
			out = cur // an approval was recorded; the quorum is not met yet
			return nil
		}
		out, err = updateInstructionTx(ctx, tx, tenantID, id, `state = 'APPROVED', approved_fingerprint = fingerprint, approved_at = now()`)
		if err != nil {
			return err
		}
		return enqueuePayment(ctx, tx, mk, PaymentChange{Kind: "APPROVED", Instruction: out})
	})
	return out, err
}

func invalidateApprovalsTx(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	_, err := tx.Exec(ctx, `UPDATE wfp_payment_approvals SET invalidated_at = now()
		WHERE tenant_id = $1 AND instruction_id = $2::uuid AND invalidated_at IS NULL`, tenantID, id)
	return err
}

// InvalidatePaymentApproval returns a VALIDATED or APPROVED instruction to DRAFT and voids
// its approvals: the protected subject changed (or an operator withdrew approval), so it
// must be validated and approved again at its new fingerprint.
func (s *PgStore) InvalidatePaymentApproval(ctx context.Context, id, reason string, mk PaymentEventBuilder) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.State != domain.PayValidated && cur.State != domain.PayApproved {
			return domain.ErrInstructionState
		}
		wasApproved := cur.State == domain.PayApproved
		if err := invalidateApprovalsTx(ctx, tx, tenantID, id); err != nil {
			return err
		}
		out, err = updateInstructionTx(ctx, tx, tenantID, id, `state = 'DRAFT', approved_fingerprint = NULL, approved_at = NULL,
			invalidation_reason = $3, invalidated_at = now()`, reason)
		if err != nil {
			return err
		}
		if wasApproved || cur.SignaturesCollected > 0 {
			return enqueuePayment(ctx, tx, mk, PaymentChange{Kind: "INVALIDATED", Instruction: out, Reason: reason})
		}
		return nil
	})
	return out, err
}

// CancelPaymentInstruction cancels an instruction that was never submitted to Banking.
func (s *PgStore) CancelPaymentInstruction(ctx context.Context, id string, expectedVersion int, reason string, mk PaymentEventBuilder) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrInstructionVersionConflict
		}
		switch cur.State {
		case domain.PayDraft, domain.PayValidated, domain.PayApproved:
		default:
			return domain.ErrInstructionState
		}
		if err := invalidateApprovalsTx(ctx, tx, tenantID, id); err != nil {
			return err
		}
		out, err = updateInstructionTx(ctx, tx, tenantID, id, `state = 'CANCELLED', cancel_reason = $3, approved_fingerprint = NULL`, reason)
		if err != nil {
			return err
		}
		return enqueuePayment(ctx, tx, mk, PaymentChange{Kind: "CANCELLED", Instruction: out, Reason: reason})
	})
	return out, err
}

type InstructionSupersedeOutcome struct {
	Superseded     []domain.PaymentInstruction `json:"superseded"`
	NeedsAttention []domain.PaymentInstruction `json:"needs_banking_attention"`
}

// SupersedeInstructionsForRun follows a result's supersession (Wave 4): every instruction
// that has not reached Banking becomes SUPERSEDED. One that did (SUBMITTED_TO_BANKING or
// HANDED_OFF) is returned: its original Banking attempt must be resolved with Banking, and
// no replacement may be raised until it is.
func (s *PgStore) SupersedeInstructionsForRun(ctx context.Context, runRefID string, mk PaymentEventBuilder) (*InstructionSupersedeOutcome, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := &InstructionSupersedeOutcome{Superseded: []domain.PaymentInstruction{}, NeedsAttention: []domain.PaymentInstruction{}}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT acceptance_state FROM wfp_payroll_run_refs WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, runRefID).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRunRefNotFound
			}
			return err
		}
		if state != domain.AcceptanceSuperseded {
			return domain.ErrResultNotSuperseded
		}
		rows, err := tx.Query(ctx, `SELECT i.instruction_id::text FROM wfp_payment_instructions i
			WHERE i.tenant_id = $1 AND i.run_ref_id = $2::uuid AND i.state NOT IN ('CANCELLED', 'SUPERSEDED', 'RETURNED') ORDER BY i.created_at FOR UPDATE OF i`, tenantID, runRefID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			cur, err := lockInstructionTx(ctx, tx, tenantID, id)
			if err != nil {
				return err
			}
			if cur.State == domain.PaySubmitted || cur.State == domain.PayHandedOff {
				out.NeedsAttention = append(out.NeedsAttention, *cur)
				continue
			}
			if err := invalidateApprovalsTx(ctx, tx, tenantID, id); err != nil {
				return err
			}
			upd, err := updateInstructionTx(ctx, tx, tenantID, id, `state = 'SUPERSEDED', approved_fingerprint = NULL`)
			if err != nil {
				return err
			}
			out.Superseded = append(out.Superseded, *upd)
			if err := enqueuePayment(ctx, tx, mk, PaymentChange{Kind: "SUPERSEDED", Instruction: upd, Reason: "RESULT_SUPERSEDED"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BeginSubmitToBanking moves an APPROVED instruction to SUBMITTED_TO_BANKING (the commit
// precedes the Banking call, so a crash can never lose the fact that a submission may have
// happened). Calling it again for an instruction already submitted but not yet handed off
// is a retry: it returns the instruction with retry=true and the same Banking idempotency
// key is reused.
func (s *PgStore) BeginSubmitToBanking(ctx context.Context, id string, expectedVersion int, submitter string) (*domain.PaymentInstruction, bool, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, false, err
	}
	var out *domain.PaymentInstruction
	retry := false
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.State == domain.PaySubmitted {
			out, retry = cur, true
			return nil
		}
		if cur.State != domain.PayApproved {
			return domain.ErrNotApproved
		}
		if cur.Version != expectedVersion {
			return domain.ErrInstructionVersionConflict
		}
		out, err = updateInstructionTx(ctx, tx, tenantID, id, `state = 'SUBMITTED_TO_BANKING', submitted_by = $3, submitted_at = now(), banking_status = 'SUBMITTING'`, submitter)
		return err
	})
	return out, retry, err
}

// RecordBankingOutcome records what Banking answered for the instruction's attempt. Only a
// SUBMITTED attempt makes it HANDED_OFF; every other status (PREPARED, PENDING_UNKNOWN, a
// rejection, a quarantine, an unreachable adapter) leaves it SUBMITTED_TO_BANKING with the
// status visible: unknown is never failed and never paid.
func (s *PgStore) RecordBankingOutcome(ctx context.Context, id, attemptID, bankingStatus, bankingErr string, mk PaymentEventBuilder) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.State != domain.PaySubmitted {
			return domain.ErrInstructionState
		}
		firstAttempt := cur.AttemptID == nil && attemptID != ""
		handed := domain.HandedOffStatus(bankingStatus)
		set := `banking_status = $3, banking_error = $4, attempt_id = COALESCE(attempt_id, $5)`
		if handed {
			set += `, state = 'HANDED_OFF', handed_off_at = now()`
		}
		out, err = updateInstructionTx(ctx, tx, tenantID, id, set, bankingStatus, nullIfEmpty(bankingErr), nullIfEmpty(attemptID))
		if err != nil {
			return err
		}
		if firstAttempt || handed {
			return enqueuePayment(ctx, tx, mk, PaymentChange{Kind: "SUBMITTED", Instruction: out})
		}
		return nil
	})
	return out, err
}

// PaymentInstructionFingerprint is what Banking's initiation adapter re-fetches to verify an
// instruction before it prepares an attempt.
func (s *PgStore) PaymentInstructionFingerprint(ctx context.Context, id string) (state, fingerprint string, err error) {
	p, err := s.GetPaymentInstruction(ctx, id)
	if err != nil {
		return "", "", err
	}
	if p.ApprovedFingerprint == nil {
		return p.State, "", nil
	}
	return p.State, *p.ApprovedFingerprint, nil
}

// ReleaseRefusedSubmission returns an instruction to APPROVED when Banking refused it
// definitively BEFORE any attempt existed (nothing reached the provider), with the refusal
// kept visible. The database allows this only while no Banking attempt id was ever recorded.
func (s *PgStore) ReleaseRefusedSubmission(ctx context.Context, id, reason string) (*domain.PaymentInstruction, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.PaymentInstruction
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockInstructionTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.State != domain.PaySubmitted || cur.AttemptID != nil {
			return domain.ErrInstructionState
		}
		out, err = updateInstructionTx(ctx, tx, tenantID, id, `state = 'APPROVED', banking_status = 'REFUSED', banking_error = $3`, reason)
		return err
	})
	return out, err
}
