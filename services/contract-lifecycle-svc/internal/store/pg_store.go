// Package store provides the PostgreSQL implementation of
// contract-lifecycle-svc's persistence layer.
//
// Every method wraps its work in setRLS, which sets app.tenant_id on the
// transaction, and the Row-Level Security policies in
// 000001_initial_schema.up.sql are correctly written. That is NOT sufficient on
// its own: this pool connects as a Postgres superuser (the DATABASE_URL in
// deployments/docker-compose.yml is postgres:postgres, same as every other
// service on this platform), and Postgres superusers unconditionally bypass Row
// Level Security no matter what policies exist — ENABLE ROW LEVEL SECURITY is
// also skipped for a table's owner unless FORCE ROW LEVEL SECURITY is set, and
// here the owner is postgres too.
//
// So every method ALSO filters explicitly by tenant_id in its own SQL. This is
// the same belt-and-braces already documented in purchase-order-svc's store,
// where it was adopted after genuine CI failures in general-ledger-svc and
// tenant-entity-registry-svc. This service was written relying on RLS alone,
// which meant any caller could read and mutate another tenant's contracts
// simply by sending a different X-Tenant-Id — verified live before this was
// added, with a second tenant reading all four of the first tenant's rows.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/contract-lifecycle-svc/internal/domain"
	"zoiko.io/contract-lifecycle-svc/internal/middleware"
)

// Store defines all persistence operations for the contract lifecycle service.
type Store interface {
	CreateContract(ctx context.Context, c *domain.Contract) error
	GetContract(ctx context.Context, id string) (*domain.Contract, error)
	ListContracts(ctx context.Context, legalEntityID string) ([]domain.Contract, error)
	// UpdateContract is LEG-05's named CreateVersion command: it may only act
	// while the contract is DRAFT or REVIEW — an approved-for-signature
	// version is immutable (LEG-05 §7.1).
	UpdateContract(ctx context.Context, c *domain.Contract, changeSummary string) error
	SubmitReview(ctx context.Context, id, submittedBy string) (*domain.Contract, error)
	// ApproveContract moves REVIEW -> APPROVED. approvedBy is re-checked
	// against the locked row's submitter for segregation of duties, the same
	// two-layer pattern used across this codebase.
	ApproveContract(ctx context.Context, id, approvedBy, governanceDecisionID string) (*domain.Contract, error)
	SendForSignature(ctx context.Context, id, sentBy string) (*domain.Contract, error)
	RecordExecution(ctx context.Context, id string, req *domain.RecordExecutionRequest) (*domain.Contract, error)
	AmendContract(ctx context.Context, id string, req *domain.AmendContractRequest) (*domain.Contract, error)
	RenewContract(ctx context.Context, id string, req *domain.RenewContractRequest) (*domain.Contract, error)
	TerminateContract(ctx context.Context, id string, req *domain.TerminateContractRequest) (*domain.Contract, error)
	ListContractVersions(ctx context.Context, contractID string) ([]domain.ContractVersion, error)
}

// effectiveDateColumns is the SELECT fragment for the two effective-date
// columns, which MUST be read as text.
//
// effective_from/effective_to are DATE columns, but domain.Contract and
// domain.ContractVersion declare them as string / *string because the API
// contract is a plain "YYYY-MM-DD" — not an RFC3339 timestamp. pgx will happily
// ENCODE a Go string into a DATE parameter (it parses the literal), so every
// write path worked; it cannot DECODE a DATE into a *string, so every read that
// actually scanned a row failed. That asymmetry is why the service looked
// healthy: POST returned 201, and only GET/PUT/submit/activate/terminate — all
// of which load the row first — returned 500. A read matching zero rows also
// looked fine, because there was nothing to scan.
//
// Casting in SQL rather than changing the Go type keeps the wire shape
// identical: switching EffectiveFrom to time.Time would serialise as
// "2026-08-01T00:00:00Z" and silently break every consumer.
//
// TO_CHAR rather than ::TEXT so the format does not depend on the session's
// DateStyle. NULL effective_to passes through as NULL.
const effectiveDateColumns = `TO_CHAR(effective_from, 'YYYY-MM-DD'), TO_CHAR(effective_to, 'YYYY-MM-DD')`

// PgStore implements Store using a PostgreSQL connection pool with RLS.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore creates a new PgStore instance.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// setRLS sets the RLS tenant context variable for the current transaction.
//
// Kept even though the superuser connection bypasses the policies (see the
// package comment): it costs one statement, and it is what makes the policies
// work the moment this service is given a non-superuser role. It is defence in
// depth, never the only defence — the explicit tenant_id predicate is.
func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	if tenantID == "" {
		return domain.ErrTenantMissing
	}
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

// snapshotVersion creates an immutable version record for audit lineage.
func (s *PgStore) snapshotVersion(ctx context.Context, tx pgx.Tx, c *domain.Contract, summary string) error {
	v := &domain.ContractVersion{
		VersionID:     "cv-" + uuid.New().String(),
		ContractID:    c.ContractID,
		TenantID:      c.TenantID,
		VersionNumber: c.Version,
		Status:        c.Status,
		Title:         c.Title,
		Description:   c.Description,
		EffectiveFrom: c.EffectiveFrom,
		EffectiveTo:   c.EffectiveTo,
		ChangeSummary: summary,
		CreatedBy:     c.CreatedBy,
		CreatedAt:     time.Now().UTC(),
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO contract_versions
			(version_id, contract_id, tenant_id, version_number, status, title, description,
			 effective_from, effective_to, change_summary, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		v.VersionID, v.ContractID, v.TenantID, v.VersionNumber, string(v.Status), v.Title,
		v.Description, v.EffectiveFrom, v.EffectiveTo, v.ChangeSummary, v.CreatedBy, v.CreatedAt,
	)
	return err
}

// CreateContract inserts a new contract in DRAFT status.
func (s *PgStore) CreateContract(ctx context.Context, c *domain.Contract) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if c.ContractID == "" {
		c.ContractID = "ctr-" + uuid.New().String()
	}
	c.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = domain.ContractStatusDraft
	}
	c.Version = 1

	if c.SignatureStatus == "" {
		c.SignatureStatus = domain.SignatureStatusNotRequested
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO contracts
			(contract_id, tenant_id, legal_entity_id, contract_type, title, description,
			 counterparty_id, counterparty_name, status, signature_status, version, effective_from, effective_to,
			 currency, total_value, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		c.ContractID, c.TenantID, c.LegalEntityID, string(c.ContractType), c.Title, c.Description,
		c.CounterpartyID, c.CounterpartyName, string(c.Status), string(c.SignatureStatus), c.Version, c.EffectiveFrom, c.EffectiveTo,
		c.Currency, c.TotalValue, c.CreatedBy, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert contract: %w", err)
	}

	if err := s.snapshotVersion(ctx, tx, c, "Initial draft"); err != nil {
		return fmt.Errorf("snapshot version: %w", err)
	}

	return tx.Commit(ctx)
}

// GetContract retrieves a single contract by ID.
func (s *PgStore) GetContract(ctx context.Context, id string) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var c domain.Contract
	var ctype, status, sigStatus string
	err = tx.QueryRow(ctx, `
		SELECT contract_id, tenant_id, legal_entity_id, contract_type, title,
		       COALESCE(description,''), counterparty_id, counterparty_name, status, signature_status, version,
		       `+effectiveDateColumns+`,
		       submitted_at, submitted_by, approved_at, approved_by, signature_sent_at, executed_at, signed_by,
		       effective_at, amended_at, amended_by, renewed_at, renewed_by,
		       terminated_at, terminated_by, termination_note,
		       currency, total_value, document_vault_id, governance_decision_id, created_by, created_at, updated_at
		FROM contracts WHERE contract_id = $1 AND tenant_id = $2`,
		id, middleware.GetTenantID(ctx),
	).Scan(
		&c.ContractID, &c.TenantID, &c.LegalEntityID, &ctype, &c.Title,
		&c.Description, &c.CounterpartyID, &c.CounterpartyName, &status, &sigStatus, &c.Version,
		&c.EffectiveFrom, &c.EffectiveTo,
		&c.SubmittedAt, &c.SubmittedBy, &c.ApprovedAt, &c.ApprovedBy, &c.SignatureSentAt, &c.ExecutedAt, &c.SignedBy,
		&c.EffectiveAt, &c.AmendedAt, &c.AmendedBy, &c.RenewedAt, &c.RenewedBy,
		&c.TerminatedAt, &c.TerminatedBy, &c.TerminationNote,
		&c.Currency, &c.TotalValue, &c.DocumentVaultID, &c.GovernanceDecisionID, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrContractNotFound
		}
		return nil, err
	}
	c.ContractType = domain.ContractType(ctype)
	c.Status = domain.ContractStatus(status)
	c.SignatureStatus = domain.SignatureStatus(sigStatus)
	_ = tx.Commit(ctx)
	return &c, nil
}

// ListContracts returns all contracts for the tenant, optionally filtered by legal entity.
func (s *PgStore) ListContracts(ctx context.Context, legalEntityID string) ([]domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT contract_id, tenant_id, legal_entity_id, contract_type, title,
		       COALESCE(description,''), counterparty_id, counterparty_name, status, signature_status, version,
		       `+effectiveDateColumns+`,
		       submitted_at, submitted_by, approved_at, approved_by, signature_sent_at, executed_at, signed_by,
		       effective_at, amended_at, amended_by, renewed_at, renewed_by,
		       terminated_at, terminated_by, termination_note,
		       currency, total_value, document_vault_id, governance_decision_id, created_by, created_at, updated_at
		FROM contracts
		WHERE tenant_id = $2 AND ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC`,
		legalEntityID, middleware.GetTenantID(ctx),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Contract
	for rows.Next() {
		var c domain.Contract
		var ctype, status, sigStatus string
		if err := rows.Scan(
			&c.ContractID, &c.TenantID, &c.LegalEntityID, &ctype, &c.Title,
			&c.Description, &c.CounterpartyID, &c.CounterpartyName, &status, &sigStatus, &c.Version,
			&c.EffectiveFrom, &c.EffectiveTo,
			&c.SubmittedAt, &c.SubmittedBy, &c.ApprovedAt, &c.ApprovedBy, &c.SignatureSentAt, &c.ExecutedAt, &c.SignedBy,
			&c.EffectiveAt, &c.AmendedAt, &c.AmendedBy, &c.RenewedAt, &c.RenewedBy,
			&c.TerminatedAt, &c.TerminatedBy, &c.TerminationNote,
			&c.Currency, &c.TotalValue, &c.DocumentVaultID, &c.GovernanceDecisionID, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		c.ContractType = domain.ContractType(ctype)
		c.Status = domain.ContractStatus(status)
		c.SignatureStatus = domain.SignatureStatus(sigStatus)
		out = append(out, c)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

// UpdateContract is LEG-05's named CreateVersion command. It may only act
// while the contract is DRAFT or REVIEW: an approved-for-signature version
// is immutable (LEG-05 §7.1) — ApproveContract and everything after it is
// what makes this refuse past that point, by locking the row and checking
// status before any write, the same pattern as every other protected
// transition in this codebase.
func (s *PgStore) UpdateContract(ctx context.Context, c *domain.Contract, changeSummary string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	var currentStatus string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM contracts WHERE contract_id=$1 AND tenant_id=$2 FOR UPDATE`,
		c.ContractID, middleware.GetTenantID(ctx),
	).Scan(&currentStatus); err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return domain.ErrContractNotFound
		}
		return err
	}
	if domain.ContractStatus(currentStatus) != domain.ContractStatusDraft && domain.ContractStatus(currentStatus) != domain.ContractStatusReview {
		return domain.ErrWrongLifecycleStatus
	}

	c.Version++
	c.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE contracts
		SET title=$1, description=$2, counterparty_name=$3, effective_to=$4,
		    currency=$5, total_value=$6, version=$7, updated_at=$8
		WHERE contract_id=$9 AND tenant_id=$10`,
		c.Title, c.Description, c.CounterpartyName, c.EffectiveTo,
		c.Currency, c.TotalValue, c.Version, c.UpdatedAt, c.ContractID,
		middleware.GetTenantID(ctx),
	)
	if err != nil {
		return fmt.Errorf("update contract: %w", err)
	}

	if err := s.snapshotVersion(ctx, tx, c, changeSummary); err != nil {
		return fmt.Errorf("snapshot version: %w", err)
	}

	return tx.Commit(ctx)
}

// lockContract reads a contract FOR UPDATE inside an open transaction — the
// row lock that makes a read-then-write lifecycle transition atomic, same
// pattern as board-resolutions-svc's scanResolution.
func (s *PgStore) lockContract(ctx context.Context, tx pgx.Tx, id string) (*domain.Contract, error) {
	var c domain.Contract
	var ctype, status, sigStatus string
	err := tx.QueryRow(ctx, `
		SELECT contract_id, tenant_id, legal_entity_id, contract_type, title,
		       COALESCE(description,''), counterparty_id, counterparty_name, status, signature_status, version,
		       `+effectiveDateColumns+`,
		       submitted_at, submitted_by, approved_at, approved_by, signature_sent_at, executed_at, signed_by,
		       effective_at, amended_at, amended_by, renewed_at, renewed_by,
		       terminated_at, terminated_by, termination_note,
		       currency, total_value, document_vault_id, governance_decision_id, created_by, created_at, updated_at
		FROM contracts WHERE contract_id = $1 AND tenant_id = $2 FOR UPDATE`,
		id, middleware.GetTenantID(ctx),
	).Scan(
		&c.ContractID, &c.TenantID, &c.LegalEntityID, &ctype, &c.Title,
		&c.Description, &c.CounterpartyID, &c.CounterpartyName, &status, &sigStatus, &c.Version,
		&c.EffectiveFrom, &c.EffectiveTo,
		&c.SubmittedAt, &c.SubmittedBy, &c.ApprovedAt, &c.ApprovedBy, &c.SignatureSentAt, &c.ExecutedAt, &c.SignedBy,
		&c.EffectiveAt, &c.AmendedAt, &c.AmendedBy, &c.RenewedAt, &c.RenewedBy,
		&c.TerminatedAt, &c.TerminatedBy, &c.TerminationNote,
		&c.Currency, &c.TotalValue, &c.DocumentVaultID, &c.GovernanceDecisionID, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrContractNotFound
		}
		return nil, err
	}
	c.ContractType = domain.ContractType(ctype)
	c.Status = domain.ContractStatus(status)
	c.SignatureStatus = domain.SignatureStatus(sigStatus)
	return &c, nil
}

// SubmitReview moves DRAFT -> REVIEW.
func (s *PgStore) SubmitReview(ctx context.Context, id, submittedBy string) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	c, err := s.lockContract(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.ContractStatusDraft {
		return nil, domain.ErrWrongLifecycleStatus
	}

	now := time.Now().UTC()
	c.Status = domain.ContractStatusReview
	c.SubmittedBy = &submittedBy
	c.SubmittedAt = &now
	c.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE contracts SET status=$1, submitted_by=$2, submitted_at=$3, updated_at=$4
		WHERE contract_id=$5 AND tenant_id=$6`,
		string(c.Status), c.SubmittedBy, c.SubmittedAt, c.UpdatedAt, id, middleware.GetTenantID(ctx),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ApproveContract moves REVIEW -> APPROVED, past which the version is
// immutable (LEG-05 §7.1). Segregation of duties (LEG-05 §Authorization/SoD:
// "self-approval blocked") is re-checked against the locked row: the
// principal who submitted the contract for review may not be the one who
// approves it.
func (s *PgStore) ApproveContract(ctx context.Context, id, approvedBy, governanceDecisionID string) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	c, err := s.lockContract(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.ContractStatusReview {
		return nil, domain.ErrWrongLifecycleStatus
	}
	if c.SubmittedBy != nil && *c.SubmittedBy == approvedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}

	now := time.Now().UTC()
	c.Status = domain.ContractStatusApproved
	c.ApprovedBy = &approvedBy
	c.ApprovedAt = &now
	c.GovernanceDecisionID = &governanceDecisionID
	c.UpdatedAt = now
	c.Version++

	if _, err := tx.Exec(ctx, `
		UPDATE contracts
		SET status=$1, approved_by=$2, approved_at=$3, governance_decision_id=$4, version=$5, updated_at=$6
		WHERE contract_id=$7 AND tenant_id=$8`,
		string(c.Status), c.ApprovedBy, c.ApprovedAt, c.GovernanceDecisionID, c.Version, c.UpdatedAt, id,
		middleware.GetTenantID(ctx),
	); err != nil {
		return nil, err
	}
	if err := s.snapshotVersion(ctx, tx, c, "Contract approved"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// SendForSignature moves the Signature dimension NOT_REQUESTED -> SENT.
// Requires the contract already be APPROVED — the Instrument lifecycle
// dimension is untouched, per LEG-05 §2.2's orthogonality rule.
func (s *PgStore) SendForSignature(ctx context.Context, id, sentBy string) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	c, err := s.lockContract(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.ContractStatusApproved {
		return nil, domain.ErrWrongLifecycleStatus
	}

	now := time.Now().UTC()
	c.SignatureStatus = domain.SignatureStatusSent
	c.SignatureSentAt = &now
	c.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE contracts SET signature_status=$1, signature_sent_at=$2, updated_at=$3
		WHERE contract_id=$4 AND tenant_id=$5`,
		string(c.SignatureStatus), c.SignatureSentAt, c.UpdatedAt, id, middleware.GetTenantID(ctx),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// RecordExecution moves the Signature dimension to COMPLETED and the
// Instrument lifecycle dimension APPROVED -> EXECUTED -> EFFECTIVE.
//
// Executed immediately implies Effective in this pass: a future-dated
// effective_from that differs from the execution date would need a
// scheduler this service does not have — documented limitation, same
// honesty as the voter-roster and tax-jurisdiction deferrals elsewhere in
// this codebase.
func (s *PgStore) RecordExecution(ctx context.Context, id string, req *domain.RecordExecutionRequest) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	c, err := s.lockContract(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.ContractStatusApproved {
		return nil, domain.ErrWrongLifecycleStatus
	}
	if c.SignatureStatus != domain.SignatureStatusSent && c.SignatureStatus != domain.SignatureStatusPartiallySigned {
		return nil, domain.ErrSignatureNotSent
	}

	now := time.Now().UTC()
	c.Status = domain.ContractStatusEffective
	c.SignatureStatus = domain.SignatureStatusCompleted
	c.ExecutedAt = &now
	c.EffectiveAt = &now
	c.SignedBy = &req.SignedBy
	c.DocumentVaultID = req.DocumentVaultID
	c.UpdatedAt = now
	c.Version++

	if _, err := tx.Exec(ctx, `
		UPDATE contracts
		SET status=$1, signature_status=$2, executed_at=$3, effective_at=$4, signed_by=$5,
		    document_vault_id=$6, version=$7, updated_at=$8
		WHERE contract_id=$9 AND tenant_id=$10`,
		string(c.Status), string(c.SignatureStatus), c.ExecutedAt, c.EffectiveAt, c.SignedBy,
		c.DocumentVaultID, c.Version, c.UpdatedAt, id, middleware.GetTenantID(ctx),
	); err != nil {
		return nil, err
	}
	if err := s.snapshotVersion(ctx, tx, c, "Contract executed and effective"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// AmendContract records a protected change to an EFFECTIVE contract. The
// contract itself stays EFFECTIVE throughout — amendment is an event and a
// new immutable version, not a new status (see migration 000003's doc
// comment for why).
func (s *PgStore) AmendContract(ctx context.Context, id string, req *domain.AmendContractRequest) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	c, err := s.lockContract(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.ContractStatusEffective {
		return nil, domain.ErrWrongLifecycleStatus
	}

	if req.Title != "" {
		c.Title = req.Title
	}
	if req.Description != "" {
		c.Description = req.Description
	}
	if req.CounterpartyName != "" {
		c.CounterpartyName = req.CounterpartyName
	}
	if req.TotalValue > 0 {
		c.TotalValue = req.TotalValue
	}
	now := time.Now().UTC()
	c.AmendedBy = &req.AmendedBy
	c.AmendedAt = &now
	c.UpdatedAt = now
	c.Version++

	if _, err := tx.Exec(ctx, `
		UPDATE contracts
		SET title=$1, description=$2, counterparty_name=$3, total_value=$4,
		    amended_by=$5, amended_at=$6, version=$7, updated_at=$8
		WHERE contract_id=$9 AND tenant_id=$10`,
		c.Title, c.Description, c.CounterpartyName, c.TotalValue,
		c.AmendedBy, c.AmendedAt, c.Version, c.UpdatedAt, id, middleware.GetTenantID(ctx),
	); err != nil {
		return nil, err
	}
	summary := req.ChangeSummary
	if summary == "" {
		summary = "Contract amended"
	}
	if err := s.snapshotVersion(ctx, tx, c, summary); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// RenewContract extends an EFFECTIVE contract's term. Like AmendContract,
// the contract stays EFFECTIVE — renewal is an event and a new version, not
// a status change.
func (s *PgStore) RenewContract(ctx context.Context, id string, req *domain.RenewContractRequest) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	c, err := s.lockContract(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.ContractStatusEffective {
		return nil, domain.ErrWrongLifecycleStatus
	}

	now := time.Now().UTC()
	c.EffectiveTo = &req.NewEffectiveTo
	c.RenewedBy = &req.RenewedBy
	c.RenewedAt = &now
	c.UpdatedAt = now
	c.Version++

	if _, err := tx.Exec(ctx, `
		UPDATE contracts
		SET effective_to=$1, renewed_by=$2, renewed_at=$3, version=$4, updated_at=$5
		WHERE contract_id=$6 AND tenant_id=$7`,
		c.EffectiveTo, c.RenewedBy, c.RenewedAt, c.Version, c.UpdatedAt, id, middleware.GetTenantID(ctx),
	); err != nil {
		return nil, err
	}
	summary := req.ChangeSummary
	if summary == "" {
		summary = "Contract renewed"
	}
	if err := s.snapshotVersion(ctx, tx, c, summary); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// TerminateContract marks a contract TERMINATED and records termination
// metadata. Only a contract that has actually reached APPROVED or later may
// be terminated — a DRAFT/REVIEW contract was never in force and has
// nothing to terminate.
func (s *PgStore) TerminateContract(ctx context.Context, id string, req *domain.TerminateContractRequest) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	c, err := s.lockContract(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if c.Status.IsFinal() {
		return nil, domain.ErrContractTerminated
	}
	if c.Status == domain.ContractStatusDraft || c.Status == domain.ContractStatusReview {
		return nil, domain.ErrWrongLifecycleStatus
	}

	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")
	c.Status = domain.ContractStatusTerminated
	c.TerminatedBy = &req.TerminatedBy
	c.TerminatedAt = &now
	c.TerminationNote = &req.TerminationNote
	c.EffectiveTo = &todayStr
	c.UpdatedAt = now
	c.Version++

	_, err = tx.Exec(ctx, `
		UPDATE contracts
		SET status=$1, terminated_by=$2, terminated_at=$3, termination_note=$4,
		    effective_to=$5, version=$6, updated_at=$7
		WHERE contract_id=$8 AND tenant_id=$9`,
		string(c.Status), c.TerminatedBy, c.TerminatedAt, c.TerminationNote,
		c.EffectiveTo, c.Version, c.UpdatedAt, id, middleware.GetTenantID(ctx),
	)
	if err != nil {
		return nil, err
	}

	if err := s.snapshotVersion(ctx, tx, c, "Contract terminated: "+req.TerminationNote); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ListContractVersions returns the immutable version history of a contract.
func (s *PgStore) ListContractVersions(ctx context.Context, contractID string) ([]domain.ContractVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT version_id, contract_id, tenant_id, version_number, status, title,
		       COALESCE(description,''), `+effectiveDateColumns+`, change_summary, created_by, created_at
		FROM contract_versions
		WHERE contract_id=$1 AND tenant_id=$2
		ORDER BY version_number ASC`,
		contractID, middleware.GetTenantID(ctx),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ContractVersion
	for rows.Next() {
		var v domain.ContractVersion
		var status string
		if err := rows.Scan(
			&v.VersionID, &v.ContractID, &v.TenantID, &v.VersionNumber, &status, &v.Title,
			&v.Description, &v.EffectiveFrom, &v.EffectiveTo, &v.ChangeSummary, &v.CreatedBy, &v.CreatedAt,
		); err != nil {
			return nil, err
		}
		v.Status = domain.ContractStatus(status)
		out = append(out, v)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
