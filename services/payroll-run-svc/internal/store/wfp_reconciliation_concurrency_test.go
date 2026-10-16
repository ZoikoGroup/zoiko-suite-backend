package store_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

// Two replicas (or two operators) reconciling the same result at the same moment must not
// fail each other and must not raise the same difference twice.
func TestWave8_ConcurrentReconciliationsOfOneResultNeitherFailNorDuplicate(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1) // nothing downstream: several exceptions

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	outs := make([]*store.ReconOutcome, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], errs[i] = s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "run %d", i)
	}

	open, err := s.ListReconExceptions(ctx, res.Ref.RunRefID, "", domain.ReconStateOpen)
	require.NoError(t, err)
	seen := map[string]int{}
	for _, e := range open {
		seen[e.Code+"|"+e.SubjectKey]++
	}
	for k, c := range seen {
		assert.Equal(t, 1, c, "%s is one open exception, not %d", k, c)
	}
	created := 0
	for _, o := range outs {
		created += len(o.Created)
	}
	assert.Equal(t, len(open), created, "every exception was raised exactly once, by exactly one of the runs")
	runs, err := s.ListReconRuns(ctx, res.Ref.RunRefID)
	require.NoError(t, err)
	assert.Len(t, runs, n, "every run is still recorded as evidence")
}

func TestWave8_ConcurrentCertificationWithTenantCertifiesExactlyOnce(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	res, _ := reconciled(t, s, ctx, tenant)
	run, err := s.ExecuteReconciliation(ctx, res.Ref.RunRefID, "p-3", "corr", false, reconEv())
	require.NoError(t, err)

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.CertifyReconRun(ctx, run.Run.ReconRunID, "p-4", reconEv())
		}(i)
	}
	close(start)
	wg.Wait()
	ok, already := 0, 0
	for _, err := range errs {
		switch err {
		case nil:
			ok++
		case domain.ErrReconAlreadyCertified:
			already++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	assert.Equal(t, 1, ok, "exactly one certification wins")
	assert.Equal(t, n-1, already, "the others see it already certified")
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollReconRunCertified), "and it is announced once")
}
