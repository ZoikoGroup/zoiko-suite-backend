package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/events"
	"zoiko.io/governance-decision-log-svc/internal/sealer"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

// The "audit" query dimension (decision D2-2.F11) is the correlation id: one
// audited business trace, within the caller's tenant only.
func TestList_AuditDimensionIsTheCorrelationID(t *testing.T) {
	ctx := context.Background()
	s := store.New(appRolePool(t, openTestPool(t)), zap.NewNop())
	for id, corr := range map[string]string{"a-1": "trace-A", "a-2": "trace-A", "b-1": "trace-B"} {
		d := sampleDecision(id)
		d.CorrelationID = corr
		if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d}); err != nil {
			t.Fatal(err)
		}
	}
	other := sampleDecision("x-1")
	other.TenantID, other.CorrelationID = "tenant-2", "trace-A"
	if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: other}); err != nil {
		t.Fatal(err)
	}
	res, err := s.List(ctx, store.ListParams{TenantID: "tenant-1", CorrelationID: "trace-A"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range res.Records {
		got[d.DecisionID] = true
		if d.RetentionClass != "EV-REGULATED" || d.RecordClass != "COMPLIANCE_EVIDENCE" {
			t.Fatalf("%s: retention %q record %q", d.DecisionID, d.RetentionClass, d.RecordClass)
		}
	}
	if len(got) != 2 || !got["a-1"] || !got["a-2"] || res.Total != 2 {
		t.Fatalf("correlation trace-A in tenant-1: got %v (total %d), want a-1, a-2 only", got, res.Total)
	}
}

// C3 / R9: one decision queues its wire event AND the canonical-name alias in
// the decision's own transaction; a replay likewise.
func TestOutbox_WireAndCanonicalNamesQueuedTogether(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	d := sampleDecision("dec-c3")
	ev, err := events.DecisionRecorded(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d, IdempotencyKey: "k-c3", BodyHash: []byte{1}, Event: ev}); err != nil {
		t.Fatal(err)
	}
	scoped(t, pool, "tenant-1", func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT event_type, convert_from(payload, 'UTF8') FROM outbox WHERE aggregate_id = 'dec-c3' ORDER BY outbox_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var types []string
		var ids []map[string]any
		for rows.Next() {
			var typ, payload string
			if err := rows.Scan(&typ, &payload); err != nil {
				t.Fatal(err)
			}
			var env map[string]any
			_ = json.Unmarshal([]byte(payload), &env)
			if env["event_type"] != typ {
				t.Fatalf("outbox event_type %q but envelope says %v", typ, env["event_type"])
			}
			types = append(types, typ)
			ids = append(ids, env)
		}
		if len(types) != 2 || types[0] != "governance.decision.recorded" || types[1] != "com.zoikosuite.governance.decision.recorded" {
			t.Fatalf("queued %v, want the wire name then the canonical name", types)
		}
		if ids[1]["alias_of_event_id"] != ids[0]["event_id"] || ids[0]["payload_hash"] != ids[1]["payload_hash"] {
			t.Fatalf("alias does not mirror the wire event: %v / %v", ids[0], ids[1])
		}
	})
}

// The scheduled sealer, as the runtime role (RLS forced, no BYPASSRLS): it
// finds only tenants with unsealed decisions (000017's read marker), seals
// each through its head, and a second run finds nothing. The request path's
// tenant scoping is unchanged: without the marker the runtime role sees no
// other tenant.
func TestSealer_SealsEveryTenantAsTheRuntimeRole(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	app := appRolePool(t, admin)
	s := store.New(app, zap.NewNop())

	seedChain(t, ctx, s, 3) // tenant-1
	for _, id := range []string{"t2-1", "t2-2"} {
		d := sampleDecision(id)
		d.TenantID = "tenant-2"
		if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d}); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := s.UnsealedTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].TenantID != "tenant-1" || pending[0].HeadSeq != 3 || pending[1].TenantID != "tenant-2" || pending[1].HeadSeq != 2 {
		t.Fatalf("unsealed tenants %+v", pending)
	}

	// The marker is transaction-local and read-only: an unmarked query by the
	// runtime role still sees no rows without a tenant.
	var n int
	if err := app.QueryRow(ctx, `SELECT COUNT(*) FROM governance_decisions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unscoped read without the marker: %d rows, %v", n, err)
	}

	res, err := sealer.New(s, "svc-governance-decision-log", zap.NewNop(), nil).RunOnce(ctx)
	if err != nil || res.Sealed != 2 || res.ChainBroken != 0 || res.Failed != 0 {
		t.Fatalf("first run: %+v %v", res, err)
	}
	for tenant, head := range map[string]int64{"tenant-1": 3, "tenant-2": 2} {
		pkgs, err := s.ListPackages(ctx, tenant)
		if err != nil || len(pkgs) != 1 || pkgs[0].ToSeq != head || pkgs[0].SealedBy != "svc-governance-decision-log" {
			t.Fatalf("%s packages %+v %v", tenant, pkgs, err)
		}
		if v, err := s.VerifyChain(ctx, tenant); err != nil || !v.Valid {
			t.Fatalf("%s verify after scheduled seal: %+v %v", tenant, v, err)
		}
	}

	res, err = sealer.New(s, "svc-governance-decision-log", zap.NewNop(), nil).RunOnce(ctx)
	if err != nil || res.Sealed != 0 {
		t.Fatalf("second run sealed again: %+v %v", res, err)
	}
	if pending, _ := s.UnsealedTenants(ctx); len(pending) != 0 {
		t.Fatalf("nothing should be pending, got %+v", pending)
	}
}

// A tampered chain is never certified by the schedule: the seal is refused
// (chain_broken) and no package is written.
func TestSealer_RefusesToSealATamperedChain(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())
	seedChain(t, ctx, s, 2)

	// Rewrite a stored decision behind the append-only trigger, as a database
	// superuser could: the attack the chain exists to detect.
	if _, err := admin.Exec(ctx, `
		ALTER TABLE governance_decisions DISABLE TRIGGER governance_decisions_immutable;
		UPDATE governance_decisions SET outcome = 'GRANTED-FORGED' WHERE tenant_id = 'tenant-1' AND chain_seq = 1;
		ALTER TABLE governance_decisions ENABLE TRIGGER governance_decisions_immutable;`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	res, err := sealer.New(s, "svc", zap.NewNop(), nil).RunOnce(ctx)
	if err != nil || res.ChainBroken != 1 || res.Sealed != 0 {
		t.Fatalf("tampered chain: %+v %v", res, err)
	}
	if pkgs, _ := s.ListPackages(ctx, "tenant-1"); len(pkgs) != 0 {
		t.Fatalf("a package was sealed over a tampered chain: %+v", pkgs)
	}
}
