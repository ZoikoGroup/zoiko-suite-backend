//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

// idemKey returns a fresh Idempotency-Key for a call that isn't itself
// testing replay behavior — a distinct key per call means it never
// interferes with that call succeeding.
func idemKey() string { return uuid.NewString() }

func newMappingDraft(tenantID, mappingKey, accountCode, creator string) *domain.AccountMapping {
	return &domain.AccountMapping{
		AccountMappingID:     uuid.NewString(),
		TenantID:             tenantID,
		MappingKey:           mappingKey,
		AccountCode:          accountCode,
		EffectiveFrom:        time.Now().UTC(),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: creator,
	}
}

// TestPgStore_AccountMappingLifecycle_DraftApproveActivate proves the
// ordinary path against real Postgres: a mapping starts DRAFT, is not
// resolvable by GetCurrentAccountMapping until it is ACTIVE, and activating
// a second mapping for the same key supersedes the first — the structural
// guarantee idx_account_mappings_active (migration 000029) gives ACC-02's
// "no fallback guessing" requirement.
func TestPgStore_AccountMappingLifecycle_DraftApproveActivate(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acctA := newTestAccount(tenantID, "4000", "Revenue A")
	acctB := newTestAccount(tenantID, "4001", "Revenue B")
	if err := s.CreateAccount(ctx, acctA); err != nil {
		t.Fatalf("CreateAccount A: %v", err)
	}
	if err := s.CreateAccount(ctx, acctB); err != nil {
		t.Fatalf("CreateAccount B: %v", err)
	}

	m1 := newMappingDraft(tenantID, "REVENUE:WIDGETS", "4000", "author-1")
	if _, err := s.CreateMapping(ctx, m1, idemKey()); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if m1.Status != domain.AccountMappingStatusDraft {
		t.Fatalf("expected DRAFT, got %s", m1.Status)
	}

	if _, err := s.GetCurrentAccountMapping(ctx, tenantID, "REVENUE:WIDGETS"); !errors.Is(err, domain.ErrAccountMappingNotFound) {
		t.Fatalf("a DRAFT mapping must not resolve, got %v", err)
	}

	approved, _, err := s.ApproveMapping(ctx, tenantID, m1.AccountMappingID, "approver-1", idemKey())
	if err != nil {
		t.Fatalf("ApproveMapping: %v", err)
	}
	if approved.Status != domain.AccountMappingStatusApproved {
		t.Fatalf("expected APPROVED, got %s", approved.Status)
	}

	if _, err := s.GetCurrentAccountMapping(ctx, tenantID, "REVENUE:WIDGETS"); !errors.Is(err, domain.ErrAccountMappingNotFound) {
		t.Fatalf("an APPROVED mapping must not resolve either, got %v", err)
	}

	activated, _, err := s.ActivateMapping(ctx, tenantID, m1.AccountMappingID, time.Time{}, idemKey())
	if err != nil {
		t.Fatalf("ActivateMapping: %v", err)
	}
	if activated.Status != domain.AccountMappingStatusActive {
		t.Fatalf("expected ACTIVE, got %s", activated.Status)
	}

	resolved, err := s.GetCurrentAccountMapping(ctx, tenantID, "REVENUE:WIDGETS")
	if err != nil {
		t.Fatalf("GetCurrentAccountMapping: %v", err)
	}
	if resolved.AccountCode != "4000" {
		t.Fatalf("expected resolution to 4000, got %s", resolved.AccountCode)
	}

	// A second mapping activated for the same key supersedes the first —
	// idx_account_mappings_active permits only one ACTIVE row per key, so
	// this also proves the index doesn't block the legitimate replacement.
	m2 := newMappingDraft(tenantID, "REVENUE:WIDGETS", "4001", "author-1")
	if _, err := s.CreateMapping(ctx, m2, idemKey()); err != nil {
		t.Fatalf("CreateMapping m2: %v", err)
	}
	if _, _, err := s.ApproveMapping(ctx, tenantID, m2.AccountMappingID, "approver-1", idemKey()); err != nil {
		t.Fatalf("ApproveMapping m2: %v", err)
	}
	if _, _, err := s.ActivateMapping(ctx, tenantID, m2.AccountMappingID, time.Time{}, idemKey()); err != nil {
		t.Fatalf("ActivateMapping m2: %v", err)
	}

	resolved2, err := s.GetCurrentAccountMapping(ctx, tenantID, "REVENUE:WIDGETS")
	if err != nil {
		t.Fatalf("GetCurrentAccountMapping after supersession: %v", err)
	}
	if resolved2.AccountCode != "4001" {
		t.Fatalf("expected resolution to 4001 after supersession, got %s", resolved2.AccountCode)
	}

	oldVersion, err := s.GetMappingByID(ctx, tenantID, m1.AccountMappingID)
	if err != nil {
		t.Fatalf("GetMappingByID m1: %v", err)
	}
	if oldVersion.Status != domain.AccountMappingStatusSuperseded || oldVersion.SupersededByMappingID == nil || *oldVersion.SupersededByMappingID != m2.AccountMappingID {
		t.Fatalf("expected m1 superseded by m2, got %+v", oldVersion)
	}
}

// TestPgStore_AccountMappingLifecycle_HighRiskMakerChecker proves ACC-02's
// SoD requirement: a mapping onto a control account cannot be approved by
// its own author, and SetAccountMapping (the one-step legacy path) must
// refuse a control-account target outright rather than let it bypass
// governance.
func TestPgStore_AccountMappingLifecycle_HighRiskMakerChecker(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	control := newTestAccount(tenantID, "1100", "AR Control")
	control.IsControlAccount = true
	if err := s.CreateAccount(ctx, control); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	legacy := &domain.AccountMapping{
		AccountMappingID:     uuid.NewString(),
		TenantID:             tenantID,
		MappingKey:           "SUBLEDGER:AR",
		AccountCode:          "1100",
		EffectiveFrom:        time.Now().UTC(),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "author-1",
	}
	if err := s.SetAccountMapping(ctx, legacy); !errors.Is(err, domain.ErrMappingHighRiskRequiresGovernedLifecycle) {
		t.Fatalf("expected ErrMappingHighRiskRequiresGovernedLifecycle, got %v", err)
	}

	m := newMappingDraft(tenantID, "SUBLEDGER:AR", "1100", "author-1")
	if _, err := s.CreateMapping(ctx, m, idemKey()); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if !m.IsHighRisk {
		t.Fatalf("expected IsHighRisk=true for a control-account mapping")
	}

	if _, _, err := s.ApproveMapping(ctx, tenantID, m.AccountMappingID, "author-1", idemKey()); !errors.Is(err, domain.ErrMappingSelfApproval) {
		t.Fatalf("expected ErrMappingSelfApproval for self-approval of a high-risk mapping, got %v", err)
	}

	approved, _, err := s.ApproveMapping(ctx, tenantID, m.AccountMappingID, "approver-2", idemKey())
	if err != nil {
		t.Fatalf("ApproveMapping by a different principal: %v", err)
	}
	if approved.Status != domain.AccountMappingStatusApproved {
		t.Fatalf("expected APPROVED, got %s", approved.Status)
	}
}

// TestPgStore_AccountMappingLifecycle_SupersedeWithoutReplacement proves the
// direct-retirement negative path: an ACTIVE mapping can be superseded with
// nothing yet approved to replace it, and the key then resolves to nothing —
// the posting engine must fail closed rather than guess.
func TestPgStore_AccountMappingLifecycle_SupersedeWithoutReplacement(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acct := newTestAccount(tenantID, "5000", "Expense")
	if err := s.CreateAccount(ctx, acct); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	m := newMappingDraft(tenantID, "EXPENSE:TRAVEL", "5000", "author-1")
	if _, err := s.CreateMapping(ctx, m, idemKey()); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if _, _, err := s.ApproveMapping(ctx, tenantID, m.AccountMappingID, "approver-1", idemKey()); err != nil {
		t.Fatalf("ApproveMapping: %v", err)
	}
	if _, _, err := s.ActivateMapping(ctx, tenantID, m.AccountMappingID, time.Time{}, idemKey()); err != nil {
		t.Fatalf("ActivateMapping: %v", err)
	}

	if _, _, err := s.SupersedeMapping(ctx, tenantID, m.AccountMappingID, "approver-1", idemKey()); err != nil {
		t.Fatalf("SupersedeMapping: %v", err)
	}
	if _, err := s.GetCurrentAccountMapping(ctx, tenantID, "EXPENSE:TRAVEL"); !errors.Is(err, domain.ErrAccountMappingNotFound) {
		t.Fatalf("expected ErrAccountMappingNotFound after direct supersede, got %v", err)
	}

	// A second supersede must be refused — it is no longer ACTIVE.
	if _, _, err := s.SupersedeMapping(ctx, tenantID, m.AccountMappingID, "approver-1", idemKey()); !errors.Is(err, domain.ErrMappingNotActive) {
		t.Fatalf("expected ErrMappingNotActive on a repeat supersede, got %v", err)
	}
}

// TestPgStore_GetMappingAsOf_ReconstructsHistory proves the point-in-time
// read: a query before activation sees nothing, between activation and
// supersession sees the version that was in force then, and after
// supersession a later as_of sees the replacement — history is read from the
// rows themselves, not rewritten when a mapping is superseded.
func TestPgStore_GetMappingAsOf_ReconstructsHistory(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acctA := newTestAccount(tenantID, "6000", "COGS A")
	acctB := newTestAccount(tenantID, "6001", "COGS B")
	if err := s.CreateAccount(ctx, acctA); err != nil {
		t.Fatalf("CreateAccount A: %v", err)
	}
	if err := s.CreateAccount(ctx, acctB); err != nil {
		t.Fatalf("CreateAccount B: %v", err)
	}

	before := time.Now().UTC().Add(-time.Hour)

	m1 := newMappingDraft(tenantID, "COGS:MAIN", "6000", "author-1")
	if _, err := s.CreateMapping(ctx, m1, idemKey()); err != nil {
		t.Fatalf("CreateMapping m1: %v", err)
	}
	if _, _, err := s.ApproveMapping(ctx, tenantID, m1.AccountMappingID, "approver-1", idemKey()); err != nil {
		t.Fatalf("ApproveMapping m1: %v", err)
	}
	t1 := time.Now().UTC()
	if _, _, err := s.ActivateMapping(ctx, tenantID, m1.AccountMappingID, t1, idemKey()); err != nil {
		t.Fatalf("ActivateMapping m1: %v", err)
	}

	mid := time.Now().UTC()

	m2 := newMappingDraft(tenantID, "COGS:MAIN", "6001", "author-1")
	if _, err := s.CreateMapping(ctx, m2, idemKey()); err != nil {
		t.Fatalf("CreateMapping m2: %v", err)
	}
	if _, _, err := s.ApproveMapping(ctx, tenantID, m2.AccountMappingID, "approver-1", idemKey()); err != nil {
		t.Fatalf("ApproveMapping m2: %v", err)
	}
	t2 := time.Now().UTC()
	if _, _, err := s.ActivateMapping(ctx, tenantID, m2.AccountMappingID, t2, idemKey()); err != nil {
		t.Fatalf("ActivateMapping m2: %v", err)
	}

	after := time.Now().UTC()

	if _, err := s.GetMappingAsOf(ctx, tenantID, "COGS:MAIN", before); !errors.Is(err, domain.ErrAccountMappingNotFound) {
		t.Fatalf("expected nothing resolvable before either activation, got %v", err)
	}
	atMid, err := s.GetMappingAsOf(ctx, tenantID, "COGS:MAIN", mid)
	if err != nil || atMid.AccountCode != "6000" {
		t.Fatalf("expected 6000 to be in force at mid, got %+v (err %v)", atMid, err)
	}
	atAfter, err := s.GetMappingAsOf(ctx, tenantID, "COGS:MAIN", after)
	if err != nil || atAfter.AccountCode != "6001" {
		t.Fatalf("expected 6001 to be in force after the replacement, got %+v (err %v)", atAfter, err)
	}
}

// TestPgStore_AccountMappingLifecycle_IdempotencyKeyReplay proves the
// Engineering Interaction Wireframe's own "Mandatory controls" for all four
// ACC-02 commands: a retry with the same Idempotency-Key replays the
// original result rather than either duplicating the write (CreateMapping)
// or erroring on a state the retry itself already produced (Approve/
// Activate/Supersede) — and a key reused for a DIFFERENT mapping or command
// is refused as a genuine conflict, not silently honored.
func TestPgStore_AccountMappingLifecycle_IdempotencyKeyReplay(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acct := newTestAccount(tenantID, "7000", "Other Income")
	if err := s.CreateAccount(ctx, acct); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	createKey := idemKey()
	m := newMappingDraft(tenantID, "OTHER_INCOME:MISC", "7000", "author-1")
	if replay, err := s.CreateMapping(ctx, m, createKey); err != nil || replay {
		t.Fatalf("first CreateMapping: replay=%v err=%v", replay, err)
	}
	firstID := m.AccountMappingID

	retry := newMappingDraft(tenantID, "OTHER_INCOME:MISC", "7000", "author-1")
	if replay, err := s.CreateMapping(ctx, retry, createKey); err != nil || !replay {
		t.Fatalf("retried CreateMapping must replay: replay=%v err=%v", replay, err)
	}
	if retry.AccountMappingID != firstID {
		t.Fatalf("a replayed CreateMapping must return the ORIGINAL mapping, not draft a second one: got %s want %s", retry.AccountMappingID, firstID)
	}
	var mappingCount int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM account_mappings WHERE tenant_id = $1 AND mapping_key = $2`, tenantID, "OTHER_INCOME:MISC").Scan(&mappingCount); err != nil {
		t.Fatalf("count mappings: %v", err)
	}
	if mappingCount != 1 {
		t.Fatalf("a retried CreateMapping must not draft a second row, got %d rows", mappingCount)
	}

	// Reusing createKey for a DIFFERENT mapping is a genuine conflict, not a replay.
	other := newMappingDraft(tenantID, "OTHER_INCOME:DIFFERENT", "7000", "author-1")
	if _, err := s.CreateMapping(ctx, other, createKey); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("expected ErrIdempotencyKeyConflict reusing a key for a different mapping, got %v", err)
	}

	approveKey := idemKey()
	approved1, replay, err := s.ApproveMapping(ctx, tenantID, firstID, "approver-2", approveKey)
	if err != nil || replay {
		t.Fatalf("first ApproveMapping: replay=%v err=%v", replay, err)
	}
	approved2, replay, err := s.ApproveMapping(ctx, tenantID, firstID, "approver-2", approveKey)
	if err != nil || !replay {
		t.Fatalf("retried ApproveMapping must replay: replay=%v err=%v", replay, err)
	}
	if approved1.Status != domain.AccountMappingStatusApproved || approved2.Status != domain.AccountMappingStatusApproved {
		t.Fatalf("both the original and the replay must report APPROVED, got %s and %s", approved1.Status, approved2.Status)
	}

	activateKey := idemKey()
	activated1, replay, err := s.ActivateMapping(ctx, tenantID, firstID, time.Time{}, activateKey)
	if err != nil || replay {
		t.Fatalf("first ActivateMapping: replay=%v err=%v", replay, err)
	}
	activated2, replay, err := s.ActivateMapping(ctx, tenantID, firstID, time.Time{}, activateKey)
	if err != nil || !replay {
		t.Fatalf("retried ActivateMapping must replay: replay=%v err=%v", replay, err)
	}
	if activated1.Status != domain.AccountMappingStatusActive || activated2.Status != domain.AccountMappingStatusActive {
		t.Fatalf("both the original and the replay must report ACTIVE, got %s and %s", activated1.Status, activated2.Status)
	}

	supersedeKey := idemKey()
	superseded1, replay, err := s.SupersedeMapping(ctx, tenantID, firstID, "approver-2", supersedeKey)
	if err != nil || replay {
		t.Fatalf("first SupersedeMapping: replay=%v err=%v", replay, err)
	}
	superseded2, replay, err := s.SupersedeMapping(ctx, tenantID, firstID, "approver-2", supersedeKey)
	if err != nil || !replay {
		t.Fatalf("retried SupersedeMapping must replay: replay=%v err=%v", replay, err)
	}
	if superseded1.Status != domain.AccountMappingStatusSuperseded || superseded2.Status != domain.AccountMappingStatusSuperseded {
		t.Fatalf("both the original and the replay must report SUPERSEDED, got %s and %s", superseded1.Status, superseded2.Status)
	}
}

// TestPgStore_ActivateMapping_ConcurrentActivation_OneWinsOneGetsCleanConflict
// is ACC-02's negative path #1, "two active mappings with same precedence,"
// actually happening: two different APPROVED mappings for the SAME key, both
// racing ActivateMapping. idx_account_mappings_active (migration 000029)
// guarantees only one can ever commit as ACTIVE; this proves the loser gets
// a clean, named ErrMappingActivationConflict — not a raw constraint
// violation surfaced as a generic 503 — and that the collision is recorded
// as evidence (a MappingAmbiguityDetected event in the outbox).
//
// A plain goroutine race (start both, see who wins) is not reliable here:
// ActivateMapping's own prevID lookup means a call that starts even
// slightly after the other has COMMITTED takes the ordinary
// supersede-by-replacement path, not the race path — this was tried first
// and both calls simply succeeded sequentially, proving nothing. To force
// the actual race deterministically, transaction A's activation is driven
// by hand (the same SQL ActivateMapping itself runs) and held OPEN,
// UNCOMMITTED, across the point where transaction B's real
// s.ActivateMapping call reaches its own conflicting UPDATE and blocks on
// A's still-uncommitted row — only then is A committed, so B's block
// resolves into the exact 23505 this test exists to prove is handled
// cleanly.
func TestPgStore_ActivateMapping_ConcurrentActivation_OneWinsOneGetsCleanConflict(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acctA := newTestAccount(tenantID, "8000", "Misc A")
	acctB := newTestAccount(tenantID, "8001", "Misc B")
	if err := s.CreateAccount(ctx, acctA); err != nil {
		t.Fatalf("CreateAccount A: %v", err)
	}
	if err := s.CreateAccount(ctx, acctB); err != nil {
		t.Fatalf("CreateAccount B: %v", err)
	}

	mA := newMappingDraft(tenantID, "RACE:KEY", "8000", "author-1")
	mB := newMappingDraft(tenantID, "RACE:KEY", "8001", "author-1")
	if _, err := s.CreateMapping(ctx, mA, idemKey()); err != nil {
		t.Fatalf("CreateMapping A: %v", err)
	}
	if _, err := s.CreateMapping(ctx, mB, idemKey()); err != nil {
		t.Fatalf("CreateMapping B: %v", err)
	}
	if _, _, err := s.ApproveMapping(ctx, tenantID, mA.AccountMappingID, "approver-1", idemKey()); err != nil {
		t.Fatalf("ApproveMapping A: %v", err)
	}
	if _, _, err := s.ApproveMapping(ctx, tenantID, mB.AccountMappingID, "approver-1", idemKey()); err != nil {
		t.Fatalf("ApproveMapping B: %v", err)
	}

	// Transaction A: activate mA by hand, same SQL as ActivateMapping's own
	// final UPDATE, and leave it uncommitted.
	txA, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin txA: %v", err)
	}
	defer txA.Rollback(context.Background()) //nolint:errcheck
	if _, err := txA.Exec(context.Background(), `
		UPDATE account_mappings SET status = 'ACTIVE', activated_at = NOW(), effective_from = NOW()
		WHERE tenant_id = $1 AND account_mapping_id = $2 AND status = 'APPROVED'
	`, tenantID, mA.AccountMappingID); err != nil {
		t.Fatalf("txA activate mA: %v", err)
	}

	bDone := make(chan struct{ err error })
	go func() {
		_, _, err := s.ActivateMapping(ctx, tenantID, mB.AccountMappingID, time.Time{}, idemKey())
		bDone <- struct{ err error }{err}
	}()

	// Give B time to reach its own conflicting UPDATE and block on txA's
	// still-uncommitted row before txA is released.
	time.Sleep(300 * time.Millisecond)
	if err := txA.Commit(context.Background()); err != nil {
		t.Fatalf("commit txA: %v", err)
	}

	result := <-bDone
	if !errors.Is(result.err, domain.ErrMappingActivationConflict) {
		t.Fatalf("expected B to lose the race with ErrMappingActivationConflict, got %v", result.err)
	}

	var activeRows int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM account_mappings WHERE tenant_id = $1 AND mapping_key = $2 AND status = 'ACTIVE'`, tenantID, "RACE:KEY").Scan(&activeRows); err != nil {
		t.Fatalf("count active rows: %v", err)
	}
	if activeRows != 1 {
		t.Fatalf("expected exactly one ACTIVE row to survive the race, got %d", activeRows)
	}

	resolved, err := s.GetCurrentAccountMapping(ctx, tenantID, "RACE:KEY")
	if err != nil || resolved.AccountCode != "8000" {
		t.Fatalf("expected A (8000) to be the resolvable winner, got %+v (err %v)", resolved, err)
	}

	bRow, err := s.GetMappingByID(ctx, tenantID, mB.AccountMappingID)
	if err != nil {
		t.Fatalf("GetMappingByID B: %v", err)
	}
	if bRow.Status != domain.AccountMappingStatusApproved {
		t.Fatalf("B's own transaction must have rolled back on conflict, leaving it APPROVED, got %s", bRow.Status)
	}

	var ambiguityEvents int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM eventing_outbox
		WHERE tenant_id = $1 AND event_type = 'com.zoikosuite.accounting.account-mapping.ambiguity-detected'
	`, tenantID).Scan(&ambiguityEvents); err != nil {
		t.Fatalf("count ambiguity events: %v", err)
	}
	if ambiguityEvents != 1 {
		t.Fatalf("expected exactly one MappingAmbiguityDetected event recorded as evidence, got %d", ambiguityEvents)
	}
}
