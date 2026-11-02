package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/carta-svc/internal/domain"
)

// PgStore persists assessments in carta_assessments (migration 000001).
//
// Every statement runs inside a transaction that first installs
// app.tenant_id, so the table's FORCEd row-level security policy binds the
// query as well as the explicit tenant_id predicate each statement carries.
// Under an ordinary (NOSUPERUSER NOBYPASSRLS) role a statement that forgot
// its predicate would see nothing rather than everything.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore returns a store backed by pool.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// Ping lets the store stand in for the pool in readiness checks.
func (s *PgStore) Ping(ctx context.Context) error {
	if s.pool == nil {
		return errors.New("no database pool")
	}
	return s.pool.Ping(ctx)
}

// SaveAssessment inserts one decision record. The id and tenant are assigned
// here, never taken from the caller. The timestamp is normalised to the
// precision Postgres stores, so the object the handler returns is exactly the
// row a later read returns.
func (s *PgStore) SaveAssessment(ctx context.Context, tenantID string, asm *domain.CartaAssessment) error {
	if tenantID == "" {
		return errors.New("tenant is required")
	}
	contextJSON, err := json.Marshal(asm.Context)
	if err != nil {
		return fmt.Errorf("encode context: %w", err)
	}
	factors := asm.RiskFactors
	if factors == nil {
		factors = []string{}
	}
	factorsJSON, err := json.Marshal(factors)
	if err != nil {
		return fmt.Errorf("encode risk factors: %w", err)
	}
	ts := asm.AssessmentTimestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	ts = ts.UTC().Truncate(time.Microsecond)
	id := uuid.New().String()

	err = s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO carta_assessments
			    (id, tenant_id, legal_entity_id, subject_id, context, trust_score,
			     risk_level, decision, risk_factors, assessment_timestamp)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			id, tenantID, asm.LegalEntityID, asm.SubjectID, contextJSON, asm.TrustScore,
			string(asm.RiskLevel), string(asm.Decision), factorsJSON, ts)
		return err
	})
	if err != nil {
		return fmt.Errorf("insert assessment: %w", err)
	}
	asm.ID = id
	asm.TenantID = tenantID
	asm.AssessmentTimestamp = ts
	asm.RiskFactors = factors
	return nil
}

const selectAssessment = `
	SELECT id, tenant_id, legal_entity_id, subject_id, context, trust_score,
	       risk_level, decision, risk_factors, assessment_timestamp
	  FROM carta_assessments`

// GetAssessmentByID returns ErrNotFound for an unknown id, a malformed id, and
// another tenant's id alike.
func (s *PgStore) GetAssessmentByID(ctx context.Context, tenantID, id string) (*domain.CartaAssessment, error) {
	if tenantID == "" {
		return nil, ErrNotFound
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out *domain.CartaAssessment
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, selectAssessment+` WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		a, err := scanAssessment(row)
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get assessment: %w", err)
	}
	return out, nil
}

// ListAssessments: tenant and legal entity are both mandatory equality
// filters (legalEntityID is the authorization scope); subjectID narrows
// within them when non-empty. Newest first.
func (s *PgStore) ListAssessments(ctx context.Context, tenantID, legalEntityID, subjectID string) ([]domain.CartaAssessment, error) {
	if tenantID == "" || legalEntityID == "" {
		return []domain.CartaAssessment{}, nil
	}
	out := []domain.CartaAssessment{}
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, selectAssessment+`
			 WHERE tenant_id = $1 AND legal_entity_id = $2
			   AND ($3 = '' OR subject_id = $3)
			 ORDER BY assessment_timestamp DESC, id`,
			tenantID, legalEntityID, subjectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAssessment(rows)
			if err != nil {
				return err
			}
			out = append(out, *a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list assessments: %w", err)
	}
	return out, nil
}

func scanAssessment(row pgx.Row) (*domain.CartaAssessment, error) {
	var (
		a                        domain.CartaAssessment
		contextJSON, factorsJSON []byte
		riskLevel, decision      string
	)
	if err := row.Scan(&a.ID, &a.TenantID, &a.LegalEntityID, &a.SubjectID, &contextJSON,
		&a.TrustScore, &riskLevel, &decision, &factorsJSON, &a.AssessmentTimestamp); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(contextJSON, &a.Context); err != nil {
		return nil, fmt.Errorf("decode context: %w", err)
	}
	if err := json.Unmarshal(factorsJSON, &a.RiskFactors); err != nil {
		return nil, fmt.Errorf("decode risk factors: %w", err)
	}
	a.RiskLevel = domain.RiskLevel(riskLevel)
	a.Decision = domain.Decision(decision)
	a.AssessmentTimestamp = a.AssessmentTimestamp.UTC()
	return &a, nil
}

// inTenantTx runs fn in a transaction scoped to tenantID for row-level
// security. set_config(..., true) is transaction-local, so the scope cannot
// leak to the next borrower of the pooled connection.
func (s *PgStore) inTenantTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if s.pool == nil {
		return errors.New("store has no database pool")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set tenant scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
