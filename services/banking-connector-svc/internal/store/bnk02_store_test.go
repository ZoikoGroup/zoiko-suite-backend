package store_test

import (
	"context"
	"testing"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// TestBNK02_ConnectionLifecycle_CAS_Transitions is the real proof of the
// consent state machine's CAS enforcement: each command only succeeds
// from the states the doc names, and illegal transitions are rejected
// rather than silently applied.
func TestBNK02_ConnectionLifecycle_CAS_Transitions(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk02-a")

	conn, created, err := s.InitiateConnection(ctx, domain.InitiateConnectionParams{
		TenantID: "tenant-bnk02-a", LegalEntityID: "le-a", BankAccountID: "acct-1", ProviderRef: "plaid",
		BankName: "Test Bank", Region: "US", RequestedScope: []string{"balances", "transactions"},
		CreatedByPrincipalID: "auditor-1", CorrelationID: "corr-initiate-1",
	})
	if err != nil || !created {
		t.Fatalf("initiate: created=%v err=%v", created, err)
	}
	if conn.Status != domain.ConnStatusRequested {
		t.Fatalf("expected REQUESTED, got %s", conn.Status)
	}

	// Activating before authorization is complete must be rejected.
	if _, err := s.ActivateConnection(ctx, domain.ActivateConnectionParams{ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-a", ActorPrincipalID: "ops-1"}); err != domain.ErrInvalidConnectionTransition {
		t.Fatalf("expected ErrInvalidConnectionTransition activating a REQUESTED connection, got %v", err)
	}

	authorized, err := s.CompleteConnectionAuthorization(ctx, domain.CompleteConnectionAuthorizationParams{
		ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-a", TokenLeaseRef: "lease-abc123",
		GrantedScope: []string{"balances"}, ActorPrincipalID: "ops-1",
	})
	if err != nil || authorized.Status != domain.ConnStatusAuthorizing {
		t.Fatalf("complete authorization: status=%v err=%v", authorized, err)
	}

	active, err := s.ActivateConnection(ctx, domain.ActivateConnectionParams{ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-a", ActorPrincipalID: "ops-1"})
	if err != nil || active.Status != domain.ConnStatusActive {
		t.Fatalf("activate: status=%v err=%v", active, err)
	}

	// Reactivating an already-ACTIVE connection must be rejected — not a
	// silent no-op.
	if _, err := s.ActivateConnection(ctx, domain.ActivateConnectionParams{ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-a", ActorPrincipalID: "ops-1"}); err != domain.ErrInvalidConnectionTransition {
		t.Fatalf("expected ErrInvalidConnectionTransition re-activating an ACTIVE connection, got %v", err)
	}

	suspended, err := s.SuspendConnection(ctx, domain.SuspendConnectionParams{ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-a", Reason: "suspicious activity", ActorPrincipalID: "ops-1"})
	if err != nil || suspended.Status != domain.ConnStatusSuspended {
		t.Fatalf("suspend: status=%v err=%v", suspended, err)
	}

	reconnected, err := s.ReconnectProvider(ctx, domain.ReconnectProviderParams{ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-a", ActorPrincipalID: "ops-1"})
	if err != nil || reconnected.Status != domain.ConnStatusActive {
		t.Fatalf("reconnect: status=%v err=%v", reconnected, err)
	}

	// Full event trail must exist for every transition above.
	history, err := s.ListConnectionEvents(ctx, "tenant-bnk02-a", conn.ConnectionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	wantEvents := []string{
		domain.EventConnectionRequested, domain.EventConnectionAuthorized, domain.EventConnectionActivated,
		domain.EventConnectionSuspended, domain.EventConnectionReconnected,
	}
	if len(history) != len(wantEvents) {
		t.Fatalf("expected %d events, got %d: %+v", len(wantEvents), len(history), history)
	}
	for i, want := range wantEvents {
		if history[i].EventType != want {
			t.Fatalf("event[%d]: expected %s, got %s", i, want, history[i].EventType)
		}
	}
}

// TestBNK02_RevokedConnection_IsTerminal is the real, negative-controlled
// proof of the spec's own named negative path "revoked bank consent
// still used": REVOKED cannot be reversed by any command, and a raw
// UPDATE attempt is refused at the DB layer, not just the application
// layer.
func TestBNK02_RevokedConnection_IsTerminal(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk02-b")
	conn, _, err := s.InitiateConnection(ctx, domain.InitiateConnectionParams{
		TenantID: "tenant-bnk02-b", LegalEntityID: "le-b", BankAccountID: "acct-2", BankName: "Test Bank",
		CreatedByPrincipalID: "auditor-1", CorrelationID: "corr-revoke-1",
	})
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	revoked, err := s.RevokeConnection(ctx, domain.RevokeConnectionParams{ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-b", Reason: "compromised", ActorPrincipalID: "sec-1"})
	if err != nil || revoked.Status != domain.ConnStatusRevoked {
		t.Fatalf("revoke: status=%v err=%v", revoked, err)
	}

	// Application-layer: no command can move a REVOKED connection anywhere.
	if _, err := s.ReconnectProvider(ctx, domain.ReconnectProviderParams{ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-b", ActorPrincipalID: "ops-1"}); err != domain.ErrInvalidConnectionTransition {
		t.Fatalf("expected ErrInvalidConnectionTransition reconnecting a REVOKED connection, got %v", err)
	}
	if _, err := s.RevokeConnection(ctx, domain.RevokeConnectionParams{ConnectionID: conn.ConnectionID, TenantID: "tenant-bnk02-b", Reason: "again", ActorPrincipalID: "ops-1"}); err != domain.ErrInvalidConnectionTransition {
		t.Fatalf("expected ErrInvalidConnectionTransition re-revoking, got %v", err)
	}

	// DB-layer negative control: a raw UPDATE attempting to reactivate a
	// REVOKED row must be refused by migration 003's own trigger. Run over
	// the admin (superuser) pool deliberately — a BEFORE trigger fires
	// regardless of role, unlike RLS, which a superuser bypasses; using
	// appPool here would test RLS visibility, not the trigger.
	if _, err := admin.Exec(ctx, `UPDATE bank_connections SET status = 'ACTIVE' WHERE connection_id = $1`, conn.ConnectionID); err == nil {
		t.Fatal("expected the trigger to refuse reactivating a REVOKED connection via a raw UPDATE")
	}

	// Disable the trigger, confirm the same UPDATE now succeeds (proving
	// the trigger — not something else — was refusing it), then re-enable
	// and confirm refusal returns.
	if _, err := admin.Exec(ctx, `ALTER TABLE bank_connections DISABLE TRIGGER trg_reject_revoked_connection_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_connections SET status = 'ACTIVE' WHERE connection_id = $1`, conn.ConnectionID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := admin.Exec(ctx, `ALTER TABLE bank_connections ENABLE TRIGGER trg_reject_revoked_connection_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	// Restore REVOKED for the assertion below. The row is currently ACTIVE
	// (from the disabled-trigger write above), so this ACTIVE->REVOKED
	// update needs no bypass — the trigger only blocks writes where
	// OLD.status is already REVOKED.
	if _, err := admin.Exec(ctx, `UPDATE bank_connections SET status = 'REVOKED' WHERE connection_id = $1`, conn.ConnectionID); err != nil {
		t.Fatalf("restore revoked state: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_connections SET status = 'ACTIVE' WHERE connection_id = $1`, conn.ConnectionID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestBNK02_ConnectionEvents_AreAppendOnly is the negative-controlled
// proof that bank_connection_events cannot be edited or deleted.
func TestBNK02_ConnectionEvents_AreAppendOnly(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk02-c")
	conn, _, err := s.InitiateConnection(ctx, domain.InitiateConnectionParams{
		TenantID: "tenant-bnk02-c", LegalEntityID: "le-c", BankAccountID: "acct-3", BankName: "Test Bank",
		CreatedByPrincipalID: "auditor-1", CorrelationID: "corr-events-1",
	})
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	history, err := s.ListConnectionEvents(ctx, "tenant-bnk02-c", conn.ConnectionID)
	if err != nil || len(history) != 1 {
		t.Fatalf("expected exactly 1 event after initiate, got %d err=%v", len(history), err)
	}
	eventID := history[0].EventID

	// Run over the admin (superuser) pool deliberately — a BEFORE trigger
	// fires regardless of role, unlike RLS, which a superuser bypasses;
	// using appPool here would test RLS visibility, not the trigger.
	if _, err := admin.Exec(ctx, `UPDATE bank_connection_events SET detail = 'tampered' WHERE event_id = $1`, eventID); err == nil {
		t.Fatal("expected the trigger to refuse mutating a connection event")
	}
	if _, err := admin.Exec(ctx, `DELETE FROM bank_connection_events WHERE event_id = $1`, eventID); err == nil {
		t.Fatal("expected connection events to never be deletable")
	}

	if _, err := admin.Exec(ctx, `ALTER TABLE bank_connection_events DISABLE TRIGGER trg_reject_connection_event_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_connection_events SET detail = 'tampered-while-disabled' WHERE event_id = $1`, eventID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := admin.Exec(ctx, `ALTER TABLE bank_connection_events ENABLE TRIGGER trg_reject_connection_event_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_connection_events SET detail = 'tampered-again' WHERE event_id = $1`, eventID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestBNK02_InitiateConnection_IdempotentOnCorrelationID proves a retried
// InitiateConnection call with the same correlation_id returns the
// original row (created=false) rather than a second connection.
func TestBNK02_InitiateConnection_IdempotentOnCorrelationID(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk02-d")
	params := domain.InitiateConnectionParams{
		TenantID: "tenant-bnk02-d", LegalEntityID: "le-d", BankAccountID: "acct-4", BankName: "Test Bank",
		CreatedByPrincipalID: "auditor-1", CorrelationID: "corr-idempotent-1",
	}

	first, created, err := s.InitiateConnection(ctx, params)
	if err != nil || !created {
		t.Fatalf("first initiate: created=%v err=%v", created, err)
	}

	retryParams := params
	retryParams.BankName = "A Different Bank Name"
	second, created, err := s.InitiateConnection(ctx, retryParams)
	if err != nil {
		t.Fatalf("retried initiate: %v", err)
	}
	if created {
		t.Fatal("expected created=false on the retried call — this is a duplicate-connection bug if true")
	}
	if second.ConnectionID != first.ConnectionID {
		t.Fatalf("expected the retried call to return the ORIGINAL connection id %s, got %s", first.ConnectionID, second.ConnectionID)
	}
	if second.BankName != first.BankName {
		t.Fatalf("expected the original bank_name to be preserved, got %q", second.BankName)
	}
}
