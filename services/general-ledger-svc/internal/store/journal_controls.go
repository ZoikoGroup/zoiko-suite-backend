package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// WithdrawJournal withdraws a draft that was never submitted (standard §9: drafts may be withdrawn, posted records are
// never deleted). The journal stays on record, marked withdrawn with who, when and why, and leaves every review queue.
func (s *PgStore) WithdrawJournal(ctx context.Context, tenantID, journalID, principalID, reason string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var entity, correlation string
		err := tx.QueryRow(ctx, `
			UPDATE journal_headers
			SET approval_status = $1, withdrawn_at = $2, withdrawn_by_principal_id = $3, withdrawn_reason = $4
			WHERE journal_id = $5 AND tenant_id = $6 AND status = 'PENDING' AND approval_status = 'DRAFT'
			RETURNING legal_entity_id::text, correlation_id`,
			string(domain.ApprovalStatusCancelled), time.Now().UTC(), principalID, reason, journalID, tenantID).Scan(&entity, &correlation)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if e := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal_headers WHERE journal_id = $1 AND tenant_id = $2)`, journalID, tenantID).Scan(&exists); e != nil {
				return mapPgError(e)
			}
			if !exists {
				return domain.ErrJournalNotFound
			}
			return domain.ErrNotWithdrawable
		}
		if err != nil {
			return mapPgError(err)
		}
		return s.enqueueJournalEvent(ctx, tx, "withdrawn", tenantID, entity, journalID, principalID, correlation,
			map[string]any{"journal_id": journalID, "reason": reason})
	})
}

// ── recurring templates ─────────────────────────────────────────────────────────

const templateCols = `template_id::text, tenant_id::text, legal_entity_id::text, name, journal_type, journal_purpose, currency_code, book_id, description,
	lines, frequency, next_run_date, end_date, status, created_by, created_at, last_generated_at, anchor_day`

func scanTemplate(row pgx.Row) (*domain.RecurringTemplate, int, error) {
	var t domain.RecurringTemplate
	var lines []byte
	var created time.Time
	var last *time.Time
	var anchor int
	var jt string
	if err := row.Scan(&t.TemplateID, &t.TenantID, &t.LegalEntityID, &t.Name, &jt, &t.JournalPurpose, &t.CurrencyCode, &t.BookID, &t.Description,
		&lines, &t.Frequency, &t.NextRunDate, &t.EndDate, &t.Status, &t.CreatedBy, &created, &last, &anchor); err != nil {
		return nil, 0, err
	}
	t.JournalType = domain.JournalType(jt)
	if err := json.Unmarshal(lines, &t.Lines); err != nil {
		return nil, 0, err
	}
	t.CreatedAt = created.UTC().Format(time.RFC3339)
	if last != nil {
		s := last.UTC().Format(time.RFC3339)
		t.LastGeneratedAt = &s
	}
	return &t, anchor, nil
}

// CreateRecurringTemplate stores a template. The day of the month it first runs on becomes its anchor, so a template
// anchored on the 31st keeps landing on the last day of shorter months instead of drifting to the 28th.
func (s *PgStore) CreateRecurringTemplate(ctx context.Context, t *domain.RecurringTemplate) error {
	lines, err := json.Marshal(t.Lines)
	if err != nil {
		return err
	}
	return s.withRLS(ctx, t.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO recurring_journal_templates (template_id, tenant_id, legal_entity_id, name, journal_type, journal_purpose, currency_code, book_id,
				description, lines, frequency, next_run_date, anchor_day, end_date, status, created_by)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13, $14, 'ACTIVE', $15)`,
			t.TemplateID, t.TenantID, t.LegalEntityID, t.Name, string(t.JournalType), t.JournalPurpose, t.CurrencyCode, t.BookID, t.Description,
			string(lines), t.Frequency, t.NextRunDate, t.NextRunDate.Day(), t.EndDate, t.CreatedBy)
		if err != nil {
			if pgCode(err) == "23505" {
				return domain.ErrTemplateExists
			}
			return mapPgError(err)
		}
		t.Status = "ACTIVE"
		return nil
	})
}

// GetRecurringTemplate returns one template of the tenant.
func (s *PgStore) GetRecurringTemplate(ctx context.Context, tenantID, id string) (*domain.RecurringTemplate, error) {
	var out *domain.RecurringTemplate
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		t, _, e := scanTemplate(tx.QueryRow(ctx, `SELECT `+templateCols+` FROM recurring_journal_templates WHERE tenant_id = $1::uuid AND template_id = $2::uuid`, tenantID, id))
		out = t
		return e
	})
	return out, templateErr(err)
}

// ListRecurringTemplates lists the tenant's templates, optionally for one entity.
func (s *PgStore) ListRecurringTemplates(ctx context.Context, tenantID, legalEntityID string) ([]domain.RecurringTemplate, error) {
	out := []domain.RecurringTemplate{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+templateCols+` FROM recurring_journal_templates
			WHERE tenant_id = $1::uuid AND ($2 = '' OR legal_entity_id::text = $2) ORDER BY name LIMIT 500`, tenantID, legalEntityID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			t, _, e := scanTemplate(rows)
			if e != nil {
				return e
			}
			out = append(out, *t)
		}
		return rows.Err()
	})
	return out, err
}

// SetRecurringTemplateStatus pauses, resumes or ends a template.
func (s *PgStore) SetRecurringTemplateStatus(ctx context.Context, tenantID, id, status string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE recurring_journal_templates SET status = $3 WHERE tenant_id = $1::uuid AND template_id = $2::uuid AND status <> 'ENDED'`, tenantID, id, status)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrTemplateNotFound
		}
		return nil
	})
}

func templateErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return domain.ErrTemplateNotFound
	}
	return err
}

// nextRun is the run after d: weekly adds seven days, the others step whole months and land on the anchor day, clamped to
// the last day of a shorter month.
func nextRun(d time.Time, frequency string, anchorDay int) time.Time {
	if frequency == "WEEKLY" {
		return d.AddDate(0, 0, 7)
	}
	step := map[string]int{"MONTHLY": 1, "QUARTERLY": 3, "YEARLY": 12}[frequency]
	first := time.Date(d.Year(), d.Month()+time.Month(step), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	day := anchorDay
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
}

// GenerateRecurringJournal creates the draft journal of one run of a template and advances the template, in one
// transaction. Generating the same run again returns the journal it already made. A run before the next one due was
// generated already; a run after it would skip one and is refused.
func (s *PgStore) GenerateRecurringJournal(ctx context.Context, tenantID, templateID string, runDate domain.Date, principalID string) (*domain.JournalHeader, []domain.JournalLine, bool, error) {
	var header *domain.JournalHeader
	var lines []domain.JournalLine
	created := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		t, anchor, err := scanTemplate(tx.QueryRow(ctx, `SELECT `+templateCols+` FROM recurring_journal_templates WHERE tenant_id = $1::uuid AND template_id = $2::uuid FOR UPDATE`, tenantID, templateID))
		if err != nil {
			return templateErr(err)
		}
		correlation := fmt.Sprintf("recurring:%s:%s", templateID, runDate.String())
		if runDate.Before(t.NextRunDate.Time) {
			var id string
			if e := tx.QueryRow(ctx, `SELECT journal_id::text FROM journal_headers WHERE tenant_id = $1::uuid AND correlation_id = $2`, tenantID, correlation).Scan(&id); e != nil {
				if errors.Is(e, pgx.ErrNoRows) {
					return domain.ErrTemplateOutOfOrder
				}
				return mapPgError(e)
			}
			var st, ap string
			h := &domain.JournalHeader{}
			if e := tx.QueryRow(ctx, `SELECT `+journalHeaderColumns+` FROM journal_headers WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`, tenantID, id).Scan(scanHeaderTargets(h, &st, &ap)...); e != nil {
				return mapPgError(e)
			}
			h.Status, h.ApprovalStatus = domain.JournalStatus(st), domain.ApprovalStatus(ap)
			ls, e := queryLines(ctx, tx, tenantID, id)
			if e != nil {
				return e
			}
			header, lines = h, ls
			return nil
		}
		if t.Status != "ACTIVE" {
			return domain.ErrTemplateNotActive
		}
		if runDate.After(t.NextRunDate.Time) {
			return domain.ErrTemplateOutOfOrder
		}
		if t.EndDate != nil && runDate.After(t.EndDate.Time) {
			return domain.ErrTemplateNotActive
		}

		tpl := t.TemplateID
		hdr := &domain.JournalHeader{
			JournalID: uuid.NewString(), TenantID: tenantID, LegalEntityID: t.LegalEntityID, FiscalPeriod: runDate.Format("2006-01"),
			Status: domain.JournalStatusPending, Description: t.Description, CreatedByPrincipalID: principalID, CorrelationID: correlation,
			JournalType: t.JournalType, TransactionDate: runDate, PostingDate: runDate, CurrencyCode: t.CurrencyCode, BookID: t.BookID,
			ApprovalStatus: domain.ApprovalStatusDraft, JournalPurpose: &t.JournalPurpose, RecurringTemplateID: &tpl,
		}
		jl := make([]domain.JournalLine, len(t.Lines))
		for i, l := range t.Lines {
			jl[i] = domain.JournalLine{AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount, Description: l.Description, Dimensions: l.Dimensions}
		}
		resLines, ok, e := insertJournal(ctx, tx, tenantID, hdr, jl)
		if e != nil {
			return e
		}
		if ok {
			if e := s.enqueueJournalEvent(ctx, tx, "created", tenantID, t.LegalEntityID, hdr.JournalID, principalID, correlation,
				map[string]any{"journal_id": hdr.JournalID, "tenant_id": tenantID, "legal_entity_id": t.LegalEntityID, "fiscal_period": hdr.FiscalPeriod, "recurring_template_id": tpl}); e != nil {
				return e
			}
		}
		next := nextRun(runDate.Time, t.Frequency, anchor)
		status := "ACTIVE"
		if t.EndDate != nil && next.After(t.EndDate.Time) {
			status = "ENDED"
		}
		if _, e := tx.Exec(ctx, `UPDATE recurring_journal_templates SET next_run_date = $3, status = $4, last_generated_at = now() WHERE tenant_id = $1::uuid AND template_id = $2::uuid`,
			tenantID, templateID, next, status); e != nil {
			return mapPgError(e)
		}
		header, lines, created = hdr, resLines, ok
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return header, lines, created, nil
}

// AccountControls returns the counterparty rule of each of the given accounts that is in the chart; unregistered codes are absent.
func (s *PgStore) AccountControls(ctx context.Context, tenantID string, codes []string) (map[string]domain.AccountControl, error) {
	out := map[string]domain.AccountControl{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT account_code, COALESCE(control_type, ''), counterparty_required FROM chart_of_accounts
			WHERE tenant_id = $1::uuid AND account_code = ANY($2)`, tenantID, codes)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var code string
			var c domain.AccountControl
			if err := rows.Scan(&code, &c.ControlType, &c.CounterpartyRequired); err != nil {
				return err
			}
			out[code] = c
		}
		return rows.Err()
	})
	return out, err
}

// CheckReconciliationCase confirms that a reconciliation adjustment names a real case of the same entity that is still open, and
// returns the code of the book the case was run in: the adjustment is posted in that book.
func (s *PgStore) CheckReconciliationCase(ctx context.Context, tenantID, caseID, legalEntityID string) (string, error) {
	var book string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT r.status, b.book_code FROM reconciliation_runs r JOIN accounting_books b ON b.book_id = r.accounting_book_id
			WHERE r.tenant_id = $1::uuid AND r.reconciliation_run_id = $2::uuid AND r.legal_entity_id = $3::uuid`,
			tenantID, caseID, legalEntityID).Scan(&status, &book)
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return domain.ErrJournalNotFound
		}
		if err != nil {
			return mapPgError(err)
		}
		if status != "WARNING" && status != "BREAK" {
			return domain.ErrReconciliationCaseNotOpen
		}
		return nil
	})
	return book, err
}
