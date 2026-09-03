package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	periods      map[string]*domain.FiscalPeriod
	createErr    error
	getErr       error
	lockErr      error
	evidenceErr  error
	reopenErr    error
	reopenEvtErr error

	// Recorded so a test can assert what was actually signed and stored, rather
	// than only that the call did not error.
	evidence     []domain.CloseEvidence
	reopenEvents []domain.PeriodReopenEvent

	controlRuns  []domain.SubledgerControlRun
	createRunErr error
	listRunsErr  error

	schedules            map[string]*domain.AccrualSchedule
	createAccrualErr     error
	transitionAccrualErr error
	amendAccrualErr      error
	recognitions         map[string][]domain.RecognitionInstance // scheduleID -> instances
	createRecognitionErr error
	listRecognitionsErr  error
	activateErr          error
	completeErr          error

	prepaymentSchedules     map[string]*domain.PrepaymentSchedule
	createPrepaymentErr     error
	transitionPrepaymentErr error
	modifyPrepaymentErr     error
	prepaymentRecognitions  map[string][]domain.PrepaymentRecognitionInstance
	createPrepaymentRecErr  error
	listPrepaymentRecErr    error
	activatePrepaymentErr   error
	completePrepaymentErr   error

	allocationRules map[string]*domain.AllocationRule // keyed by rule_id (current version)
	allocationRuns  map[string]*domain.AllocationRun  // keyed by run_id
	createRuleErr   error
	approveRuleErr  error
	createRunErr2   error
	markCalcErr     error
	markPostedErr   error
	markFailedErr   error
	resultLinesErr  error

	fxRuns          map[string]*domain.FXRevaluationRun
	createFXRunErr  error
	approveFXErr    error
	markFXPostedErr error

	migrationBatches       map[string]*domain.MigrationBatch
	createMigrationErr     error
	transitionMigrationErr error

	snapshots             map[string]*domain.FinancialSnapshot
	createSnapshotErr     error
	transitionSnapshotErr error

	lineageEdges      []domain.LineageEdge
	recordEdgeErr     error
	postedJournalRefs []domain.PostedJournalRef
	postedRefsErr     error
	projectionStatus  map[string]*domain.LineageProjectionStatus
	upsertStatusErr   error
}

func newStubStore() *stubStore {
	return &stubStore{
		periods:                make(map[string]*domain.FiscalPeriod),
		schedules:              make(map[string]*domain.AccrualSchedule),
		recognitions:           make(map[string][]domain.RecognitionInstance),
		prepaymentSchedules:    make(map[string]*domain.PrepaymentSchedule),
		prepaymentRecognitions: make(map[string][]domain.PrepaymentRecognitionInstance),
		allocationRules:        make(map[string]*domain.AllocationRule),
		allocationRuns:         make(map[string]*domain.AllocationRun),
		fxRuns:                 make(map[string]*domain.FXRevaluationRun),
		migrationBatches:       make(map[string]*domain.MigrationBatch),
		snapshots:              make(map[string]*domain.FinancialSnapshot),
		projectionStatus:       make(map[string]*domain.LineageProjectionStatus),
	}
}

func (s *stubStore) CreateFiscalPeriod(_ context.Context, fp *domain.FiscalPeriod) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	for _, existing := range s.periods {
		if existing.LegalEntityID == fp.LegalEntityID && existing.PeriodName == fp.PeriodName {
			*fp = *existing
			return false, nil
		}
	}
	s.periods[fp.FiscalPeriodID] = fp
	return true, nil
}

func (s *stubStore) GetFiscalPeriod(_ context.Context, id string) (*domain.FiscalPeriod, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	fp, ok := s.periods[id]
	if !ok {
		return nil, domain.ErrFiscalPeriodNotFound
	}
	return fp, nil
}

func (s *stubStore) GetFiscalPeriodByName(_ context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	for _, fp := range s.periods {
		if fp.LegalEntityID == legalEntityID && fp.PeriodName == name {
			return fp, nil
		}
	}
	return nil, domain.ErrFiscalPeriodNotFound
}

func (s *stubStore) ListFiscalPeriods(_ context.Context, legalEntityID string) ([]domain.FiscalPeriod, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	var out []domain.FiscalPeriod
	for _, fp := range s.periods {
		if fp.LegalEntityID == legalEntityID {
			out = append(out, *fp)
		}
	}
	return out, nil
}

func (s *stubStore) LockFiscalPeriod(_ context.Context, id string, lockedAt time.Time, evidenceDocID string) error {
	if s.lockErr != nil {
		return s.lockErr
	}
	fp, ok := s.periods[id]
	if !ok {
		return domain.ErrFiscalPeriodNotFound
	}
	if fp.CloseStatus != "OPEN" {
		return domain.ErrPeriodAlreadyLocked
	}
	fp.CloseStatus = "LOCKED"
	t := lockedAt
	fp.CloseLockedAt = &t
	doc := evidenceDocID
	fp.EvidenceDocumentID = &doc
	return nil
}

func (s *stubStore) CreateCloseEvidence(_ context.Context, ev *domain.CloseEvidence) error {
	if s.evidenceErr != nil {
		return s.evidenceErr
	}
	s.evidence = append(s.evidence, *ev)
	return nil
}

func (s *stubStore) ReopenFiscalPeriod(_ context.Context, id string, reopenedAt time.Time) error {
	if s.reopenErr != nil {
		return s.reopenErr
	}
	fp, ok := s.periods[id]
	if !ok {
		return domain.ErrFiscalPeriodNotFound
	}
	if fp.CloseStatus != "LOCKED" {
		return domain.ErrPeriodNotLocked
	}
	fp.CloseStatus = "OPEN"
	fp.CloseLockedAt = nil
	fp.EvidenceDocumentID = nil
	return nil
}

func (s *stubStore) CreateReopenEvent(_ context.Context, ev *domain.PeriodReopenEvent) error {
	if s.reopenEvtErr != nil {
		return s.reopenEvtErr
	}
	s.reopenEvents = append(s.reopenEvents, *ev)
	return nil
}

func (s *stubStore) CreateControlRun(_ context.Context, run *domain.SubledgerControlRun) error {
	if s.createRunErr != nil {
		return s.createRunErr
	}
	s.controlRuns = append(s.controlRuns, *run)
	return nil
}

func (s *stubStore) ListControlRuns(_ context.Context, legalEntityID, fiscalPeriod string) ([]domain.SubledgerControlRun, error) {
	if s.listRunsErr != nil {
		return nil, s.listRunsErr
	}
	var out []domain.SubledgerControlRun
	for _, run := range s.controlRuns {
		if run.LegalEntityID == legalEntityID && run.FiscalPeriod == fiscalPeriod {
			out = append(out, run)
		}
	}
	return out, nil
}

func (s *stubStore) CreateAccrualSchedule(_ context.Context, sch *domain.AccrualSchedule) error {
	if s.createAccrualErr != nil {
		return s.createAccrualErr
	}
	cp := *sch
	s.schedules[sch.ScheduleID] = &cp
	return nil
}

func (s *stubStore) GetAccrualSchedule(_ context.Context, scheduleID string) (*domain.AccrualSchedule, error) {
	sch, ok := s.schedules[scheduleID]
	if !ok {
		return nil, domain.ErrAccrualNotFound
	}
	cp := *sch
	return &cp, nil
}

func (s *stubStore) ListAccrualSchedules(_ context.Context, legalEntityID string) ([]domain.AccrualSchedule, error) {
	var out []domain.AccrualSchedule
	for _, sch := range s.schedules {
		if sch.LegalEntityID == legalEntityID {
			out = append(out, *sch)
		}
	}
	return out, nil
}

func (s *stubStore) transitionAccrual(scheduleID, fromStatus, toStatus string) error {
	if s.transitionAccrualErr != nil {
		return s.transitionAccrualErr
	}
	sch, ok := s.schedules[scheduleID]
	if !ok {
		return domain.ErrAccrualNotFound
	}
	if sch.Status != fromStatus {
		return domain.ErrInvalidAccrualTransition
	}
	sch.Status = toStatus
	return nil
}

func (s *stubStore) SubmitAccrualSchedule(_ context.Context, scheduleID, principalID string, at time.Time) error {
	if err := s.transitionAccrual(scheduleID, domain.AccrualStatusDraft, domain.AccrualStatusPendingApproval); err != nil {
		return err
	}
	s.schedules[scheduleID].SubmittedAt, s.schedules[scheduleID].SubmittedByPrincipalID = &at, &principalID
	return nil
}

func (s *stubStore) ApproveAccrualSchedule(_ context.Context, scheduleID, principalID string, at time.Time) error {
	if err := s.transitionAccrual(scheduleID, domain.AccrualStatusPendingApproval, domain.AccrualStatusApproved); err != nil {
		return err
	}
	s.schedules[scheduleID].ApprovedAt, s.schedules[scheduleID].ApprovedByPrincipalID = &at, &principalID
	return nil
}

func (s *stubStore) ActivateAccrualSchedule(_ context.Context, scheduleID string) error {
	if s.activateErr != nil {
		return s.activateErr
	}
	err := s.transitionAccrual(scheduleID, domain.AccrualStatusApproved, domain.AccrualStatusActive)
	if errors.Is(err, domain.ErrInvalidAccrualTransition) {
		return nil
	}
	return err
}

func (s *stubStore) CompleteAccrualSchedule(_ context.Context, scheduleID string) error {
	if s.completeErr != nil {
		return s.completeErr
	}
	return s.transitionAccrual(scheduleID, domain.AccrualStatusActive, domain.AccrualStatusCompleted)
}

func (s *stubStore) CancelAccrualSchedule(_ context.Context, scheduleID, fromStatus, principalID string, at time.Time) error {
	if err := s.transitionAccrual(scheduleID, fromStatus, domain.AccrualStatusCancelled); err != nil {
		return err
	}
	s.schedules[scheduleID].CancelledAt, s.schedules[scheduleID].CancelledByPrincipalID = &at, &principalID
	return nil
}

func (s *stubStore) AmendAccrualSchedule(_ context.Context, scheduleID string, totalAmount float64, periodCount int) error {
	if s.amendAccrualErr != nil {
		return s.amendAccrualErr
	}
	sch, ok := s.schedules[scheduleID]
	if !ok {
		return domain.ErrAccrualNotFound
	}
	if sch.Status != domain.AccrualStatusApproved && sch.Status != domain.AccrualStatusActive {
		return domain.ErrInvalidAccrualTransition
	}
	sch.TotalAmount, sch.PeriodCount = totalAmount, periodCount
	return nil
}

func (s *stubStore) CreateRecognitionInstance(_ context.Context, inst *domain.RecognitionInstance) (bool, error) {
	if s.createRecognitionErr != nil {
		return false, s.createRecognitionErr
	}
	for _, existing := range s.recognitions[inst.ScheduleID] {
		if existing.FiscalPeriod == inst.FiscalPeriod {
			*inst = existing
			return false, nil
		}
	}
	s.recognitions[inst.ScheduleID] = append(s.recognitions[inst.ScheduleID], *inst)
	return true, nil
}

func (s *stubStore) ListRecognitionInstances(_ context.Context, scheduleID string) ([]domain.RecognitionInstance, error) {
	if s.listRecognitionsErr != nil {
		return nil, s.listRecognitionsErr
	}
	return s.recognitions[scheduleID], nil
}

func (s *stubStore) CreatePrepaymentSchedule(_ context.Context, sch *domain.PrepaymentSchedule) error {
	if s.createPrepaymentErr != nil {
		return s.createPrepaymentErr
	}
	cp := *sch
	s.prepaymentSchedules[sch.ScheduleID] = &cp
	return nil
}

func (s *stubStore) GetPrepaymentSchedule(_ context.Context, scheduleID string) (*domain.PrepaymentSchedule, error) {
	sch, ok := s.prepaymentSchedules[scheduleID]
	if !ok {
		return nil, domain.ErrPrepaymentNotFound
	}
	cp := *sch
	return &cp, nil
}

func (s *stubStore) ListPrepaymentSchedules(_ context.Context, legalEntityID string) ([]domain.PrepaymentSchedule, error) {
	var out []domain.PrepaymentSchedule
	for _, sch := range s.prepaymentSchedules {
		if sch.LegalEntityID == legalEntityID {
			out = append(out, *sch)
		}
	}
	return out, nil
}

func (s *stubStore) transitionPrepayment(scheduleID, fromStatus, toStatus string) error {
	if s.transitionPrepaymentErr != nil {
		return s.transitionPrepaymentErr
	}
	sch, ok := s.prepaymentSchedules[scheduleID]
	if !ok {
		return domain.ErrPrepaymentNotFound
	}
	if sch.Status != fromStatus {
		return domain.ErrInvalidPrepaymentTransition
	}
	sch.Status = toStatus
	return nil
}

func (s *stubStore) ApprovePrepaymentSchedule(_ context.Context, scheduleID, principalID string, at time.Time) error {
	if err := s.transitionPrepayment(scheduleID, domain.PrepaymentStatusDraft, domain.PrepaymentStatusApproved); err != nil {
		return err
	}
	s.prepaymentSchedules[scheduleID].ApprovedAt, s.prepaymentSchedules[scheduleID].ApprovedByPrincipalID = &at, &principalID
	return nil
}

func (s *stubStore) ActivatePrepaymentSchedule(_ context.Context, scheduleID string) error {
	if s.activatePrepaymentErr != nil {
		return s.activatePrepaymentErr
	}
	err := s.transitionPrepayment(scheduleID, domain.PrepaymentStatusApproved, domain.PrepaymentStatusActive)
	if errors.Is(err, domain.ErrInvalidPrepaymentTransition) {
		return nil
	}
	return err
}

func (s *stubStore) CompletePrepaymentSchedule(_ context.Context, scheduleID string) error {
	if s.completePrepaymentErr != nil {
		return s.completePrepaymentErr
	}
	return s.transitionPrepayment(scheduleID, domain.PrepaymentStatusActive, domain.PrepaymentStatusCompleted)
}

func (s *stubStore) TerminatePrepaymentSchedule(_ context.Context, scheduleID, fromStatus, principalID, reason, treatment string, at time.Time) error {
	if err := s.transitionPrepayment(scheduleID, fromStatus, domain.PrepaymentStatusTerminated); err != nil {
		return err
	}
	sch := s.prepaymentSchedules[scheduleID]
	sch.TerminatedAt, sch.TerminatedByPrincipalID = &at, &principalID
	sch.TerminationReason, sch.TerminationFinalTreatment = &reason, &treatment
	return nil
}

func (s *stubStore) ModifyFuturePrepaymentSchedule(_ context.Context, scheduleID string, totalAmount float64, periodCount int) error {
	if s.modifyPrepaymentErr != nil {
		return s.modifyPrepaymentErr
	}
	sch, ok := s.prepaymentSchedules[scheduleID]
	if !ok {
		return domain.ErrPrepaymentNotFound
	}
	if sch.Status != domain.PrepaymentStatusApproved && sch.Status != domain.PrepaymentStatusActive {
		return domain.ErrInvalidPrepaymentTransition
	}
	sch.TotalAmount, sch.PeriodCount = totalAmount, periodCount
	return nil
}

func (s *stubStore) CreatePrepaymentRecognition(_ context.Context, inst *domain.PrepaymentRecognitionInstance) (bool, error) {
	if s.createPrepaymentRecErr != nil {
		return false, s.createPrepaymentRecErr
	}
	for _, existing := range s.prepaymentRecognitions[inst.ScheduleID] {
		if existing.FiscalPeriod == inst.FiscalPeriod {
			*inst = existing
			return false, nil
		}
	}
	s.prepaymentRecognitions[inst.ScheduleID] = append(s.prepaymentRecognitions[inst.ScheduleID], *inst)
	return true, nil
}

func (s *stubStore) ListPrepaymentRecognitions(_ context.Context, scheduleID string) ([]domain.PrepaymentRecognitionInstance, error) {
	if s.listPrepaymentRecErr != nil {
		return nil, s.listPrepaymentRecErr
	}
	return s.prepaymentRecognitions[scheduleID], nil
}

func (s *stubStore) CreateAllocationRule(_ context.Context, rule *domain.AllocationRule) error {
	if s.createRuleErr != nil {
		return s.createRuleErr
	}
	cp := *rule
	s.allocationRules[rule.RuleID] = &cp
	return nil
}

func (s *stubStore) GetCurrentAllocationRule(_ context.Context, ruleID string) (*domain.AllocationRule, error) {
	rule, ok := s.allocationRules[ruleID]
	if !ok {
		return nil, domain.ErrAllocationRuleNotFound
	}
	cp := *rule
	return &cp, nil
}

func (s *stubStore) GetAllocationRuleVersion(_ context.Context, ruleVersionID string) (*domain.AllocationRule, error) {
	for _, rule := range s.allocationRules {
		if rule.RuleVersionID == ruleVersionID {
			cp := *rule
			return &cp, nil
		}
	}
	return nil, domain.ErrAllocationRuleNotFound
}

func (s *stubStore) ListAllocationRules(_ context.Context, legalEntityID string) ([]domain.AllocationRule, error) {
	var out []domain.AllocationRule
	for _, rule := range s.allocationRules {
		if rule.LegalEntityID == legalEntityID {
			out = append(out, *rule)
		}
	}
	return out, nil
}

func (s *stubStore) ApproveAllocationRule(_ context.Context, ruleVersionID, principalID string, at time.Time) error {
	if s.approveRuleErr != nil {
		return s.approveRuleErr
	}
	for _, rule := range s.allocationRules {
		if rule.RuleVersionID == ruleVersionID {
			if rule.Status != domain.AllocationRuleStatusDraft {
				return domain.ErrInvalidAllocationRuleTransition
			}
			rule.Status = domain.AllocationRuleStatusApproved
			rule.ApprovedAt, rule.ApprovedByPrincipalID = &at, &principalID
			return nil
		}
	}
	return domain.ErrAllocationRuleNotFound
}

func (s *stubStore) ActivateAllocationRule(_ context.Context, ruleVersionID string) error {
	for _, rule := range s.allocationRules {
		if rule.RuleVersionID == ruleVersionID && rule.Status == domain.AllocationRuleStatusApproved {
			rule.Status = domain.AllocationRuleStatusActive
			return nil
		}
	}
	return nil
}

func (s *stubStore) CreateAllocationRun(_ context.Context, run *domain.AllocationRun) error {
	if s.createRunErr2 != nil {
		return s.createRunErr2
	}
	cp := *run
	s.allocationRuns[run.RunID] = &cp
	return nil
}

func (s *stubStore) GetAllocationRunByRuleAndPeriod(_ context.Context, ruleID, fiscalPeriod string) (*domain.AllocationRun, error) {
	for _, run := range s.allocationRuns {
		if run.RuleID == ruleID && run.FiscalPeriod == fiscalPeriod {
			cp := *run
			return &cp, nil
		}
	}
	return nil, domain.ErrAllocationRunNotFound
}

func (s *stubStore) GetAllocationRun(_ context.Context, runID string) (*domain.AllocationRun, error) {
	run, ok := s.allocationRuns[runID]
	if !ok {
		return nil, domain.ErrAllocationRunNotFound
	}
	cp := *run
	return &cp, nil
}

func (s *stubStore) MarkAllocationRunCalculated(_ context.Context, runID string, sourceAmount float64, at time.Time) error {
	if s.markCalcErr != nil {
		return s.markCalcErr
	}
	run, ok := s.allocationRuns[runID]
	if !ok {
		return domain.ErrAllocationRunNotFound
	}
	run.Status, run.SourceAmount, run.CalculatedAt = domain.AllocationRunStatusCalculated, sourceAmount, &at
	return nil
}

func (s *stubStore) MarkAllocationRunPosted(_ context.Context, runID, journalID string, at time.Time) error {
	if s.markPostedErr != nil {
		return s.markPostedErr
	}
	run, ok := s.allocationRuns[runID]
	if !ok {
		return domain.ErrAllocationRunNotFound
	}
	run.Status, run.JournalID, run.PostedAt = domain.AllocationRunStatusPosted, &journalID, &at
	return nil
}

func (s *stubStore) MarkAllocationRunFailed(_ context.Context, runID, reason string) error {
	if s.markFailedErr != nil {
		return s.markFailedErr
	}
	run, ok := s.allocationRuns[runID]
	if !ok {
		return domain.ErrAllocationRunNotFound
	}
	run.Status, run.FailureReason = domain.AllocationRunStatusFailed, &reason
	return nil
}

func (s *stubStore) CreateAllocationResultLines(_ context.Context, runID string, lines []domain.AllocationResultLine) error {
	if s.resultLinesErr != nil {
		return s.resultLinesErr
	}
	run, ok := s.allocationRuns[runID]
	if !ok {
		return domain.ErrAllocationRunNotFound
	}
	run.ResultLines = append(run.ResultLines, lines...)
	return nil
}

func (s *stubStore) ListAllocationExceptions(_ context.Context, legalEntityID string) ([]domain.AllocationRun, error) {
	var out []domain.AllocationRun
	for _, run := range s.allocationRuns {
		if run.LegalEntityID == legalEntityID && run.Status == domain.AllocationRunStatusFailed {
			out = append(out, *run)
		}
	}
	return out, nil
}

func (s *stubStore) CreateFXRevaluationRun(_ context.Context, run *domain.FXRevaluationRun) error {
	if s.createFXRunErr != nil {
		return s.createFXRunErr
	}
	cp := *run
	s.fxRuns[run.RunID] = &cp
	return nil
}

func (s *stubStore) GetFXRevaluationRun(_ context.Context, runID string) (*domain.FXRevaluationRun, error) {
	run, ok := s.fxRuns[runID]
	if !ok {
		return nil, domain.ErrFXRevaluationRunNotFound
	}
	cp := *run
	return &cp, nil
}

func (s *stubStore) ListFXRevaluationRuns(_ context.Context, legalEntityID, fiscalPeriod string) ([]domain.FXRevaluationRun, error) {
	var out []domain.FXRevaluationRun
	for _, run := range s.fxRuns {
		if run.LegalEntityID == legalEntityID && run.FiscalPeriod == fiscalPeriod {
			out = append(out, *run)
		}
	}
	return out, nil
}

func (s *stubStore) ApproveFXRevaluationRun(_ context.Context, runID, principalID string, at time.Time) error {
	if s.approveFXErr != nil {
		return s.approveFXErr
	}
	run, ok := s.fxRuns[runID]
	if !ok {
		return domain.ErrFXRevaluationRunNotFound
	}
	if run.Status != domain.FXRevaluationStatusReview {
		return domain.ErrInvalidFXRevaluationTransition
	}
	run.Status = domain.FXRevaluationStatusApproved
	run.ApprovedAt, run.ApprovedByPrincipalID = &at, &principalID
	return nil
}

func (s *stubStore) MarkFXRevaluationPosted(_ context.Context, runID, journalID, principalID string, at time.Time) error {
	if s.markFXPostedErr != nil {
		return s.markFXPostedErr
	}
	run, ok := s.fxRuns[runID]
	if !ok {
		return domain.ErrFXRevaluationRunNotFound
	}
	if run.Status != domain.FXRevaluationStatusApproved {
		return domain.ErrInvalidFXRevaluationTransition
	}
	run.Status, run.JournalID, run.PostedAt, run.PostedByPrincipalID = domain.FXRevaluationStatusPosted, &journalID, &at, &principalID
	return nil
}

func (s *stubStore) CreateMigrationBatch(_ context.Context, b *domain.MigrationBatch) error {
	if s.createMigrationErr != nil {
		return s.createMigrationErr
	}
	cp := *b
	s.migrationBatches[b.BatchID] = &cp
	return nil
}

func (s *stubStore) GetMigrationBatchBySourceSystem(_ context.Context, legalEntityID, fiscalPeriod, sourceSystemName string) (*domain.MigrationBatch, error) {
	for _, b := range s.migrationBatches {
		if b.LegalEntityID == legalEntityID && b.FiscalPeriod == fiscalPeriod && b.SourceSystemName == sourceSystemName {
			cp := *b
			return &cp, nil
		}
	}
	return nil, domain.ErrMigrationBatchNotFound
}

func (s *stubStore) GetMigrationBatch(_ context.Context, batchID string) (*domain.MigrationBatch, error) {
	b, ok := s.migrationBatches[batchID]
	if !ok {
		return nil, domain.ErrMigrationBatchNotFound
	}
	cp := *b
	return &cp, nil
}

func (s *stubStore) transitionMigration(batchID, fromStatus, toStatus string) error {
	if s.transitionMigrationErr != nil {
		return s.transitionMigrationErr
	}
	b, ok := s.migrationBatches[batchID]
	if !ok {
		return domain.ErrMigrationBatchNotFound
	}
	if b.Status != fromStatus {
		return domain.ErrInvalidMigrationBatchTransition
	}
	b.Status = toStatus
	return nil
}

func (s *stubStore) MarkMigrationBatchValidated(_ context.Context, batchID string, at time.Time) error {
	if err := s.transitionMigration(batchID, domain.MigrationBatchStatusLoaded, domain.MigrationBatchStatusValidated); err != nil {
		return err
	}
	s.migrationBatches[batchID].ValidatedAt = &at
	return nil
}

func (s *stubStore) QuarantineMigrationBatch(_ context.Context, batchID, fromStatus, reason string) error {
	if err := s.transitionMigration(batchID, fromStatus, domain.MigrationBatchStatusQuarantined); err != nil {
		return err
	}
	s.migrationBatches[batchID].QuarantineReason = &reason
	return nil
}

func (s *stubStore) ApproveMigrationBatch(_ context.Context, batchID, principalID string, at time.Time) error {
	if err := s.transitionMigration(batchID, domain.MigrationBatchStatusValidated, domain.MigrationBatchStatusApproved); err != nil {
		return err
	}
	s.migrationBatches[batchID].ApprovedAt, s.migrationBatches[batchID].ApprovedByPrincipalID = &at, &principalID
	return nil
}

func (s *stubStore) MarkMigrationBatchPosted(_ context.Context, batchID, journalID string, at time.Time) error {
	if err := s.transitionMigration(batchID, domain.MigrationBatchStatusApproved, domain.MigrationBatchStatusPosted); err != nil {
		return err
	}
	s.migrationBatches[batchID].JournalID, s.migrationBatches[batchID].PostedAt = &journalID, &at
	return nil
}

func (s *stubStore) MarkMigrationBatchReconciled(_ context.Context, batchID string, at time.Time) error {
	if err := s.transitionMigration(batchID, domain.MigrationBatchStatusPosted, domain.MigrationBatchStatusReconciled); err != nil {
		return err
	}
	s.migrationBatches[batchID].ReconciledAt = &at
	return nil
}

func (s *stubStore) CertifyMigrationBatch(_ context.Context, batchID, principalID, reason string, at time.Time) error {
	if err := s.transitionMigration(batchID, domain.MigrationBatchStatusReconciled, domain.MigrationBatchStatusCertified); err != nil {
		return err
	}
	b := s.migrationBatches[batchID]
	b.CertifiedAt, b.CertifiedByPrincipalID, b.CertificationReason = &at, &principalID, &reason
	return nil
}

func (s *stubStore) ListQuarantinedMigrationBatches(_ context.Context, legalEntityID string) ([]domain.MigrationBatch, error) {
	var out []domain.MigrationBatch
	for _, b := range s.migrationBatches {
		if b.LegalEntityID == legalEntityID && b.Status == domain.MigrationBatchStatusQuarantined {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (s *stubStore) CreateFinancialSnapshot(_ context.Context, snap *domain.FinancialSnapshot) error {
	if s.createSnapshotErr != nil {
		return s.createSnapshotErr
	}
	cp := *snap
	s.snapshots[snap.SnapshotID] = &cp
	return nil
}

func (s *stubStore) GetFinancialSnapshot(_ context.Context, snapshotID string) (*domain.FinancialSnapshot, error) {
	snap, ok := s.snapshots[snapshotID]
	if !ok {
		return nil, domain.ErrFinancialSnapshotNotFound
	}
	cp := *snap
	return &cp, nil
}

func (s *stubStore) transitionSnapshot(snapshotID, fromStatus, toStatus string) error {
	if s.transitionSnapshotErr != nil {
		return s.transitionSnapshotErr
	}
	snap, ok := s.snapshots[snapshotID]
	if !ok {
		return domain.ErrFinancialSnapshotNotFound
	}
	if snap.Status != fromStatus {
		return domain.ErrInvalidSnapshotTransition
	}
	snap.Status = toStatus
	return nil
}

func (s *stubStore) SealFinancialSnapshot(_ context.Context, snapshotID, contentHash, signature string, at time.Time) error {
	if err := s.transitionSnapshot(snapshotID, domain.SnapshotStatusDraft, domain.SnapshotStatusSealed); err != nil {
		return err
	}
	snap := s.snapshots[snapshotID]
	snap.ContentHash, snap.Signature, snap.SealedAt = &contentHash, &signature, &at
	return nil
}

func (s *stubStore) CertifyFinancialSnapshot(_ context.Context, snapshotID, principalID, reason string, at time.Time) error {
	if err := s.transitionSnapshot(snapshotID, domain.SnapshotStatusSealed, domain.SnapshotStatusCertified); err != nil {
		return err
	}
	snap := s.snapshots[snapshotID]
	snap.CertifiedAt, snap.CertifiedByPrincipalID, snap.CertificationReason = &at, &principalID, &reason
	return nil
}

func (s *stubStore) SupersedeFinancialSnapshot(_ context.Context, snapshotID, fromStatus, newSnapshotID string, at time.Time) error {
	if err := s.transitionSnapshot(snapshotID, fromStatus, domain.SnapshotStatusSuperseded); err != nil {
		return err
	}
	snap := s.snapshots[snapshotID]
	snap.SupersededBySnapshotID, snap.SupersededAt = &newSnapshotID, &at
	return nil
}

func (s *stubStore) ListSnapshotSupersession(_ context.Context, legalEntityID, purpose string) ([]domain.FinancialSnapshot, error) {
	var out []domain.FinancialSnapshot
	for _, snap := range s.snapshots {
		if snap.LegalEntityID == legalEntityID && snap.Purpose == purpose {
			out = append(out, *snap)
		}
	}
	return out, nil
}

func (s *stubStore) RecordLineageEdge(_ context.Context, edge *domain.LineageEdge) error {
	if s.recordEdgeErr != nil {
		return s.recordEdgeErr
	}
	for _, e := range s.lineageEdges {
		if e.FromType == edge.FromType && e.FromID == edge.FromID && e.ToType == edge.ToType && e.ToID == edge.ToID {
			return nil // idempotent no-op, mirrors the real UNIQUE constraint
		}
	}
	s.lineageEdges = append(s.lineageEdges, *edge)
	return nil
}

func (s *stubStore) ListLineageEdgesTo(_ context.Context, toType, toID string) ([]domain.LineageEdge, error) {
	var out []domain.LineageEdge
	for _, e := range s.lineageEdges {
		if e.ToType == toType && e.ToID == toID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *stubStore) ListPostedJournalRefs(_ context.Context, legalEntityID string) ([]domain.PostedJournalRef, error) {
	if s.postedRefsErr != nil {
		return nil, s.postedRefsErr
	}
	return s.postedJournalRefs, nil
}

func (s *stubStore) GetLineageProjectionStatus(_ context.Context, legalEntityID string) (*domain.LineageProjectionStatus, error) {
	if st, ok := s.projectionStatus[legalEntityID]; ok {
		cp := *st
		return &cp, nil
	}
	return &domain.LineageProjectionStatus{LegalEntityID: legalEntityID, Status: domain.LineageProjectionCurrent}, nil
}

func (s *stubStore) UpsertLineageProjectionStatus(_ context.Context, legalEntityID, status string, degradedReason *string, at *time.Time) error {
	if s.upsertStatusErr != nil {
		return s.upsertStatusErr
	}
	existing, ok := s.projectionStatus[legalEntityID]
	lastRebuiltAt := at
	if ok && at == nil {
		lastRebuiltAt = existing.LastRebuiltAt
	}
	s.projectionStatus[legalEntityID] = &domain.LineageProjectionStatus{
		LegalEntityID: legalEntityID, Status: status, DegradedReason: degradedReason, LastRebuiltAt: lastRebuiltAt,
	}
	return nil
}

type stubPublisher struct {
	started, blocked, closed, reopened, controlException int
	lastControlExceptionRun                              domain.SubledgerControlRun
}

func (p *stubPublisher) PublishCloseStarted(_ context.Context, _, _ string, _ domain.FiscalPeriod) {
	p.started++
}
func (p *stubPublisher) PublishCloseBlocked(_ context.Context, _, _ string, _ domain.FiscalPeriod, _ []string) {
	p.blocked++
}
func (p *stubPublisher) PublishClosed(_ context.Context, _, _ string, _ domain.FiscalPeriod, _ string) {
	p.closed++
}
func (p *stubPublisher) PublishReopened(_ context.Context, _, _ string, _ domain.FiscalPeriod, _ string) {
	p.reopened++
}
func (p *stubPublisher) PublishSubledgerControlException(_ context.Context, _, _ string, run domain.SubledgerControlRun) {
	p.controlException++
	p.lastControlExceptionRun = run
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubClients struct {
	unpostedCount int
	unpostedErr   error
	unsettledAP   int
	apErr         error
	unsettledAR   int
	arErr         error
	uploadErr     error
	trialBalances map[string]float64
	trialBalErr   error

	apPeriodStart, apPeriodEnd time.Time
	arPeriodStart, arPeriodEnd time.Time

	controlAccountCodes map[string]string // mapping key -> account code
	mappingErr          error
	apSubledgerTotal    float64
	apSubledgerErr      error
	arSubledgerTotal    float64
	arSubledgerErr      error

	postJournalErr   error
	postedJournalID  string
	lastPostedAmount float64
	lastPostedPeriod string
	postJournalCalls int

	accountStatuses      map[string]string // account_code -> status
	accountStatusErr     error
	postAllocationErr    error
	postedAllocJournalID string
	lastAllocDebitLines  []domain.AllocationJournalLine
	postAllocationCalls  int

	accountTypes         map[string]string // account_code -> account_type
	accountTypeErr       error
	postMultiLineErr     error
	postedMultiJournalID string
	lastMultiLines       []domain.JournalLineInput
	postMultiLineCalls   int
}

func (c *stubClients) GetUnpostedJournalsCount(_ context.Context, _, _, _ string) (int, error) {
	return c.unpostedCount, c.unpostedErr
}
func (c *stubClients) CompileTrialBalance(_ context.Context, _, _, _, _ string) (map[string]float64, error) {
	if c.trialBalErr != nil {
		return nil, c.trialBalErr
	}
	if c.trialBalances != nil {
		return c.trialBalances, nil
	}
	return map[string]float64{"1000-Cash": 10000.00}, nil
}

// The AP/AR counts take the period bounds, and the stub RECORDS them: the
// defect being guarded against is the handler failing to pass the period
// through, which a stub that ignored its arguments could not catch.
func (c *stubClients) GetUnsettledAPInvoicesCount(_ context.Context, _, _ string, periodStart, periodEnd time.Time) (int, error) {
	c.apPeriodStart, c.apPeriodEnd = periodStart, periodEnd
	return c.unsettledAP, c.apErr
}
func (c *stubClients) GetUnsettledARInvoicesCount(_ context.Context, _, _ string, periodStart, periodEnd time.Time) (int, error) {
	c.arPeriodStart, c.arPeriodEnd = periodStart, periodEnd
	return c.unsettledAR, c.arErr
}
func (c *stubClients) UploadCloseEvidence(_ context.Context, _, _, _ string, _ map[string]float64, _ string) (string, error) {
	if c.uploadErr != nil {
		return "", c.uploadErr
	}
	return "doc-evidence-uuid-001", nil
}

func (c *stubClients) GetControlAccountCode(_ context.Context, _, mappingKey string) (string, error) {
	if c.mappingErr != nil {
		return "", c.mappingErr
	}
	if code, ok := c.controlAccountCodes[mappingKey]; ok {
		return code, nil
	}
	return "", domain.ErrControlAccountMappingNotFound
}

func (c *stubClients) GetAPSubledgerTotal(_ context.Context, _, _ string) (float64, error) {
	return c.apSubledgerTotal, c.apSubledgerErr
}

func (c *stubClients) GetARSubledgerTotal(_ context.Context, _, _ string) (float64, error) {
	return c.arSubledgerTotal, c.arSubledgerErr
}

func (c *stubClients) PostAccrualRecognitionJournal(_ context.Context, _, _, fiscalPeriod, _, _, _, _, _ string, amount float64) (string, error) {
	c.postJournalCalls++
	c.lastPostedAmount = amount
	c.lastPostedPeriod = fiscalPeriod
	if c.postJournalErr != nil {
		return "", c.postJournalErr
	}
	if c.postedJournalID != "" {
		return c.postedJournalID, nil
	}
	return "journal-uuid-001", nil
}

func (c *stubClients) GetAccountStatus(_ context.Context, _, _, accountCode string) (string, error) {
	if c.accountStatusErr != nil {
		return "", c.accountStatusErr
	}
	if status, ok := c.accountStatuses[accountCode]; ok {
		return status, nil
	}
	return "", domain.ErrRecipientAccountInvalid
}

func (c *stubClients) PostAllocationJournal(_ context.Context, _, _, _, _, _, _, _ string, _ float64, debitLines []domain.AllocationJournalLine) (string, error) {
	c.postAllocationCalls++
	c.lastAllocDebitLines = debitLines
	if c.postAllocationErr != nil {
		return "", c.postAllocationErr
	}
	if c.postedAllocJournalID != "" {
		return c.postedAllocJournalID, nil
	}
	return "journal-alloc-001", nil
}

func (c *stubClients) GetAccountType(_ context.Context, _, _, accountCode string) (string, error) {
	if c.accountTypeErr != nil {
		return "", c.accountTypeErr
	}
	if t, ok := c.accountTypes[accountCode]; ok {
		return t, nil
	}
	return "", domain.ErrNonMonetaryItemIncluded
}

func (c *stubClients) PostMultiLineJournal(_ context.Context, _, _, _, _, _, _ string, lines []domain.JournalLineInput) (string, error) {
	c.postMultiLineCalls++
	c.lastMultiLines = lines
	if c.postMultiLineErr != nil {
		return "", c.postMultiLineErr
	}
	if c.postedMultiJournalID != "" {
		return c.postedMultiJournalID, nil
	}
	return "journal-fx-001", nil
}

// ── router factory ─────────────────────────────────────────────────────────────

// testSigningKey stands in for CLOSE_SIGNING_KEY. Deliberately NOT the tenant
// id: keying the signature with the tenant was the defect, and a test that
// reused it could not tell the fix from the bug.
var testSigningKey = []byte("test-close-signing-key")

const testTenantID = "tenant-abc"

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, cl *stubClients) chi.Router {
	r := chi.NewRouter()
	// The real TenantContext middleware reading X-Tenant-Id, not a hardcoded
	// context stuffer: whether a request carries a verified tenant scope is now
	// part of what these tests cover, and a stuffer made every request look
	// scoped no matter what it sent.
	r.Use(middleware.TenantContext())
	h := handler.New(s, pub, authz, cl, testSigningKey, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	return doReqAs(r, method, path, body, principalID, testTenantID)
}

// doReqAs sends a request as a caller the gateway verified as tenantID. An
// empty tenantID sends no X-Tenant-Id at all.
func doReqAs(r chi.Router, method, path string, body any, principalID, tenantID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// Compile-time proof the stubs still satisfy the contracts the handler depends
// on — a stub that has silently fallen behind an interface is how a green suite
// stops meaning anything.
var (
	_ handler.Store   = (*stubStore)(nil)
	_ handler.Clients = (*stubClients)(nil)
)

// ── CreateFiscalPeriod tests ──────────────────────────────────────────────────

func TestCreateFiscalPeriod_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/", map[string]any{
		"legal_entity_id": "le-1",
		"period_name":     "2024-Q1",
		"period_start":    "2024-01-01T00:00:00Z",
		"period_end":      "2024-03-31T23:59:59Z",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateFiscalPeriod_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/", map[string]any{
		"legal_entity_id": "le-1",
		"period_name":     "2024-Q1",
		"period_start":    "2024-01-01T00:00:00Z",
		"period_end":      "2024-03-31T23:59:59Z",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestCreateFiscalPeriod_MissingFields(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/", map[string]any{
		"legal_entity_id": "le-1",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreateFiscalPeriod_HappyPath(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/", map[string]any{
		"legal_entity_id": "le-1",
		"period_name":     "2024-Q1",
		"period_start":    "2024-01-01T00:00:00Z",
		"period_end":      "2024-03-31T23:59:59Z",
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var fp domain.FiscalPeriod
	if err := json.NewDecoder(rr.Body).Decode(&fp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fp.CloseStatus != "OPEN" {
		t.Errorf("expected OPEN got %q", fp.CloseStatus)
	}
	if fp.TenantID != "tenant-abc" {
		t.Errorf("tenant isolation: expected tenant-abc got %q", fp.TenantID)
	}
}

func TestCreateFiscalPeriod_Retried_ReturnsOriginalNotDuplicate(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	body := map[string]any{
		"legal_entity_id": "le-1",
		"period_name":     "2024-Q1",
		"period_start":    "2024-01-01T00:00:00Z",
		"period_end":      "2024-03-31T23:59:59Z",
	}

	first := doReq(r, http.MethodPost, "/v1/close/periods/", body, "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	var firstFP domain.FiscalPeriod
	_ = json.NewDecoder(first.Body).Decode(&firstFP)

	retry := doReq(r, http.MethodPost, "/v1/close/periods/", body, "principal-1")
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call for the same (legal_entity_id, period_name), got %d: %s", retry.Code, retry.Body.String())
	}
	var retryFP domain.FiscalPeriod
	_ = json.NewDecoder(retry.Body).Decode(&retryFP)
	if retryFP.FiscalPeriodID != firstFP.FiscalPeriodID {
		t.Fatalf("retried call resolved to a different fiscal_period_id (%s) than the original (%s)", retryFP.FiscalPeriodID, firstFP.FiscalPeriodID)
	}
	if len(s.periods) != 1 {
		t.Fatalf("expected exactly 1 fiscal period to exist, got %d — a retry must not create a duplicate", len(s.periods))
	}
}

// ── GetPeriodStatus tests ─────────────────────────────────────────────────────

func TestGetPeriodStatus_MissingParams(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/close/periods/status", nil, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestGetPeriodStatus_NotFound_DefaultsOpen(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/close/periods/status?legal_entity_id=le-1&period_name=2024-Q1", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	var resp map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["close_status"] != "OPEN" {
		t.Errorf("expected OPEN default got %q", resp["close_status"])
	}
}

func TestGetPeriodStatus_LockedPeriod(t *testing.T) {
	s := newStubStore()
	docID := "doc-001"
	s.periods["fp-1"] = &domain.FiscalPeriod{
		FiscalPeriodID:     "fp-1",
		TenantID:           "tenant-abc",
		LegalEntityID:      "le-1",
		PeriodName:         "2024-Q1",
		CloseStatus:        "LOCKED",
		EvidenceDocumentID: &docID,
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/close/periods/status?legal_entity_id=le-1&period_name=2024-Q1", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	var resp map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["close_status"] != "LOCKED" {
		t.Errorf("expected LOCKED got %q", resp["close_status"])
	}
}

// ── LockPeriod tests ──────────────────────────────────────────────────────────

func TestLockPeriod_UnsettledAPBlocksClose(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "OPEN",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{unsettledAP: 1})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 with unsettled AP invoices outstanding, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLockPeriod_UnsettledARBlocksClose(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "OPEN",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{unsettledAR: 1})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 with unsettled AR invoices outstanding, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLockPeriod_GLQueryFails_FailsClosed(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "OPEN",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{unpostedErr: domain.ErrGLServiceUnavailable})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when general-ledger-svc is unreachable (fail closed), got %d: %s", rr.Code, rr.Body.String())
	}
	if s.periods["fp-open"].CloseStatus != "OPEN" {
		t.Fatalf("period must remain OPEN when a readiness check couldn't be performed, got %s", s.periods["fp-open"].CloseStatus)
	}
}

func TestLockPeriod_TrialBalanceCompileFails_FailsClosed(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "OPEN",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{trialBalErr: domain.ErrGLServiceUnavailable})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when trial balance compilation fails, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.periods["fp-open"].CloseStatus != "OPEN" {
		t.Fatalf("period must remain OPEN when evidence generation failed, got %s", s.periods["fp-open"].CloseStatus)
	}
}

func TestLockPeriod_EvidenceUploadFails_FailsClosed(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "OPEN",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{uploadErr: domain.ErrVaultServiceUnavailable})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when document-vault-svc upload fails, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.periods["fp-open"].CloseStatus != "OPEN" {
		t.Fatalf("period must remain OPEN when close evidence couldn't be recorded, got %s", s.periods["fp-open"].CloseStatus)
	}
}

func TestLockPeriod_AuthorizationDenied_Returns(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "OPEN",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

// ── ListFiscalPeriods tests ───────────────────────────────────────────────────

func TestListFiscalPeriods_RequiresLegalEntityID(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/close/periods/", nil, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without legal_entity_id, got %d", rr.Code)
	}
}

func TestLockPeriod_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/nonexistent-id/lock", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
}

func TestLockPeriod_AlreadyLocked(t *testing.T) {
	s := newStubStore()
	s.periods["fp-locked"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-locked",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "LOCKED",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-locked/lock", nil, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d", rr.Code)
	}
}

func TestLockPeriod_ReadinessBlocked_UnpostedJournals(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "OPEN",
	}
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubClients{unpostedCount: 3})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (blocked) got %d: %s", rr.Code, rr.Body.String())
	}
	if pub.blocked != 1 {
		t.Errorf("expected 1 CloseBlocked event got %d", pub.blocked)
	}
	var resp domain.ReadinessCheckResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.IsReady {
		t.Error("expected is_ready=false")
	}
	if len(resp.BlockingIssues) == 0 {
		t.Error("expected blocking issues")
	}
}

func TestLockPeriod_HappyPath(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		PeriodStart:    time.Now().Add(-30 * 24 * time.Hour),
		PeriodEnd:      time.Now().Add(-1 * time.Hour),
		CloseStatus:    "OPEN",
	}
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.PeriodLockResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CloseStatus != "LOCKED" {
		t.Errorf("expected LOCKED got %q", resp.CloseStatus)
	}
	if resp.EvidenceDocumentID == "" {
		t.Error("evidence_document_id must be set")
	}
	if resp.VerificationHash == "" {
		t.Error("verification_hash must be set")
	}
	if pub.started != 1 {
		t.Errorf("expected 1 CloseStarted event got %d", pub.started)
	}
	if pub.closed != 1 {
		t.Errorf("expected 1 Closed event got %d", pub.closed)
	}
}

// ── ReopenPeriod Tests (ACC-14 invariant #6) ─────────────────────────────────────

func TestReopenPeriod_HappyPath(t *testing.T) {
	s := newStubStore()
	s.periods["fp-locked"] = &domain.FiscalPeriod{
		FiscalPeriodID:     "fp-locked",
		TenantID:           "tenant-abc",
		LegalEntityID:      "le-1",
		PeriodName:         "2024-Q1",
		CloseStatus:        "LOCKED",
		EvidenceDocumentID: strPtr("doc-1"),
	}
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-locked/reopen",
		domain.ReopenPeriodRequest{Reason: "material misstatement found in AP accrual, correcting entry required"}, "principal-1")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.FiscalPeriod
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CloseStatus != "OPEN" {
		t.Errorf("expected OPEN got %q", resp.CloseStatus)
	}
	if resp.EvidenceDocumentID != nil {
		t.Error("expected evidence_document_id cleared on reopen")
	}
	if pub.reopened != 1 {
		t.Errorf("expected 1 Reopened event got %d", pub.reopened)
	}
	if len(s.reopenEvents) != 1 {
		t.Fatalf("expected 1 permanent reopen event recorded, got %d", len(s.reopenEvents))
	}
	if s.reopenEvents[0].Reason == "" || s.reopenEvents[0].ReopenedByPrincipalID != "principal-1" {
		t.Errorf("expected reopen event to carry the reason and acting principal, got %+v", s.reopenEvents[0])
	}
	// The period's OWN prior close evidence in close_evidences must survive —
	// only the pointer on the period is cleared, never the historical row.
	// (stubStore doesn't model close_evidences deletion, so this asserts the
	// handler never attempted to touch it — no evidence-deletion call exists
	// anywhere in Store, by design.)
}

func TestReopenPeriod_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	s.periods["fp-locked"] = &domain.FiscalPeriod{FiscalPeriodID: "fp-locked", TenantID: "tenant-abc", LegalEntityID: "le-1", CloseStatus: "LOCKED"}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-locked/reopen", domain.ReopenPeriodRequest{Reason: ""}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
	if len(s.reopenEvents) != 0 {
		t.Error("expected no reopen event recorded when reason is missing")
	}
}

// TestReopenPeriod_NotLocked_Returns422 proves an OPEN (or otherwise
// non-LOCKED) period cannot be "reopened" — this is a LOCKED-only
// transition, mirroring LockFiscalPeriod's own OPEN-only guard.
func TestReopenPeriod_NotLocked_Returns422(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{FiscalPeriodID: "fp-open", TenantID: "tenant-abc", LegalEntityID: "le-1", CloseStatus: "OPEN"}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/reopen", domain.ReopenPeriodRequest{Reason: "test"}, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestReopenPeriod_UsesDistinctAuthorizationAction proves reopen is
// gated by its OWN action (actionPeriodReopen), not silently reusing
// actionCloseInitiate — a principal denied specifically for reopen must be
// refused even though closing/locking uses a different action entirely.
func TestReopenPeriod_UsesDistinctAuthorizationAction(t *testing.T) {
	s := newStubStore()
	s.periods["fp-locked"] = &domain.FiscalPeriod{FiscalPeriodID: "fp-locked", TenantID: "tenant-abc", LegalEntityID: "le-1", CloseStatus: "LOCKED"}
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(s, &stubPublisher{}, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	rr := doReq(rt, http.MethodPost, "/v1/close/periods/fp-locked/reopen", domain.ReopenPeriodRequest{Reason: "test"}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "PERIOD_REOPEN" {
		t.Errorf("expected authz checked against PERIOD_REOPEN, got %q", seenAction)
	}
}

func TestReopenPeriod_EventNotRecorded_Returns500(t *testing.T) {
	s := newStubStore()
	s.periods["fp-locked"] = &domain.FiscalPeriod{FiscalPeriodID: "fp-locked", TenantID: "tenant-abc", LegalEntityID: "le-1", CloseStatus: "LOCKED"}
	s.reopenEvtErr = domain.ErrStoreUnavailable
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-locked/reopen", domain.ReopenPeriodRequest{Reason: "test"}, "principal-1")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ACC-06 (Subledger Control) ──────────────────────────────────────────────────

func TestRunSubledgerControl_APMatched_RecordsRunAndDoesNotPublish(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	cl := &stubClients{
		controlAccountCodes: map[string]string{"AP_CONTROL": "2000-AP"},
		apSubledgerTotal:    5000.00,
		trialBalances:       map[string]float64{"2000-AP": 5000.00},
	}
	r := newRouter(s, pub, &stubAuthZ{}, cl)

	req := domain.RunSubledgerControlRequest{
		LegalEntityID:            "le-1",
		FiscalPeriod:             "2026-08",
		Subledger:                "AP",
		ControlAccountMappingKey: "AP_CONTROL",
	}
	rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/", req, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var run domain.SubledgerControlRun
	if err := json.NewDecoder(rr.Body).Decode(&run); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if run.Status != "MATCHED" {
		t.Errorf("expected MATCHED, got %q", run.Status)
	}
	if run.ControlAccountCode != "2000-AP" {
		t.Errorf("expected resolved account code 2000-AP, got %q", run.ControlAccountCode)
	}
	if len(s.controlRuns) != 1 {
		t.Fatalf("expected 1 persisted control run, got %d", len(s.controlRuns))
	}
	if pub.controlException != 0 {
		t.Errorf("expected no exception published for a MATCHED run, got %d", pub.controlException)
	}
}

func TestRunSubledgerControl_ARMismatch_RecordsExceptionAndPublishes(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	cl := &stubClients{
		controlAccountCodes: map[string]string{"AR_CONTROL": "1200-AR"},
		arSubledgerTotal:    12000.00,
		trialBalances:       map[string]float64{"1200-AR": 9500.00},
	}
	r := newRouter(s, pub, &stubAuthZ{}, cl)

	req := domain.RunSubledgerControlRequest{
		LegalEntityID:            "le-1",
		FiscalPeriod:             "2026-08",
		Subledger:                "AR",
		ControlAccountMappingKey: "AR_CONTROL",
	}
	rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/", req, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var run domain.SubledgerControlRun
	if err := json.NewDecoder(rr.Body).Decode(&run); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if run.Status != "EXCEPTION" {
		t.Errorf("expected EXCEPTION, got %q", run.Status)
	}
	if run.DifferenceAmount != 2500.00 {
		t.Errorf("expected difference 2500.00, got %v", run.DifferenceAmount)
	}
	if pub.controlException != 1 {
		t.Fatalf("expected exception published exactly once, got %d", pub.controlException)
	}
	if pub.lastControlExceptionRun.ControlRunID != run.ControlRunID {
		t.Errorf("published exception does not match the persisted run")
	}
}

func TestRunSubledgerControl_InvalidSubledger_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	req := domain.RunSubledgerControlRequest{
		LegalEntityID:            "le-1",
		FiscalPeriod:             "2026-08",
		Subledger:                "GL",
		ControlAccountMappingKey: "AP_CONTROL",
	}
	rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/", req, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRunSubledgerControl_UnmappedControlAccount_Returns422(t *testing.T) {
	// No mapping key registered — ACC-06 must never guess which GL account a
	// subledger reconciles against, and must not silently reconcile against
	// nothing either.
	cl := &stubClients{controlAccountCodes: map[string]string{}}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.RunSubledgerControlRequest{
		LegalEntityID:            "le-1",
		FiscalPeriod:             "2026-08",
		Subledger:                "AP",
		ControlAccountMappingKey: "NEVER_SET",
	}
	rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/", req, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRunSubledgerControl_NoTrialBalanceLineForControlAccount_Returns422(t *testing.T) {
	// The mapping resolves, but the account never posted this period — no
	// balance is not zero, and must not read as a silent match.
	cl := &stubClients{
		controlAccountCodes: map[string]string{"AP_CONTROL": "2000-AP"},
		trialBalances:       map[string]float64{"1000-Cash": 100.00},
	}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.RunSubledgerControlRequest{
		LegalEntityID:            "le-1",
		FiscalPeriod:             "2026-08",
		Subledger:                "AP",
		ControlAccountMappingKey: "AP_CONTROL",
	}
	rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/", req, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRunSubledgerControl_SubledgerPageTruncated_Returns503(t *testing.T) {
	cl := &stubClients{
		controlAccountCodes: map[string]string{"AP_CONTROL": "2000-AP"},
		apSubledgerErr:      domain.ErrSubledgerPageTruncated,
	}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.RunSubledgerControlRequest{
		LegalEntityID:            "le-1",
		FiscalPeriod:             "2026-08",
		Subledger:                "AP",
		ControlAccountMappingKey: "AP_CONTROL",
	}
	rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/", req, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRunSubledgerControl_UsesDistinctAuthorizationAction(t *testing.T) {
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	cl := &stubClients{controlAccountCodes: map[string]string{"AP_CONTROL": "2000-AP"}}
	h := handler.New(newStubStore(), &stubPublisher{}, authz, cl, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	req := domain.RunSubledgerControlRequest{
		LegalEntityID:            "le-1",
		FiscalPeriod:             "2026-08",
		Subledger:                "AP",
		ControlAccountMappingKey: "AP_CONTROL",
	}
	rr := doReq(rt, http.MethodPost, "/v1/subledger-control/runs/", req, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "SUBLEDGER_CONTROL_RUN" {
		t.Errorf("expected authz checked against SUBLEDGER_CONTROL_RUN, got %q", seenAction)
	}
}

func TestListSubledgerControlRuns_ReturnsOnlyMatchingEntityAndPeriod(t *testing.T) {
	s := newStubStore()
	s.controlRuns = []domain.SubledgerControlRun{
		{ControlRunID: "run-1", LegalEntityID: "le-1", FiscalPeriod: "2026-08", Subledger: "AP"},
		{ControlRunID: "run-2", LegalEntityID: "le-1", FiscalPeriod: "2026-07", Subledger: "AP"},
		{ControlRunID: "run-3", LegalEntityID: "le-2", FiscalPeriod: "2026-08", Subledger: "AR"},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/subledger-control/runs/?legal_entity_id=le-1&fiscal_period=2026-08", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var runs []domain.SubledgerControlRun
	if err := json.NewDecoder(rr.Body).Decode(&runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 || runs[0].ControlRunID != "run-1" {
		t.Fatalf("expected only run-1, got %+v", runs)
	}
}

// ── ACC-07 (Accruals) ────────────────────────────────────────────────────────────

func createApprovedAccrual(t *testing.T, s *stubStore, r chi.Router, totalAmount float64, periodCount int, start string) string {
	t.Helper()
	req := domain.CreateAccrualRequest{
		LegalEntityID:     "le-1",
		Description:       "Q3 audit fee accrual",
		PolicyVersion:     "policy-v1",
		TotalAmount:       totalAmount,
		StartFiscalPeriod: start,
		PeriodCount:       periodCount,
		DebitAccountCode:  "6100-AuditFee",
		CreditAccountCode: "2100-AccruedLiabilities",
	}
	rr := doReq(r, http.MethodPost, "/v1/accruals/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var sch domain.AccrualSchedule
	_ = json.NewDecoder(rr.Body).Decode(&sch)

	if rr := doReq(r, http.MethodPost, "/v1/accruals/"+sch.ScheduleID+"/submit", nil, "preparer-1"); rr.Code != http.StatusOK {
		t.Fatalf("submit failed: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, "/v1/accruals/"+sch.ScheduleID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}
	return sch.ScheduleID
}

func TestCreateAccrual_InvalidAmount_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	req := domain.CreateAccrualRequest{
		LegalEntityID: "le-1", Description: "x", PolicyVersion: "v1",
		TotalAmount: 0, StartFiscalPeriod: "2026-01", PeriodCount: 3,
		DebitAccountCode: "6100", CreditAccountCode: "2100",
	}
	rr := doReq(r, http.MethodPost, "/v1/accruals/", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAccrualLifecycle_CreateSubmitApprove(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedAccrual(t, s, r, 1200.00, 3, "2026-01")
	sch, err := s.GetAccrualSchedule(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sch.Status != domain.AccrualStatusApproved {
		t.Errorf("expected APPROVED, got %q", sch.Status)
	}
}

func TestApproveAccrual_UsesDistinctAuthorizationAction(t *testing.T) {
	s := newStubStore()
	s.schedules["sch-1"] = &domain.AccrualSchedule{ScheduleID: "sch-1", LegalEntityID: "le-1", Status: domain.AccrualStatusPendingApproval}
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(s, &stubPublisher{}, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	rr := doReq(rt, http.MethodPost, "/v1/accruals/sch-1/approve", nil, "approver-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "ACCRUAL_APPROVE" {
		t.Errorf("expected ACCRUAL_APPROVE, got %q", seenAction)
	}
}

func TestRunAccrualRecognition_PostsJournalAndActivatesSchedule(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")

	req := domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}
	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var inst domain.RecognitionInstance
	if err := json.NewDecoder(rr.Body).Decode(&inst); err != nil {
		t.Fatal(err)
	}
	if inst.RecognizedAmount != 300.00 {
		t.Errorf("expected 300.00, got %v", inst.RecognizedAmount)
	}
	if cl.postJournalCalls != 1 {
		t.Fatalf("expected exactly 1 journal post, got %d", cl.postJournalCalls)
	}
	sch, _ := s.GetAccrualSchedule(context.Background(), id)
	if sch.Status != domain.AccrualStatusActive {
		t.Errorf("expected ACTIVE after first recognition, got %q", sch.Status)
	}
}

func TestRunAccrualRecognition_LastPeriodAbsorbsRoundingRemainder(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	// 1000.00 / 3 = 333.33... — the third period must absorb the remainder
	// so the three installments sum exactly to 1000.00.
	id := createApprovedAccrual(t, s, r, 1000.00, 3, "2026-01")

	var total float64
	for _, period := range []string{"2026-01", "2026-02", "2026-03"} {
		req := domain.RunAccrualRecognitionRequest{FiscalPeriod: period}
		rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", req, "preparer-1")
		if rr.Code != http.StatusCreated {
			t.Fatalf("recognize %s failed: %d %s", period, rr.Code, rr.Body.String())
		}
		var inst domain.RecognitionInstance
		_ = json.NewDecoder(rr.Body).Decode(&inst)
		total += inst.RecognizedAmount
	}
	if total != 1000.00 {
		t.Errorf("expected installments to sum to 1000.00, got %v", total)
	}
	sch, _ := s.GetAccrualSchedule(context.Background(), id)
	if sch.Status != domain.AccrualStatusCompleted {
		t.Errorf("expected COMPLETED after all periods recognized, got %q", sch.Status)
	}
}

func TestRunAccrualRecognition_Replay_IsIdempotentNoDuplicateJournal(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")

	req := domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}
	first := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", req, "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first recognize failed: %d %s", first.Code, first.Body.String())
	}
	replay := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", req, "preparer-1")
	if replay.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d: %s", replay.Code, replay.Body.String())
	}
	instances, err := s.ListRecognitionInstances(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected exactly 1 recognition instance after replay, got %d", len(instances))
	}
}

func TestRunAccrualRecognition_PeriodOutOfRange_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")

	req := domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-05"} // outside the 3-period window
	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRunAccrualRecognition_LockedPeriod_Returns422AndDoesNotPost(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")
	s.periods["fp-locked"] = &domain.FiscalPeriod{FiscalPeriodID: "fp-locked", TenantID: testTenantID, LegalEntityID: "le-1", PeriodName: "2026-01", CloseStatus: "LOCKED"}

	req := domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}
	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.postJournalCalls != 0 {
		t.Errorf("expected no journal posted into a LOCKED period, got %d calls", cl.postJournalCalls)
	}
}

func TestRunAccrualRecognition_NotApprovedYet_Returns422(t *testing.T) {
	s := newStubStore()
	s.schedules["sch-draft"] = &domain.AccrualSchedule{
		ScheduleID: "sch-draft", LegalEntityID: "le-1", Status: domain.AccrualStatusDraft,
		StartFiscalPeriod: "2026-01", PeriodCount: 3, TotalAmount: 900,
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	req := domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}
	rr := doReq(r, http.MethodPost, "/v1/accruals/sch-draft/recognize", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAmendFutureSchedule_CannotDropBelowRecognizedPeriods(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedAccrual(t, s, r, 1500.00, 5, "2026-01")
	for _, period := range []string{"2026-01", "2026-02"} {
		recReq := domain.RunAccrualRecognitionRequest{FiscalPeriod: period}
		if rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", recReq, "preparer-1"); rr.Code != http.StatusCreated {
			t.Fatalf("recognize %s failed: %d %s", period, rr.Code, rr.Body.String())
		}
	}

	// 2 periods already recognized — amending down to 1 would invalidate
	// permanent recognition history, and must be refused.
	amendReq := domain.AmendFutureScheduleRequest{TotalAmount: 600.00, PeriodCount: 1}
	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/amend", amendReq, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAmendFutureSchedule_ValidAmendment_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")

	amendReq := domain.AmendFutureScheduleRequest{TotalAmount: 1200.00, PeriodCount: 4}
	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/amend", amendReq, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	sch, _ := s.GetAccrualSchedule(context.Background(), id)
	if sch.TotalAmount != 1200.00 || sch.PeriodCount != 4 {
		t.Errorf("amendment did not apply: %+v", sch)
	}
}

func TestCancelFutureAccrual_AlreadyCompleted_Returns422(t *testing.T) {
	s := newStubStore()
	s.schedules["sch-done"] = &domain.AccrualSchedule{ScheduleID: "sch-done", LegalEntityID: "le-1", Status: domain.AccrualStatusCompleted}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/accruals/sch-done/cancel", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCancelFutureAccrual_FromApproved_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")
	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/cancel", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	sch, _ := s.GetAccrualSchedule(context.Background(), id)
	if sch.Status != domain.AccrualStatusCancelled {
		t.Errorf("expected CANCELLED, got %q", sch.Status)
	}
}

// ── ACC-08 (Prepayments & Deferrals) ────────────────────────────────────────────

func createApprovedPrepayment(t *testing.T, s *stubStore, r chi.Router, totalAmount float64, periodCount int, start string) string {
	t.Helper()
	req := domain.CreatePrepaymentRequest{
		LegalEntityID:     "le-1",
		Description:       "Annual insurance prepayment",
		TotalAmount:       totalAmount,
		StartFiscalPeriod: start,
		PeriodCount:       periodCount,
		DebitAccountCode:  "6200-Insurance",
		CreditAccountCode: "1400-PrepaidInsurance",
	}
	rr := doReq(r, http.MethodPost, "/v1/prepayments/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var sch domain.PrepaymentSchedule
	_ = json.NewDecoder(rr.Body).Decode(&sch)

	if rr := doReq(r, http.MethodPost, "/v1/prepayments/"+sch.ScheduleID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}
	return sch.ScheduleID
}

func TestPrepaymentLifecycle_CreateApprove(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedPrepayment(t, s, r, 1200.00, 12, "2026-01")
	sch, err := s.GetPrepaymentSchedule(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sch.Status != domain.PrepaymentStatusApproved {
		t.Errorf("expected APPROVED, got %q", sch.Status)
	}
}

func TestApprovePrepayment_UsesDistinctAuthorizationAction(t *testing.T) {
	s := newStubStore()
	s.prepaymentSchedules["ppy-1"] = &domain.PrepaymentSchedule{ScheduleID: "ppy-1", LegalEntityID: "le-1", Status: domain.PrepaymentStatusDraft}
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(s, &stubPublisher{}, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	rr := doReq(rt, http.MethodPost, "/v1/prepayments/ppy-1/approve", nil, "approver-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "PREPAYMENT_APPROVE" {
		t.Errorf("expected PREPAYMENT_APPROVE, got %q", seenAction)
	}
}

func TestRunPrepaymentRecognition_Replay_IsIdempotent(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedPrepayment(t, s, r, 1200.00, 12, "2026-01")

	req := domain.RunPrepaymentRecognitionRequest{FiscalPeriod: "2026-01"}
	first := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/recognize", req, "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first recognize failed: %d %s", first.Code, first.Body.String())
	}
	replay := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/recognize", req, "preparer-1")
	if replay.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d: %s", replay.Code, replay.Body.String())
	}
	if cl.postJournalCalls != 2 {
		// Both calls DO reach the client stub (it doesn't fake GL's own
		// idempotency) — what matters is exactly one evidence row exists.
		t.Fatalf("expected 2 client calls (GL itself dedupes), got %d", cl.postJournalCalls)
	}
	instances, _ := s.ListPrepaymentRecognitions(context.Background(), id)
	if len(instances) != 1 {
		t.Fatalf("expected exactly 1 recognition instance after replay, got %d", len(instances))
	}
}

func TestGetPrepaymentRemainingBalance_ReflectsRecognizedHistory(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedPrepayment(t, s, r, 1200.00, 12, "2026-01")

	req := domain.RunPrepaymentRecognitionRequest{FiscalPeriod: "2026-01"}
	if rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/recognize", req, "preparer-1"); rr.Code != http.StatusCreated {
		t.Fatalf("recognize failed: %d %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodGet, "/v1/prepayments/"+id+"/remaining-balance", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["remaining_balance"] != 1100.00 {
		t.Errorf("expected remaining_balance 1100.00 (1200 - 100), got %v", body["remaining_balance"])
	}
}

func TestModifyPrepayment_CannotDropBelowRecognizedPeriods(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedPrepayment(t, s, r, 2400.00, 12, "2026-01")
	for _, period := range []string{"2026-01", "2026-02"} {
		req := domain.RunPrepaymentRecognitionRequest{FiscalPeriod: period}
		if rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/recognize", req, "preparer-1"); rr.Code != http.StatusCreated {
			t.Fatalf("recognize %s failed: %d %s", period, rr.Code, rr.Body.String())
		}
	}

	// 2 periods already recognized — this is ACC-08's own negative path,
	// "Backdate schedule change over recognized periods," and must be
	// blocked.
	modReq := domain.ModifyFutureScheduleRequest{TotalAmount: 1200.00, PeriodCount: 1}
	rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/modify", modReq, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestTerminatePrepayment_MissingFinalBalanceTreatment_Returns400(t *testing.T) {
	// ACC-08's own negative path: "Terminate without final balance
	// treatment" must be blocked, not silently defaulted.
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedPrepayment(t, s, r, 1200.00, 12, "2026-01")

	rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/terminate", domain.TerminatePrepaymentRequest{Reason: "contract cancelled"}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestTerminatePrepayment_WriteOff_PostsNoJournal(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedPrepayment(t, s, r, 1200.00, 12, "2026-01")

	req := domain.TerminatePrepaymentRequest{Reason: "contract cancelled", FinalBalanceTreatment: domain.TerminationTreatmentWriteOff}
	rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/terminate", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.postJournalCalls != 0 {
		t.Errorf("expected WRITE_OFF to post no journal, got %d calls", cl.postJournalCalls)
	}
	sch, _ := s.GetPrepaymentSchedule(context.Background(), id)
	if sch.Status != domain.PrepaymentStatusTerminated {
		t.Errorf("expected TERMINATED, got %q", sch.Status)
	}
}

func TestTerminatePrepayment_RecognizeRemaining_PostsFinalSettlementJournal(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedPrepayment(t, s, r, 1200.00, 12, "2026-01")
	// Recognize one period first (100.00), leaving 1100.00 remaining.
	recReq := domain.RunPrepaymentRecognitionRequest{FiscalPeriod: "2026-01"}
	if rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/recognize", recReq, "preparer-1"); rr.Code != http.StatusCreated {
		t.Fatalf("recognize failed: %d %s", rr.Code, rr.Body.String())
	}

	req := domain.TerminatePrepaymentRequest{
		Reason: "policy replaced", FinalBalanceTreatment: domain.TerminationTreatmentRecognizeRemaining, FiscalPeriod: "2026-02",
	}
	rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/terminate", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.postJournalCalls != 2 { // one periodic recognition + one final settlement
		t.Fatalf("expected 2 journal posts total, got %d", cl.postJournalCalls)
	}
	if cl.lastPostedAmount != 1100.00 {
		t.Errorf("expected final settlement of 1100.00, got %v", cl.lastPostedAmount)
	}
	instances, _ := s.ListPrepaymentRecognitions(context.Background(), id)
	if len(instances) != 2 {
		t.Fatalf("expected 2 recognition instances (1 periodic + 1 termination), got %d", len(instances))
	}
}

func TestTerminatePrepayment_RecognizeRemainingWithoutFiscalPeriod_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createApprovedPrepayment(t, s, r, 1200.00, 12, "2026-01")

	req := domain.TerminatePrepaymentRequest{Reason: "x", FinalBalanceTreatment: domain.TerminationTreatmentRecognizeRemaining}
	rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/terminate", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ACC-09 (Allocation Engine) ──────────────────────────────────────────────────

func createApprovedAllocationRule(t *testing.T, s *stubStore, cl *stubClients, r chi.Router, drivers []domain.AllocationDriver) string {
	t.Helper()
	if cl.accountStatuses == nil {
		cl.accountStatuses = map[string]string{}
	}
	for _, d := range drivers {
		cl.accountStatuses[d.RecipientAccountCode] = "ACTIVE"
	}
	req := domain.CreateAllocationRuleRequest{
		LegalEntityID:     "le-1",
		Name:              "IT shared cost allocation",
		SourceAccountCode: "5000-ITSharedCost",
		Drivers:           drivers,
	}
	rr := doReq(r, http.MethodPost, "/v1/allocation-rules/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create rule failed: %d %s", rr.Code, rr.Body.String())
	}
	var rule domain.AllocationRule
	_ = json.NewDecoder(rr.Body).Decode(&rule)

	if rr := doReq(r, http.MethodPost, "/v1/allocation-rules/"+rule.RuleID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve rule failed: %d %s", rr.Code, rr.Body.String())
	}
	return rule.RuleID
}

func evenDrivers() []domain.AllocationDriver {
	return []domain.AllocationDriver{
		{RecipientAccountCode: "6100-Sales", WeightPercentage: 40},
		{RecipientAccountCode: "6200-Ops", WeightPercentage: 35},
		{RecipientAccountCode: "6300-Admin", WeightPercentage: 25},
	}
}

func TestCreateAllocationRule_NoDrivers_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	req := domain.CreateAllocationRuleRequest{LegalEntityID: "le-1", Name: "x", SourceAccountCode: "5000"}
	rr := doReq(r, http.MethodPost, "/v1/allocation-rules/", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestApproveAllocationRule_DriversDoNotSumTo100_Returns422(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"6100-Sales": "ACTIVE", "6200-Ops": "ACTIVE"}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.CreateAllocationRuleRequest{
		LegalEntityID: "le-1", Name: "bad rule", SourceAccountCode: "5000-Shared",
		Drivers: []domain.AllocationDriver{
			{RecipientAccountCode: "6100-Sales", WeightPercentage: 40},
			{RecipientAccountCode: "6200-Ops", WeightPercentage: 40}, // sums to 80, not 100
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/allocation-rules/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var rule domain.AllocationRule
	_ = json.NewDecoder(rr.Body).Decode(&rule)

	approveRR := doReq(r, http.MethodPost, "/v1/allocation-rules/"+rule.RuleID+"/approve", nil, "approver-1")
	if approveRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", approveRR.Code, approveRR.Body.String())
	}
}

func TestApproveAllocationRule_InvalidRecipientAccount_Returns422(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"6100-Sales": "ACTIVE"}} // 6200-Ops never registered
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.CreateAllocationRuleRequest{
		LegalEntityID: "le-1", Name: "rule", SourceAccountCode: "5000-Shared",
		Drivers: []domain.AllocationDriver{
			{RecipientAccountCode: "6100-Sales", WeightPercentage: 60},
			{RecipientAccountCode: "6200-Ops", WeightPercentage: 40},
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/allocation-rules/", req, "preparer-1")
	var rule domain.AllocationRule
	_ = json.NewDecoder(rr.Body).Decode(&rule)

	approveRR := doReq(r, http.MethodPost, "/v1/allocation-rules/"+rule.RuleID+"/approve", nil, "approver-1")
	if approveRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", approveRR.Code, approveRR.Body.String())
	}
}

func TestApproveAllocationRule_UsesDistinctAuthorizationAction(t *testing.T) {
	s := newStubStore()
	s.allocationRules["rule-1"] = &domain.AllocationRule{RuleID: "rule-1", RuleVersionID: "rule-1", LegalEntityID: "le-1", Status: domain.AllocationRuleStatusDraft}
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(s, &stubPublisher{}, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	rr := doReq(rt, http.MethodPost, "/v1/allocation-rules/rule-1/approve", nil, "approver-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "ALLOCATION_RULE_APPROVE" {
		t.Errorf("expected ALLOCATION_RULE_APPROVE, got %q", seenAction)
	}
}

func TestExecuteAllocation_SplitsSourceAmountAcrossDrivers(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{trialBalances: map[string]float64{"5000-ITSharedCost": 1000.00}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	req := domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}
	rr := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var run domain.AllocationRun
	if err := json.NewDecoder(rr.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.AllocationRunStatusPosted {
		t.Fatalf("expected POSTED, got %q", run.Status)
	}
	var total float64
	for _, l := range run.ResultLines {
		total += l.AllocatedAmount
	}
	if total != 1000.00 {
		t.Errorf("expected result lines to sum to exactly 1000.00, got %v", total)
	}
	if len(run.ResultLines) != 3 {
		t.Fatalf("expected 3 result lines, got %d", len(run.ResultLines))
	}
}

func TestExecuteAllocation_RoundingResidualAbsorbedByLastDriver(t *testing.T) {
	// 1000 split three ways at 33.33/33.33/33.34 — 33.33% of 1000.00
	// rounds to 333.30 twice, leaving a residual that must land entirely
	// on the last driver so the three shares still sum to exactly 1000.00.
	s := newStubStore()
	cl := &stubClients{trialBalances: map[string]float64{"5000-ITSharedCost": 1000.00}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	drivers := []domain.AllocationDriver{
		{RecipientAccountCode: "6100-Sales", WeightPercentage: 33.3333},
		{RecipientAccountCode: "6200-Ops", WeightPercentage: 33.3333},
		{RecipientAccountCode: "6300-Admin", WeightPercentage: 33.3334},
	}
	ruleID := createApprovedAllocationRule(t, s, cl, r, drivers)

	req := domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}
	rr := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var run domain.AllocationRun
	_ = json.NewDecoder(rr.Body).Decode(&run)
	var total float64
	for _, l := range run.ResultLines {
		total += l.AllocatedAmount
	}
	if total != 1000.00 {
		t.Errorf("expected result lines to sum to exactly 1000.00, got %v", total)
	}
}

func TestExecuteAllocation_Rerun_ReturnsSameRunWithoutReposting(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{trialBalances: map[string]float64{"5000-ITSharedCost": 900.00}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	req := domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}
	first := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first execute failed: %d %s", first.Code, first.Body.String())
	}
	var firstRun domain.AllocationRun
	_ = json.NewDecoder(first.Body).Decode(&firstRun)

	rerun := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1")
	if rerun.Code != http.StatusOK {
		t.Fatalf("expected 200 on rerun got %d: %s", rerun.Code, rerun.Body.String())
	}
	var rerunResult domain.AllocationRun
	_ = json.NewDecoder(rerun.Body).Decode(&rerunResult)
	if rerunResult.RunID != firstRun.RunID {
		t.Errorf("expected rerun to return the SAME run, got a different run_id")
	}
	if cl.postAllocationCalls != 1 {
		t.Fatalf("expected exactly 1 journal post across both calls, got %d", cl.postAllocationCalls)
	}
}

func TestExecuteAllocation_SourceBalanceNotFound_Returns422(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{trialBalances: map[string]float64{"9999-Other": 500.00}} // source account never posted
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	req := domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}
	rr := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExecuteAllocation_JournalPostingFails_RunMarkedFailedNotSilentlyLost(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{trialBalances: map[string]float64{"5000-ITSharedCost": 900.00}, postAllocationErr: domain.ErrGLServiceUnavailable}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	req := domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}
	rr := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}

	// The run must be visible as FAILED (an exception), not vanished.
	excRR := doReq(r, http.MethodGet, "/v1/allocation-runs/exceptions?legal_entity_id=le-1", nil, "preparer-1")
	if excRR.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", excRR.Code, excRR.Body.String())
	}
	var exceptions []domain.AllocationRun
	_ = json.NewDecoder(excRR.Body).Decode(&exceptions)
	if len(exceptions) != 1 || exceptions[0].Status != domain.AllocationRunStatusFailed {
		t.Fatalf("expected exactly 1 FAILED exception, got %+v", exceptions)
	}
}

func TestExecuteAllocation_ExistingFailedRun_MustUseReprocess(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{trialBalances: map[string]float64{"5000-ITSharedCost": 900.00}, postAllocationErr: domain.ErrGLServiceUnavailable}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	req := domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}
	if rr := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected first attempt to fail 503, got %d: %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (use reprocess) got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReprocessAllocationRun_RetriesFailedRunWithSameAmounts(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{trialBalances: map[string]float64{"5000-ITSharedCost": 900.00}, postAllocationErr: domain.ErrGLServiceUnavailable}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	req := domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}
	first := doReq(r, http.MethodPost, "/v1/allocation-runs/", req, "preparer-1")
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected first attempt to fail: %d", first.Code)
	}
	var failedRun domain.AllocationRun
	// Fetch the failed run via the exceptions list since the create response has no body worth trusting on failure.
	excRR := doReq(r, http.MethodGet, "/v1/allocation-runs/exceptions?legal_entity_id=le-1", nil, "preparer-1")
	var exceptions []domain.AllocationRun
	_ = json.NewDecoder(excRR.Body).Decode(&exceptions)
	if len(exceptions) != 1 {
		t.Fatalf("expected 1 failed run, got %d", len(exceptions))
	}
	failedRun = exceptions[0]

	cl.postAllocationErr = nil // dependency recovers
	reprocessRR := doReq(r, http.MethodPost, "/v1/allocation-runs/"+failedRun.RunID+"/reprocess", nil, "preparer-1")
	if reprocessRR.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", reprocessRR.Code, reprocessRR.Body.String())
	}
	var posted domain.AllocationRun
	_ = json.NewDecoder(reprocessRR.Body).Decode(&posted)
	if posted.Status != domain.AllocationRunStatusPosted {
		t.Errorf("expected POSTED after reprocess, got %q", posted.Status)
	}

	// Reprocessing an already-POSTED run must be refused — the spec's own
	// negative path, "Rerun duplicates posting."
	secondReprocess := doReq(r, http.MethodPost, "/v1/allocation-runs/"+failedRun.RunID+"/reprocess", nil, "preparer-1")
	if secondReprocess.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", secondReprocess.Code, secondReprocess.Body.String())
	}
}

// ── ACC-10 (Foreign Currency Revaluation) ───────────────────────────────────────

func TestStartRevaluation_NoItems_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	req := domain.StartRevaluationRequest{LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7100-FXGainLoss"}
	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestStartRevaluation_RateMissingForCurrency_Returns422(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{
		trialBalances: map[string]float64{"1100-ForeignCash": 5000.00},
		accountTypes:  map[string]string{"1100-ForeignCash": domain.AccountTypeAsset},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.StartRevaluationRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7100-FXGainLoss",
		RateSet: map[string]float64{"GBP": 1.25}, // no EUR rate
		Items:   []domain.RevaluationItemInput{{AccountCode: "1100-ForeignCash", CurrencyCode: "EUR", ForeignAmount: 4000}},
	}
	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestStartRevaluation_NonMonetaryItemIncluded_Returns422(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{
		trialBalances: map[string]float64{"6100-Travel": 2000.00},
		accountTypes:  map[string]string{"6100-Travel": "EXPENSE"}, // not ASSET/LIABILITY
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.StartRevaluationRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7100-FXGainLoss",
		RateSet: map[string]float64{"EUR": 1.10},
		Items:   []domain.RevaluationItemInput{{AccountCode: "6100-Travel", CurrencyCode: "EUR", ForeignAmount: 2000}},
	}
	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestStartRevaluation_AssetGain_ComputesCorrectAdjustment(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{
		trialBalances: map[string]float64{"1100-ForeignCash": 4400.00}, // booked at old rate
		accountTypes:  map[string]string{"1100-ForeignCash": domain.AccountTypeAsset},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.StartRevaluationRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7100-FXGainLoss",
		RateSet: map[string]float64{"EUR": 1.15}, // 4000 EUR * 1.15 = 4600.00
		Items:   []domain.RevaluationItemInput{{AccountCode: "1100-ForeignCash", CurrencyCode: "EUR", ForeignAmount: 4000}},
	}
	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var run domain.FXRevaluationRun
	_ = json.NewDecoder(rr.Body).Decode(&run)
	if len(run.Items) != 1 || run.Items[0].AdjustmentAmount != 200.00 {
		t.Fatalf("expected adjustment 200.00 (4600-4400), got %+v", run.Items)
	}
}

func TestApproveRevaluation_UsesDistinctAuthorizationAction(t *testing.T) {
	s := newStubStore()
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{RunID: "run-1", LegalEntityID: "le-1", Status: domain.FXRevaluationStatusReview}
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(s, &stubPublisher{}, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	rr := doReq(rt, http.MethodPost, "/v1/fx-revaluations/run-1/approve", nil, "approver-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "FX_REVALUATION_APPROVE" {
		t.Errorf("expected FX_REVALUATION_APPROVE, got %q", seenAction)
	}
}

func TestPostRevaluation_Replay_DoesNotRepostJournal(t *testing.T) {
	s := newStubStore()
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{
		RunID: "run-1", LegalEntityID: "le-1", Status: domain.FXRevaluationStatusApproved, FXGainLossAccountCode: "7100-FXGainLoss",
		Items: []domain.FXRevaluationItem{{AccountCode: "1100-ForeignCash", AccountType: domain.AccountTypeAsset, AdjustmentAmount: 200.00}},
	}
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)

	first := doReq(r, http.MethodPost, "/v1/fx-revaluations/run-1/post", nil, "preparer-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first post failed: %d %s", first.Code, first.Body.String())
	}
	if cl.postMultiLineCalls != 1 {
		t.Fatalf("expected 1 journal post, got %d", cl.postMultiLineCalls)
	}

	replay := doReq(r, http.MethodPost, "/v1/fx-revaluations/run-1/post", nil, "preparer-1")
	if replay.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay got %d: %s", replay.Code, replay.Body.String())
	}
	if cl.postMultiLineCalls != 1 {
		t.Fatalf("expected still only 1 journal post after replay, got %d", cl.postMultiLineCalls)
	}
}

func TestPostRevaluation_NetGainAndLossLinesBalance(t *testing.T) {
	s := newStubStore()
	// Asset gained 200 (debit asset 200), liability grew by 50 (a loss:
	// credit liability 50, debit loss 50). Net gain = 200 - 50 = 150,
	// credited to the FX account. Total debits = 200 (asset) + 50 (loss)
	// = 250; total credits = 50 (liability) + 150 (fx gain) = 200 ...
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{
		RunID: "run-1", LegalEntityID: "le-1", Status: domain.FXRevaluationStatusApproved, FXGainLossAccountCode: "7100-FXGainLoss",
		Items: []domain.FXRevaluationItem{
			{AccountCode: "1100-ForeignCash", AccountType: domain.AccountTypeAsset, AdjustmentAmount: 200.00},
			{AccountCode: "2100-ForeignPayable", AccountType: domain.AccountTypeLiability, AdjustmentAmount: 50.00},
		},
	}
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)

	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/run-1/post", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var totalDebits, totalCredits float64
	for _, l := range cl.lastMultiLines {
		totalDebits += l.DebitAmount
		totalCredits += l.CreditAmount
	}
	if totalDebits != totalCredits {
		t.Errorf("journal does not balance: debits=%v credits=%v", totalDebits, totalCredits)
	}
}

func TestReversePriorRevaluation_PriorNotPosted_Returns422(t *testing.T) {
	s := newStubStore()
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{RunID: "run-1", LegalEntityID: "le-1", Status: domain.FXRevaluationStatusReview}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/reverse", domain.ReversePriorRevaluationRequest{PriorRunID: "run-1"}, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReversePriorRevaluation_NegatesPriorAdjustments(t *testing.T) {
	s := newStubStore()
	postedAt := time.Now().UTC()
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{
		RunID: "run-1", LegalEntityID: "le-1", FiscalPeriod: "2026-01", Status: domain.FXRevaluationStatusPosted,
		FXGainLossAccountCode: "7100-FXGainLoss", PostedAt: &postedAt,
		Items: []domain.FXRevaluationItem{
			{AccountCode: "1100-ForeignCash", AccountType: domain.AccountTypeAsset, CurrencyCode: "EUR", ForeignAmount: 4000, BookAmount: 4400, ClosingRate: 1.15, RevaluedAmount: 4600, AdjustmentAmount: 200.00},
		},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/reverse", domain.ReversePriorRevaluationRequest{PriorRunID: "run-1"}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var reversal domain.FXRevaluationRun
	_ = json.NewDecoder(rr.Body).Decode(&reversal)
	if reversal.ReversalOfRunID == nil || *reversal.ReversalOfRunID != "run-1" {
		t.Errorf("expected reversal_of_run_id run-1, got %v", reversal.ReversalOfRunID)
	}
	if len(reversal.Items) != 1 || reversal.Items[0].AdjustmentAmount != -200.00 {
		t.Fatalf("expected negated adjustment -200.00, got %+v", reversal.Items)
	}
}

// ── ACC-17 (Opening Balance & Migration) ────────────────────────────────────────

func balancedMigrationEntries() []domain.MigrationCrosswalkEntry {
	return []domain.MigrationCrosswalkEntry{
		{SourceReferenceID: "SRC-1", SourceAccountCode: "LEGACY-CASH", TargetAccountCode: "1000-Cash", DebitAmount: 10000.00},
		{SourceReferenceID: "SRC-2", SourceAccountCode: "LEGACY-EQUITY", TargetAccountCode: "3000-OpeningEquity", CreditAmount: 10000.00},
	}
}

func createValidatedMigrationBatch(t *testing.T, s *stubStore, cl *stubClients, r chi.Router) string {
	t.Helper()
	if cl.accountStatuses == nil {
		cl.accountStatuses = map[string]string{}
	}
	cl.accountStatuses["1000-Cash"] = "ACTIVE"
	cl.accountStatuses["3000-OpeningEquity"] = "ACTIVE"

	req := domain.CreateMigrationBatchRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:abc123",
		ExpectedRowCount: 2, ExpectedTotalDebits: 10000.00, ExpectedTotalCredits: 10000.00,
		Entries: balancedMigrationEntries(),
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var batch domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&batch)

	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1"); rr.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", rr.Code, rr.Body.String())
	}
	return batch.BatchID
}

func TestCreateMigrationBatch_DuplicateSourceReference_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	req := domain.CreateMigrationBatchRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, Entries: []domain.MigrationCrosswalkEntry{
			{SourceReferenceID: "SRC-1", TargetAccountCode: "1000-Cash", DebitAmount: 100},
			{SourceReferenceID: "SRC-1", TargetAccountCode: "3000-Equity", CreditAmount: 100},
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateMigrationBatch_Idempotent_ReturnsSameBatch(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:abc",
		ExpectedRowCount: 2, ExpectedTotalDebits: 10000, ExpectedTotalCredits: 10000, Entries: balancedMigrationEntries(),
	}
	first := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first create failed: %d %s", first.Code, first.Body.String())
	}
	var firstBatch domain.MigrationBatch
	_ = json.NewDecoder(first.Body).Decode(&firstBatch)

	replay := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	if replay.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay got %d: %s", replay.Code, replay.Body.String())
	}
	var replayBatch domain.MigrationBatch
	_ = json.NewDecoder(replay.Body).Decode(&replayBatch)
	if replayBatch.BatchID != firstBatch.BatchID {
		t.Errorf("expected the same batch on replay, got a different batch_id")
	}
}

func TestValidateOpeningBalances_UnbalancedTB_QuarantinesBatch(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"1000-Cash": "ACTIVE", "3000-Equity": "ACTIVE"}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, ExpectedTotalDebits: 10000, ExpectedTotalCredits: 9000,
		Entries: []domain.MigrationCrosswalkEntry{
			{SourceReferenceID: "SRC-1", TargetAccountCode: "1000-Cash", DebitAmount: 10000},
			{SourceReferenceID: "SRC-2", TargetAccountCode: "3000-Equity", CreditAmount: 9000}, // does not balance
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	var batch domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&batch)

	validateRR := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1")
	if validateRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", validateRR.Code, validateRR.Body.String())
	}
	stored, _ := s.GetMigrationBatch(context.Background(), batch.BatchID)
	if stored.Status != domain.MigrationBatchStatusQuarantined {
		t.Errorf("expected QUARANTINED, got %q", stored.Status)
	}
}

func TestValidateOpeningBalances_SuspenseAccountTargeted_QuarantinesBatch(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"1000-Cash": "ACTIVE", "9999-Suspense": "ACTIVE"}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, ExpectedTotalDebits: 500, ExpectedTotalCredits: 500,
		Entries: []domain.MigrationCrosswalkEntry{
			{SourceReferenceID: "SRC-1", TargetAccountCode: "1000-Cash", DebitAmount: 500},
			{SourceReferenceID: "SRC-2", TargetAccountCode: "9999-Suspense", CreditAmount: 500}, // forced plug
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	var batch domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&batch)

	validateRR := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1")
	if validateRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", validateRR.Code, validateRR.Body.String())
	}
}

func TestValidateOpeningBalances_ControlTotalsMismatch_QuarantinesBatch(t *testing.T) {
	// Row counts and both sides balance internally, but the loaded totals
	// don't match what the SOURCE system itself declared — the deeper
	// version of "values differ."
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"1000-Cash": "ACTIVE", "3000-Equity": "ACTIVE"}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, ExpectedTotalDebits: 99999.00, ExpectedTotalCredits: 99999.00, // source declared this
		Entries: []domain.MigrationCrosswalkEntry{
			{SourceReferenceID: "SRC-1", TargetAccountCode: "1000-Cash", DebitAmount: 10000},
			{SourceReferenceID: "SRC-2", TargetAccountCode: "3000-Equity", CreditAmount: 10000}, // but this is what actually loaded
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	var batch domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&batch)

	validateRR := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1")
	if validateRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", validateRR.Code, validateRR.Body.String())
	}
}

func TestApproveMigrationBatch_UsesDistinctAuthorizationAction(t *testing.T) {
	s := newStubStore()
	s.migrationBatches["batch-1"] = &domain.MigrationBatch{BatchID: "batch-1", LegalEntityID: "le-1", Status: domain.MigrationBatchStatusValidated}
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(s, &stubPublisher{}, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	rr := doReq(rt, http.MethodPost, "/v1/migration-batches/batch-1/approve", nil, "approver-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "MIGRATION_BATCH_APPROVE" {
		t.Errorf("expected MIGRATION_BATCH_APPROVE, got %q", seenAction)
	}
}

func TestCommitOpeningPosting_Replay_DoesNotRepost(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	batchID := createValidatedMigrationBatch(t, s, cl, r)
	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}

	first := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first commit failed: %d %s", first.Code, first.Body.String())
	}
	if cl.postMultiLineCalls != 1 {
		t.Fatalf("expected 1 journal post, got %d", cl.postMultiLineCalls)
	}

	replay := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1")
	if replay.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay got %d: %s", replay.Code, replay.Body.String())
	}
	if cl.postMultiLineCalls != 1 {
		t.Fatalf("expected still only 1 journal post after replay, got %d", cl.postMultiLineCalls)
	}
	var posted domain.MigrationBatch
	_ = json.NewDecoder(replay.Body).Decode(&posted)
	if posted.Status != domain.MigrationBatchStatusReconciled {
		t.Errorf("expected RECONCILED after commit, got %q", posted.Status)
	}
}

func TestCommitOpeningPosting_LockedPeriod_Returns422AndDoesNotPost(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	batchID := createValidatedMigrationBatch(t, s, cl, r)
	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}
	s.periods["fp-locked"] = &domain.FiscalPeriod{FiscalPeriodID: "fp-locked", TenantID: testTenantID, LegalEntityID: "le-1", PeriodName: "2026-01", CloseStatus: "LOCKED"}

	rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.postMultiLineCalls != 0 {
		t.Errorf("expected no journal posted into a LOCKED period, got %d calls", cl.postMultiLineCalls)
	}
}

func TestCertifyMigrationAccounting_RequiresReason(t *testing.T) {
	s := newStubStore()
	s.migrationBatches["batch-1"] = &domain.MigrationBatch{BatchID: "batch-1", LegalEntityID: "le-1", Status: domain.MigrationBatchStatusReconciled}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/batch-1/certify", domain.CertifyMigrationBatchRequest{}, "certifier-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestFullMigrationLifecycle_ReachesCertified(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	batchID := createValidatedMigrationBatch(t, s, cl, r)
	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1"); rr.Code != http.StatusOK {
		t.Fatalf("commit failed: %d %s", rr.Code, rr.Body.String())
	}
	certifyRR := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/certify", domain.CertifyMigrationBatchRequest{Reason: "source-to-target reconciled and signed off"}, "certifier-1")
	if certifyRR.Code != http.StatusOK {
		t.Fatalf("certify failed: %d %s", certifyRR.Code, certifyRR.Body.String())
	}
	var final domain.MigrationBatch
	_ = json.NewDecoder(certifyRR.Body).Decode(&final)
	if final.Status != domain.MigrationBatchStatusCertified {
		t.Errorf("expected CERTIFIED, got %q", final.Status)
	}
}

// ── ACC-16 (Signed Financial Snapshot) ──────────────────────────────────────────

func createSealedSnapshot(t *testing.T, r chi.Router, hasUnresolvedExceptions bool) string {
	t.Helper()
	req := domain.CreateFinancialSnapshotRequest{
		LegalEntityID: "le-1", Purpose: "AUDIT", Content: `{"trial_balance":{"1000-Cash":5000}}`,
		SourceReferences: `["trial_balance_snapshot:tbs-1"]`, HasUnresolvedExceptions: hasUnresolvedExceptions,
	}
	rr := doReq(r, http.MethodPost, "/v1/financial-snapshots/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var snap domain.FinancialSnapshot
	_ = json.NewDecoder(rr.Body).Decode(&snap)

	if rr := doReq(r, http.MethodPost, "/v1/financial-snapshots/"+snap.SnapshotID+"/seal", nil, "preparer-1"); rr.Code != http.StatusOK {
		t.Fatalf("seal failed: %d %s", rr.Code, rr.Body.String())
	}
	return snap.SnapshotID
}

func TestCreateFinancialSnapshot_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/financial-snapshots/", domain.CreateFinancialSnapshotRequest{LegalEntityID: "le-1"}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSealSnapshot_ProducesHashAndSignature(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createSealedSnapshot(t, r, false)
	rr := doReq(r, http.MethodGet, "/v1/financial-snapshots/"+id, nil, "preparer-1")
	var snap domain.FinancialSnapshot
	_ = json.NewDecoder(rr.Body).Decode(&snap)
	if snap.Status != domain.SnapshotStatusSealed {
		t.Fatalf("expected SEALED, got %q", snap.Status)
	}
	if snap.ContentHash == nil || *snap.ContentHash == "" || snap.Signature == nil || *snap.Signature == "" {
		t.Fatalf("expected a non-empty content_hash and signature, got %+v", snap)
	}
}

func TestSealSnapshot_NoSigningKey_Returns503(t *testing.T) {
	s := newStubStore()
	h := handler.New(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{}, nil, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)

	createRR := doReq(rt, http.MethodPost, "/v1/financial-snapshots/", domain.CreateFinancialSnapshotRequest{
		LegalEntityID: "le-1", Purpose: "AUDIT", Content: "x",
	}, "preparer-1")
	var snap domain.FinancialSnapshot
	_ = json.NewDecoder(createRR.Body).Decode(&snap)

	sealRR := doReq(rt, http.MethodPost, "/v1/financial-snapshots/"+snap.SnapshotID+"/seal", nil, "preparer-1")
	if sealRR.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", sealRR.Code, sealRR.Body.String())
	}
}

func TestCertifySnapshot_UnresolvedException_Returns422(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createSealedSnapshot(t, r, true) // has_unresolved_exceptions = true
	rr := doReq(r, http.MethodPost, "/v1/financial-snapshots/"+id+"/certify", domain.CertifySnapshotRequest{Reason: "period close"}, "certifier-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCertifySnapshot_RequiresReason(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	id := createSealedSnapshot(t, r, false)
	rr := doReq(r, http.MethodPost, "/v1/financial-snapshots/"+id+"/certify", domain.CertifySnapshotRequest{}, "certifier-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCertifySnapshot_UsesDistinctAuthorizationAction(t *testing.T) {
	s := newStubStore()
	s.snapshots["snap-1"] = &domain.FinancialSnapshot{SnapshotID: "snap-1", LegalEntityID: "le-1", Status: domain.SnapshotStatusSealed}
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(s, &stubPublisher{}, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	rr := doReq(rt, http.MethodPost, "/v1/financial-snapshots/snap-1/certify", domain.CertifySnapshotRequest{Reason: "x"}, "certifier-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "SNAPSHOT_CERTIFY" {
		t.Errorf("expected SNAPSHOT_CERTIFY, got %q", seenAction)
	}
}

func TestSupersedeSnapshot_MarksPriorSupersededAndCreatesNew(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	priorID := createSealedSnapshot(t, r, false)

	req := domain.CreateFinancialSnapshotRequest{Purpose: "AUDIT", Content: `{"trial_balance":{"1000-Cash":5200}}`, SourceReferences: `["trial_balance_snapshot:tbs-2"]`}
	rr := doReq(r, http.MethodPost, "/v1/financial-snapshots/"+priorID+"/supersede", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var next domain.FinancialSnapshot
	_ = json.NewDecoder(rr.Body).Decode(&next)
	if next.Status != domain.SnapshotStatusDraft {
		t.Errorf("expected new snapshot in DRAFT, got %q", next.Status)
	}

	priorRR := doReq(r, http.MethodGet, "/v1/financial-snapshots/"+priorID, nil, "preparer-1")
	var prior domain.FinancialSnapshot
	_ = json.NewDecoder(priorRR.Body).Decode(&prior)
	if prior.Status != domain.SnapshotStatusSuperseded {
		t.Errorf("expected prior SUPERSEDED, got %q", prior.Status)
	}
	if prior.SupersededBySnapshotID == nil || *prior.SupersededBySnapshotID != next.SnapshotID {
		t.Errorf("expected prior to point at the new snapshot, got %v", prior.SupersededBySnapshotID)
	}
}

// ── ACC-18 (Source-to-Report Traceability) ──────────────────────────────────────

func TestAccrualRecognition_RecordsLineageEdge(t *testing.T) {
	// Real integration test: a normal accrual recognition (already
	// covered by ACC-07's own tests) must ALSO leave a real lineage edge
	// behind — the whole point of ACC-18 is indexing what other
	// capabilities already produce, not a bolt-on nobody actually wires.
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")
	recReq := domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}
	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", recReq, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("recognize failed: %d %s", rr.Code, rr.Body.String())
	}
	var inst domain.RecognitionInstance
	_ = json.NewDecoder(rr.Body).Decode(&inst)

	edges, err := s.ListLineageEdgesTo(context.Background(), "journal", inst.JournalID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].FromType != "accrual_recognition" || edges[0].FromID != inst.RecognitionInstanceID {
		t.Fatalf("expected exactly 1 lineage edge from this recognition to its journal, got %+v", edges)
	}
}

func TestTraceJournalToSource_NoEdges_ReturnsEmptyNotError(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/lineage/journals/unknown-journal/source", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var edges []domain.LineageEdge
	_ = json.NewDecoder(rr.Body).Decode(&edges)
	if len(edges) != 0 {
		t.Errorf("expected no edges, got %+v", edges)
	}
}

func TestVerifyLineageCompleteness_ReportsGapForUnrecordedEdge(t *testing.T) {
	s := newStubStore()
	// A journal general-ledger-svc has, but this service never recorded a
	// lineage edge for — the spec's own negative path, "Missing
	// journal-source link."
	s.postedJournalRefs = []domain.PostedJournalRef{
		{FromType: "allocation_run", FromID: "run-1", JournalID: "journal-1"},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/lineage/verify?legal_entity_id=le-1", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var report domain.LineageCompletenessReport
	_ = json.NewDecoder(rr.Body).Decode(&report)
	if report.Complete {
		t.Error("expected Complete=false")
	}
	if len(report.Gaps) != 1 || report.Gaps[0].JournalID != "journal-1" {
		t.Fatalf("expected 1 gap for journal-1, got %+v", report.Gaps)
	}
}

func TestVerifyLineageCompleteness_NoGapsWhenEdgeRecorded(t *testing.T) {
	s := newStubStore()
	s.postedJournalRefs = []domain.PostedJournalRef{
		{FromType: "allocation_run", FromID: "run-1", JournalID: "journal-1"},
	}
	s.lineageEdges = []domain.LineageEdge{
		{FromType: "allocation_run", FromID: "run-1", ToType: "journal", ToID: "journal-1"},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/lineage/verify?legal_entity_id=le-1", nil, "preparer-1")
	var report domain.LineageCompletenessReport
	_ = json.NewDecoder(rr.Body).Decode(&report)
	if !report.Complete {
		t.Errorf("expected Complete=true, got gaps: %+v", report.Gaps)
	}
}

func TestRebuildLineageProjection_ClosesGapsAndRestoresCurrent(t *testing.T) {
	s := newStubStore()
	s.postedJournalRefs = []domain.PostedJournalRef{
		{FromType: "fx_revaluation_run", FromID: "run-1", JournalID: "journal-1"},
	}
	s.projectionStatus["le-1"] = &domain.LineageProjectionStatus{LegalEntityID: "le-1", Status: domain.LineageProjectionDegraded}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	rr := doReq(r, http.MethodPost, "/v1/lineage/rebuild?legal_entity_id=le-1", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var status domain.LineageProjectionStatus
	_ = json.NewDecoder(rr.Body).Decode(&status)
	if status.Status != domain.LineageProjectionCurrent {
		t.Errorf("expected CURRENT after rebuild, got %q", status.Status)
	}

	edges, _ := s.ListLineageEdgesTo(context.Background(), "journal", "journal-1")
	if len(edges) != 1 {
		t.Fatalf("expected rebuild to record the missing edge, got %d", len(edges))
	}
}

func TestRebuildLineageProjection_UsesDistinctAuthorizationAction(t *testing.T) {
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(newStubStore(), &stubPublisher{}, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)
	rr := doReq(rt, http.MethodPost, "/v1/lineage/rebuild?legal_entity_id=le-1", nil, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "LINEAGE_REBUILD" {
		t.Errorf("expected LINEAGE_REBUILD, got %q", seenAction)
	}
}

func strPtr(s string) *string { return &s }

type recordingAuthZ struct {
	onCheck func(principalID, legalEntityID, actionType string) error
}

func (a *recordingAuthZ) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	return a.onCheck(principalID, legalEntityID, actionType)
}
