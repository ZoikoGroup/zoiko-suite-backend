package store

// REF-10 Payment Terms (migration 000031): terms, their immutable versions, the
// lifecycle Draft -> Active -> Superseded/Retired, and the lookup of the version
// that governs a date.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

const ptvCols = `v.term_version_id::text, t.term_code, v.version, v.status, v.basis_date_rule, v.due_method, v.due_days, v.due_day_of_month,
	v.business_day_convention, COALESCE(v.calendar_code, ''), v.grace_days, COALESCE(v.discount_percent::text, ''), v.discount_days,
	v.controlled_credit, v.rationale, COALESCE(v.source_ref, ''), to_char(v.effective_from, 'YYYY-MM-DD'), to_char(v.ends_on, 'YYYY-MM-DD'),
	v.formula_text, v.content_digest, v.created_at, v.created_by, v.approved_at, v.approved_by, v.superseded_at,
	v.superseded_by_version_id::text, v.retired_at, v.retired_by, v.retire_reason`

const ptvFrom = ` FROM payment_term_versions v JOIN payment_terms t USING (term_id) `

func scanPaymentTermVersion(row pgx.Row) (*domain.PaymentTermVersion, error) {
	var v domain.PaymentTermVersion
	if err := row.Scan(&v.TermVersionID, &v.TermCode, &v.Version, &v.Status, &v.BasisDateRule, &v.DueMethod, &v.DueDays, &v.DueDayOfMonth,
		&v.BusinessDayConvention, &v.CalendarCode, &v.GraceDays, &v.DiscountPercent, &v.DiscountDays,
		&v.ControlledCredit, &v.Rationale, &v.SourceRef, &v.EffectiveFrom, &v.EndsOn,
		&v.FormulaText, &v.ContentDigest, &v.CreatedAt, &v.CreatedBy, &v.ApprovedAt, &v.ApprovedBy, &v.SupersededAt,
		&v.SupersededByVersionID, &v.RetiredAt, &v.RetiredBy, &v.RetireReason); err != nil {
		return nil, err
	}
	v.DiscountPercent = domain.NormalisePercent(v.DiscountPercent)
	return &v, nil
}

func (s *PgStore) loadPaymentTermVersion(ctx context.Context, q querier, lock bool, where string, args ...any) (*domain.PaymentTermVersion, error) {
	query := `SELECT ` + ptvCols + ptvFrom + `WHERE ` + where
	if lock {
		query += ` FOR UPDATE OF v`
	}
	return scanPaymentTermVersion(q.QueryRow(ctx, query, args...))
}

func (s *PgStore) termFail(op string, err error) error {
	if isExclusionViolation(err) {
		return domain.ErrPaymentTermOverlap
	}
	return s.registryFail(op, err, nil)
}

// CreatePaymentTerm registers a term, or replays it when the code already names a
// term with the same name and jurisdiction.
func (s *PgStore) CreatePaymentTerm(ctx context.Context, code, name string, jurisdictionID *string, actor string) (*domain.PaymentTerm, bool, error) {
	name = strings.TrimSpace(name)
	switch {
	case !domain.ValidTermCode(code):
		return nil, false, domain.ErrInvalidPaymentTerm
	case name == "" || len(name) > 512 || strings.TrimSpace(actor) == "":
		return nil, false, domain.ErrInvalidPaymentTerm
	}
	if jurisdictionID != nil {
		if _, err := s.FindByIDAny(ctx, *jurisdictionID); err != nil {
			if errors.Is(err, domain.ErrJurisdictionNotFound) {
				return nil, false, domain.ErrInvalidReference
			}
			return nil, false, err
		}
	}
	const cols = `term_id::text, term_code, term_name, jurisdiction_id::text, created_at, created_by`
	var t domain.PaymentTerm
	err := s.pool.QueryRow(ctx, `
		INSERT INTO payment_terms (term_code, term_name, jurisdiction_id, created_by) VALUES ($1, $2, $3::uuid, $4)
		ON CONFLICT (term_code) DO NOTHING RETURNING `+cols, code, name, jurisdictionID, actor).
		Scan(&t.TermID, &t.TermCode, &t.TermName, &t.JurisdictionID, &t.CreatedAt, &t.CreatedBy)
	if err == nil {
		return &t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CreatePaymentTerm", err, nil)
	}
	existing, err := s.GetPaymentTerm(ctx, code)
	if err != nil {
		return nil, false, err
	}
	if existing.TermName != name || derefStr(existing.JurisdictionID) != derefStr(jurisdictionID) {
		return nil, false, domain.ErrConflict
	}
	return existing, false, nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// GetPaymentTerm returns a term by its code.
func (s *PgStore) GetPaymentTerm(ctx context.Context, code string) (*domain.PaymentTerm, error) {
	var t domain.PaymentTerm
	err := s.pool.QueryRow(ctx, `SELECT term_id::text, term_code, term_name, jurisdiction_id::text, created_at, created_by FROM payment_terms WHERE term_code = $1`, code).
		Scan(&t.TermID, &t.TermCode, &t.TermName, &t.JurisdictionID, &t.CreatedAt, &t.CreatedBy)
	if err != nil {
		return nil, s.registryFail("GetPaymentTerm", err, domain.ErrPaymentTermNotFound)
	}
	return &t, nil
}

// GetPaymentTermVersion returns one version of a term.
func (s *PgStore) GetPaymentTermVersion(ctx context.Context, code string, version int) (*domain.PaymentTermVersion, error) {
	if _, err := s.GetPaymentTerm(ctx, code); err != nil {
		return nil, err
	}
	v, err := s.loadPaymentTermVersion(ctx, s.pool, false, `t.term_code = $1 AND v.version = $2`, code, version)
	if err != nil {
		return nil, s.registryFail("GetPaymentTermVersion", err, domain.ErrPaymentTermVersionNotFound)
	}
	return v, nil
}

// ListPaymentTermVersions returns every version of a term, oldest first.
func (s *PgStore) ListPaymentTermVersions(ctx context.Context, code string) ([]*domain.PaymentTermVersion, error) {
	if _, err := s.GetPaymentTerm(ctx, code); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+ptvCols+ptvFrom+`WHERE t.term_code = $1 ORDER BY v.version`, code)
	if err != nil {
		return nil, s.registryFail("ListPaymentTermVersions", err, nil)
	}
	defer rows.Close()
	out := []*domain.PaymentTermVersion{}
	for rows.Next() {
		v, err := scanPaymentTermVersion(rows)
		if err != nil {
			return nil, s.registryFail("ListPaymentTermVersions scan", err, nil)
		}
		out = append(out, v)
	}
	return out, s.registryFail("ListPaymentTermVersions rows", rows.Err(), nil)
}

// CreatePaymentTermVersion drafts the next version of a term. The number, the
// formula in words and the content digest are assigned here, never by the caller.
func (s *PgStore) CreatePaymentTermVersion(ctx context.Context, code string, in domain.PaymentTermVersionInput, actor string) (*domain.PaymentTermVersion, error) {
	if problems := in.Validate(); len(problems) > 0 {
		return nil, &PaymentTermInvalid{Problems: problems}
	}
	if strings.TrimSpace(actor) == "" {
		return nil, domain.ErrInvalidPaymentTerm
	}
	in.Rationale = strings.TrimSpace(in.Rationale)
	// "2", "2.0" and "2.0000" are one discount: the stored formula and digest agree.
	in.DiscountPercent = domain.NormalisePercent(in.DiscountPercent)
	tx, err := s.begin(ctx, "CreatePaymentTermVersion")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var termID string
	if err := tx.QueryRow(ctx, `SELECT term_id::text FROM payment_terms WHERE term_code = $1 FOR UPDATE`, code).Scan(&termID); err != nil {
		return nil, s.registryFail("CreatePaymentTermVersion lock", err, domain.ErrPaymentTermNotFound)
	}
	if in.CalendarCode != "" {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM regulatory_calendars WHERE calendar_code = $1)`, in.CalendarCode).Scan(&exists); err != nil {
			return nil, s.registryFail("CreatePaymentTermVersion calendar", err, nil)
		}
		if !exists {
			return nil, domain.ErrCalendarNotFound
		}
	}
	var discount *string
	if in.DiscountPercent != "" {
		discount = &in.DiscountPercent
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO payment_term_versions
			(term_id, version, basis_date_rule, due_method, due_days, due_day_of_month, business_day_convention, calendar_code,
			 grace_days, discount_percent, discount_days, controlled_credit, rationale, source_ref, formula_text, content_digest,
			 effective_from, created_by)
		VALUES ($1::uuid, COALESCE((SELECT MAX(version) FROM payment_term_versions WHERE term_id = $1::uuid), 0) + 1,
		        $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11, $12, $13, $14, $15, $16::date, $17)
		RETURNING term_version_id::text`,
		termID, in.BasisDateRule, in.DueMethod, in.DueDays, in.DueDayOfMonth, in.BusinessDayConvention, nilIfBlank(in.CalendarCode),
		in.GraceDays, discount, in.DiscountDays, in.ControlledCredit, in.Rationale, nilIfBlank(in.SourceRef), in.FormulaText(), in.ContentDigest(code),
		in.EffectiveFrom, actor).Scan(&id); err != nil {
		return nil, s.registryFail("CreatePaymentTermVersion insert", err, nil)
	}
	rec, err := s.loadPaymentTermVersion(ctx, tx, false, `v.term_version_id::text = $1`, id)
	if err != nil {
		return nil, s.registryFail("CreatePaymentTermVersion read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("CreatePaymentTermVersion commit", err, nil)
	}
	return rec, nil
}

// PaymentTermInvalid lists everything wrong with a drafted term.
type PaymentTermInvalid struct{ Problems []string }

func (e *PaymentTermInvalid) Error() string {
	return domain.ErrInvalidPaymentTerm.Error() + ": " + strings.Join(e.Problems, "; ")
}

func (e *PaymentTermInvalid) Is(target error) bool { return target == domain.ErrInvalidPaymentTerm }

// lockedVersion locks a term and loads one of its versions for update.
func (s *PgStore) lockedVersion(ctx context.Context, tx pgx.Tx, code string, version int) (*domain.PaymentTermVersion, error) {
	var termID string
	if err := tx.QueryRow(ctx, `SELECT term_id::text FROM payment_terms WHERE term_code = $1 FOR UPDATE`, code).Scan(&termID); err != nil {
		return nil, s.registryFail("lock payment term", err, domain.ErrPaymentTermNotFound)
	}
	v, err := s.loadPaymentTermVersion(ctx, tx, true, `v.term_id = $1::uuid AND v.version = $2`, termID, version)
	if err != nil {
		return nil, s.registryFail("load payment term version", err, domain.ErrPaymentTermVersionNotFound)
	}
	return v, nil
}

// calendarReady refuses to put a term into force by a calendar that has no
// published version: every due date it gave would be refused.
func (s *PgStore) calendarReady(ctx context.Context, tx pgx.Tx, calendarCode string) error {
	if calendarCode == "" {
		return nil
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id)
		WHERE c.calendar_code = $1 AND v.status = 'PUBLISHED')`, calendarCode).Scan(&ok); err != nil {
		return s.registryFail("calendar ready", err, nil)
	}
	if !ok {
		return domain.ErrCalendarNotReady
	}
	return nil
}

func (s *PgStore) activate(ctx context.Context, tx pgx.Tx, id, actor string) error {
	_, err := tx.Exec(ctx, `UPDATE payment_term_versions SET status = 'ACTIVE', approved_at = NOW(), approved_by = $2 WHERE term_version_id = $1::uuid`, id, actor)
	return err
}

// ApprovePaymentTermVersion approves a DRAFT version, which puts it in force from
// its effective_from. The approver must not be the author. It reports whether
// this call approved it (false: it was already active).
//
// A version cannot be approved over another approved version of the same term
// that governs any of its period: replacing a term in force is
// SupersedePaymentTermVersion, which names the version it replaces.
func (s *PgStore) ApprovePaymentTermVersion(ctx context.Context, code string, version int, actor string) (*domain.PaymentTermVersion, bool, error) {
	tx, err := s.begin(ctx, "ApprovePaymentTermVersion")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockedVersion(ctx, tx, code, version)
	if err != nil {
		return nil, false, err
	}
	switch v.Status {
	case domain.TermActive:
		return v, false, nil
	case domain.TermDraft:
	default:
		return nil, false, domain.ErrPaymentTermNotDraft
	}
	if v.CreatedBy == actor {
		return nil, false, domain.ErrNotIndependent
	}
	if err := s.calendarReady(ctx, tx, v.CalendarCode); err != nil {
		return nil, false, err
	}
	if err := s.activate(ctx, tx, v.TermVersionID, actor); err != nil {
		return nil, false, s.termFail("ApprovePaymentTermVersion", err)
	}
	out, err := s.loadPaymentTermVersion(ctx, tx, false, `v.term_version_id::text = $1`, v.TermVersionID)
	if err != nil {
		return nil, false, s.registryFail("ApprovePaymentTermVersion read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("ApprovePaymentTermVersion commit", err, nil)
	}
	return out, true, nil
}

// SupersedePaymentTermVersion approves a DRAFT version over the ACTIVE version it
// names, in one transaction. The replaced version stops governing on the new
// one's effective_from, so every date before it keeps the replaced version's
// rule. It returns the new version, the replaced one, and whether this call did
// it (false: it had already been done).
func (s *PgStore) SupersedePaymentTermVersion(ctx context.Context, code string, version, supersedes int, actor string) (*domain.PaymentTermVersion, *domain.PaymentTermVersion, bool, error) {
	tx, err := s.begin(ctx, "SupersedePaymentTermVersion")
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	next, err := s.lockedVersion(ctx, tx, code, version)
	if err != nil {
		return nil, nil, false, err
	}
	old, err := s.loadPaymentTermVersion(ctx, tx, true, `t.term_code = $1 AND v.version = $2`, code, supersedes)
	if err != nil {
		return nil, nil, false, s.registryFail("SupersedePaymentTermVersion old", err, domain.ErrPaymentTermVersionNotFound)
	}
	if next.TermVersionID == old.TermVersionID {
		return nil, nil, false, domain.ErrInvalidSupersession
	}
	// Already done: the new version is active and the old one was superseded by it.
	if next.Status == domain.TermActive && old.Status == domain.TermSuperseded && old.SupersededByVersionID != nil && *old.SupersededByVersionID == next.TermVersionID {
		return next, old, false, nil
	}
	if next.Status != domain.TermDraft {
		return nil, nil, false, domain.ErrPaymentTermNotDraft
	}
	if old.Status != domain.TermActive {
		return nil, nil, false, domain.ErrPaymentTermNotActive
	}
	if next.CreatedBy == actor {
		return nil, nil, false, domain.ErrNotIndependent
	}
	from, _ := time.Parse("2006-01-02", next.EffectiveFrom)
	oldFrom, _ := time.Parse("2006-01-02", old.EffectiveFrom)
	if !from.After(oldFrom) {
		return nil, nil, false, domain.ErrInvalidSupersession
	}
	if old.EndsOn != nil {
		if oldEnd, _ := time.Parse("2006-01-02", *old.EndsOn); from.After(oldEnd) {
			return nil, nil, false, domain.ErrInvalidSupersession
		}
	}
	if err := s.calendarReady(ctx, tx, next.CalendarCode); err != nil {
		return nil, nil, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_term_versions SET status = 'SUPERSEDED', superseded_at = NOW(), superseded_by_version_id = $2::uuid, ends_on = $3::date
		WHERE term_version_id = $1::uuid`, old.TermVersionID, next.TermVersionID, next.EffectiveFrom); err != nil {
		return nil, nil, false, s.termFail("SupersedePaymentTermVersion old", err)
	}
	if err := s.activate(ctx, tx, next.TermVersionID, actor); err != nil {
		return nil, nil, false, s.termFail("SupersedePaymentTermVersion new", err)
	}
	newRec, err := s.loadPaymentTermVersion(ctx, tx, false, `v.term_version_id::text = $1`, next.TermVersionID)
	if err != nil {
		return nil, nil, false, s.registryFail("SupersedePaymentTermVersion read", err, nil)
	}
	oldRec, err := s.loadPaymentTermVersion(ctx, tx, false, `v.term_version_id::text = $1`, old.TermVersionID)
	if err != nil {
		return nil, nil, false, s.registryFail("SupersedePaymentTermVersion read old", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, s.registryFail("SupersedePaymentTermVersion commit", err, nil)
	}
	return newRec, oldRec, true, nil
}

// RetirePaymentTermVersion withdraws a version. A DRAFT is simply withdrawn. An
// ACTIVE version stops governing on endsOn (default today, which must be after
// its effective_from), so dates before it keep the rule. A superseded version is
// already over. It reports whether this call retired it.
func (s *PgStore) RetirePaymentTermVersion(ctx context.Context, code string, version int, reason string, endsOn *string, actor string) (*domain.PaymentTermVersion, bool, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 2000 || strings.TrimSpace(actor) == "" {
		return nil, false, domain.ErrInvalidPaymentTerm
	}
	tx, err := s.begin(ctx, "RetirePaymentTermVersion")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockedVersion(ctx, tx, code, version)
	if err != nil {
		return nil, false, err
	}
	switch v.Status {
	case domain.TermRetired:
		return v, false, nil
	case domain.TermSuperseded:
		return nil, false, domain.ErrPaymentTermNotActive
	case domain.TermDraft:
		if _, err := tx.Exec(ctx, `UPDATE payment_term_versions SET status = 'RETIRED', retired_at = NOW(), retired_by = $2, retire_reason = $3 WHERE term_version_id = $1::uuid`,
			v.TermVersionID, actor, reason); err != nil {
			return nil, false, s.termFail("RetirePaymentTermVersion draft", err)
		}
	default: // ACTIVE
		end := time.Now().UTC().Format("2006-01-02")
		if endsOn != nil {
			if _, err := time.Parse("2006-01-02", *endsOn); err != nil {
				return nil, false, domain.ErrInvalidPaymentTerm
			}
			end = *endsOn
		}
		if end <= v.EffectiveFrom {
			return nil, false, &PaymentTermInvalid{Problems: []string{"ends_on must be after the version's effective_from, " + v.EffectiveFrom + ": it stops governing from that day"}}
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_term_versions SET status = 'RETIRED', retired_at = NOW(), retired_by = $2, retire_reason = $3, ends_on = $4::date
			WHERE term_version_id = $1::uuid`, v.TermVersionID, actor, reason, end); err != nil {
			return nil, false, s.termFail("RetirePaymentTermVersion active", err)
		}
	}
	out, err := s.loadPaymentTermVersion(ctx, tx, false, `v.term_version_id::text = $1`, v.TermVersionID)
	if err != nil {
		return nil, false, s.registryFail("RetirePaymentTermVersion read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("RetirePaymentTermVersion commit", err, nil)
	}
	return out, true, nil
}

// ResolvePaymentTermVersion is the approved version of a term that governs a date.
//
// Without a pin it is the version whose period holds the date and that was
// already approved at knownAt (default now). A version that stopped governing
// only AFTER knownAt is still open as known then, so a due date asked as it was
// known then is never changed by a later supersession or retirement. With a pin
// it is that version, once approved, whatever its period, so a calculation can be
// reproduced exactly. The bool is whether the version was pinned.
func (s *PgStore) ResolvePaymentTermVersion(ctx context.Context, code, on string, knownAt *time.Time, pin *int) (*domain.PaymentTermVersion, bool, error) {
	if _, err := s.GetPaymentTerm(ctx, code); err != nil {
		return nil, false, err
	}
	known := time.Now().UTC()
	if knownAt != nil {
		known = *knownAt
	}
	var v *domain.PaymentTermVersion
	var err error
	if pin != nil {
		v, err = s.loadPaymentTermVersion(ctx, s.pool, false,
			`t.term_code = $1 AND v.version = $2 AND v.approved_at IS NOT NULL AND v.approved_at <= $3`, code, *pin, known)
	} else {
		v, err = s.loadPaymentTermVersion(ctx, s.pool, false, `t.term_code = $1 AND v.approved_at IS NOT NULL AND v.approved_at <= $2
			AND v.effective_from <= $3::date
			AND (v.ends_on IS NULL OR $3::date < v.ends_on OR COALESCE(v.superseded_at, v.retired_at) > $2)
			ORDER BY v.version DESC LIMIT 1`, code, known, on)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, domain.ErrNoPaymentTermVersion
	}
	if err != nil {
		return nil, false, s.registryFail("ResolvePaymentTermVersion", err, nil)
	}
	return v, pin != nil, nil
}
