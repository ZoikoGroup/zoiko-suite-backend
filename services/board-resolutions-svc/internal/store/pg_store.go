package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/board-resolutions-svc/internal/domain"
	"zoiko.io/board-resolutions-svc/internal/middleware"
)

type Store interface {
	CreateMeeting(ctx context.Context, m *domain.BoardMeeting) error
	GetMeeting(ctx context.Context, id string) (*domain.BoardMeeting, error)
	ListMeetings(ctx context.Context, legalEntityID string) ([]domain.BoardMeeting, error)

	CreateResolution(ctx context.Context, r *domain.BoardResolution) error
	GetResolution(ctx context.Context, id string) (*domain.BoardResolution, error)
	ListResolutions(ctx context.Context, legalEntityID, meetingID, status string) ([]domain.BoardResolution, error)
	RecordVotes(ctx context.Context, id string, req *domain.RecordVotesRequest) (*domain.BoardResolution, error)
	PassResolution(ctx context.Context, id string, req *domain.PassResolutionRequest) (*domain.BoardResolution, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID))
	return err
}

func (s *PgStore) CreateMeeting(ctx context.Context, m *domain.BoardMeeting) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if m.MeetingID == "" {
		m.MeetingID = "mtg-" + uuid.New().String()
	}
	m.TenantID = middleware.GetTenantID(ctx)
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
		return fmt.Errorf("insert meeting: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetMeeting(ctx context.Context, id string) (*domain.BoardMeeting, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var m domain.BoardMeeting
	var status string
	err = tx.QueryRow(ctx, `
		SELECT meeting_id, tenant_id, legal_entity_id, title, scheduled_at, COALESCE(location,''), status,
		       COALESCE(minutes_summary,''), effective_from, effective_to, created_by, created_at, updated_at
		FROM board_meetings WHERE meeting_id = $1`, id,
	).Scan(
		&m.MeetingID, &m.TenantID, &m.LegalEntityID, &m.Title, &m.ScheduledAt, &m.Location, &status,
		&m.MinutesSummary, &m.EffectiveFrom, &m.EffectiveTo, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrMeetingNotFound
		}
		return nil, err
	}
	m.Status = domain.MeetingStatus(status)
	_ = tx.Commit(ctx)
	return &m, nil
}

func (s *PgStore) ListMeetings(ctx context.Context, legalEntityID string) ([]domain.BoardMeeting, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT meeting_id, tenant_id, legal_entity_id, title, scheduled_at, COALESCE(location,''), status,
		       COALESCE(minutes_summary,''), effective_from, effective_to, created_by, created_at, updated_at
		FROM board_meetings
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY scheduled_at DESC`, legalEntityID,
	)
	if err != nil {
		return nil, err
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
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) CreateResolution(ctx context.Context, r *domain.BoardResolution) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if r.ResolutionID == "" {
		r.ResolutionID = "res-" + uuid.New().String()
	}
	r.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	if r.Status == "" {
		r.Status = domain.ResolutionStatusProposed
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
		return fmt.Errorf("insert resolution: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetResolution(ctx context.Context, id string) (*domain.BoardResolution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var r domain.BoardResolution
	var category, status string
	err = tx.QueryRow(ctx, `
		SELECT resolution_id, meeting_id, tenant_id, legal_entity_id, resolution_number, title, content, category,
		       status, votes_for, votes_against, abstentions, passed_at, passed_by, document_vault_id,
		       effective_from, effective_to, created_by, created_at, updated_at
		FROM board_resolutions WHERE resolution_id = $1`, id,
	).Scan(
		&r.ResolutionID, &r.MeetingID, &r.TenantID, &r.LegalEntityID, &r.ResolutionNumber, &r.Title, &r.Content, &category,
		&status, &r.VotesFor, &r.VotesAgainst, &r.Abstentions, &r.PassedAt, &r.PassedBy, &r.DocumentVaultID,
		&r.EffectiveFrom, &r.EffectiveTo, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrResolutionNotFound
		}
		return nil, err
	}
	r.Category = domain.ResolutionCategory(category)
	r.Status = domain.ResolutionStatus(status)
	_ = tx.Commit(ctx)
	return &r, nil
}

func (s *PgStore) ListResolutions(ctx context.Context, legalEntityID, meetingID, status string) ([]domain.BoardResolution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT resolution_id, meeting_id, tenant_id, legal_entity_id, resolution_number, title, content, category,
		       status, votes_for, votes_against, abstentions, passed_at, passed_by, document_vault_id,
		       effective_from, effective_to, created_by, created_at, updated_at
		FROM board_resolutions
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR meeting_id = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC`, legalEntityID, meetingID, status,
	)
	if err != nil {
		return nil, err
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
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) RecordVotes(ctx context.Context, id string, req *domain.RecordVotesRequest) (*domain.BoardResolution, error) {
	r, err := s.GetResolution(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status == domain.ResolutionStatusPassed || r.Status == domain.ResolutionStatusRejected || r.Status == domain.ResolutionStatusRescinded {
		return nil, domain.ErrResolutionAlreadyFinalized
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	r.VotesFor = req.VotesFor
	r.VotesAgainst = req.VotesAgainst
	r.Abstentions = req.Abstentions
	r.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE board_resolutions
		SET votes_for=$1, votes_against=$2, abstentions=$3, updated_at=$4
		WHERE resolution_id=$5`,
		r.VotesFor, r.VotesAgainst, r.Abstentions, r.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *PgStore) PassResolution(ctx context.Context, id string, req *domain.PassResolutionRequest) (*domain.BoardResolution, error) {
	r, err := s.GetResolution(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status == domain.ResolutionStatusPassed || r.Status == domain.ResolutionStatusRescinded {
		return nil, domain.ErrResolutionAlreadyFinalized
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	r.Status = domain.ResolutionStatusPassed
	r.PassedBy = &req.PassedBy
	r.PassedAt = &now
	r.DocumentVaultID = req.DocumentVaultID
	r.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE board_resolutions
		SET status=$1, passed_by=$2, passed_at=$3, document_vault_id=$4, updated_at=$5
		WHERE resolution_id=$6`,
		string(r.Status), r.PassedBy, r.PassedAt, r.DocumentVaultID, r.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
