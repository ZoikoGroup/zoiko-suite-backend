package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

// ErrTokenizationNotConfigured is returned when tokenization is enforced but
// the store was given no tokenizer: a configuration error that refuses the
// write rather than storing the raw value.
var ErrTokenizationNotConfigured = errors.New("tokenization enforced but no tokenizer configured")

// WithTokenization sets how payee account identifiers are stored
// (TOKENIZATION_ENFORCED).
//
// enforced=false is today's behaviour, unchanged: the raw identifier is
// stored, the documented known gap in internal/domain's package doc and
// SPEC_DEVIATIONS. The tokenizer is never called. enforced=true replaces the
// raw value with a token from t before anything is written; if t fails, the
// write is refused. There is no "store it raw" fallback — that would be the
// very gap this exists to close.
func WithTokenization(t tokenization.Tokenizer, enforced bool) Option {
	return func(s *PgStore) { s.tokenizer, s.tokenizationEnforced = t, enforced }
}

// insertIdentifierToken writes a destination's token row (migration 000007)
// in tx. source is WRITE for a destination proposed tokenized, BACKFILL for a
// legacy row the backfill tokenized.
func insertIdentifierToken(ctx context.Context, tx pgx.Tx, destinationID, tenantID, token, source string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO payee_destination_identifier_tokens (destination_id, tenant_id, account_identifier_token, source)
		VALUES ($1, $2, $3, $4)`, destinationID, tenantID, token, source)
	if err != nil {
		return fmt.Errorf("insert account-identifier token: %w", err)
	}
	return nil
}

// protectAccountIdentifier returns the value to persist for a raw account
// identifier: the raw value itself while tokenization is not enforced, its
// token when it is. It runs before the write's transaction opens, so a slow
// or failing tokenization call never holds a transaction open, and a refusal
// leaves nothing written.
func (s *PgStore) protectAccountIdentifier(ctx context.Context, scope tokenization.Scope, raw string) (string, error) {
	if !s.tokenizationEnforced {
		return raw, nil
	}
	if s.tokenizer == nil {
		return "", ErrTokenizationNotConfigured
	}
	token, err := s.tokenizer.Tokenize(ctx, scope, tokenization.ClassificationRestricted, raw)
	if err != nil {
		return "", fmt.Errorf("tokenize account identifier: %w", err)
	}
	return token, nil
}
