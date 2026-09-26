package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/ledger"
)

var (
	// ErrActionTokenNotFound indicates no token matches the given hash under the tenant context.
	ErrActionTokenNotFound = errors.New("action token not found")

	// ErrActionTokenAlreadyConsumed indicates the token has already been consumed (single-use invariant).
	ErrActionTokenAlreadyConsumed = errors.New("action token already consumed")

	// ErrActionTokenExpired indicates the token validity period has lapsed.
	ErrActionTokenExpired = errors.New("action token has expired")

	// ErrActionTokenRevoked indicates the token was administratively revoked before use.
	ErrActionTokenRevoked = errors.New("action token has been revoked")

	// ErrActionTokenInvalid indicates the token is not in an active, consumable state.
	ErrActionTokenInvalid = errors.New("action token is invalid")
)

// CreateActionToken persists a single-use, signed action token with tenant-scoped RLS.
func (s *PgStore) CreateActionToken(ctx context.Context, token *ledger.ActionToken) error {
	if strings.TrimSpace(token.TenantID) == "" {
		return fmt.Errorf("missing tenant_id")
	}
	if strings.TrimSpace(token.TokenHash) == "" {
		return fmt.Errorf("missing token_hash")
	}
	if strings.TrimSpace(token.MessageIntentID) == "" {
		return fmt.Errorf("missing message_intent_id")
	}
	if strings.TrimSpace(token.RecipientPrincipalID) == "" {
		return fmt.Errorf("missing recipient_principal_id")
	}
	if strings.TrimSpace(token.Purpose) == "" {
		return fmt.Errorf("missing purpose")
	}
	if strings.TrimSpace(token.TargetActionURL) == "" {
		return fmt.Errorf("missing target_action_url")
	}
	if token.TokenID == "" {
		token.TokenID = uuid.NewString()
	}
	if token.TargetMethod == "" {
		token.TargetMethod = "POST"
	}
	if token.Status == "" {
		token.Status = ledger.ActionTokenStatusActive
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now().UTC()
	}

	return s.withRLS(ctx, token.TenantID, func(tx pgx.Tx) error {
		const insertSQL = `
			INSERT INTO action_tokens (
				token_id, token_hash, message_intent_id, tenant_id, recipient_principal_id,
				purpose, target_action_url, target_method, payload, status, expires_at, created_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
			);
		`
		var payloadData []byte
		if len(token.Payload) > 0 {
			payloadData = token.Payload
		}
		_, err := tx.Exec(ctx, insertSQL,
			token.TokenID,
			token.TokenHash,
			token.MessageIntentID,
			token.TenantID,
			token.RecipientPrincipalID,
			token.Purpose,
			token.TargetActionURL,
			token.TargetMethod,
			payloadData,
			string(token.Status),
			token.ExpiresAt,
			token.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert action_token: %w", err)
		}
		return nil
	})
}

// GetActionTokenByHash fetches an action token by its SHA-256 hash under tenant-scoped RLS.
func (s *PgStore) GetActionTokenByHash(ctx context.Context, tenantID, tokenHash string) (*ledger.ActionToken, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("missing tenant_id")
	}
	if strings.TrimSpace(tokenHash) == "" {
		return nil, fmt.Errorf("missing token_hash")
	}

	var tok ledger.ActionToken
	var statusStr string
	var payloadData []byte

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const selectSQL = `
			SELECT token_id, token_hash, message_intent_id, tenant_id, recipient_principal_id,
			       purpose, target_action_url, target_method, payload, status,
			       expires_at, consumed_at, consumed_by_ip, created_at
			FROM action_tokens
			WHERE token_hash = $1 AND tenant_id = $2;
		`
		err := tx.QueryRow(ctx, selectSQL, tokenHash, tenantID).Scan(
			&tok.TokenID,
			&tok.TokenHash,
			&tok.MessageIntentID,
			&tok.TenantID,
			&tok.RecipientPrincipalID,
			&tok.Purpose,
			&tok.TargetActionURL,
			&tok.TargetMethod,
			&payloadData,
			&statusStr,
			&tok.ExpiresAt,
			&tok.ConsumedAt,
			&tok.ConsumedByIP,
			&tok.CreatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrActionTokenNotFound
			}
			return fmt.Errorf("get action_token: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	tok.Status = ledger.ActionTokenStatus(statusStr)
	if len(payloadData) > 0 {
		tok.Payload = payloadData
	}
	return &tok, nil
}

// ConsumeActionToken atomically consumes a single-use action token with FOR UPDATE row locking and RLS.
// If the token is already consumed, expired, or revoked, it returns an explicit domain error.
func (s *PgStore) ConsumeActionToken(ctx context.Context, tenantID, tokenHash, clientIP string) (*ledger.ActionToken, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("missing tenant_id")
	}
	if strings.TrimSpace(tokenHash) == "" {
		return nil, fmt.Errorf("missing token_hash")
	}

	var tok ledger.ActionToken
	var statusStr string
	var payloadData []byte

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const lockSQL = `
			SELECT token_id, token_hash, message_intent_id, tenant_id, recipient_principal_id,
			       purpose, target_action_url, target_method, payload, status,
			       expires_at, consumed_at, consumed_by_ip, created_at
			FROM action_tokens
			WHERE token_hash = $1 AND tenant_id = $2
			FOR UPDATE;
		`
		err := tx.QueryRow(ctx, lockSQL, tokenHash, tenantID).Scan(
			&tok.TokenID,
			&tok.TokenHash,
			&tok.MessageIntentID,
			&tok.TenantID,
			&tok.RecipientPrincipalID,
			&tok.Purpose,
			&tok.TargetActionURL,
			&tok.TargetMethod,
			&payloadData,
			&statusStr,
			&tok.ExpiresAt,
			&tok.ConsumedAt,
			&tok.ConsumedByIP,
			&tok.CreatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrActionTokenNotFound
			}
			return fmt.Errorf("lock action_token: %w", err)
		}

		currentStatus := ledger.ActionTokenStatus(statusStr)
		if currentStatus == ledger.ActionTokenStatusConsumed {
			return ErrActionTokenAlreadyConsumed
		}
		if currentStatus == ledger.ActionTokenStatusRevoked {
			return ErrActionTokenRevoked
		}
		now := time.Now().UTC()
		if currentStatus == ledger.ActionTokenStatusExpired || now.After(tok.ExpiresAt) {
			_, _ = tx.Exec(ctx, `UPDATE action_tokens SET status = 'EXPIRED' WHERE token_id = $1`, tok.TokenID)
			return ErrActionTokenExpired
		}
		if currentStatus != ledger.ActionTokenStatusActive {
			return ErrActionTokenInvalid
		}

		const updateSQL = `
			UPDATE action_tokens
			SET status = 'CONSUMED',
			    consumed_at = $1,
			    consumed_by_ip = $2
			WHERE token_id = $3;
		`
		var ipVal *string
		if strings.TrimSpace(clientIP) != "" {
			trimmed := strings.TrimSpace(clientIP)
			ipVal = &trimmed
		}
		if _, err := tx.Exec(ctx, updateSQL, now, ipVal, tok.TokenID); err != nil {
			return fmt.Errorf("update action_token to CONSUMED: %w", err)
		}

		tok.Status = ledger.ActionTokenStatusConsumed
		tok.ConsumedAt = &now
		tok.ConsumedByIP = ipVal
		if len(payloadData) > 0 {
			tok.Payload = payloadData
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &tok, nil
}

// RevokeActionToken marks an active action token as REVOKED under tenant RLS.
func (s *PgStore) RevokeActionToken(ctx context.Context, tenantID, tokenID string) error {
	if strings.TrimSpace(tenantID) == "" {
		return fmt.Errorf("missing tenant_id")
	}
	if strings.TrimSpace(tokenID) == "" {
		return fmt.Errorf("missing token_id")
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const updateSQL = `
			UPDATE action_tokens
			SET status = 'REVOKED'
			WHERE token_id = $1 AND tenant_id = $2 AND status = 'ACTIVE';
		`
		_, err := tx.Exec(ctx, updateSQL, tokenID, tenantID)
		if err != nil {
			return fmt.Errorf("revoke action_token: %w", err)
		}
		return nil
	})
}
