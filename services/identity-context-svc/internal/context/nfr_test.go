package context_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
)

// DoD gate 9: "NFR/load/failure tests meet approved class."
//
// The resolver's doc comment has always said "Resolve() targets P99 < 50ms
// end-to-end". That was a comment, not a gate — nothing measured it, so a
// change that doubled the latency would have shipped with the claim intact.
//
// WHAT THIS MEASURES, AND WHAT IT DOES NOT. These run against the in-memory
// fixtures, so they measure the RESOLVER'S OWN work: token verification,
// envelope assembly, signing, the six-dimension orchestration and the
// allocation profile. They do not measure Postgres, Redis, Kafka or the
// upstream registries.
//
// That is the useful half to guard in CI. Infrastructure latency varies with
// the environment and would make this test flap; the resolver's own cost is
// deterministic, and it is the part a code change can regress. The end-to-end
// budget is verified against a running stack by the load profile in
// docs/runbook.md, which this test complements rather than replaces.
//
// The in-process budget is therefore deliberately much tighter than 50ms: if
// the resolver's own work approaches the whole end-to-end budget, there is no
// room left for the network.

// resolverSelfBudgetP99 bounds the resolver's own per-call cost.
//
// 5ms leaves 45ms of the 50ms end-to-end target for everything this test
// stubs out — two registry round trips, a Redis read, a Postgres write. If
// this ever fails, the regression is in this service's code, not its
// dependencies, which is exactly the signal worth having.
const resolverSelfBudgetP99 = 5 * time.Millisecond

// loadIterations is kept modest so the test adds seconds, not minutes, to CI.
// It is enough to make a P99 meaningful without making the suite unpleasant.
const loadIterations = 2000

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func summarise(t *testing.T, name string, samples []time.Duration) {
	t.Helper()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	var total time.Duration
	for _, s := range samples {
		total += s
	}
	t.Logf("%s: n=%d mean=%s p50=%s p95=%s p99=%s max=%s",
		name, len(samples), total/time.Duration(len(samples)),
		percentile(samples, 0.50), percentile(samples, 0.95),
		percentile(samples, 0.99), samples[len(samples)-1])
}

// TestResolveLatencyMeetsItsBudget is the sequential profile.
func TestResolveLatencyMeetsItsBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("load profile skipped in -short")
	}

	resolver := defaultFixture().build()
	samples := make([]time.Duration, 0, loadIterations)

	// One untimed call so lazily-initialised state (signer warm-up, map
	// growth) is not attributed to the first measured sample.
	_, err := resolver.Resolve(context.Background(), baseRequest)
	require.NoError(t, err)

	for i := 0; i < loadIterations; i++ {
		start := time.Now()
		result, err := resolver.Resolve(context.Background(), baseRequest)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.NotEmpty(t, result.EnvelopeJWT)
		samples = append(samples, elapsed)
	}

	summarise(t, "resolve (sequential)", samples)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	p99 := percentile(samples, 0.99)
	require.LessOrEqual(t, p99, resolverSelfBudgetP99,
		"resolver's own P99 is %s, budget %s — this leaves no room for the network within the 50ms end-to-end target",
		p99, resolverSelfBudgetP99)
}

// TestResolveLatencyUnderConcurrency is the profile that actually resembles
// production, and the one that catches a different class of regression:
// a mutex introduced on the hot path is invisible sequentially.
func TestResolveLatencyUnderConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("load profile skipped in -short")
	}

	const workers = 16
	const perWorker = loadIterations / workers

	resolver := defaultFixture().build()

	var mu sync.Mutex
	samples := make([]time.Duration, 0, workers*perWorker)

	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, perWorker)
			for i := 0; i < perWorker; i++ {
				start := time.Now()
				_, err := resolver.Resolve(context.Background(), baseRequest)
				elapsed := time.Since(start)
				if err != nil {
					errs <- err
					return
				}
				local = append(local, elapsed)
			}
			mu.Lock()
			samples = append(samples, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	summarise(t, fmt.Sprintf("resolve (%d workers)", workers), samples)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	// A looser budget under contention: 16 goroutines on a CI box with fewer
	// cores will queue, and that queueing is the machine's, not the code's.
	// The number still bounds a real regression — a lock held across the whole
	// resolution would blow past it by orders of magnitude.
	concurrentBudget := 4 * resolverSelfBudgetP99
	p99 := percentile(samples, 0.99)
	require.LessOrEqual(t, p99, concurrentBudget,
		"resolver P99 under %d-way concurrency is %s, budget %s", workers, p99, concurrentBudget)
}

// TestResolveGoroutinesAllDrain.
//
// Every resolution used to start fire-and-forget publish goroutines. The
// biggest one is gone — identity.context.resolved is now written inline, in
// the same transaction as the session row — but some remain, and a goroutine
// that never returns would surface as a slow memory climb under load rather
// than as a test failure anywhere.
//
// NOTE ON WHAT "success path" MEANS HERE. The default fixture's risk cache is
// EMPTY, so every success also raises the risk-signal-unavailable alarm in a
// goroutine. That is correct behaviour, not a leak: a service whose risk
// pipeline is not wired should say so on every resolution. The test therefore
// asserts that the goroutines DRAIN, which is the property that matters, and
// covers the genuinely goroutine-free path separately below.
func TestResolveGoroutinesAllDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("goroutine profile skipped in -short")
	}

	resolver := defaultFixture().build()

	for i := 0; i < 200; i++ {
		_, err := resolver.Resolve(context.Background(), baseRequest)
		require.NoError(t, err)
	}

	// Drain bounds the wait; a goroutine that never returns would keep it from
	// completing. This is exactly what main.go relies on at SIGTERM.
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, resolver.Drain(drainCtx),
		"in-flight publish goroutines did not complete within the drain budget")
}

// TestResolveWithAHealthyRiskCacheSpawnsNothing covers the path a correctly
// wired estate actually takes.
//
// With a real risk signal present there is no alarm to raise, and the
// context.resolved event is written inline — so a successful resolution starts
// no goroutine at all. Drain returning instantly is the assertion.
func TestResolveWithAHealthyRiskCacheSpawnsNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("goroutine profile skipped in -short")
	}

	f := defaultFixture()
	f.riskSignals = &mockRiskSignalCache{signal: &domain.RiskSignalCache{
		RiskSignalID: "rs-1",
		PrincipalID:  activePrincipal.PrincipalID,
		SignalValue:  10,
		SignalSource: "INTELLIGENCE_PLANE",
		ValidTo:      time.Now().Add(time.Hour),
	}}
	resolver := f.build()

	for i := 0; i < 200; i++ {
		_, err := resolver.Resolve(context.Background(), baseRequest)
		require.NoError(t, err)
	}

	// A tight budget on purpose: with nothing in flight this returns
	// immediately, and anything that makes it wait is a regression.
	drainCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	require.NoError(t, resolver.Drain(drainCtx),
		"a fully-resolved session with a live risk signal should start no background work")
}

// TestFailedResolutionsAlsoMeetTheBudget.
//
// A refusal must not be slower than a success. If it is, the difference is a
// timing oracle: an attacker probing tenants can tell "no such principal" from
// "wrong entity" by the clock, whichever way the response bodies are worded.
func TestFailedResolutionsAlsoMeetTheBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("load profile skipped in -short")
	}

	f := defaultFixture()
	f.principals = &mockPrincipalStore{principal: nil} // never resolves
	resolver := f.build()

	samples := make([]time.Duration, 0, 500)
	for i := 0; i < 500; i++ {
		start := time.Now()
		_, err := resolver.Resolve(context.Background(), baseRequest)
		samples = append(samples, time.Since(start))
		require.Error(t, err)
	}

	summarise(t, "resolve (refusal)", samples)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	require.LessOrEqual(t, percentile(samples, 0.99), resolverSelfBudgetP99,
		"a refusal must not cost more than a success")
}

// ── Benchmarks ───────────────────────────────────────────────────────────────
//
// Run with: go test ./internal/context -bench=. -benchmem -run=^$
//
// The allocation count is the number worth watching over time. A change that
// doubles allocations per resolution will not fail the latency budget on a
// fast machine and will absolutely be felt under production GC pressure.

func BenchmarkResolve(b *testing.B) {
	resolver := defaultFixture().build()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := resolver.Resolve(ctx, baseRequest); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResolveParallel(b *testing.B) {
	resolver := defaultFixture().build()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			if _, err := resolver.Resolve(ctx, baseRequest); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkExplain(b *testing.B) {
	sc := issuedSession()
	asOf := sc.IssuedAt.Add(time.Minute)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = identityctx.Explain(sc, asOf)
	}
}
