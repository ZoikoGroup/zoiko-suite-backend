package store

// REF-06 accounting books on Postgres (migration 000023). See registry.BookStore for the contract.
//
// Every command takes the book's row lock and checks expected_version under it. A basis change closes the current version
// and adds the next, prospectively; a status change appends a transition. "Could this book post on that date" is
// reconstructed from the transitions and the basis versions, never from the current status.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var _ registry.BookStore = (*PgStore)(nil)

const bookColumns = `book_id, tenant_id, COALESCE(legal_entity_id::text,''), COALESCE(group_code,''), book_code, name, book_type, purpose, status,
	material, current_basis, record_version, created_by, created_at, COALESCE(activated_by,'')`

func scanBook(row pgx.Row) (*domain.AccountingBook, error) {
	var b domain.AccountingBook
	if err := row.Scan(&b.BookID, &b.TenantID, &b.LegalEntityID, &b.GroupCode, &b.BookCode, &b.Name, &b.BookType, &b.Purpose, &b.Status,
		&b.Material, &b.CurrentBasis, &b.RecordVersion, &b.CreatedBy, &b.CreatedAt, &b.ActivatedBy); err != nil {
		return nil, err
	}
	return &b, nil
}

const basisColumns = `version_no, framework, COALESCE(framework_detail,''), functional_currency, presentation_currency, COALESCE(chart_ref,''),
	COALESCE(policy_ref,''), valid_from, valid_to, COALESCE(reason,''), evidence_ref, recorded_by, recorded_at`

func scanBasis(row pgx.Row) (*domain.BookBasisVersion, error) {
	var v domain.BookBasisVersion
	var from time.Time
	var to *time.Time
	if err := row.Scan(&v.VersionNo, &v.Framework, &v.FrameworkDetail, &v.FunctionalCurrency, &v.PresentationCurrency, &v.ChartRef, &v.PolicyRef,
		&from, &to, &v.Reason, &v.EvidenceRef, &v.RecordedBy, &v.RecordedAt); err != nil {
		return nil, err
	}
	v.ValidFrom, v.ValidTo = fmtDate(from), fmtDatePtr(to)
	return &v, nil
}

const bookTransitionColumns = `seq, from_status, to_status, effective_date, command, COALESCE(reason,''), evidence_ref, actor,
	COALESCE(version_before,0), version_after, occurred_at`

func scanBookTransition(row pgx.Row) (*domain.BookTransition, error) {
	var t domain.BookTransition
	var from *string
	var eff time.Time
	if err := row.Scan(&t.Seq, &from, &t.To, &eff, &t.Command, &t.Reason, &t.EvidenceRef, &t.Actor, &t.VersionBefore, &t.VersionAfter, &t.OccurredAt); err != nil {
		return nil, err
	}
	if from != nil {
		t.From = *from
	}
	t.EffectiveDate = fmtDate(eff)
	return &t, nil
}

func insertBookBasis(ctx context.Context, tx pgx.Tx, tid, bookID string, no int, f domain.BookBasisFields, from time.Time, reason, actor, corr string) (*domain.BookBasisVersion, error) {
	return scanBasis(tx.QueryRow(ctx, `INSERT INTO book_basis_versions (basis_id, tenant_id, book_id, version_no, framework, framework_detail, functional_currency,
		presentation_currency, chart_ref, policy_ref, valid_from, reason, evidence_ref, recorded_by, correlation_id)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,NULLIF($9,''),NULLIF($10,''),$11,NULLIF($12,''),$13,$14,NULLIF($15,'')) RETURNING `+basisColumns,
		uuid.NewString(), tid, bookID, no, string(f.Framework), strings.TrimSpace(f.FrameworkDetail), f.FunctionalCurrency, f.PresentationCurrency,
		strings.TrimSpace(f.ChartRef), strings.TrimSpace(f.PolicyRef), from, strings.TrimSpace(reason), strings.TrimSpace(f.EvidenceRef), actor, corr))
}

func insertBookTransition(ctx context.Context, tx pgx.Tx, tid, bookID string, seq int, from *string, to string, eff time.Time, command, reason, evidence, actor string,
	before *int64, after int64, corr string) (*domain.BookTransition, error) {
	return scanBookTransition(tx.QueryRow(ctx, `INSERT INTO book_transitions (transition_id, tenant_id, book_id, seq, from_status, to_status, effective_date,
		command, reason, evidence_ref, actor, version_before, version_after, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13,NULLIF($14,'')) RETURNING `+bookTransitionColumns,
		uuid.NewString(), tid, bookID, seq, from, to, eff, command, reason, evidence, actor, before, after, corr))
}

func lockBook(ctx context.Context, tx pgx.Tx, tid, id string) (*domain.AccountingBook, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrNotFound
	}
	b, err := scanBook(tx.QueryRow(ctx, `SELECT `+bookColumns+` FROM accounting_books WHERE book_id = $1 AND tenant_id = $2 FOR UPDATE`, id, tid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return b, err
}

func currentBasis(ctx context.Context, tx pgx.Tx, bookID string) (*domain.BookBasisVersion, error) {
	return scanBasis(tx.QueryRow(ctx, `SELECT `+basisColumns+` FROM book_basis_versions WHERE book_id = $1 ORDER BY version_no DESC LIMIT 1`, bookID))
}

// CreateAccountingBook opens a DRAFT book.
func (s *PgStore) CreateAccountingBook(ctx context.Context, req domain.CreateAccountingBookRequest, actor string) (*registry.BookOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.BookOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if req.LegalEntityID != "" {
			if err := entityExists(ctx, tx, tid, req.LegalEntityID); err != nil {
				return err
			}
		}
		from := time.Now().UTC()
		if req.ValidFrom != "" {
			from, _ = domain.ParseDate(req.ValidFrom)
		}
		from = asOfDate(from)
		id := uuid.NewString()
		b, err := scanBook(tx.QueryRow(ctx, `INSERT INTO accounting_books (book_id, tenant_id, legal_entity_id, group_code, book_code, name, book_type, purpose,
			material, created_by, correlation_id) VALUES ($1,$2,NULLIF($3,'')::uuid,NULLIF($4,''),$5,$6,$7,$8,$9,$10,NULLIF($11,'')) RETURNING `+bookColumns,
			id, tid, req.LegalEntityID, strings.TrimSpace(req.GroupCode), req.BookCode, strings.TrimSpace(req.Name), string(req.BookType),
			strings.TrimSpace(req.Purpose), req.BookType.Material(), actor, req.CorrelationID))
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: book code %s is already used by this entity or group, and a code is never reused", registry.ErrConflict, req.BookCode)
			}
			return err
		}
		basis, err := insertBookBasis(ctx, tx, tid, id, 1, req.BookBasisFields, from, "", actor, req.CorrelationID)
		if err != nil {
			return err
		}
		t, err := insertBookTransition(ctx, tx, tid, id, 1, nil, domain.BookDraft, from, "CreateAccountingBook", "", basis.EvidenceRef, actor, nil, 1, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.BookOutcome{Book: *b, Basis: *basis, Transition: t}
		rec, err := events.BookCreatedRecord(*b, *basis, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// AmendBookBasis adds the next basis version, prospectively.
func (s *PgStore) AmendBookBasis(ctx context.Context, bookID string, req domain.AmendBookBasisRequest, actor string, allowUnversioned bool) (*registry.BookOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.BookOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		b, err := lockBook(ctx, tx, tid, bookID)
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, b.RecordVersion, allowUnversioned, "book"); err != nil {
			return err
		}
		if b.Status == domain.BookRetired {
			return fmt.Errorf("%w: a retired book keeps its basis for history and takes no new one", registry.ErrStateConflict)
		}
		if !domain.FrameworkAllowedFor(b.BookType, req.Framework) {
			return fmt.Errorf("%w: framework %s is not allowed for a %s book", registry.ErrInvalidInput, req.Framework, b.BookType)
		}
		cur, err := currentBasis(ctx, tx, bookID)
		if err != nil {
			return err
		}
		if cur.Framework == req.Framework && cur.FrameworkDetail == strings.TrimSpace(req.FrameworkDetail) && cur.FunctionalCurrency == req.FunctionalCurrency &&
			cur.PresentationCurrency == req.PresentationCurrency && cur.ChartRef == strings.TrimSpace(req.ChartRef) && cur.PolicyRef == strings.TrimSpace(req.PolicyRef) {
			return fmt.Errorf("%w: the basis is already this", registry.ErrInvalidInput)
		}
		today := asOfDate(time.Now().UTC())
		from := today
		if req.ValidFrom != "" {
			f, _ := domain.ParseDate(req.ValidFrom)
			from = f
		}
		curFrom, _ := domain.ParseDate(cur.ValidFrom)
		switch {
		case b.Status == domain.BookDraft:
			// A draft was never in use: it may be corrected, on or after the day the current basis starts.
			if from.Before(curFrom) {
				return fmt.Errorf("%w: valid_from is before the current basis starts on %s", registry.ErrInvalidInput, cur.ValidFrom)
			}
		default:
			// s4.16: a basis change cannot rewrite how earlier periods were booked, so it is prospective.
			if from.Before(today) {
				return fmt.Errorf("%w: a basis change is prospective; valid_from cannot be before today (%s)", registry.ErrInvalidInput, fmtDate(today))
			}
			if !from.After(curFrom) {
				return fmt.Errorf("%w: valid_from must be after the current basis starts on %s", registry.ErrInvalidInput, cur.ValidFrom)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE book_basis_versions SET valid_to = $3 WHERE book_id = $1 AND version_no = $2`, bookID, cur.VersionNo, from); err != nil {
			return err
		}
		next, err := insertBookBasis(ctx, tx, tid, bookID, cur.VersionNo+1, req.BookBasisFields, from, req.Reason, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		nb, err := scanBook(tx.QueryRow(ctx, `UPDATE accounting_books SET current_basis = $3, record_version = record_version + 1 WHERE book_id = $1 AND tenant_id = $2 RETURNING `+bookColumns,
			bookID, tid, next.VersionNo))
		if err != nil {
			return err
		}
		cur.ValidTo = fmtDate(from)
		out = &registry.BookOutcome{Book: *nb, Basis: *next}
		rec, err := events.BookBasisChangedRecord(*nb, *cur, *next, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ChangeBookStatus activates a book, suspends its posting or retires it.
func (s *PgStore) ChangeBookStatus(ctx context.Context, bookID, command, to string, req domain.BookStatusRequest, actor string, allowUnversioned bool) (*registry.BookOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.BookOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		// Two activations of parallel books of one entity and type are serialised, so the one-live-per-framework check
		// cannot be raced past.
		pre, err := scanBook(tx.QueryRow(ctx, `SELECT `+bookColumns+` FROM accounting_books WHERE book_id = $1 AND tenant_id = $2`, bookID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if to == domain.BookActive && pre.LegalEntityID != "" {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('book-activate:' || $1::text || ':' || $2 || ':' || $3, 0))`,
				tid, pre.LegalEntityID, string(pre.BookType)); err != nil {
				return err
			}
		}
		b, err := lockBook(ctx, tx, tid, bookID)
		if err != nil {
			return err
		}
		if err := checkRecordVersion(req.ExpectedVersion, b.RecordVersion, allowUnversioned, "book"); err != nil {
			return err
		}
		if !domain.BookCanMoveTo(b.Status, to) {
			return fmt.Errorf("%w: a %s book cannot move to %s", registry.ErrStateConflict, b.Status, to)
		}
		basis, err := currentBasis(ctx, tx, bookID)
		if err != nil {
			return err
		}
		if to == domain.BookActive {
			// s4.16: the creator of a material statutory-basis book cannot self-approve its activation.
			if b.Material && actor == b.CreatedBy {
				return fmt.Errorf("%w: a statutory, tax or regulatory book is activated by someone other than the principal who created it", registry.ErrSelfApproval)
			}
			// Parallel books stay separate: a second live book of one entity, type and framework would make "the" book ambiguous.
			if b.Material && b.LegalEntityID != "" {
				var clash string
				err := tx.QueryRow(ctx, `SELECT o.book_code FROM accounting_books o JOIN book_basis_versions v ON v.book_id = o.book_id AND v.valid_to IS NULL
					WHERE o.tenant_id = $1 AND o.legal_entity_id = $2::uuid AND o.book_type = $3 AND o.book_id <> $4
					  AND o.status IN ('ACTIVE','POSTING_SUSPENDED') AND v.framework = $5 LIMIT 1`,
					tid, b.LegalEntityID, string(b.BookType), bookID, string(basis.Framework)).Scan(&clash)
				if err == nil {
					return fmt.Errorf("%w: book %s is already a live %s book of this entity on %s; a parallel book differs in framework",
						registry.ErrConflict, clash, b.BookType, basis.Framework)
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
		}
		eff := asOfDate(time.Now().UTC())
		if req.EffectiveDate != "" {
			eff, _ = domain.ParseDate(req.EffectiveDate)
		}
		var lastSeq int
		var lastEff time.Time
		if err := tx.QueryRow(ctx, `SELECT seq, effective_date FROM book_transitions WHERE book_id = $1 ORDER BY seq DESC LIMIT 1`, bookID).Scan(&lastSeq, &lastEff); err != nil {
			return err
		}
		if eff.Before(lastEff) {
			return fmt.Errorf("%w: effective_date is before the last change, on %s", registry.ErrInvalidInput, fmtDate(lastEff))
		}
		nb, err := scanBook(tx.QueryRow(ctx, `UPDATE accounting_books SET status = $3::text, record_version = record_version + 1,
			activated_by = CASE WHEN $3::text = 'ACTIVE' THEN $4::text ELSE activated_by END WHERE book_id = $1 AND tenant_id = $2 RETURNING `+bookColumns,
			bookID, tid, to, actor))
		if err != nil {
			return err
		}
		prev, before := b.Status, b.RecordVersion
		t, err := insertBookTransition(ctx, tx, tid, bookID, lastSeq+1, &prev, to, eff, command, strings.TrimSpace(req.Reason), strings.TrimSpace(req.EvidenceRef),
			actor, &before, nb.RecordVersion, req.CorrelationID)
		if err != nil {
			return err
		}
		out = &registry.BookOutcome{Book: *nb, Basis: *basis, Transition: t}
		rec, err := events.BookStatusRecord(*nb, *basis, *t, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// GetBook returns a book's identity.
func (s *PgStore) GetBook(ctx context.Context, bookID string) (*domain.AccountingBook, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(bookID); err != nil {
		return nil, nil
	}
	var out *domain.AccountingBook
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		b, err := scanBook(tx.QueryRow(ctx, `SELECT `+bookColumns+` FROM accounting_books WHERE book_id = $1 AND tenant_id = $2`, bookID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = b
		return err
	})
	return out, err
}

func basisInForce(ctx context.Context, tx pgx.Tx, bookID string, day time.Time) (*domain.BookBasisVersion, error) {
	v, err := scanBasis(tx.QueryRow(ctx, `SELECT `+basisColumns+` FROM book_basis_versions WHERE book_id = $1 AND valid_from <= $2
		AND (valid_to IS NULL OR valid_to > $2) ORDER BY version_no DESC LIMIT 1`, bookID, day))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// GetBookView returns the book as it was on asOf.
func (s *PgStore) GetBookView(ctx context.Context, bookID string, asOf time.Time, history bool) (*domain.BookView, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(bookID); err != nil {
		return nil, nil
	}
	var out *domain.BookView
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		b, err := scanBook(tx.QueryRow(ctx, `SELECT `+bookColumns+` FROM accounting_books WHERE book_id = $1 AND tenant_id = $2`, bookID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		day := asOfDate(asOf)
		view := &domain.BookView{Book: *b, AsOf: fmtDate(day)}
		if view.Basis, err = basisInForce(ctx, tx, bookID, day); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT to_status FROM book_transitions WHERE book_id = $1 AND effective_date <= $2 ORDER BY effective_date DESC, seq DESC LIMIT 1`, bookID, day).Scan(&view.StatusAsOf)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		view.Eligibility = domain.EvaluatePosting(bookID, day, view.StatusAsOf, view.Basis)
		if history {
			rows, err := tx.Query(ctx, `SELECT `+bookTransitionColumns+` FROM book_transitions WHERE book_id = $1 ORDER BY seq`, bookID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				t, err := scanBookTransition(rows)
				if err != nil {
					return err
				}
				view.History = append(view.History, *t)
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		out = view
		return nil
	})
	return out, err
}

// ListBooks lists an entity's or a group's books.
func (s *PgStore) ListBooks(ctx context.Context, q domain.BookSearchQuery) ([]domain.AccountingBook, error) {
	tid := domain.TenantFromContext(ctx)
	out := []domain.AccountingBook{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if q.LegalEntityID != "" {
			if err := entityExists(ctx, tx, tid, q.LegalEntityID); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT `+bookColumns+` FROM accounting_books WHERE tenant_id = $1
			AND ($2::text = '' OR legal_entity_id::text = $2::text) AND ($3::text = '' OR group_code = $3::text)
			AND ($4::text = '' OR book_type = $4::text) AND ($5::text = '' OR status = $5::text) ORDER BY book_code, book_id`,
			tid, q.LegalEntityID, q.GroupCode, string(q.Type), q.Status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBook(rows)
			if err != nil {
				return err
			}
			out = append(out, *b)
		}
		return rows.Err()
	})
	return out, err
}

// ExplainBookBasis returns the basis in force on asOf, every version, and how the book differs from its siblings.
func (s *PgStore) ExplainBookBasis(ctx context.Context, bookID string, asOf time.Time) (*domain.BookBasisExplanation, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(bookID); err != nil {
		return nil, nil
	}
	var out *domain.BookBasisExplanation
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		b, err := scanBook(tx.QueryRow(ctx, `SELECT `+bookColumns+` FROM accounting_books WHERE book_id = $1 AND tenant_id = $2`, bookID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		day := asOfDate(asOf)
		ex := &domain.BookBasisExplanation{Book: *b, AsOf: fmtDate(day), AllowedFrameworks: domain.AllowedFrameworks(b.BookType),
			Versions: []domain.BookBasisVersion{}, ParallelBooks: []domain.ParallelBook{}}
		if ex.InForce, err = basisInForce(ctx, tx, bookID, day); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+basisColumns+` FROM book_basis_versions WHERE book_id = $1 ORDER BY version_no`, bookID)
		if err != nil {
			return err
		}
		for rows.Next() {
			v, err := scanBasis(rows)
			if err != nil {
				rows.Close()
				return err
			}
			ex.Versions = append(ex.Versions, *v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if b.LegalEntityID != "" {
			var cur string
			if err := tx.QueryRow(ctx, `SELECT default_currency_code FROM legal_entities WHERE legal_entity_id = $1 AND tenant_id = $2`, b.LegalEntityID, tid).Scan(&cur); err != nil {
				return err
			}
			ex.EntityDefaultCurrency = cur
			if ex.InForce != nil {
				m := ex.InForce.FunctionalCurrency == cur
				ex.FunctionalMatchesEntity = &m
			}
			prows, err := tx.Query(ctx, `SELECT o.book_id::text, o.book_code, o.book_type, o.status, COALESCE(v.framework,'')
				FROM accounting_books o LEFT JOIN book_basis_versions v ON v.book_id = o.book_id AND v.valid_to IS NULL
				WHERE o.tenant_id = $1 AND o.legal_entity_id = $2::uuid AND o.book_id <> $3 ORDER BY o.book_code`, tid, b.LegalEntityID, bookID)
			if err != nil {
				return err
			}
			defer prows.Close()
			for prows.Next() {
				var pb domain.ParallelBook
				if err := prows.Scan(&pb.BookID, &pb.BookCode, &pb.BookType, &pb.Status, &pb.Framework); err != nil {
					return err
				}
				ex.ParallelBooks = append(ex.ParallelBooks, pb)
			}
			if err := prows.Err(); err != nil {
				return err
			}
		}
		ex.Notes = []string{
			"The framework and the functional and presentation currencies are versioned attributes of this book; a change is prospective and never rewrites how earlier periods were booked.",
			"A book is separate from every other book of the entity: statutory, management, tax, consolidation and regulatory books are never conflated, even where their charts overlap.",
			"The book states a basis. It does not hold ledger lines or balances (ACC-05) or accounting policy content (REF-07).",
		}
		out = ex
		return nil
	})
	return out, err
}
