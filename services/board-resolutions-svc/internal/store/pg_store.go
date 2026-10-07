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

	// OpenVoting freezes the voter roster and quorum threshold and moves the
	// resolution PROPOSED -> OPEN. LEG-04 §6.1: quorum/eligibility are
	// evaluated against this frozen population, not whoever happens to vote.
	OpenVoting(ctx context.Context, id string, voterPrincipalIDs []string, quorumThreshold int) (*domain.BoardResolution, error)
	// CastVote records one roster member's vote. Refuses a non-roster voter
	// and a second vote from the same voter.
	CastVote(ctx context.Context, id, voterPrincipalID string, vote domain.Vote, castBy string) (*domain.CastVoteRecord, error)
	// CloseVoting tallies the vote ledger against the frozen roster/threshold
	// and resolves PASSED or FAILED. closedBy is the authenticated principal,
	// checked against CreatedBy for segregation of duties the same way
	// PassResolution used to.
	CloseVoting(ctx context.Context, id, closedBy string, req *domain.CloseVotingRequest) (*domain.BoardResolution, error)
	// SupersedeResolution marks a PASSED resolution SUPERSEDED, recording
	// what superseded it. A later resolution replacing an earlier one does
	// not retroactively edit the earlier one's content or vote ledger.
	SupersedeResolution(ctx context.Context, id string, req *domain.SupersedeResolutionRequest) (*domain.BoardResolution, error)

	GetVoterRoster(ctx context.Context, id string) ([]domain.VoterRosterEntry, error)
	GetVoteLedger(ctx context.Context, id string) ([]domain.CastVoteRecord, error)
	GetQuorumEvidence(ctx context.Context, id string) (*domain.QuorumEvidence, error)
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
	       quorum_threshold, voting_opened_at, voting_closed_at, superseded_by,
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
		&r.QuorumThreshold, &r.VotingOpenedAt, &r.VotingClosedAt, &r.SupersededBy,
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
			&r.QuorumThreshold, &r.VotingOpenedAt, &r.VotingClosedAt, &r.SupersededBy,
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

// OpenVoting freezes the voter roster and quorum threshold and moves the
// resolution PROPOSED -> OPEN.
//
// The roster is written inside the same transaction and row lock as the
// status transition: a resolution read as PROPOSED by two concurrent
// OpenVoting calls must not both succeed in writing a roster, since the
// second write would silently redefine "frozen" for a resolution that was
// already open for voting under the first roster.
func (s *PgStore) OpenVoting(ctx context.Context, id string, voterPrincipalIDs []string, quorumThreshold int) (*domain.BoardResolution, error) {
	if len(voterPrincipalIDs) == 0 {
		return nil, domain.ErrEmptyRoster
	}
	if quorumThreshold < 1 || quorumThreshold > len(voterPrincipalIDs) {
		return nil, domain.ErrInvalidQuorumThreshold
	}

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
	if r.Status != domain.ResolutionStatusProposed {
		if r.Status.IsFinal() {
			return nil, domain.ErrResolutionAlreadyFinalized
		}
		return nil, domain.ErrResolutionNotOpen
	}

	now := time.Now().UTC()
	seen := make(map[string]bool, len(voterPrincipalIDs))
	for _, voterID := range voterPrincipalIDs {
		if voterID == "" || seen[voterID] {
			continue
		}
		seen[voterID] = true
		if _, err := tx.Exec(ctx, `
			INSERT INTO board_resolution_voter_roster (resolution_id, tenant_id, voter_principal_id, frozen_at)
			VALUES ($1,$2,$3,$4)`,
			id, tenantID, voterID, now,
		); err != nil {
			return nil, mapPgError(err)
		}
	}

	r.Status = domain.ResolutionStatusOpen
	r.QuorumThreshold = &quorumThreshold
	r.VotingOpenedAt = &now
	r.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE board_resolutions
		SET status=$1, quorum_threshold=$2, voting_opened_at=$3, updated_at=$4
		WHERE resolution_id=$5 AND tenant_id=$6`,
		string(r.Status), quorumThreshold, r.VotingOpenedAt, r.UpdatedAt, id, tenantID,
	)
	if err != nil {
		return nil, mapPgError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// CastVote records one roster member's vote. One row per (resolution, voter)
// — the primary key refuses a second vote from the same voter rather than
// overwriting it.
func (s *PgStore) CastVote(ctx context.Context, id, voterPrincipalID string, vote domain.Vote, castBy string) (*domain.CastVoteRecord, error) {
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
	if r.Status != domain.ResolutionStatusOpen {
		return nil, domain.ErrResolutionNotOpen
	}

	var onRoster bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM board_resolution_voter_roster WHERE resolution_id=$1 AND tenant_id=$2 AND voter_principal_id=$3)`,
		id, tenantID, voterPrincipalID,
	).Scan(&onRoster); err != nil {
		return nil, mapPgError(err)
	}
	if !onRoster {
		return nil, domain.ErrNotEligibleVoter
	}

	var alreadyVoted bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM board_resolution_votes WHERE resolution_id=$1 AND tenant_id=$2 AND voter_principal_id=$3)`,
		id, tenantID, voterPrincipalID,
	).Scan(&alreadyVoted); err != nil {
		return nil, mapPgError(err)
	}
	if alreadyVoted {
		return nil, domain.ErrAlreadyVoted
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO board_resolution_votes (resolution_id, tenant_id, voter_principal_id, vote, cast_at, cast_by)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		id, tenantID, voterPrincipalID, string(vote), now, castBy,
	); err != nil {
		return nil, mapPgError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &domain.CastVoteRecord{
		ResolutionID: id, VoterPrincipalID: voterPrincipalID, Vote: vote, CastAt: now, CastBy: castBy,
	}, nil
}

// CloseVoting tallies the vote ledger against the frozen roster/threshold and
// resolves PASSED or FAILED.
//
// closedBy carries the same segregation-of-duties check PassResolution used
// to make directly: the resolution's own drafter may not be the principal who
// closes voting on it, because closing is what finalizes the result into
// force, same as the old PassResolution was.
func (s *PgStore) CloseVoting(ctx context.Context, id, closedBy string, req *domain.CloseVotingRequest) (*domain.BoardResolution, error) {
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
	if r.Status != domain.ResolutionStatusOpen {
		return nil, domain.ErrResolutionNotOpen
	}
	if r.CreatedBy == closedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}

	var votesFor, votesAgainst, abstentions, votesCast int
	rows, err := tx.Query(ctx,
		`SELECT vote FROM board_resolution_votes WHERE resolution_id=$1 AND tenant_id=$2`, id, tenantID)
	if err != nil {
		return nil, mapPgError(err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		votesCast++
		switch domain.Vote(v) {
		case domain.VoteFor:
			votesFor++
		case domain.VoteAgainst:
			votesAgainst++
		case domain.VoteAbstain:
			abstentions++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	quorumThreshold := 0
	if r.QuorumThreshold != nil {
		quorumThreshold = *r.QuorumThreshold
	}
	quorumMet := votesCast >= quorumThreshold

	now := time.Now().UTC()
	r.VotesFor, r.VotesAgainst, r.Abstentions = votesFor, votesAgainst, abstentions
	r.VotingClosedAt = &now
	r.UpdatedAt = now

	if quorumMet && votesFor > votesAgainst {
		r.Status = domain.ResolutionStatusPassed
		r.PassedBy = &closedBy
		r.PassedAt = &now
		r.DocumentVaultID = req.DocumentVaultID
	} else {
		r.Status = domain.ResolutionStatusFailed
	}

	_, err = tx.Exec(ctx, `
		UPDATE board_resolutions
		SET status=$1, votes_for=$2, votes_against=$3, abstentions=$4, voting_closed_at=$5,
		    passed_by=$6, passed_at=$7, document_vault_id=$8, updated_at=$9
		WHERE resolution_id=$10 AND tenant_id=$11`,
		string(r.Status), r.VotesFor, r.VotesAgainst, r.Abstentions, r.VotingClosedAt,
		r.PassedBy, r.PassedAt, r.DocumentVaultID, r.UpdatedAt, id, tenantID,
	)
	if err != nil {
		return nil, mapPgError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// SupersedeResolution marks a PASSED resolution SUPERSEDED. It does not touch
// the resolution's content or vote ledger — superseding records that a later
// resolution replaced this one's force, not that this one never happened.
func (s *PgStore) SupersedeResolution(ctx context.Context, id string, req *domain.SupersedeResolutionRequest) (*domain.BoardResolution, error) {
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
	if r.Status != domain.ResolutionStatusPassed {
		return nil, domain.ErrResolutionNotPassed
	}

	now := time.Now().UTC()
	r.Status = domain.ResolutionStatusSuperseded
	r.SupersededBy = &req.SupersededBy
	r.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE board_resolutions
		SET status=$1, superseded_by=$2, updated_at=$3
		WHERE resolution_id=$4 AND tenant_id=$5`,
		string(r.Status), r.SupersededBy, r.UpdatedAt, id, tenantID,
	)
	if err != nil {
		return nil, mapPgError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *PgStore) GetVoterRoster(ctx context.Context, id string) ([]domain.VoterRosterEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx,
		`SELECT resolution_id, voter_principal_id, frozen_at FROM board_resolution_voter_roster
		 WHERE resolution_id=$1 AND tenant_id=$2 ORDER BY voter_principal_id`, id, tenantID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()

	var out []domain.VoterRosterEntry
	for rows.Next() {
		var e domain.VoterRosterEntry
		if err := rows.Scan(&e.ResolutionID, &e.VoterPrincipalID, &e.FrozenAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) GetVoteLedger(ctx context.Context, id string) ([]domain.CastVoteRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx,
		`SELECT resolution_id, voter_principal_id, vote, cast_at, cast_by FROM board_resolution_votes
		 WHERE resolution_id=$1 AND tenant_id=$2 ORDER BY cast_at`, id, tenantID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()

	var out []domain.CastVoteRecord
	for rows.Next() {
		var rec domain.CastVoteRecord
		var v string
		if err := rows.Scan(&rec.ResolutionID, &rec.VoterPrincipalID, &v, &rec.CastAt, &rec.CastBy); err != nil {
			return nil, err
		}
		rec.Vote = domain.Vote(v)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) GetQuorumEvidence(ctx context.Context, id string) (*domain.QuorumEvidence, error) {
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

	var rosterSize int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM board_resolution_voter_roster WHERE resolution_id=$1 AND tenant_id=$2`,
		id, tenantID,
	).Scan(&rosterSize); err != nil {
		return nil, mapPgError(err)
	}

	var votesCast int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM board_resolution_votes WHERE resolution_id=$1 AND tenant_id=$2`,
		id, tenantID,
	).Scan(&votesCast); err != nil {
		return nil, mapPgError(err)
	}

	quorumThreshold := 0
	if r.QuorumThreshold != nil {
		quorumThreshold = *r.QuorumThreshold
	}

	_ = tx.Commit(ctx)
	return &domain.QuorumEvidence{
		ResolutionID:    id,
		RosterSize:      rosterSize,
		QuorumThreshold: quorumThreshold,
		VotesCast:       votesCast,
		VotesFor:        r.VotesFor,
		VotesAgainst:    r.VotesAgainst,
		Abstentions:     r.Abstentions,
		QuorumMet:       votesCast >= quorumThreshold,
	}, nil
}
