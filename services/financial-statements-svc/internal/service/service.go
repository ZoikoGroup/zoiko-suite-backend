// Package service holds financial-statements-svc's business logic: the
// orchestration between the HTTP handler, the store, financial-close-svc's
// ACC-16 snapshot client and authorization-svc. No SQL and no HTTP framing
// lives here.
package service

import (
	"context"

	"go.uber.org/zap"

	"zoiko.io/financial-statements-svc/internal/clients"
	"zoiko.io/financial-statements-svc/internal/domain"
)

// Store is the persistence contract this service depends on. Every method
// that changes a FinancialStatementSet's state does so transactionally,
// together with whatever outbox event that state change implies — see
// internal/store's doc comment.
type Store interface {
	CreateSet(ctx context.Context, tenantID string, set *domain.FinancialStatementSet, correlationID string) error
	GetSet(ctx context.Context, tenantID, setID string) (*domain.FinancialStatementSet, error)
	ListSets(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.FinancialStatementSet, error)
	GetFacts(ctx context.Context, tenantID, setID string) ([]domain.StatementFact, error)
	GetFact(ctx context.Context, tenantID, factID string) (*domain.StatementFact, error)

	// RefreshFacts replaces every fact for setID and advances
	// last_refreshed_at/by — only legal from DRAFT (CmdRefreshDraft is a
	// self-loop; see domain/transition.go).
	RefreshFacts(ctx context.Context, tenantID, setID string, expectedVersion int64, facts []domain.StatementFact, actorID, correlationID string) error

	// Validate persists validationErrors. An empty slice moves the set to
	// VALIDATED (and enqueues the event); a non-empty one leaves it in DRAFT
	// with the errors recorded — a failed validation is a legitimate
	// outcome, not a transition error.
	Validate(ctx context.Context, tenantID, setID string, expectedVersion int64, validationErrors []string, correlationID string) error

	SubmitForReview(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error
	RequestChanges(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error

	// Approve enforces that actorID is not the preparer (see domain's SoD
	// note) and authored none of the set's disclosures, inside the same
	// transaction that loads the row, so the check and the write can never
	// race.
	Approve(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error

	// Certify enforces that actorID is not the preparer and authored none
	// of the set's disclosures (ZS-SVC-H-001 §4.1: "Preparer cannot
	// independently certify material statutory statements").
	Certify(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, reason, correlationID string) error

	Publish(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error

	// AddDisclosure appends d to setID under the set's row lock: it checks
	// the set is DRAFT (CmdAddDisclosure), assigns the next sequence, bumps
	// the set's version and enqueues the event, all in one transaction. A
	// non-empty d.IdempotencyKey already used on this set returns the
	// stored disclosure and replayed=true — checked BEFORE the state and
	// version checks, so a retry still succeeds after the set has moved on —
	// or CONTEXT_INVALID if the key was used for different content.
	AddDisclosure(ctx context.Context, tenantID, setID string, expectedVersion int64, d *domain.DisclosureInstance, correlationID string) (stored *domain.DisclosureInstance, replayed bool, err error)
	ListDisclosures(ctx context.Context, tenantID, setID string) ([]domain.DisclosureInstance, error)

	// Supersede marks old SUPERSEDED and inserts newSet, in one transaction.
	Supersede(ctx context.Context, tenantID, oldSetID string, expectedOldVersion int64, newSet *domain.FinancialStatementSet, actorID, correlationID string) error
}

// AuthZClient is the authorization-svc contract this service depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Authorization actions (ZS-IAM-001 style naming).
const (
	ActionStatementSetCreate   = "FINANCIAL_STATEMENT_SET_CREATE"
	ActionStatementSetView     = "FINANCIAL_STATEMENT_SET_VIEW"
	ActionStatementSetRefresh  = "FINANCIAL_STATEMENT_SET_REFRESH"
	ActionStatementSetValidate = "FINANCIAL_STATEMENT_SET_VALIDATE"
	ActionStatementSetReview   = "FINANCIAL_STATEMENT_SET_REVIEW"
	ActionStatementSetApprove  = "FINANCIAL_STATEMENT_SET_APPROVE"
	ActionStatementSetCertify  = "FINANCIAL_STATEMENT_SET_CERTIFY"
	ActionStatementSetPublish  = "FINANCIAL_STATEMENT_SET_PUBLISH"
	// ActionStatementSetDisclosureAdd is separate from REFRESH so a tenant
	// can let someone write notes without letting them rebuild the figures.
	ActionStatementSetDisclosureAdd = "FINANCIAL_STATEMENT_SET_DISCLOSURE_ADD"
)

// Service is REP-01's business logic.
type Service struct {
	store     Store
	snapshots clients.SnapshotReader
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, snapshots clients.SnapshotReader, authz AuthZClient, log *zap.Logger) *Service {
	return &Service{store: store, snapshots: snapshots, authz: authz, log: log}
}
