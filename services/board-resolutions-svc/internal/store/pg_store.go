package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/board-resolutions-svc/internal/domain"
	"zoiko.io/board-resolutions-svc/internal/middleware"
)

type Store interface {
	CreateMeeting(ctx context.Context, m *domain.BoardMeeting) error
	GetMeeting(ctx context.Context, id string) (*domain.BoardMeeting, error)
	ListMeetings(ctx context.Context, f domain.MeetingFilter) ([]domain.BoardMeeting, error)

	CreateResolution(ctx context.Context, r *domain.BoardResolution) error
	GetResolution(ctx context.Context, id string) (*domain.BoardResolution, error)
	ListResolutions(ctx context.Context, f domain.ResolutionFilter) ([]domain.BoardResolution, error)
	RecordVotes(ctx context.Context, id string, req *domain.RecordVotesRequest) (*domain.BoardResolution, error)
	// PassResolution finalizes the resolution as PASSED, attributed to
	// passedBy — the authenticated principal, established by the handler, not
	// a name carried in the request body.
	PassResolution(ctx context.Context, id, passedBy string, req *domain.PassResolutionRequest) (*domain.BoardResolution, error)
}

// effectiveDateColumns is the SELECT fragment for the two effective-date
// columns, which MUST be read as text.
//
// effective_from/effective_to are DATE columns, but domain.BoardMeeting and
// domain.BoardResolution declare them as string / *string because the API
// contract is a plain "YYYY-MM-DD", not an RFC3339 timestamp. pgx happily
// ENCODEs a Go string into a DATE parameter on write (INSERT/RETURNING
// worked), but cannot DECODE a DATE into a *string on a plain SELECT — every
// read here failed with a scan error, surfaced generically as "failed to
// get resolution"/"failed to get meeting" with no logged detail (same
// asymmetric read/write bug already fixed in contract-lifecycle-svc's
// store).
//
// TO_CHAR rather than ::TEXT so the format does not depend on the session's
// DateStyle. NULL effective_to passes through as NULL.
const effectiveDateColumns = `TO_CHAR(effective_from, 'YYYY-MM-DD'), TO_CHAR(effective_to, 'YYYY-MM-DD')`

const meetingColumns = `meeting_id, tenant_id, legal_entity_id, title, scheduled_at, COALESCE(location,''), status,
	       COALESCE(minutes_summary,''), ` + effectiveDateColumns + `, created_by, created_at, updated_at`

const resolutionColumns = `resolution_id, meeting_id, tenant_id, legal_entity_id, resolution_number, title, content, category,
	       status, votes_for, votes_against, abstentions, passed_at, passed_by, document_vault_id,
	       ` + effectiveDateColumns + `, created_by, created_at, updated_at`

// mapPgError translates the Postgres failures that are really caller mistakes
// into domain errors, so they stop arriving at the handler as a generic
// "failed to …" 500.
//
// 22007/22008 are what a malformed effective_from ("next tuesday") does to a
// DATE column: it dies inside the driver before any row is written, and used
// to answer 500 — an outage status for a bad date in a form.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "22007", "22008", "22P02":
		return domain.ErrInvalidField
	}
	return err
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// setRLS installs the caller's tenant for the transaction and returns it, so
// callers add the explicit predicate below from the same value rather than
// reading the context twice.
//
// This was `fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID)` — the
// tenant id arrives on a request header, so it was raw caller input
// interpolated into SQL. `X-Tenant-Id: x'; ALTER TABLE board_resolutions
// DISABLE ROW LEVEL SECURITY; --` ran as written, on the statement whose whole
// job is enforcing tenant isolation. set_config() takes it as a parameter, so
// there is no longer a string being assembled at all.
func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) (string, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return "", err
	}
	return tenantID, nil
}

// tenantOf is the explicit predicate every query carries in addition to RLS,
// and it refuses an empty tenant rather than defaulting one.
//
// Relying on the policy alone was a single point of failure with no defence in
// depth: these services connect as the database OWNER, and Postgres exempts
// the owner from row-level security unless the table is declared FORCE ROW
// LEVEL SECURITY — which this schema did not. So `WHERE resolution_id = $1`
// with no tenant predicate read across every tenant in the database, and the
// policy that was supposed to stop it never applied to this connection at all.
// Migration 000002 adds FORCE; these predicates mean the isolation does not
// depend on it having been applied.
//
// An empty tenant is refused for a related reason: under RLS alone the
// predicate would simply match nothing, so every query would return zero rows
// and read as "this tenant has no board resolutions" rather than as the
// missing scope it is.
func tenantOf(ctx context.Context) (string, error) {
	tenantID := middleware.GetTenantID(ctx)
	if tenantID == "" {
		return "", domain.ErrTenantMissing
	}
	return tenantID, nil
}

// pageClause appends the LIMIT/OFFSET for a register read.
//
// Both lists were unbounded: every meeting and every resolution a tenant had
// ever recorded, in one response, growing forever. The ORDER BY carries the
// primary key as a tiebreaker because the sort columns are not unique — two
// resolutions created in the same transaction share created_at, and Postgres
// is free to order them differently between queries, so a paged read could
// show one row twice and skip another.
func pageClause(args []any, limit, offset int) ([]any, string) {
	args = append(args, limit)
	clause := fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, offset)
	clause += fmt.Sprintf(" OFFSET $%d", len(args))
	return args, clause
}

func (s *PgStore) CreateMeeting(ctx context.Context, m *domain.BoardMeeting) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	if m.MeetingID == "" {
		m.MeetingID = "mtg-" + uuid.New().String()
	}
	m.TenantID = tenantID
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	if m.Status == "" {
		m.Status = domain.MeetingStatusScheduled
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO board_meetings
			(meeting_id, tenant_id, legal_entity_id, title, scheduled_at, location, status,
			 minutes_summary, effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		m.MeetingID, m.TenantID, m.LegalEntityID, m.Title, m.ScheduledAt, m.Location, string(m.Status),
		m.MinutesSummary, m.EffectiveFrom, m.EffectiveTo, m.CreatedBy, m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert meeting: %w", mapPgError(err))
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetMeeting(ctx context.Context, id string) (*domain.BoardMeeting, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	var m domain.BoardMeeting
	var status string
	err = tx.QueryRow(ctx, `
		SELECT `+meetingColumns+`
		FROM board_meetings WHERE meeting_id = $1 AND tenant_id = $2`, id, tenantID,
	).Scan(
		&m.MeetingID, &m.TenantID, &m.LegalEntityID, &m.Title, &m.ScheduledAt, &m.Location, &status,
		&m.MinutesSummary, &m.EffectiveFrom, &m.EffectiveTo, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrMeetingNotFound
		}
		return nil, mapPgError(err)
	}
	m.Status = domain.MeetingStatus(status)
	_ = tx.Commit(ctx)
	return &m, nil
}

func (s *PgStore) ListMeetings(ctx context.Context, f domain.MeetingFilter) ([]domain.BoardMeeting, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	args := []any{tenantID, f.LegalEntityID}
	args, page := pageClause(args, f.Limit, f.Offset)
	rows, err := tx.Query(ctx, `
		SELECT `+meetingColumns+`
		FROM board_meetings
		WHERE tenant_id = $1
		  AND ($2 = '' OR legal_entity_id = $2)
		ORDER BY scheduled_at DESC, meeting_id DESC`+page, args...,
	)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()

	var out []domain.BoardMeeting
	for rows.Next() {
		var m domain.BoardMeeting
		var status string
		if err := rows.Scan(
			&m.MeetingID, &m.TenantID, &m.LegalEntityID, &m.Title, &m.ScheduledAt, &m.Location, &status,
			&m.MinutesSummary, &m.EffectiveFrom, &m.EffectiveTo, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, err
		}
		m.Status = domain.MeetingStatus(status)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) CreateResolution(ctx context.Context, r *domain.BoardResolution) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	if r.ResolutionID == "" {
		r.ResolutionID = "res-" + uuid.New().String()
	}
	r.TenantID = tenantID
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	if r.Status == "" {
		r.Status = domain.ResolutionStatusProposed
	}

	// A resolution may cite a meeting, but only one of this tenant's meetings.
	// meeting_id was written verbatim with no check at all, so a resolution
	// could be filed against a meeting id that belonged to another tenant or
	// to nothing — and then listing that meeting's resolutions silently
	// omitted it, because the join it implies was never real.
	if r.MeetingID != "" {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM board_meetings WHERE meeting_id = $1 AND tenant_id = $2)`,
			r.MeetingID, tenantID,
		).Scan(&exists); err != nil {
			return mapPgError(err)
		}
		if !exists {
			return domain.ErrMeetingNotFound
		}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO board_resolutions
			(resolution_id, meeting_id, tenant_id, legal_entity_id, resolution_number, title, content, category,
			 status, votes_for, votes_against, abstentions, effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		r.ResolutionID, r.MeetingID, r.TenantID, r.LegalEntityID, r.ResolutionNumber, r.Title, r.Content,
		string(r.Category), string(r.Status), r.VotesFor, r.VotesAgainst, r.Abstentions,
		r.EffectiveFrom, r.EffectiveTo, r.CreatedBy, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert resolution: %w", mapPgError(err))
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetResolution(ctx context.Context, id string) (*domain.BoardResolution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	r, err := scanResolution(ctx, tx, id, tenantID, false)
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return r, nil
}

// scanResolution reads one resolution inside an open transaction. forUpdate
// takes the row lock that makes a read-then-write transition atomic.
func scanResolution(ctx context.Context, tx pgx.Tx, id, tenantID string, forUpdate bool) (*domain.BoardResolution, error) {
	query := `
		SELECT ` + resolutionColumns + `
		FROM board_resolutions WHERE resolution_id = $1 AND tenant_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}

	var r domain.BoardResolution
	var category, status string
	err := tx.QueryRow(ctx, query, id, tenantID).Scan(
		&r.ResolutionID, &r.MeetingID, &r.TenantID, &r.LegalEntityID, &r.ResolutionNumber, &r.Title, &r.Content, &category,
		&status, &r.VotesFor, &r.VotesAgainst, &r.Abstentions, &r.PassedAt, &r.PassedBy, &r.DocumentVaultID,
		&r.EffectiveFrom, &r.EffectiveTo, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrResolutionNotFound
		}
		return nil, mapPgError(err)
	}
	r.Category = domain.ResolutionCategory(category)
	r.Status = domain.ResolutionStatus(status)
	return &r, nil
}

func (s *PgStore) ListResolutions(ctx context.Context, f domain.ResolutionFilter) ([]domain.BoardResolution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	args := []any{tenantID, f.LegalEntityID, f.MeetingID, f.Status}
	args, page := pageClause(args, f.Limit, f.Offset)
	rows, err := tx.Query(ctx, `
		SELECT `+resolutionColumns+`
		FROM board_resolutions
		WHERE tenant_id = $1
		  AND ($2 = '' OR legal_entity_id = $2)
		  AND ($3 = '' OR meeting_id = $3)
		  AND ($4 = '' OR status = $4)
		ORDER BY created_at DESC, resolution_id DESC`+page, args...,
	)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()

	var out []domain.BoardResolution
	for rows.Next() {
		var r domain.BoardResolution
		var cat, stat string
		if err := rows.Scan(
			&r.ResolutionID, &r.MeetingID, &r.TenantID, &r.LegalEntityID, &r.ResolutionNumber, &r.Title, &r.Content, &cat,
			&stat, &r.VotesFor, &r.VotesAgainst, &r.Abstentions, &r.PassedAt, &r.PassedBy, &r.DocumentVaultID,
			&r.EffectiveFrom, &r.EffectiveTo, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		r.Category = domain.ResolutionCategory(cat)
		r.Status = domain.ResolutionStatus(stat)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	_ = tx.Commit(ctx)
	return out, nil
}

// RecordVotes tallies a resolution's votes.
//
// The status check and the write now share one transaction and one row lock.
// They used to be two: GetResolution opened its own transaction, committed,
// and only then did a second transaction UPDATE — so two concurrent requests
// both read PROPOSED and both wrote, and a tally could be applied to a
// resolution another request had finalized in between.
func (s *PgStore) RecordVotes(ctx context.Context, id string, req *domain.RecordVotesRequest) (*domain.BoardResolution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	r, err := scanResolution(ctx, tx, id, tenantID, true)
	if err != nil {
		return nil, err
	}
	if r.Status.IsFinal() {
		return nil, domain.ErrResolutionAlreadyFinalized
	}

	r.VotesFor = req.VotesFor
	r.VotesAgainst = req.VotesAgainst
	r.Abstentions = req.Abstentions
	r.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE board_resolutions
		SET votes_for=$1, votes_against=$2, abstentions=$3, updated_at=$4
		WHERE resolution_id=$5 AND tenant_id=$6`,
		r.VotesFor, r.VotesAgainst, r.Abstentions, r.UpdatedAt, id, tenantID,
	)
	if err != nil {
		return nil, mapPgError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// PassResolution finalizes a resolution as PASSED.
//
// Two fixes beyond the shared transaction: the finalized check now covers
// REJECTED as well. It listed only PASSED and RESCINDED, so a resolution the
// board had already rejected could be passed into force afterwards — the one
// transition the status is there to prevent. RecordVotes had the complete list
// all along; only the closing action was missing it.
//
// passed_by is the authenticated principal, passed in by the handler. It used
// to be whatever string the request body carried, which made the attribution
// on a finalized board resolution — the record of who put it into force —
// self-declared by the caller.
func (s *PgStore) PassResolution(ctx context.Context, id, passedBy string, req *domain.PassResolutionRequest) (*domain.BoardResolution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	r, err := scanResolution(ctx, tx, id, tenantID, true)
	if err != nil {
		return nil, err
	}
	if r.Status.IsFinal() {
		return nil, domain.ErrResolutionAlreadyFinalized
	}

	// Re-check segregation of duties against the locked row. The handler
	// checks it too, on the read it did before calling out to
	// evidence-requirements-svc — but that read is stale by the time the write
	// happens, and this is the check the doctrine actually rests on.
	if r.CreatedBy == passedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}

	now := time.Now().UTC()
	r.Status = domain.ResolutionStatusPassed
	r.PassedBy = &passedBy
	r.PassedAt = &now
	r.DocumentVaultID = req.DocumentVaultID
	r.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE board_resolutions
		SET status=$1, passed_by=$2, passed_at=$3, document_vault_id=$4, updated_at=$5
		WHERE resolution_id=$6 AND tenant_id=$7`,
		string(r.Status), r.PassedBy, r.PassedAt, r.DocumentVaultID, r.UpdatedAt, id, tenantID,
	)
	if err != nil {
		return nil, mapPgError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}
