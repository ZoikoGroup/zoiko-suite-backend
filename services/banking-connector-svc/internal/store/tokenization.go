package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/tokenization"
)

// ErrTokenizationNotConfigured is returned when tokenization is enforced but
// the store was given no tokenizer: a configuration error that refuses the
// write rather than storing the raw value.
var ErrTokenizationNotConfigured = errors.New("tokenization enforced but no tokenizer configured")

// WithTokenization sets how account numbers are stored (TOKENIZATION_ENFORCED).
//
// enforced=false is today's behaviour, unchanged: the raw account number is
// stored, a documented known gap (SPEC_DEVIATIONS). The tokenizer is never
// called. enforced=true replaces the raw value with a token from t before
// anything is written; if t fails, the write is refused. There is no "store it
// raw" fallback — that would be the very gap this exists to close.
func WithTokenization(t tokenization.Tokenizer, enforced bool) Option {
	return func(s *PgStore) { s.tokenizer, s.tokenizationEnforced = t, enforced }
}

// insertAccountToken writes c's token row (migration 010) in tx. source is
// WRITE for a connection created tokenized, BACKFILL for a legacy row the
// backfill tokenized.
func insertAccountToken(ctx context.Context, tx pgx.Tx, c *domain.BankConnection, source string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO bank_connection_account_tokens (connection_id, tenant_id, account_number_token, account_number_masked, source)
		VALUES ($1, $2, $3, $4, $5)`,
		c.ConnectionID, c.TenantID, c.AccountNumberToken, c.AccountNumberMasked, source)
	if err != nil {
		return fmt.Errorf("insert account-number token: %w", err)
	}
	return nil
}

// protectAccountNumber returns the value to persist for a raw account number:
// the raw value itself while tokenization is not enforced, its token when it
// is. It runs before the write's transaction opens, so a slow or failing
// tokenization call never holds a transaction open, and a refusal leaves
// nothing written.
func (p *PgStore) protectAccountNumber(ctx context.Context, scope tokenization.Scope, raw string) (string, error) {
	if !p.tokenizationEnforced {
		return raw, nil
	}
	if p.tokenizer == nil {
		return "", ErrTokenizationNotConfigured
	}
	token, err := p.tokenizer.Tokenize(ctx, scope, tokenization.ClassificationRestricted, raw)
	if err != nil {
		return "", fmt.Errorf("tokenize account number: %w", err)
	}
	return token, nil
}
