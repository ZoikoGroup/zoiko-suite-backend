package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
)

// Callback receipts (migration 008): independent, append-only evidence of provider callbacks.

const receiptCols = `receipt_id::text, tenant_id, connection_id::text, connector_id, operation_id, contract_version_id::text, COALESCE(attempt_id::text, ''),
	state, auth_result, auth_mechanism, key_label, reason_code, xic_code, COALESCE(dedupe_key, ''), COALESCE(original_receipt_id::text, ''), body_hash, body_size,
	provider_reference, reported_status, target_state, attempt_state_before, attempt_state_after, needs_reconciliation, source_net_hash, correlation_id, received_at`

func scanReceipt(row pgx.Row, r *outbound.Receipt) error {
	return row.Scan(&r.ReceiptID, &r.TenantID, &r.ConnectionID, &r.ConnectorID, &r.OperationID, &r.ContractVersionID, &r.AttemptID, &r.State, &r.AuthResult,
		&r.AuthMechanism, &r.KeyLabel, &r.ReasonCode, &r.XICCode, &r.DedupeKey, &r.OriginalReceiptID, &r.BodyHash, &r.BodySize, &r.ProviderReference,
		&r.ReportedStatus, &r.TargetState, &r.AttemptStateBefore, &r.AttemptStateAfter, &r.NeedsReconciliation, &r.SourceNetHash, &r.CorrelationID, &r.ReceivedAt)
}

func insertReceipt(ctx context.Context, tx pgx.Tx, r *outbound.Receipt) error {
	nul := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_callback_receipts (receipt_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, attempt_id, state,
		auth_result, auth_mechanism, key_label, reason_code, xic_code, dedupe_key, original_receipt_id, body_hash, body_size, provider_reference, reported_status,
		target_state, attempt_state_before, attempt_state_after, needs_reconciliation, source_net_hash, correlation_id, received_at)
		VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6::uuid,$7::uuid,$8,$9,$10,$11,$12,$13,$14,$15::uuid,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
		r.ReceiptID, r.TenantID, r.ConnectionID, r.ConnectorID, r.OperationID, r.ContractVersionID, nul(r.AttemptID), r.State, r.AuthResult, r.AuthMechanism, r.KeyLabel,
		r.ReasonCode, r.XICCode, nul(r.DedupeKey), nul(r.OriginalReceiptID), r.BodyHash, r.BodySize, r.ProviderReference, r.ReportedStatus, r.TargetState,
		r.AttemptStateBefore, r.AttemptStateAfter, r.NeedsReconciliation, r.SourceNetHash, r.CorrelationID, r.ReceivedAt)
	return err
}

// InsertReceipt implements outbound.Repo.
func (r *OutboundRepo) InsertReceipt(ctx context.Context, rc *outbound.Receipt, out []outbound.OutEvent) (*outbound.Receipt, error) {
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := insertReceipt(ctx, tx, rc); err != nil {
			return err
		}
		for _, o := range out {
			if err := r.enqueue(ctx, tx, rc.TenantID, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return nil, nil
	}
	if errors.Is(outboundErr(err), outbound.ErrDuplicate) && rc.DedupeKey != "" {
		holder, gerr := r.GetReceiptByDedupe(ctx, rc.TenantID, rc.DedupeKey)
		if gerr != nil {
			return nil, gerr
		}
		return holder, outbound.ErrDuplicate
	}
	return nil, outboundErr(err)
}

// GetReceipt implements outbound.Repo.
func (r *OutboundRepo) GetReceipt(ctx context.Context, id string) (*outbound.Receipt, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, outbound.ErrReceiptNotFound
	}
	var rc outbound.Receipt
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanReceipt(tx.QueryRow(ctx, `SELECT `+receiptCols+` FROM xic_callback_receipts WHERE receipt_id=$1::uuid`, id), &rc)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, outbound.ErrReceiptNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rc, nil
}

// GetReceiptByDedupe implements outbound.Repo.
func (r *OutboundRepo) GetReceiptByDedupe(ctx context.Context, tenant, key string) (*outbound.Receipt, error) {
	var rc outbound.Receipt
	err := r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return scanReceipt(tx.QueryRow(ctx, `SELECT `+receiptCols+` FROM xic_callback_receipts WHERE tenant_id=$1 AND dedupe_key=$2`, tenant, key), &rc)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, outbound.ErrReceiptNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rc, nil
}

// ListReceipts implements outbound.Repo.
func (r *OutboundRepo) ListReceipts(ctx context.Context, f outbound.ReceiptFilter) ([]outbound.Receipt, error) {
	if f.Limit < 1 || f.Limit > 500 {
		f.Limit = 100
	}
	out := []outbound.Receipt{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var connID, attID any
		if f.ConnectionID != "" {
			if _, err := uuid.Parse(f.ConnectionID); err != nil {
				return nil
			}
			connID = f.ConnectionID
		}
		if f.AttemptID != "" {
			if _, err := uuid.Parse(f.AttemptID); err != nil {
				return nil
			}
			attID = f.AttemptID
		}
		rows, err := tx.Query(ctx, `SELECT `+receiptCols+` FROM xic_callback_receipts
			WHERE ($1='' OR state=$1) AND ($2::uuid IS NULL OR connection_id=$2::uuid) AND ($3::uuid IS NULL OR attempt_id=$3::uuid) AND (NOT $4 OR needs_reconciliation)
			ORDER BY received_at DESC, receipt_id LIMIT $5`, f.State, connID, attID, f.NeedsReconciliation, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rc outbound.Receipt
			if err := scanReceipt(rows, &rc); err != nil {
				return err
			}
			out = append(out, rc)
		}
		return rows.Err()
	})
	return out, err
}

// FindAttempt implements outbound.Repo: by the provider's reference first, else by the domain request reference.
func (r *OutboundRepo) FindAttempt(ctx context.Context, connectionID, operationID, reference, clientRef string) (*outbound.Attempt, error) {
	if _, err := uuid.Parse(connectionID); err != nil {
		return nil, outbound.ErrNotFound
	}
	var a *outbound.Attempt
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var id string
		if reference != "" {
			err := tx.QueryRow(ctx, `SELECT attempt_id::text FROM xic_outbound_attempts WHERE connection_id=$1::uuid AND operation_id=$2 AND provider_reference=$3
				ORDER BY created_at LIMIT 1`, connectionID, operationID, reference).Scan(&id)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if id == "" && clientRef != "" {
			err := tx.QueryRow(ctx, `SELECT attempt_id::text FROM xic_outbound_attempts WHERE connection_id=$1::uuid AND operation_id=$2 AND domain_request_ref=$3`,
				connectionID, operationID, clientRef).Scan(&id)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if id == "" {
			return outbound.ErrNotFound
		}
		var err error
		a, err = loadOutbound(ctx, tx, id, false, false)
		return err
	})
	return a, err
}
