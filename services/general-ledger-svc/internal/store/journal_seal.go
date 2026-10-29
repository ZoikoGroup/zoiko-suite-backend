package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// sealJournal gives a journal that has just been posted its human-readable number and its content hash (standard §8.1,
// §36), inside the posting transaction. It is idempotent: a journal that is already sealed keeps what it has.
//
// The number comes from a counter row per (tenant, entity, book, year) incremented under the row lock, so numbers are
// gap-free: a posting that rolls back gives its number back, and two postings in the same scope serialise on the row. The
// number is for people; nothing in the schema uses it as a key.
//
// The hash covers what makes the posting what it is — scope, dates, type, currency, lineage and every line — and is
// computed by the database from the stored rows, so every posting path (event posting, approved journals, reversals)
// seals the same way and a later audit can recompute it from the tables alone.
func sealJournal(ctx context.Context, tx pgx.Tx, tenantID, journalID string) (number string, err error) {
	var entity, bookKey string
	var postingDate time.Time
	var existingNumber, existingHash *string
	err = tx.QueryRow(ctx, `
		SELECT legal_entity_id::text, COALESCE(book_id, ''), posting_date, journal_number, journal_hash
		FROM journal_headers WHERE journal_id = $1 AND tenant_id = $2 FOR UPDATE`, journalID, tenantID).
		Scan(&entity, &bookKey, &postingDate, &existingNumber, &existingHash)
	if err != nil {
		return "", mapPgError(err)
	}
	if existingHash != nil {
		if existingNumber != nil {
			return *existingNumber, nil
		}
		return "", nil // posted before numbering existed
	}

	// The balance equation of §8.3, enforced where it cannot be bypassed: the database refuses to seal a journal whose
	// debits and credits differ, whichever path produced it.
	var lines int
	var balanced bool
	if err = tx.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(debit_amount), 0) = COALESCE(sum(credit_amount), 0)
		FROM journal_lines WHERE journal_id = $1 AND tenant_id = $2`, journalID, tenantID).Scan(&lines, &balanced); err != nil {
		return "", mapPgError(err)
	}
	if lines == 0 || !balanced {
		return "", domain.ErrUnbalancedJournal
	}

	var n int64
	err = tx.QueryRow(ctx, `
		INSERT INTO journal_number_counters (tenant_id, legal_entity_id, book_key, scope_year, last_number)
		VALUES ($1, $2::uuid, $3, $4, 1)
		ON CONFLICT (tenant_id, legal_entity_id, book_key, scope_year)
		DO UPDATE SET last_number = journal_number_counters.last_number + 1
		RETURNING last_number`, tenantID, entity, bookKey, postingDate.Year()).Scan(&n)
	if err != nil {
		return "", fmt.Errorf("allocate journal number: %w", mapPgError(err))
	}
	number = fmt.Sprintf("JE-%d-%06d", postingDate.Year(), n)

	tag, err := tx.Exec(ctx, `
		UPDATE journal_headers h SET journal_number = $3, journal_hash = (
			SELECT encode(sha256(convert_to(
				h.tenant_id::text || '|' || h.legal_entity_id::text || '|' || COALESCE(h.book_id, '') || '|' || h.posting_date::text || '|' ||
				h.journal_type || '|' || h.currency_code || '|' || COALESCE(h.reversal_of_journal_id::text, '') || '|' ||
				COALESCE(h.source_accounting_event_id::text, '') || COALESCE('|' || h.evidence_bundle_id, '') || E'\n' ||
				COALESCE((SELECT string_agg(l.line_number || ':' || l.account_code || ':' || l.debit_amount::text || ':' || l.credit_amount::text || ':' ||
				                            COALESCE(l.transaction_amount::text, '') || ':' || COALESCE(l.transaction_currency, '') || ':' ||
				                            COALESCE(l.posting_rule_line_ref, '') || COALESCE(':' || l.tax_detail_ref, ''), E'\n' ORDER BY l.line_number)
				          FROM journal_lines l WHERE l.journal_id = h.journal_id), ''),
				'UTF8')), 'hex'))
		WHERE h.journal_id = $1 AND h.tenant_id = $2 AND h.journal_hash IS NULL`, journalID, tenantID, number)
	if err != nil {
		return "", fmt.Errorf("seal journal: %w", mapPgError(err))
	}
	if tag.RowsAffected() != 1 {
		return "", errors.New("seal journal: the journal changed while it was being sealed")
	}
	return number, nil
}
