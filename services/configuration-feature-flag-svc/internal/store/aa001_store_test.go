package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── AA-001 parity tests ────────────────────────────────────────────────────────
//
// These assert the store-layer guarantees the AA-001 architecture added on top
// of the migration-000001 write path: every real transition mints an immutable
// snapshot and Find/List are served from it; emergency changes are time-boxed
// and swept; change sets carry an approval trail and only activate after it;
// attestations are single-use.
//
// Every test relies on openTestPool's full migration suite plus the seeded
// definitions that make the INV-05 write gate admit the keys it writes — the
// same fixture the legacy pg_store tests now use.

func TestAASnapshot_ConfigWriteMintsAndFindServesNewest(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	base := domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}

	v1 := base
	v1.Value = []byte(`100`)
	if _, created, err := s.UpsertConfigEntry(ctx, v1); err != nil || !created {
		t.Fatalf("v1 write: created=%v err=%v", created, err)
	}

	v2 := base
	v2.Value = []byte(`200`)
	if _, created, err := s.UpsertConfigEntry(ctx, v2); err != nil || !created {
		t.Fatalf("v2 write: created=%v err=%v", created, err)
	}

	var snapCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM config_snapshots WHERE environment = $1`, "staging",
	).Scan(&snapCount); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapCount != 2 {
		t.Fatalf("each real transition must mint exactly one snapshot, got %d for 2 transitions", snapCount)
	}

	var epoch int64
	if err := pool.QueryRow(ctx,
		`SELECT current_epoch FROM config_snapshot_epochs WHERE environment = $1`, "staging",
	).Scan(&epoch); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if epoch != 2 {
		t.Fatalf("expected epoch 2 after two transitions, got %d", epoch)
	}

	cur, err := s.FindCurrentConfigEntry(ctx, "payroll.batch_size", "staging", nil)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if string(cur.Value) != "200" {
		t.Fatalf("Find must serve the newest snapshot's value, got %s", cur.Value)
	}
}

func TestAA_NoOpWriteDoesNotMintSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	params := domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Value: []byte(`100`), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}
	if _, created, err := s.UpsertConfigEntry(ctx, params); err != nil || !created {
		t.Fatalf("first write: created=%v err=%v", created, err)
	}
	if _, created, err := s.UpsertConfigEntry(ctx, params); err != nil || created {
		t.Fatalf("idempotent repeat: created=%v err=%v", created, err)
	}

	var snapCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM config_snapshots WHERE environment = $1`, "staging",
	).Scan(&snapCount); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapCount != 1 {
		t.Fatalf("an idempotent no-op writes nothing, so it must not mint — got %d snapshots", snapCount)
	}
}

func TestAA_EmergencyChange_ActivateAppliesAndMints(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Value: []byte(`100`), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("baseline write: %v", err)
	}

	ec, err := s.CreateEmergencyChange(ctx, domain.CreateEmergencyChangeParams{
		Key: "payroll.batch_size", Environment: "staging",
		NewValue:         []byte(`250`),
		Reason:           "payroll output stall",
		IncidentID:       "INC-42",
		ActorPrincipalID: "oncall-1",
		ExpiresAt:        time.Now().Add(2 * time.Hour),
		CallerTenantID:   testCallerTenant,
	})
	if err != nil {
		t.Fatalf("create emergency change: %v", err)
	}
	if ec.Status != domain.EmergencyStatusOpen {
		t.Fatalf("expected a new emergency change to be OPEN, got %s", ec.Status)
	}

	act, err := s.ActivateEmergencyChange(ctx, ec.EmergencyChangeID, testCallerTenant, "oncall-1")
	if err != nil {
		t.Fatalf("activate emergency change: %v", err)
	}
	if act.Status != domain.EmergencyStatusActive {
		t.Fatalf("expected the emergency change to be ACTIVE, got %s", act.Status)
	}

	cur, err := s.FindCurrentConfigEntry(ctx, "payroll.batch_size", "staging", nil)
	if err != nil {
		t.Fatalf("find after emergency activation: %v", err)
	}
	if string(cur.Value) != "250" {
		t.Fatalf("the emergency value must be the currently-effective one, got %s", cur.Value)
	}

	// A still-open, unexpired emergency change must survive a sweep pass.
	res, err := s.SweepExpired(ctx, "staging")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.ExpiredEmergencyChanges != 0 {
		t.Fatalf("an unexpired emergency change must not be swept, got %d expired", res.ExpiredEmergencyChanges)
	}
}

func TestAA_EmergencyChange_OpenExpiredChangeIsSwept(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	ec, err := s.CreateEmergencyChange(ctx, domain.CreateEmergencyChangeParams{
		Key: "payroll.batch_size", Environment: "staging",
		NewValue:         []byte(`500`),
		Reason:           "latent change, never activated",
		IncidentID:       "INC-99",
		ActorPrincipalID: "oncall-1",
		ExpiresAt:        time.Now().Add(-10 * time.Minute),
		CallerTenantID:   testCallerTenant,
	})
	if err != nil {
		t.Fatalf("create emergency change: %v", err)
	}

	res, err := s.SweepExpired(ctx, "staging")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.ExpiredEmergencyChanges != 1 {
		t.Fatalf("expected the sweep to expire exactly the overdue OPEN change, got %d", res.ExpiredEmergencyChanges)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM emergency_changes WHERE emergency_change_id = $1`, ec.EmergencyChangeID,
	).Scan(&status); err != nil {
		t.Fatalf("reload status: %v", err)
	}
	if status != domain.EmergencyStatusExpired {
		t.Fatalf("expected the swept change to be EXPIRED, got %s", status)
	}
}

func TestAA_Change_ApproveThenActivateAppliesConfig(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Value: []byte(`100`), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("baseline write: %v", err)
	}

	change, err := s.CreateChange(ctx, domain.CreateChangeParams{
		ChangeClass:      domain.ChangeClassC2,
		Environment:      "staging",
		Parts: []domain.ChangePart{{
			Kind:     domain.PartKindConfig,
			Key:      "payroll.batch_size",
			Scope:    domain.ChangePartScope{Environment: "staging"},
			NewValue: []byte(`300`),
		}},
		ApprovalRequired: true,
		CallerTenantID:   testCallerTenant,
		ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create change: %v", err)
	}
	if change.Status != domain.ChangeStatusProposed {
		t.Fatalf("a new change must be PROPOSED, got %s", change.Status)
	}

	appr, err := s.ApproveChange(ctx, change.ChangeID, domain.ChangeApproval{
		Approved:      true,
		ByPrincipalID: "approver-1",
		ApprovedAt:    time.Now(),
	}, testCallerTenant)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if appr.Status != domain.ChangeStatusApproved {
		t.Fatalf("an approved change must be APPROVED, got %s", appr.Status)
	}

	act, err := s.ActivateChange(ctx, change.ChangeID, testCallerTenant, "operator-1")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if act.Status != domain.ChangeStatusVerified {
		t.Fatalf("an activated change must be VERIFIED, got %s", act.Status)
	}

	cur, err := s.FindCurrentConfigEntry(ctx, "payroll.batch_size", "staging", nil)
	if err != nil {
		t.Fatalf("find after activation: %v", err)
	}
	if string(cur.Value) != "300" {
		t.Fatalf("the activated change's value must be currently effective, got %s", cur.Value)
	}
}

func TestAA_Change_RejectedStaysProposedAndCannotActivate(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Value: []byte(`100`), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("baseline write: %v", err)
	}

	change, err := s.CreateChange(ctx, domain.CreateChangeParams{
		ChangeClass:      domain.ChangeClassC2,
		Environment:      "staging",
		Parts: []domain.ChangePart{{
			Kind:     domain.PartKindConfig,
			Key:      "payroll.batch_size",
			Scope:    domain.ChangePartScope{Environment: "staging"},
			NewValue: []byte(`300`),
		}},
		ApprovalRequired: true,
		CallerTenantID:   testCallerTenant,
		ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create change: %v", err)
	}

	rej, err := s.ApproveChange(ctx, change.ChangeID, domain.ChangeApproval{
		Approved:      false,
		ByPrincipalID: "approver-2",
		ApprovedAt:    time.Now(),
	}, testCallerTenant)
	if err != nil {
		t.Fatalf("reject approval: %v", err)
	}
	if rej.Status != domain.ChangeStatusProposed {
		t.Fatalf("a rejected change must stay PROPOSED, got %s", rej.Status)
	}

	if _, err := s.ActivateChange(ctx, change.ChangeID, testCallerTenant, "operator-1"); !errors.Is(err, domain.ErrChangeApprovalRequired) {
		t.Fatalf("a change without approval must be refused at activation, got %v", err)
	}
}

func TestAA_AttestationReplay_Refused(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "new_ui")

	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "new_ui", Enabled: true, Environment: "staging", RolloutPercentage: 50,
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("seed flag: %v", err)
	}

	params := domain.RecordAttestationParams{
		RuntimeID:      "runtime-1",
		AttestKey:      "att-1",
		Environment:    "staging",
		ObservedEpoch:  1,
		ObservedDigest: "d",
		CallerTenantID: testCallerTenant,
	}
	if _, err := s.RecordAttestation(ctx, params); err != nil {
		t.Fatalf("first attestation: %v", err)
	}
	if _, err := s.RecordAttestation(ctx, params); !errors.Is(err, domain.ErrValueConstraintFailed) {
		t.Fatalf("a replayed attestation with the same (runtime, attest_key) must be refused, got %v", err)
	}
}