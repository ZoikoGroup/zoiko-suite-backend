package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── Audit 2026-09-23, gap 4: drift detection and runtime attestation ─────────
//
// "A fleet running stale config is undetectable." Attestations were stored and
// never compared with anything. Each test reports an observation and asserts
// the finding CFG records and announces — or, for a correct runtime, that it
// records nothing.

func currentSnap(t *testing.T, pool *pgxpool.Pool, env string) (id string, epoch int64, digest string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT snapshot_id, epoch, digest FROM config_snapshots
		WHERE environment = $1 ORDER BY epoch DESC LIMIT 1`, env).Scan(&id, &epoch, &digest); err != nil {
		t.Fatalf("current snapshot: %v", err)
	}
	return
}

func attest(t *testing.T, s *store.PgStore, runtime, key, env string, snapID *string, epoch int64, digest string) *domain.AttestationResult {
	t.Helper()
	res, err := s.RecordAttestation(context.Background(), domain.RecordAttestationParams{
		RuntimeID: runtime, AttestKey: key, Environment: env, ObservedSnapshotID: snapID,
		ObservedEpoch: epoch, ObservedDigest: digest, CallerTenantID: testCallerTenant,
	})
	if err != nil {
		t.Fatalf("attest %s/%s: %v", runtime, key, err)
	}
	return res
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestGap4_AttestationClassifiesDrift(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	if err := upsertStaging(s, "payroll.batch_size", `100`); err != nil {
		t.Fatalf("v1: %v", err)
	}
	oldID, oldEpoch, oldDigest := currentSnap(t, pool, "staging")
	if err := upsertStaging(s, "payroll.batch_size", `200`); err != nil {
		t.Fatalf("v2: %v", err)
	}
	curID, curEpoch, curDigest := currentSnap(t, pool, "staging")

	// Correct runtime: no finding, nothing recorded.
	if res := attest(t, s, "rt-ok", "k1", "staging", &curID, curEpoch, curDigest); res.Drift != nil {
		t.Fatalf("a runtime serving the current snapshot must not drift, got %+v", res.Drift)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM drift_events WHERE runtime_id = 'rt-ok'`); n != 0 {
		t.Fatalf("no drift must be recorded for a converged runtime, got %d", n)
	}

	cases := []struct {
		name, runtime      string
		snapID             *string
		epoch              int64
		digest             string
		wantClass, wantSev string
	}{
		{"older genuine snapshot (NP-23)", "rt-stale", &oldID, oldEpoch, oldDigest, domain.DriftStale, domain.SeverityMedium},
		{"digest does not match the epoch (NP-24)", "rt-tampered", nil, curEpoch, "0000deadbeef", domain.DriftUnauthorized, domain.SeverityCritical},
		{"snapshot id does not match the epoch", "rt-mismatch", &oldID, curEpoch, curDigest, domain.DriftUnauthorized, domain.SeverityCritical},
		{"epoch never issued", "rt-future", nil, curEpoch + 5, curDigest, domain.DriftUnauthorized, domain.SeverityCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := attest(t, s, tc.runtime, "k1", "staging", tc.snapID, tc.epoch, tc.digest)
			if res.Drift == nil || res.Drift.DriftClass != tc.wantClass || res.Drift.Severity != tc.wantSev {
				t.Fatalf("expected %s/%s, got %+v", tc.wantClass, tc.wantSev, res.Drift)
			}
			if res.Drift.DesiredDigest == nil || *res.Drift.DesiredDigest != curDigest || res.Drift.DriftID == "" {
				t.Errorf("the finding must carry the exact desired digest and be recorded: %+v", res.Drift)
			}
			if n := count(t, pool, `SELECT COUNT(*) FROM drift_events WHERE runtime_id = $1 AND drift_class = $2`, tc.runtime, tc.wantClass); n != 1 {
				t.Errorf("expected one recorded finding, got %d", n)
			}
			if n := count(t, pool, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'config.drift.detected' AND aggregate_key = $1`, res.Drift.DriftID); n != 1 {
				t.Errorf("the finding must be announced as config.drift.detected, got %d events", n)
			}
		})
	}

	// An environment where CFG has issued nothing.
	if res := attest(t, s, "rt-nowhere", "k1", "qa", nil, 1, "abc"); res.Drift == nil || res.Drift.DriftClass != domain.DriftUnknown {
		t.Errorf("attesting where nothing was issued must be UNKNOWN, got %+v", res.Drift)
	}
}

// The sweep finds what attestation alone cannot: a runtime that simply never
// moved to the new snapshot, and one that stopped attesting (NP-26).
func TestGap4_SweepDetectsStaleAndSilentRuntimes(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	defer store.SetDriftConvergenceWindowForTest(0)()
	seedConfig(t, pool, "payroll.batch_size")
	if err := upsertStaging(s, "payroll.batch_size", `100`); err != nil {
		t.Fatalf("v1: %v", err)
	}
	id1, e1, d1 := currentSnap(t, pool, "staging")
	attest(t, s, "rt-laggard", "k1", "staging", &id1, e1, d1)
	attest(t, s, "rt-silent", "k1", "staging", &id1, e1, d1)
	if _, err := pool.Exec(ctx, `UPDATE runtime_attestations SET freshness_deadline = NOW() - INTERVAL '1 minute' WHERE runtime_id = 'rt-silent'`); err != nil {
		t.Fatalf("age attestation: %v", err)
	}

	if err := upsertStaging(s, "payroll.batch_size", `200`); err != nil {
		t.Fatalf("v2: %v", err)
	}
	id2, e2, d2 := currentSnap(t, pool, "staging")
	attest(t, s, "rt-current", "k1", "staging", &id2, e2, d2)

	res, err := s.SweepExpired(ctx, "staging")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.DriftFindings != 2 {
		t.Fatalf("expected 2 findings (laggard STALE, silent UNKNOWN), got %d", res.DriftFindings)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM drift_events WHERE runtime_id = 'rt-laggard' AND drift_class = 'STALE' AND desired_snapshot_id = $1`, id2); n != 1 {
		t.Errorf("laggard: expected one STALE finding against the current snapshot, got %d", n)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM drift_events WHERE runtime_id = 'rt-silent' AND drift_class = 'UNKNOWN'`); n != 1 {
		t.Errorf("silent runtime: expected one UNKNOWN finding, got %d", n)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM drift_events WHERE runtime_id = 'rt-current'`); n != 0 {
		t.Errorf("a converged runtime must not be flagged, got %d", n)
	}

	// A runtime that stays stale is one finding, not one per sweep.
	if res, err := s.SweepExpired(ctx, "staging"); err != nil || res.DriftFindings != 0 {
		t.Errorf("second sweep must not duplicate open findings: %+v %v", res, err)
	}
}

// Within the convergence window a runtime on the previous snapshot is still
// converging, not drifting.
func TestGap4_SweepRespectsConvergenceWindow(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	if err := upsertStaging(s, "payroll.batch_size", `100`); err != nil {
		t.Fatalf("v1: %v", err)
	}
	id1, e1, d1 := currentSnap(t, pool, "staging")
	attest(t, s, "rt-laggard", "k1", "staging", &id1, e1, d1)
	if err := upsertStaging(s, "payroll.batch_size", `200`); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if res, err := s.SweepExpired(ctx, "staging"); err != nil || res.DriftFindings != 0 {
		t.Fatalf("inside the window nothing is stale yet: %+v %v", res, err)
	}
}
