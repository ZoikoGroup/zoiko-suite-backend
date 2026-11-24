package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// CFG-1 (API-3): the store compares the caller's If-Match with the row it has
// locked, so a stale ETag is refused inside the write transaction and nothing
// changes.

func ifMatch(v int64) context.Context {
	return domain.WithPrecondition(context.Background(), domain.Precondition{Version: v})
}

func wantStale(t *testing.T, what string, err error, current int64) {
	t.Helper()
	var stale *domain.StaleVersionError
	if !errors.As(err, &stale) {
		t.Fatalf("%s: want StaleVersionError, got %v", what, err)
	}
	if stale.Current != current {
		t.Fatalf("%s: current version %d, want %d", what, stale.Current, current)
	}
}

func TestCFG1_ApproveChange_StaleIfMatchRefusedUnderTheLock(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	c, err := s.CreateChange(ctx, domain.CreateChangeParams{
		ChangeClass: domain.ChangeClassC2, Environment: "staging", ApprovalRequired: true,
		Parts: []domain.ChangePart{
			{Kind: domain.PartKindConfig, Key: "payroll.batch_size", Scope: domain.ChangePartScope{Environment: "staging"}, NewValue: []byte(`300`)},
		},
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The create response is where a caller first gets the ETag, so it must
	// carry the stored version (it used to answer 0).
	if c.RecordVersion != 1 {
		t.Fatalf("a new change must report record_version 1, got %d", c.RecordVersion)
	}
	approval := domain.ChangeApproval{Approved: true, ByPrincipalID: "approver-1", ApprovedAt: time.Now()}

	_, err = s.ApproveChange(ifMatch(c.RecordVersion+1), c.ChangeID, approval, testCallerTenant)
	wantStale(t, "approve with a stale ETag", err, c.RecordVersion)
	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM config_changes WHERE change_id = $1", c.ChangeID).Scan(&status); err != nil || status != c.Status {
		t.Fatalf("a refused approval changed the change: status %q (was %q) %v", status, c.Status, err)
	}

	approved, err := s.ApproveChange(ifMatch(c.RecordVersion), c.ChangeID, approval, testCallerTenant)
	if err != nil {
		t.Fatalf("approve with the current ETag: %v", err)
	}
	if approved.RecordVersion <= c.RecordVersion {
		t.Fatalf("approval did not advance record_version: %d -> %d", c.RecordVersion, approved.RecordVersion)
	}

	// The ETag the approval was made with is now stale for activation, even
	// though the handler's own pre-check is not in play here.
	_, err = s.ActivateChange(ifMatch(c.RecordVersion), c.ChangeID, testCallerTenant, "operator-1")
	wantStale(t, "activate with the pre-approval ETag", err, approved.RecordVersion)
	verified, err := s.ActivateChange(ifMatch(approved.RecordVersion), c.ChangeID, testCallerTenant, "operator-1")
	if err != nil {
		t.Fatalf("activate with the current ETag: %v", err)
	}

	// Rollback names the version of the change it undoes.
	_, err = s.RollbackChange(ifMatch(approved.RecordVersion), c.ChangeID, testCallerTenant, "admin-2", "")
	wantStale(t, "rollback with a stale ETag", err, verified.RecordVersion)
	if _, err := s.RollbackChange(ifMatch(verified.RecordVersion), c.ChangeID, testCallerTenant, "admin-2", ""); err != nil {
		t.Fatalf("rollback with the current ETag: %v", err)
	}
}

func TestCFG1_ReleasePlan_VersionPrecondition(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "reports.advanced")
	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "reports.advanced", Enabled: true, RolloutPercentage: 100, Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("flag: %v", err)
	}
	publish := func(pc domain.Precondition) (*domain.ReleasePlan, error) {
		return s.CreateReleasePlan(domain.WithPrecondition(ctx, pc), domain.CreateReleasePlanParams{
			FlagKey: "reports.advanced", Environment: "staging", Strategy: domain.StrategyAllOrNothing,
			TargetingRules: json.RawMessage(`{"percentage":100}`),
			CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
		})
	}

	_, err := publish(domain.Precondition{Version: 1})
	wantStale(t, "If-Match before any plan exists", err, 0)

	first, err := publish(domain.Precondition{NoneMatch: true})
	if err != nil || first.Version != 1 {
		t.Fatalf("first plan with If-None-Match: *: %+v %v", first, err)
	}
	_, err = publish(domain.Precondition{NoneMatch: true})
	wantStale(t, "If-None-Match: * once a plan exists", err, 1)

	second, err := publish(domain.Precondition{Version: 1})
	if err != nil || second.Version != 2 {
		t.Fatalf("second plan with If-Match v1: %+v %v", second, err)
	}
	_, err = publish(domain.Precondition{Version: 1})
	wantStale(t, "a second publisher holding v1", err, 2)

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM release_plans WHERE flag_key = 'reports.advanced'").Scan(&n); err != nil || n != 2 {
		t.Fatalf("refused publishes left plans behind: %d rows %v", n, err)
	}
}

func TestCFG1_FlagRemoval_VersionPrecondition(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "legacy.checkout")
	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "legacy.checkout", Enabled: true, RolloutPercentage: 100, Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("flag: %v", err)
	}
	retired, err := s.RetireFlag(ctx, domain.RetireFlagParams{
		Key: "legacy.checkout", Environment: "staging", FinalEnabled: true, FinalRollout: 100,
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if retired.RecordVersion != 1 {
		t.Fatalf("a new retirement must report record_version 1, got %d", retired.RecordVersion)
	}
	params := store.MarkFlagRemovedParams{
		Key: "legacy.checkout", Environment: "staging",
		ConsumerScanEvidence: json.RawMessage(`{"scan":"repo-wide","references":0}`),
		CallerTenantID:       testCallerTenant, ActorPrincipalID: "admin-1",
	}

	_, err = s.MarkFlagRemoved(ifMatch(retired.RecordVersion+1), params)
	wantStale(t, "removal with a stale ETag", err, retired.RecordVersion)
	var reusable bool
	if err := pool.QueryRow(ctx, "SELECT reusable FROM flag_retirements WHERE key = 'legacy.checkout'").Scan(&reusable); err != nil || reusable {
		t.Fatalf("a refused removal freed the key: reusable=%v %v", reusable, err)
	}

	removed, err := s.MarkFlagRemoved(ifMatch(retired.RecordVersion), params)
	if err != nil {
		t.Fatalf("removal with the current ETag: %v", err)
	}
	if !removed.Reusable || removed.RecordVersion != retired.RecordVersion+1 {
		t.Fatalf("removal: reusable=%v record_version=%d, want true and %d", removed.Reusable, removed.RecordVersion, retired.RecordVersion+1)
	}
}
