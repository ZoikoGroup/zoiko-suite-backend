//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
)

func idemRequest(partyRef, account string) domain.ProposeDestinationRequest {
	return domain.ProposeDestinationRequest{
		LegalEntityID: "le-idem", PartyRef: partyRef, FinancialInstitution: "Idem Bank",
		AccountIdentifier: account, CountryCode: "GB", Currency: "GBP",
		PayeeName: "Idem Payee", SourceType: domain.SourceManualEntry,
	}
}

func idemVerifyRequest() domain.VerifyDestinationRequest {
	return domain.VerifyDestinationRequest{
		VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "evidence-idem",
	}
}

func mustProposeIdem(t *testing.T, f *fixture, ctx context.Context, req domain.ProposeDestinationRequest, key string) *domain.PayeeDestination {
	t.Helper()
	d, replay, err := f.s.ProposeDestination(ctx, f.tenant, req, "proposer", "corr-propose", key)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if replay {
		t.Fatalf("unexpected replay on first propose")
	}
	return d
}

func mustVerifyIdem(t *testing.T, f *fixture, ctx context.Context, destinationID, key string) *domain.PayeeDestination {
	t.Helper()
	d, replay, err := f.s.VerifyDestination(ctx, destinationID, idemVerifyRequest(), "verifier", key)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if replay {
		t.Fatalf("unexpected replay on first verify")
	}
	return d
}

func mustApproveIdem(t *testing.T, f *fixture, ctx context.Context, destinationID, key string) *domain.PayeeDestination {
	t.Helper()
	d, replay, err := f.s.ApproveDestination(ctx, destinationID, "approver", "corr-approve", key)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if replay {
		t.Fatalf("unexpected replay on first approve")
	}
	return d
}

func TestCommandIdempotency_ReplaysEveryCommand(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-idem-all-" + uuid.New().String()

	proposeKey := "idem-propose-" + uuid.New().String()
	req := idemRequest(partyRef, "GB29IDEM00000000000001")
	d, replay, err := f.s.ProposeDestination(ctx, f.tenant, req, "proposer", "corr-propose", proposeKey)
	if err != nil || replay {
		t.Fatalf("first propose: replay=%v err=%v", replay, err)
	}
	replayed, replay, err := f.s.ProposeDestination(ctx, f.tenant, req, "proposer", "corr-propose", proposeKey)
	if err != nil || !replay {
		t.Fatalf("propose replay: replay=%v err=%v", replay, err)
	}
	if replayed.DestinationID != d.DestinationID {
		t.Fatalf("propose replay returned %s, want %s", replayed.DestinationID, d.DestinationID)
	}
	versions, err := f.s.ListVersions(ctx, partyRef)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("propose replay created %d destinations, want 1", len(versions))
	}
	if got := len(outboxEventsFor(t, ctx, f, f.tenant, d.DestinationID)); got != 1 {
		t.Fatalf("propose replay produced %d outbox events, want 1", got)
	}

	verifyKey := "idem-verify-" + uuid.New().String()
	if _, replay, err = f.s.VerifyDestination(ctx, d.DestinationID, idemVerifyRequest(), "verifier", verifyKey); err != nil || replay {
		t.Fatalf("first verify: replay=%v err=%v", replay, err)
	}
	verified, replay, err := f.s.VerifyDestination(ctx, d.DestinationID, idemVerifyRequest(), "verifier", verifyKey)
	if err != nil || !replay {
		t.Fatalf("verify replay: replay=%v err=%v", replay, err)
	}
	if verified.Status != domain.StatusVerified {
		t.Fatalf("verify replay status = %s, want VERIFIED", verified.Status)
	}

	approveKey := "idem-approve-" + uuid.New().String()
	if _, replay, err = f.s.ApproveDestination(ctx, d.DestinationID, "approver", "corr-approve", approveKey); err != nil || replay {
		t.Fatalf("first approve: replay=%v err=%v", replay, err)
	}
	approved, replay, err := f.s.ApproveDestination(ctx, d.DestinationID, "approver", "corr-approve", approveKey)
	if err != nil || !replay {
		t.Fatalf("approve replay: replay=%v err=%v", replay, err)
	}
	if approved.Status != domain.StatusApprovalPending {
		t.Fatalf("approve replay status = %s, want APPROVAL_PENDING", approved.Status)
	}

	activateKey := "idem-activate-" + uuid.New().String()
	if _, replay, err = f.s.ActivateDestination(ctx, d.DestinationID, "activator", "corr-activate", activateKey); err != nil || replay {
		t.Fatalf("first activate: replay=%v err=%v", replay, err)
	}
	activated, replay, err := f.s.ActivateDestination(ctx, d.DestinationID, "activator", "corr-activate", activateKey)
	if err != nil || !replay {
		t.Fatalf("activate replay: replay=%v err=%v", replay, err)
	}
	if activated.Status != domain.StatusActive {
		t.Fatalf("activate replay status = %s, want ACTIVE", activated.Status)
	}

	suspendKey := "idem-suspend-" + uuid.New().String()
	if _, replay, err = f.s.SuspendDestination(ctx, d.DestinationID, domain.SuspendDestinationRequest{Reason: "fraud"}, "suspender", "corr-suspend", suspendKey); err != nil || replay {
		t.Fatalf("first suspend: replay=%v err=%v", replay, err)
	}
	suspended, replay, err := f.s.SuspendDestination(ctx, d.DestinationID, domain.SuspendDestinationRequest{Reason: "fraud"}, "suspender", "corr-suspend", suspendKey)
	if err != nil || !replay {
		t.Fatalf("suspend replay: replay=%v err=%v", replay, err)
	}
	if suspended.Status != domain.StatusSuspended {
		t.Fatalf("suspend replay status = %s, want SUSPENDED", suspended.Status)
	}

	// Replaying activate after a later suspend returns the CURRENT row; it
	// must not reactivate the destination or duplicate an event.
	afterSuspend, replay, err := f.s.ActivateDestination(ctx, d.DestinationID, "activator", "corr-activate", activateKey)
	if err != nil || !replay {
		t.Fatalf("activate replay after suspend: replay=%v err=%v", replay, err)
	}
	if afterSuspend.Status != domain.StatusSuspended {
		t.Fatalf("activate replay changed later state to %s, want SUSPENDED", afterSuspend.Status)
	}
	events, err := f.s.ListEvents(ctx, d.DestinationID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("replays duplicated the audit trail: got %d events, want 5", len(events))
	}
	if got := len(outboxEventsFor(t, ctx, f, f.tenant, d.DestinationID)); got != 4 {
		t.Fatalf("replays duplicated outbox rows: got %d, want 4 (proposed/approved/activated/suspended)", got)
	}

	// The sixth command, supersede, on a separate destination.
	party2 := "party-idem-supersede-" + uuid.New().String()
	second := mustProposeIdem(t, f, ctx, idemRequest(party2, "GB29IDEM00000000000002"), "idem-propose-2-"+uuid.New().String())
	mustVerifyIdem(t, f, ctx, second.DestinationID, "idem-verify-2-"+uuid.New().String())
	mustApproveIdem(t, f, ctx, second.DestinationID, "idem-approve-2-"+uuid.New().String())
	supersedeKey := "idem-supersede-" + uuid.New().String()
	if _, replay, err = f.s.SupersedeDestination(ctx, second.DestinationID, domain.SupersedeDestinationRequest{Reason: "replaced"}, "admin", "corr-supersede", supersedeKey); err != nil || replay {
		t.Fatalf("first supersede: replay=%v err=%v", replay, err)
	}
	superseded, replay, err := f.s.SupersedeDestination(ctx, second.DestinationID, domain.SupersedeDestinationRequest{Reason: "replaced"}, "admin", "corr-supersede", supersedeKey)
	if err != nil || !replay {
		t.Fatalf("supersede replay: replay=%v err=%v", replay, err)
	}
	if superseded.Status != domain.StatusSuperseded {
		t.Fatalf("supersede replay status = %s, want SUPERSEDED", superseded.Status)
	}
}

func TestCommandIdempotency_RefusedCommandDoesNotBurnKey(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-idem-refused-" + uuid.New().String()
	d := mustProposeIdem(t, f, ctx, idemRequest(partyRef, "GB29IDEM00000000000003"), "idem-refused-propose-"+uuid.New().String())

	key := "idem-refused-approve-" + uuid.New().String()
	if _, _, err := f.s.ApproveDestination(ctx, d.DestinationID, "approver", "corr", key); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("approve before verify: got %v, want ErrInvalidTransition", err)
	}
	current, err := f.s.FindDestination(ctx, d.DestinationID)
	if err != nil {
		t.Fatalf("find after refused approve: %v", err)
	}
	if current.Status != domain.StatusCandidate {
		t.Fatalf("refused approve changed status to %s", current.Status)
	}

	mustVerifyIdem(t, f, ctx, d.DestinationID, "idem-refused-verify-"+uuid.New().String())
	approved, replay, err := f.s.ApproveDestination(ctx, d.DestinationID, "approver", "corr", key)
	if err != nil {
		t.Fatalf("approve with the previously refused key: %v", err)
	}
	if replay {
		t.Fatalf("refused command must not have claimed the key; got replay")
	}
	if approved.Status != domain.StatusApprovalPending {
		t.Fatalf("approve status = %s, want APPROVAL_PENDING", approved.Status)
	}
}

func TestCommandIdempotency_KeyReuseAcrossCommandsIsConflict(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-idem-command-" + uuid.New().String()
	key := "idem-command-key-" + uuid.New().String()
	d := mustProposeIdem(t, f, ctx, idemRequest(partyRef, "GB29IDEM00000000000004"), key)

	if _, _, err := f.s.VerifyDestination(ctx, d.DestinationID, idemVerifyRequest(), "verifier", key); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("reusing a propose key for verify: got %v, want ErrIdempotencyKeyConflict", err)
	}
	current, err := f.s.FindDestination(ctx, d.DestinationID)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if current.Status != domain.StatusCandidate {
		t.Fatalf("conflicting command changed status to %s", current.Status)
	}
}

func TestCommandIdempotency_KeyReuseAcrossDestinationsIsConflict(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	key := "idem-destination-key-" + uuid.New().String()
	d1 := mustProposeIdem(t, f, ctx, idemRequest("party-idem-dest-1-"+uuid.New().String(), "GB29IDEM00000000000005"), "idem-dest-propose-1-"+uuid.New().String())
	d2 := mustProposeIdem(t, f, ctx, idemRequest("party-idem-dest-2-"+uuid.New().String(), "GB29IDEM00000000000006"), "idem-dest-propose-2-"+uuid.New().String())

	if _, _, err := f.s.VerifyDestination(ctx, d1.DestinationID, idemVerifyRequest(), "verifier", key); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, _, err := f.s.VerifyDestination(ctx, d2.DestinationID, idemVerifyRequest(), "verifier", key); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("key reuse across destinations: got %v, want ErrIdempotencyKeyConflict", err)
	}
	current, err := f.s.FindDestination(ctx, d2.DestinationID)
	if err != nil {
		t.Fatalf("find second: %v", err)
	}
	if current.Status != domain.StatusCandidate {
		t.Fatalf("conflicting destination changed status to %s", current.Status)
	}
}

func TestCommandIdempotency_ProposeSameKeyDifferentRequestIsConflict(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-idem-body-" + uuid.New().String()
	key := "idem-body-key-" + uuid.New().String()

	first := idemRequest(partyRef, "GB29IDEM00000000000007")
	d := mustProposeIdem(t, f, ctx, first, key)

	second := first
	second.AccountIdentifier = "GB29IDEM00000000000008"
	if _, _, err := f.s.ProposeDestination(ctx, f.tenant, second, "proposer", "corr", key); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key with a different request: got %v, want ErrIdempotencyKeyConflict", err)
	}
	versions, err := f.s.ListVersions(ctx, partyRef)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 || versions[0].DestinationID != d.DestinationID {
		t.Fatalf("conflicting propose created an extra destination: %+v", versions)
	}
}

func TestCommandIdempotency_ConcurrentProposeExecutesOnce(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-idem-concurrent-propose-" + uuid.New().String()
	req := idemRequest(partyRef, "GB29IDEM00000000000010")
	key := "idem-concurrent-propose-" + uuid.New().String()

	type result struct {
		d      *domain.PayeeDestination
		replay bool
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			d, replay, err := f.s.ProposeDestination(ctx, f.tenant, req, "proposer", "corr-propose", key)
			results <- result{d: d, replay: replay, err: err}
		}()
	}
	close(start)

	var ids []string
	replays := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent propose: %v", r.err)
		}
		ids = append(ids, r.d.DestinationID)
		if r.replay {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("concurrent propose: got %d replays, want exactly 1", replays)
	}
	if ids[0] != ids[1] {
		t.Fatalf("concurrent propose returned two destinations: %s and %s", ids[0], ids[1])
	}
	versions, err := f.s.ListVersions(ctx, partyRef)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("concurrent propose created %d destinations, want 1", len(versions))
	}
	if got := len(outboxEventsFor(t, ctx, f, f.tenant, ids[0])); got != 1 {
		t.Fatalf("concurrent propose produced %d outbox events, want 1", got)
	}
}

func TestCommandIdempotency_ConcurrentRetryExecutesOnce(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-idem-concurrent-" + uuid.New().String()
	d := mustProposeIdem(t, f, ctx, idemRequest(partyRef, "GB29IDEM00000000000009"), "idem-concurrent-propose-"+uuid.New().String())
	key := "idem-concurrent-verify-" + uuid.New().String()

	type result struct {
		replay bool
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, replay, err := f.s.VerifyDestination(ctx, d.DestinationID, idemVerifyRequest(), "verifier", key)
			results <- result{replay: replay, err: err}
		}()
	}
	close(start)

	replays := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent verify: %v", r.err)
		}
		if r.replay {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("concurrent retry: got %d replays, want exactly 1", replays)
	}

	current, err := f.s.FindDestination(ctx, d.DestinationID)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if current.Status != domain.StatusVerified {
		t.Fatalf("status = %s, want VERIFIED", current.Status)
	}
	events, err := f.s.ListEvents(ctx, d.DestinationID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("concurrent retry produced %d audit events, want 2 (proposed + verified)", len(events))
	}
}
