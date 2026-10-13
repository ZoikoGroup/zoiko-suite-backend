package store_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"zoiko.io/authorization-svc/internal/store"
)

// AZ-P2 (13 Oct 2026, pass 2): the service role could not execute the
// SECURITY DEFINER maintenance functions on a volume whose migrations ran
// before the role existed — the guarded GRANTs were skipped and only the
// REVOKE FROM PUBLIC landed. 000032 re-asserts them. Run as the service role
// (AUTHZ_IT_DSN), this asserts the privilege and then uses it.
func TestDefinerFunctionsIT_ServiceRoleCanRunMaintenance(t *testing.T) {
	pool := outboxPool(t)
	ctx := context.Background()
	for _, fn := range []string{
		"create_access_decision_log_partition(date)",
		"detach_access_decision_log_partitions_before(date)",
		"authz_purge_published_outbox(interval)",
		"authz_purge_idempotency_keys(interval)",
	} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_function_privilege(current_user, $1, 'EXECUTE')`, fn).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("the service role cannot EXECUTE %s", fn)
		}
	}
	s := store.New(pool, zap.NewNop())
	month := time.Now().UTC().AddDate(0, 3, 0)
	if _, err := s.EnsureAccessDecisionPartition(ctx, month); err != nil {
		t.Fatalf("partition maintenance as the service role: %v", err)
	}
}
