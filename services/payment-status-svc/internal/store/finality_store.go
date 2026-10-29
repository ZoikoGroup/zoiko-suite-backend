package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/payment-status-svc/internal/domain"
)

// finalityCommandSpec is the only per-command difference between
// ConfirmSettlement and RecordRejection. Keeping the transition, event and
// finality-basis values here makes the shared transactional path impossible to
// drift between the two commands.
type finalityCommandSpec struct {
	command        string
	target         domain.ExecutionStatus
	eventType      string
	finalitySource string
	mappingVersion string
}

type finalityInput struct {
	providerReference string
	bankReference     string
	reason            string
	effectiveDate     string
	evidenceReference string
	evidenceHash      string
}

type finalityClaim struct {
	command     string
	paymentID   string
	requestHash string
}

const finalityEvidenceColumns = `
	evidence_id::text, tenant_id, payment_id::text, command, provider_reference, bank_reference,
	reason, effective_at, evidence_reference, evidence_hash, mapping_version, finality_source,
	actor_principal_id, idempotency_key, created_at`

func scanFinalityEvidence(row pgx.Row) (*domain.FinalityEvidence, error) {
	e := &domain.FinalityEvidence{}
	err := row.Scan(&e.EvidenceID, &e.TenantID, &e.PaymentID, &e.Command, &e.ProviderReference, &e.BankReference,
		&e.Reason, &e.EffectiveAt, &e.EvidenceReference, &e.EvidenceHash, &e.MappingVersion, &e.FinalitySource,
		&e.ActorPrincipalID, &e.IdempotencyKey, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func lookupFinalityClaim(ctx context.Context, tx pgx.Tx, tenantID, idempotencyKey string) (*finalityClaim, error) {
	var c finalityClaim
	err := tx.QueryRow(ctx, `
		SELECT command, payment_id::text, request_hash
		FROM payment_command_idempotency
		WHERE COALESCE(tenant_id::text, '') = $1 AND idempotency_key = $2`,
		tenantID, idempotencyKey).Scan(&c.command, &c.paymentID, &c.requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// finalityRequestHash is the request identity stored with the key. It covers
// the command, the payment and every evidence field, so reusing a key for a
// different request is detected even when the target payment is the same.
func finalityRequestHash(spec finalityCommandSpec, paymentID string, in finalityInput, effectiveAt time.Time) (string, error) {
	canonical := struct {
		Command           string
		PaymentID         string
		ProviderReference string
		BankReference     string
		Reason            string
		EffectiveAt       time.Time
		EvidenceReference string
		EvidenceHash      string
	}{
		Command:           spec.command,
		PaymentID:         paymentID,
		ProviderReference: in.providerReference,
		BankReference:     in.bankReference,
		Reason:            in.reason,
		EffectiveAt:       effectiveAt.UTC(),
		EvidenceReference: in.evidenceReference,
		EvidenceHash:      in.evidenceHash,
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (s *PgStore) ConfirmSettlement(ctx context.Context, paymentID string, req domain.ConfirmSettlementRequest, principalID, idempotencyKey, correlationID string) (*domain.PaymentExecutionState, *domain.FinalityEvidence, bool, error) {
	if err := req.Validate(); err != nil {
		return nil, nil, false, err
	}
	return s.recordFinality(ctx, paymentID, finalityInput{
		providerReference: req.ProviderReference,
		bankReference:     req.BankReference,
		reason:            req.Reason,
		effectiveDate:     req.EffectiveDate,
		evidenceReference: req.EvidenceReference,
		evidenceHash:      req.EvidenceHash,
	}, finalityCommandSpec{
		command:        domain.CommandConfirmSettlement,
		target:         domain.StatusSettled,
		eventType:      domain.EventPaymentSettled,
		finalitySource: domain.FinalitySourceManualConfirmation,
		mappingVersion: domain.ManualConfirmationMappingVersion,
	}, principalID, idempotencyKey, correlationID)
}

func (s *PgStore) RecordRejection(ctx context.Context, paymentID string, req domain.RecordRejectionRequest, principalID, idempotencyKey, correlationID string) (*domain.PaymentExecutionState, *domain.FinalityEvidence, bool, error) {
	if err := req.Validate(); err != nil {
		return nil, nil, false, err
	}
	return s.recordFinality(ctx, paymentID, finalityInput{
		providerReference: req.ProviderReference,
		bankReference:     req.BankReference,
		reason:            req.Reason,
		effectiveDate:     req.EffectiveDate,
		evidenceReference: req.EvidenceReference,
		evidenceHash:      req.EvidenceHash,
	}, finalityCommandSpec{
		command:        domain.CommandRecordRejection,
		target:         domain.StatusRejected,
		eventType:      domain.EventPaymentRejected,
		finalitySource: domain.FinalitySourceManualRejection,
		mappingVersion: domain.ManualRejectionMappingVersion,
	}, principalID, idempotencyKey, correlationID)
}

// recordFinality is the shared transactional path for the two manual finality
// commands. The row lock serialises concurrent attempts; the idempotency claim,
// status change, evidence row, status-history row and outbox event all commit
// or roll back together. A refused command therefore never consumes its key.
func (s *PgStore) recordFinality(ctx context.Context, paymentID string, in finalityInput, spec finalityCommandSpec, principalID, idempotencyKey, correlationID string) (*domain.PaymentExecutionState, *domain.FinalityEvidence, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return nil, nil, false, domain.ErrIdempotencyKeyRequired
	}
	if len(idempotencyKey) > 200 {
		return nil, nil, false, fmt.Errorf("%w: Idempotency-Key must be at most 200 characters", domain.ErrInvalidFinalityRequest)
	}
	in.providerReference = strings.TrimSpace(in.providerReference)
	in.bankReference = strings.TrimSpace(in.bankReference)
	in.reason = strings.TrimSpace(in.reason)
	in.effectiveDate = strings.TrimSpace(in.effectiveDate)
	in.evidenceReference = strings.TrimSpace(in.evidenceReference)
	in.evidenceHash = strings.ToLower(strings.TrimSpace(in.evidenceHash))

	effectiveAt, err := domain.ParseEffectiveDate(in.effectiveDate)
	if err != nil {
		return nil, nil, false, err
	}
	requestHash, err := finalityRequestHash(spec, paymentID, in, effectiveAt)
	if err != nil {
		return nil, nil, false, fmt.Errorf("hash finality request: %w", err)
	}

	var p *domain.PaymentExecutionState
	var evidence *domain.FinalityEvidence
	replay := false

	err = s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1 FOR UPDATE`, paymentID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrPaymentNotFound
			}
			return err
		}

		// SoD: the creator of the payment cannot also be the principal who
		// manually asserts its final settlement or rejection. This is the same
		// rule PollPaymentStatus, ResolveStatusConflict and RecordReturn enforce,
		// repeated in the store so a direct caller cannot bypass the handler.
		// It is checked before the idempotency replay branch as well: a creator
		// must not be able to obtain the manual-finality result by replaying a
		// key another principal used.
		if current.CreatedByPrincipalID != "" && current.CreatedByPrincipalID == principalID {
			return domain.ErrSelfResolutionForbidden
		}

		claim, err := lookupFinalityClaim(ctx, tx, strOrEmpty(current.TenantID), idempotencyKey)
		if err != nil {
			return err
		}
		if claim != nil {
			if claim.command != spec.command || claim.paymentID != paymentID || claim.requestHash != requestHash {
				return domain.ErrIdempotencyKeyConflict
			}
			evidence, err = scanFinalityEvidence(tx.QueryRow(ctx, `
				SELECT `+finalityEvidenceColumns+`
				FROM payment_finality_evidence
				WHERE payment_id = $1 AND command = $2`, paymentID, spec.command))
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("idempotency claim %q has no finality evidence", idempotencyKey)
			}
			if err != nil {
				return err
			}
			p = current
			replay = true
			return nil
		}

		if current.HasOpenConflict {
			return domain.ErrOpenConflictExists
		}
		if !domain.CanConfirmFinality(current.Status) {
			return domain.ErrInvalidTransition
		}

		p, err = scanState(tx.QueryRow(ctx, `
			UPDATE payment_execution_states
			SET status = $2, finality_source = $3, mapping_version = $4, updated_at = NOW()
			WHERE payment_id = $1
			RETURNING `+stateColumns,
			paymentID, string(spec.target), spec.finalitySource, spec.mappingVersion,
		))
		if errors.Is(err, pgx.ErrNoRows) || isRegressionBlocked(err) {
			return domain.ErrInvalidTransition
		}
		if err != nil {
			return err
		}

		evidence = &domain.FinalityEvidence{
			PaymentID:         paymentID,
			Command:           spec.command,
			ProviderReference: in.providerReference,
			BankReference:     in.bankReference,
			Reason:            in.reason,
			EffectiveAt:       effectiveAt,
			EvidenceReference: in.evidenceReference,
			EvidenceHash:      in.evidenceHash,
			MappingVersion:    spec.mappingVersion,
			FinalitySource:    spec.finalitySource,
			ActorPrincipalID:  principalID,
			IdempotencyKey:    idempotencyKey,
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO payment_finality_evidence (
				tenant_id, payment_id, command, provider_reference, bank_reference, reason, effective_at,
				evidence_reference, evidence_hash, mapping_version, finality_source, actor_principal_id, idempotency_key
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			RETURNING evidence_id::text, created_at`,
			p.TenantID, paymentID, spec.command, in.providerReference, in.bankReference, in.reason, effectiveAt,
			in.evidenceReference, in.evidenceHash, spec.mappingVersion, spec.finalitySource, principalID, idempotencyKey,
		).Scan(&evidence.EvidenceID, &evidence.CreatedAt)
		if isUniqueViolation(err) {
			// A second finality evidence row for this payment/command is not a
			// legitimate retry; retries are resolved by the idempotency claim
			// above. Treat it as a state conflict.
			return domain.ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		evidence.TenantID = p.TenantID

		_, err = tx.Exec(ctx, `
			INSERT INTO payment_command_idempotency (
				tenant_id, idempotency_key, command, payment_id, request_hash
			) VALUES ($1, $2, $3, $4, $5)`,
			p.TenantID, idempotencyKey, spec.command, paymentID, requestHash)
		if isUniqueViolation(err) {
			return domain.ErrIdempotencyKeyConflict
		}
		if err != nil {
			return err
		}

		detail := fmt.Sprintf(
			"manual finality evidence: command=%s reason=%q provider_reference=%q bank_reference=%q effective_at=%s evidence_reference=%q evidence_hash=%q",
			spec.command, in.reason, in.providerReference, in.bankReference,
			effectiveAt.Format(time.RFC3339Nano), in.evidenceReference, in.evidenceHash,
		)
		if err := s.recordEvent(ctx, tx, p.TenantID, paymentID, spec.eventType, string(current.Status), string(spec.target), "", detail, principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, canonicalForLegacy(spec.eventType), spec.eventType,
			"payment-execution-state", paymentID, strOrEmpty(p.TenantID), p.LegalEntityID, principalID, correlationID, p)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPaymentNotFound),
			errors.Is(err, domain.ErrOpenConflictExists),
			errors.Is(err, domain.ErrInvalidTransition),
			errors.Is(err, domain.ErrIdempotencyKeyConflict),
			errors.Is(err, domain.ErrSelfResolutionForbidden),
			errors.Is(err, domain.ErrInvalidFinalityRequest):
			return nil, nil, false, err
		default:
			s.log.Error("pg finality command failed", zap.String("command", spec.command), zap.Error(err))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}
	return p, evidence, replay, nil
}

func (s *PgStore) ListFinalityEvidence(ctx context.Context, paymentID string) ([]domain.FinalityEvidence, error) {
	var out []domain.FinalityEvidence
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+finalityEvidenceColumns+`
			FROM payment_finality_evidence
			WHERE payment_id = $1
			ORDER BY created_at ASC`, paymentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanFinalityEvidence(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListFinalityEvidence failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
