package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/ledger"
)

// The legacy reasons ranked by how much traffic they stop: an unsubscribe or a
// complaint scopes to marketing, a hard bounce to every purpose, an operator
// hold to every purpose regardless of the stream rules.
//
// AddSuppression upserts on (tenant, address, stream), and without a rank a
// later, weaker event overwrote a stronger one: an unsubscribe arriving after a
// hard bounce rewrote it as UNSUBSCRIBE, which the send gate scopes to
// marketing, so transactional and security mail resumed to a dead address.
const (
	rankCase     = ` WHEN 'UNSUBSCRIBE' THEN 1 WHEN 'COMPLAINT' THEN 2 WHEN 'HARD_BOUNCE' THEN 3 WHEN 'ADMIN_SUPPRESSED' THEN 4 ELSE 0 END)`
	rankEXCLUDED = `(CASE EXCLUDED.reason` + rankCase
	rankExisting = `(CASE email_suppressions.reason` + rankCase
)

// AddSuppression records an email suppression entry with tenant-scoped RLS.
// It is idempotent on (tenant_id, recipient_email, source_stream), and it never
// weakens a recorded reason: a stronger reason replaces a weaker one, while an
// equal or weaker one leaves the row and the time it was first recorded as
// they were. Lifting a suppression is not an upsert; it is the governed NCD
// lift (POST /v1/suppressions/{id}/lift).
func (s *PgStore) AddSuppression(ctx context.Context, supp *ledger.EmailSuppression) error {
	if strings.TrimSpace(supp.TenantID) == "" {
		return fmt.Errorf("missing tenant_id")
	}
	if strings.TrimSpace(supp.RecipientEmail) == "" {
		return fmt.Errorf("missing recipient_email")
	}
	if supp.Reason == "" {
		return fmt.Errorf("missing suppression reason")
	}
	if supp.SourceStream == "" {
		supp.SourceStream = "ALL"
	}
	if supp.SuppressionID == "" {
		supp.SuppressionID = uuid.NewString()
	}
	if supp.CreatedAt.IsZero() {
		supp.CreatedAt = time.Now().UTC()
	}

	normEmail := strings.ToLower(strings.TrimSpace(supp.RecipientEmail))

	return s.withRLS(ctx, supp.TenantID, func(tx pgx.Tx) error {
		const insertSQL = `
			INSERT INTO email_suppressions (
				suppression_id, tenant_id, recipient_email, reason, source_stream,
				provider_name, raw_metadata, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant_id, recipient_email, source_stream) WHERE lifted_at IS NULL
			DO UPDATE SET
				reason = EXCLUDED.reason,
				provider_name = EXCLUDED.provider_name,
				raw_metadata = EXCLUDED.raw_metadata,
				created_at = EXCLUDED.created_at
			WHERE ` + rankEXCLUDED + ` > ` + rankExisting + `;
		`
		_, err := tx.Exec(ctx, insertSQL,
			supp.SuppressionID, supp.TenantID, normEmail, string(supp.Reason), supp.SourceStream,
			supp.ProviderName, supp.RawMetadata, supp.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert email_suppression: %w", err)
		}
		return nil
	})
}

// IsEmailSuppressed checks if recipientEmail has an active suppression entry under tenantID
// for the given sender stream, applying the 8-level precedence rules from ZS-COMMS-EMAIL-001 §4:
// - Security (S0) and Core Transactional (T0) messages cannot be disabled by user unsubscribes.
// - S0 and T0 are only blocked by physical delivery barriers (HARD_BOUNCE, COMPLAINT, ADMIN_SUPPRESSED).
// - Operational (A1), Lifecycle (L1), and Marketing (M1) messages are blocked by any active suppression.
func (s *PgStore) IsEmailSuppressed(
	ctx context.Context,
	tenantID string,
	recipientEmail string,
	stream ledger.SenderStream,
	commClass ledger.CommunicationClass,
) (bool, string, error) {
	if strings.TrimSpace(tenantID) == "" {
		return false, "", fmt.Errorf("missing tenant_id")
	}
	normEmail := strings.ToLower(strings.TrimSpace(recipientEmail))
	if normEmail == "" {
		return false, "", fmt.Errorf("missing recipient_email")
	}

	var isSuppressed bool
	var blockReason string

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const querySQL = `
			SELECT reason, source_stream
			FROM email_suppressions
			WHERE tenant_id = $1
			  AND recipient_email = $2
			  AND lifted_at IS NULL
			  AND (source_stream = 'ALL' OR source_stream = $3);
		`
		rows, err := tx.Query(ctx, querySQL, tenantID, normEmail, string(stream))
		if err != nil {
			return fmt.Errorf("query email_suppressions: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var reason, rowStream string
			if err := rows.Scan(&reason, &rowStream); err != nil {
				return fmt.Errorf("scan email_suppression: %w", err)
			}

			// Precedence rule per §4:
			// Unsubscribe must never disable S0 (Security) or T0 (Transactional) notices
			if commClass == ledger.ClassS0 || commClass == ledger.ClassT0 {
				if reason == string(ledger.SuppressionReasonUnsubscribe) {
					continue
				}
				isSuppressed = true
				blockReason = reason
				return nil
			}

			// For A1, L1, M1, any suppression blocks delivery
			isSuppressed = true
			blockReason = reason
			return nil
		}
		return rows.Err()
	})

	if err != nil {
		return false, "", err
	}
	return isSuppressed, blockReason, nil
}

// ListSuppressions returns the active suppressions for a tenant.
func (s *PgStore) ListSuppressions(ctx context.Context, tenantID string, limit, offset int) ([]*ledger.EmailSuppression, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("missing tenant_id")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	var results []*ledger.EmailSuppression

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const listSQL = `
			SELECT suppression_id, tenant_id, recipient_email, reason, source_stream,
			       provider_name, raw_metadata, created_at
			FROM email_suppressions
			WHERE tenant_id = $1 AND lifted_at IS NULL
			ORDER BY created_at DESC
			LIMIT $2 OFFSET $3;
		`
		rows, err := tx.Query(ctx, listSQL, tenantID, limit, offset)
		if err != nil {
			return fmt.Errorf("list email_suppressions: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var supp ledger.EmailSuppression
			var reasonStr string
			if err := rows.Scan(
				&supp.SuppressionID, &supp.TenantID, &supp.RecipientEmail, &reasonStr,
				&supp.SourceStream, &supp.ProviderName, &supp.RawMetadata, &supp.CreatedAt,
			); err != nil {
				return fmt.Errorf("scan suppression: %w", err)
			}
			supp.Reason = ledger.SuppressionReason(reasonStr)
			results = append(results, &supp)
		}
		return rows.Err()
	})

	if err != nil {
		return nil, err
	}
	return results, nil
}
