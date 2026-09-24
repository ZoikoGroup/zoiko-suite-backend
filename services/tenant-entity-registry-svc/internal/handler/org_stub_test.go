package handler_test

// The ORG-02/ORG-03 half of stubSvc.
//
// Same contract as the rest of the stub: every method returns s.err, so one
// stub drives the whole error-mapping table, and records the identifier the
// handler pulled out of the URL so a test can catch a handler reading the
// wrong chi parameter.
//
// gotAsOf and gotCommand exist because two of these handlers do real parsing
// before they reach the service — the as_of query parameter and the command
// path segment — and a test that only checked the status code could not tell
// a correctly parsed value from a silently defaulted one.

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

func (s *stubSvc) ExecuteTenantCommand(_ context.Context, id string, command domain.TenantCommand, req domain.ExecuteTenantCommandRequest) (*registry.TenantCommandResult, error) {
	s.gotID = id
	s.gotCommand = command
	s.gotCorrID = req.CorrelationID
	return &registry.TenantCommandResult{
		FromState:  domain.TenantLifecycleActive,
		ToState:    domain.TenantLifecycleSuspended,
		NewVersion: 2,
		Status:     domain.TenantStatusSuspended,
	}, s.err
}

func (s *stubSvc) ChangeDefaultLocale(_ context.Context, id string, req domain.ChangeDefaultLocaleRequest) (*domain.Tenant, error) {
	s.gotID = id
	s.gotCorrID = req.CorrelationID
	return &domain.Tenant{TenantID: id, PrimaryLocale: req.PrimaryLocale}, s.err
}

func (s *stubSvc) ListTenantLifecycleHistory(_ context.Context, id string) ([]*domain.TenantLifecycleEvent, error) {
	s.gotID = id
	return []*domain.TenantLifecycleEvent{{TenantID: id, ToState: domain.TenantLifecycleActive}}, s.err
}

func (s *stubSvc) GetTenantDefaults(_ context.Context, id string) (*domain.TenantDefaults, error) {
	s.gotID = id
	return &domain.TenantDefaults{TenantID: id}, s.err
}

func (s *stubSvc) BindTenantHost(_ context.Context, id string, req domain.BindTenantHostRequest) (*domain.TenantHostBinding, error) {
	s.gotID = id
	return &domain.TenantHostBinding{TenantID: id, Hostname: req.Hostname}, s.err
}

func (s *stubSvc) ListTenantHostBindings(_ context.Context, id string) ([]*domain.TenantHostBinding, error) {
	s.gotID = id
	return []*domain.TenantHostBinding{{TenantID: id}}, s.err
}

func (s *stubSvc) ResolveTenantByHost(_ context.Context, hostname string) (*domain.ResolvedTenantByHost, error) {
	s.gotHostname = hostname
	return &domain.ResolvedTenantByHost{Hostname: hostname, TenantID: tenantID}, s.err
}

// VerifyHostTenant returns hostTenantErr, NOT s.err.
//
// Kept separate on purpose: it runs before every command handler, so wiring it
// to s.err would make every error-mapping test fail at the host check instead
// of at the operation under test, and the test would still pass for the wrong
// reason.
func (s *stubSvc) VerifyHostTenant(_ context.Context, hostname, claimed string) error {
	s.gotHostname = hostname
	s.gotClaimedTenant = claimed
	return s.hostTenantErr
}

func (s *stubSvc) AmendLegalProfile(_ context.Context, id string, req domain.AmendLegalProfileRequest) (*domain.LegalEntityProfileVersion, error) {
	s.gotID = id
	s.gotCorrID = req.CorrelationID
	return &domain.LegalEntityProfileVersion{LegalEntityID: id, VersionNumber: 2}, s.err
}

func (s *stubSvc) ChangeLegalName(_ context.Context, id string, req domain.ChangeLegalNameRequest) (*domain.LegalEntityProfileVersion, error) {
	s.gotID = id
	s.gotCorrID = req.CorrelationID
	return &domain.LegalEntityProfileVersion{LegalEntityID: id, LegalName: req.LegalName, VersionNumber: 2}, s.err
}

func (s *stubSvc) ChangeRegisteredOffice(_ context.Context, id string, req domain.ChangeRegisteredOfficeRequest) (*domain.LegalEntityProfileVersion, error) {
	s.gotID = id
	s.gotCorrID = req.CorrelationID
	return &domain.LegalEntityProfileVersion{LegalEntityID: id, VersionNumber: 2}, s.err
}

func (s *stubSvc) ListEntityVersions(_ context.Context, id string) ([]*domain.LegalEntityProfileVersion, error) {
	s.gotID = id
	return []*domain.LegalEntityProfileVersion{{LegalEntityID: id, VersionNumber: 1}}, s.err
}

func (s *stubSvc) GetLegalEntityAsOf(_ context.Context, id string, asOf time.Time) (*domain.EntityAsOf, error) {
	s.gotID = id
	s.gotAsOf = asOf
	return &domain.EntityAsOf{LegalEntityID: id, AsOf: asOf}, s.err
}

func (s *stubSvc) FindByRegistryNumber(_ context.Context, registrationNumber, jurisdictionID string) ([]*domain.LegalEntity, error) {
	s.gotRegistryNumber = registrationNumber
	s.gotJurisdiction = jurisdictionID
	return []*domain.LegalEntity{{LegalEntityID: entityID}}, s.err
}

func (s *stubSvc) ListRegistryConflicts(_ context.Context, openOnly bool) ([]*domain.EntityRegistryConflict, error) {
	s.gotOpenOnly = openOnly
	return []*domain.EntityRegistryConflict{{ConflictID: "conflict-1"}}, s.err
}

func (s *stubSvc) ResolveRegistryConflict(_ context.Context, id string, _ domain.ResolveRegistryConflictRequest) error {
	s.gotID = id
	return s.err
}

// ── Verified maker-checker ──────────────────────────────────────────────────

func (s *stubSvc) ListApprovalRequests(context.Context, bool) ([]*domain.ApprovalRequest, error) {
	return []*domain.ApprovalRequest{}, s.err
}

func (s *stubSvc) GetApprovalRequest(_ context.Context, id string) (*domain.ApprovalRequest, error) {
	s.gotID = id
	return &domain.ApprovalRequest{ApprovalRequestID: id}, s.err
}

func (s *stubSvc) ApproveRequest(_ context.Context, id string, _ domain.ApproveRequestBody) (*domain.ApprovalOutcome, error) {
	s.gotID = id
	return &domain.ApprovalOutcome{}, s.err
}

func (s *stubSvc) RejectRequest(_ context.Context, id string, _ domain.RejectRequestBody) (*domain.ApprovalRequest, error) {
	s.gotID = id
	return &domain.ApprovalRequest{ApprovalRequestID: id}, s.err
}

// ── ORG-03 verification and merge ───────────────────────────────────────────

func (s *stubSvc) RequestEntityVerification(_ context.Context, id string, _ domain.RequestEntityVerificationRequest) error {
	s.gotID = id
	return s.err
}

func (s *stubSvc) ActivateLegalEntity(_ context.Context, id string, _ domain.ActivateLegalEntityRequest) (*domain.LegalEntity, error) {
	s.gotID = id
	return &domain.LegalEntity{LegalEntityID: id, EntityStatus: domain.EntityStatusActive}, s.err
}

func (s *stubSvc) MergeDuplicateCandidate(_ context.Context, id string, _ domain.MergeDuplicateCandidateRequest) error {
	s.gotID = id
	return s.err
}

func (s *stubSvc) UnmergeEntity(_ context.Context, id string, _ domain.UnmergeEntityRequest) error {
	s.gotID = id
	return s.err
}

func (s *stubSvc) ListEntityMergeRecords(_ context.Context, id string) ([]*domain.EntityMergeRecord, error) {
	s.gotID = id
	return []*domain.EntityMergeRecord{}, s.err
}
