package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

const scopeCols = `scope_version_id::text, account_id::text, account_code, legal_entity_id::text, accounting_book_id::text, version, account_name, parent_account_id::text, status,
	effective_from::text, effective_to::text, change_reason, evidence_refs, created_at, created_by`

const noBook = `'00000000-0000-0000-0000-000000000000'::uuid`

func scanScopeVersion(row pgx.Row) (*domain.AccountScopeVersion, error) {
	var v domain.AccountScopeVersion
	if err := row.Scan(&v.ScopeVersionID, &v.AccountID, &v.AccountCode, &v.LegalEntityID, &v.AccountingBookID, &v.Version, &v.AccountName, &v.ParentAccountID, &v.Status,
		&v.EffectiveFrom, &v.EffectiveTo, &v.ChangeReason, &v.EvidenceRefs, &v.CreatedAt, &v.CreatedByPrincipal); err != nil {
		return nil, err
	}
	return &v, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// scopedStatusSQL is the status of the most specific scoped version in force that sets one: a version of the book beats one of the
// entity, and a version that sets only a name does not hide the status of the one below it.
const scopedStatusSQL = `SELECT status FROM account_scope_versions
	WHERE tenant_id = $1::uuid AND account_code = $2 AND legal_entity_id = $3::uuid AND (accounting_book_id = NULLIF($4, '')::uuid OR accounting_book_id IS NULL)
	  AND status IS NOT NULL AND effective_from <= $5::date AND (effective_to IS NULL OR effective_to > $5::date)
	ORDER BY (accounting_book_id IS NOT NULL) DESC, effective_from DESC LIMIT 1`

func scopedStatus(ctx context.Context, q queryRower, tenant, code, entity, book string, on time.Time) (string, bool, error) {
	var st string
	err := q.QueryRow(ctx, scopedStatusSQL, tenant, code, entity, book, on).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return st, true, nil
}

var _ kernel.ScopedAccountStatuser = (*KernelRepo)(nil)

// ScopedAccountStatus implements kernel.ScopedAccountStatuser: the status a scoped version gives the account in the book, when one does.
func (r *KernelRepo) ScopedAccountStatus(ctx context.Context, tenant, code, bookID string, on time.Time) (status string, overridden bool, err error) {
	if _, perr := uuid.Parse(bookID); perr != nil {
		return "", false, nil
	}
	err = r.tx(ctx, tenant, func(t pgx.Tx) error {
		var entity string
		if e := t.QueryRow(ctx, `SELECT legal_entity_id::text FROM accounting_books WHERE tenant_id = $1::uuid AND book_id = $2::uuid`, tenant, bookID).Scan(&entity); e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				return nil
			}
			return e
		}
		var e error
		status, overridden, e = scopedStatus(ctx, t, tenant, code, entity, bookID, on)
		return e
	})
	return status, overridden, err
}

// ScopedAccountStatus is the legacy posting path view of the same thing: the book is named by its code or id, and may be empty.
func (s *PgStore) ScopedAccountStatus(ctx context.Context, tenantID, code, entityID, bookRef string, on time.Time) (status string, overridden bool, err error) {
	if _, perr := uuid.Parse(entityID); perr != nil {
		return "", false, nil
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		book := ""
		if bookRef != "" {
			e := tx.QueryRow(ctx, `SELECT book_id::text FROM accounting_books WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND (book_code = $3 OR book_id::text = $3)`,
				tenantID, entityID, bookRef).Scan(&book)
			if e != nil && !errors.Is(e, pgx.ErrNoRows) {
				return e
			}
		}
		var e error
		status, overridden, e = scopedStatus(ctx, tx, tenantID, code, entityID, book, on)
		return e
	})
	return status, overridden, err
}

// effectiveParent is the parent of an account in a scope on a date: a scoped version that sets one, else the tenant parent. forced holds
// parents not yet stored, so a proposed change can be checked before it is written.
func effectiveParent(ctx context.Context, tx pgx.Tx, tenant, accountID, entity, book string, on time.Time, forced map[string]*string) (*string, error) {
	if p, ok := forced[accountID]; ok {
		return p, nil
	}
	var p *string
	err := tx.QueryRow(ctx, `SELECT parent_account_id::text FROM account_scope_versions
		WHERE tenant_id = $1::uuid AND account_id = $2::uuid AND legal_entity_id = $3::uuid AND (accounting_book_id = NULLIF($4, '')::uuid OR accounting_book_id IS NULL)
		  AND parent_account_id IS NOT NULL AND effective_from <= $5::date AND (effective_to IS NULL OR effective_to > $5::date)
		ORDER BY (accounting_book_id IS NOT NULL) DESC, effective_from DESC LIMIT 1`, tenant, accountID, entity, book, on).Scan(&p)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT parent_account_id::text FROM chart_of_accounts WHERE tenant_id = $1::uuid AND account_id = $2::uuid`, tenant, accountID).Scan(&p)
	return p, err
}

// CreateAccountScopeVersion records a version of an account for an entity or one of its books. A newer version of the same scope closes
// the one before it; the tenant version and every other scope are untouched.
func (s *PgStore) CreateAccountScopeVersion(ctx context.Context, tenantID, accountCode string, req domain.CreateAccountScopeVersionRequest, effective time.Time, principalID string) (*domain.AccountScopeVersion, error) {
	var out *domain.AccountScopeVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		acct, err := accountForUpdate(ctx, tx, tenantID, accountCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAccountNotFound
		}
		if err != nil {
			return mapPgError(err)
		}
		if req.AccountingBookID != "" {
			var owner string
			e := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM accounting_books WHERE tenant_id = $1::uuid AND book_id = $2::uuid`, tenantID, req.AccountingBookID).Scan(&owner)
			if errors.Is(e, pgx.ErrNoRows) || (e == nil && owner != req.LegalEntityID) {
				return domain.ErrAccountScopeBookMismatch
			}
			if e != nil {
				return mapPgError(e)
			}
		}
		var parentID *string
		if req.ParentAccountCode != nil && *req.ParentAccountCode != "" {
			var pid string
			e := tx.QueryRow(ctx, `SELECT account_id::text FROM chart_of_accounts WHERE tenant_id = $1::uuid AND account_code = $2`, tenantID, *req.ParentAccountCode).Scan(&pid)
			if errors.Is(e, pgx.ErrNoRows) {
				return domain.ErrParentAccountNotFound
			}
			if e != nil {
				return mapPgError(e)
			}
			// the new parent must not lead back to the account in the hierarchy as this scope sees it
			forced := map[string]*string{acct.AccountID: &pid}
			cur := &pid
			for depth := 0; cur != nil && depth < 128; depth++ {
				if *cur == acct.AccountID {
					return domain.ErrAccountHierarchyCycle
				}
				cur, e = effectiveParent(ctx, tx, tenantID, *cur, req.LegalEntityID, req.AccountingBookID, effective, forced)
				if e != nil {
					return mapPgError(e)
				}
			}
			parentID = &pid
		}
		scope := `tenant_id = $1::uuid AND account_id = $2::uuid AND legal_entity_id = $3::uuid AND COALESCE(accounting_book_id, ` + noBook + `) = COALESCE(NULLIF($4, '')::uuid, ` + noBook + `)`
		var prevFrom time.Time
		hadPrev := true
		e := tx.QueryRow(ctx, `SELECT effective_from FROM account_scope_versions WHERE `+scope+` AND effective_to IS NULL FOR UPDATE`, tenantID, acct.AccountID, req.LegalEntityID, req.AccountingBookID).Scan(&prevFrom)
		if errors.Is(e, pgx.ErrNoRows) {
			hadPrev = false
		} else if e != nil {
			return mapPgError(e)
		}
		var maxVersion int
		if e := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM account_scope_versions WHERE `+scope, tenantID, acct.AccountID, req.LegalEntityID, req.AccountingBookID).Scan(&maxVersion); e != nil {
			return mapPgError(e)
		}
		if hadPrev {
			if !effective.After(prevFrom) {
				return domain.ErrAccountEffectiveDate
			}
			if _, e := tx.Exec(ctx, `UPDATE account_scope_versions SET effective_to = $5::date WHERE `+scope+` AND effective_to IS NULL`,
				tenantID, acct.AccountID, req.LegalEntityID, req.AccountingBookID, effective); e != nil {
				return mapPgError(e)
			}
		}
		row := tx.QueryRow(ctx, `INSERT INTO account_scope_versions (scope_version_id, tenant_id, account_id, account_code, legal_entity_id, accounting_book_id, version, account_name, parent_account_id, status,
				effective_from, change_reason, evidence_refs, created_by)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, NULLIF($6, '')::uuid, $7, $8, $9::uuid, $10, $11::date, $12, $13, $14) RETURNING `+scopeCols,
			uuid.NewString(), tenantID, acct.AccountID, accountCode, req.LegalEntityID, req.AccountingBookID, maxVersion+1, req.AccountName, parentID, req.Status, effective, req.Reason, req.EvidenceRefs, principalID)
		out, err = scanScopeVersion(row)
		return mapPgError(err)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListAccountScopeVersions lists the scoped versions of an account for an entity: the entity versions first, then each book.
func (s *PgStore) ListAccountScopeVersions(ctx context.Context, tenantID, accountCode, entityID string) ([]domain.AccountScopeVersion, error) {
	out := []domain.AccountScopeVersion{}
	if _, perr := uuid.Parse(entityID); perr != nil {
		return out, nil
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT `+scopeCols+` FROM account_scope_versions WHERE tenant_id = $1::uuid AND account_code = $2 AND legal_entity_id = $3::uuid
			ORDER BY accounting_book_id NULLS FIRST, version DESC`, tenantID, accountCode, entityID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			v, e := scanScopeVersion(rows)
			if e != nil {
				return e
			}
			out = append(out, *v)
		}
		return rows.Err()
	})
	return out, err
}

// GetScopedAccountAsOf is the account for an entity (and optionally a book) on a date: the tenant version with the most specific scoped
// version in force laid over it, field by field.
func (s *PgStore) GetScopedAccountAsOf(ctx context.Context, tenantID, accountCode, entityID, bookID string, asOf time.Time) (*domain.ScopedAccount, error) {
	base, err := s.GetAccountAsOf(ctx, tenantID, accountCode, asOf)
	if err != nil {
		return nil, err
	}
	out := &domain.ScopedAccount{AccountCode: accountCode, AccountName: base.AccountName, AccountType: base.AccountType, Status: base.Status, ParentAccountID: base.ParentAccountID,
		LegalEntityID: entityID, AccountingBookID: bookID, AsOf: asOf.Format("2006-01-02"), TenantVersion: base.Version, NameScope: "TENANT", ParentScope: "TENANT", StatusScope: "TENANT"}
	if _, perr := uuid.Parse(entityID); perr != nil {
		return out, nil
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT `+scopeCols+` FROM account_scope_versions
			WHERE tenant_id = $1::uuid AND account_code = $2 AND legal_entity_id = $3::uuid AND (accounting_book_id = NULLIF($4, '')::uuid OR accounting_book_id IS NULL)
			  AND effective_from <= $5::date AND (effective_to IS NULL OR effective_to > $5::date)
			ORDER BY (accounting_book_id IS NOT NULL) DESC, effective_from DESC`, tenantID, accountCode, entityID, bookID, asOf)
		if e != nil {
			return e
		}
		defer rows.Close()
		var named, parented, stated bool
		for rows.Next() {
			v, e := scanScopeVersion(rows)
			if e != nil {
				return e
			}
			level := "ENTITY"
			if v.AccountingBookID != nil {
				level = "BOOK"
			}
			out.ScopeVersions = append(out.ScopeVersions, v.Version)
			if v.AccountName != nil && !named {
				out.AccountName, out.NameScope, named = *v.AccountName, level, true
			}
			if v.ParentAccountID != nil && !parented {
				out.ParentAccountID, out.ParentScope, parented = v.ParentAccountID, level, true
			}
			if v.Status != nil && !stated {
				out.Status, out.StatusScope, stated = *v.Status, level, true
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
