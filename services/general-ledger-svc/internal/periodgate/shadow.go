package periodgate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/close"
)

// Metrics are the shadow-gate Prometheus collectors. All label values are
// drawn from the closed enums in this package, so cardinality is bounded:
// 3 legacy x 5 gate x 3 agree = 45 series max, plus 4 skip reasons.
type Metrics struct {
	Comparisons *prometheus.CounterVec
	Seconds     prometheus.Histogram
	Skipped     *prometheus.CounterVec
}

// Skip reasons.
const (
	SkipNoDate        = "no_date"
	SkipCircuitOpen   = "circuit_open"
	SkipOverload      = "overload"
	SkipLegacyAborted = "legacy_aborted"
)

func NewMetrics(reg prometheus.Registerer) *Metrics {
	cl := prometheus.Labels{"service": "general-ledger-svc"}
	m := &Metrics{
		Comparisons: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gl_period_gate_comparisons_total",
			Help:        "Shadow comparisons of the legacy close check against the REF-05 gate.",
			ConstLabels: cl,
		}, []string{"legacy", "gate", "agree"}),
		Seconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        "gl_period_gate_shadow_seconds",
			Help:        "Latency of the shadow REF-05 resolve call.",
			ConstLabels: cl,
			Buckets:     []float64{.005, .01, .025, .05, .1, .2, .3, .5, 1},
		}),
		Skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "gl_period_gate_shadow_skipped_total",
			Help:        "Shadow comparisons not performed, by reason.",
			ConstLabels: cl,
		}, []string{"reason"}),
	}
	if reg != nil {
		reg.MustRegister(m.Comparisons, m.Seconds, m.Skipped)
	}
	return m
}

// maxInflight bounds concurrent shadow goroutines so a slow gate can never
// accumulate unbounded work; excess is skipped (reason=overload).
const maxInflight = 64

// legacyWait is a safety bound on how long a shadow goroutine waits for the
// legacy result (legacy retries can legitimately take a few seconds).
const legacyWait = 30 * time.Second

// Shadow wraps a close.Client. Its CheckPeriodOpenAt returns EXACTLY the inner
// client's result. The gate call runs concurrently in a separate goroutine and
// the comparison is recorded after both finish; the caller never waits for it.
type Shadow struct {
	inner   close.Client
	gate    Resolver
	m       *Metrics
	log     *zap.Logger
	timeout time.Duration
	sem     chan struct{}
	wg      sync.WaitGroup
}

func NewShadow(inner close.Client, gate Resolver, m *Metrics, log *zap.Logger, timeout time.Duration) *Shadow {
	return &Shadow{inner: inner, gate: gate, m: m, log: log, timeout: timeout, sem: make(chan struct{}, maxInflight)}
}

// Settings configures Wrap.
type Settings struct {
	Mode    string // "off" | "shadow"
	URL     string
	Timeout time.Duration
}

// Wrap returns inner untouched in off mode (so no gate client, goroutine or
// metric exists), or a Shadow wrapper in shadow mode.
func Wrap(s Settings, inner close.Client, reg prometheus.Registerer, log *zap.Logger) close.Client {
	if s.Mode != "shadow" {
		return inner
	}
	return NewShadow(inner, NewClient(s.URL, s.Timeout), NewMetrics(reg), log, s.Timeout)
}

func (s *Shadow) CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error {
	return s.inner.CheckPeriodOpen(ctx, tenantID, legalEntityID, periodName)
}

type legacyResult struct {
	err error
	ok  bool
}

func (s *Shadow) CheckPeriodOpenAt(ctx context.Context, tenantID string, ref close.PeriodRef) error {
	ch := s.startShadow(ctx, tenantID, ref)
	if ch == nil {
		return s.inner.CheckPeriodOpenAt(ctx, tenantID, ref)
	}
	sent := false
	defer func() {
		if !sent { // inner panicked: release the shadow goroutine without a result
			ch <- legacyResult{}
		}
	}()
	err := s.inner.CheckPeriodOpenAt(ctx, tenantID, ref)
	ch <- legacyResult{err: err, ok: true}
	sent = true
	return err
}

// startShadow launches the gate call and returns the channel on which the
// caller must deliver the legacy result, or nil when no shadow is running.
func (s *Shadow) startShadow(ctx context.Context, tenantID string, ref close.PeriodRef) (ch chan legacyResult) {
	defer func() {
		if r := recover(); r != nil { // shadow bookkeeping must never panic the request
			ch = nil
		}
	}()
	if ref.PostingDate.IsZero() {
		s.m.Skipped.WithLabelValues(SkipNoDate).Inc()
		return nil
	}
	select {
	case s.sem <- struct{}{}:
	default:
		s.m.Skipped.WithLabelValues(SkipOverload).Inc()
		return nil
	}
	ch = make(chan legacyResult, 1)
	s.wg.Add(1)
	// Detach from the request's cancellation (the goroutine outlives the
	// handler) but keep its values; the strict timeout in run bounds it.
	go s.run(context.WithoutCancel(ctx), tenantID, ref, ch)
	return ch
}

func (s *Shadow) run(ctx context.Context, tenantID string, ref close.PeriodRef, ch chan legacyResult) {
	defer s.wg.Done()
	defer func() { <-s.sem }()
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("period gate shadow panicked (ignored)", zap.String("panic", fmt.Sprint(r)))
		}
	}()

	tctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	start := time.Now()
	res := s.gate.Resolve(tctx, tenantID, ref.LegalEntityID, ref.PostingDate.String())
	if res.Skipped {
		s.m.Skipped.WithLabelValues(SkipCircuitOpen).Inc()
		return
	}
	s.m.Seconds.Observe(time.Since(start).Seconds())

	lr := waitLegacy(ch)
	if !lr.ok {
		s.m.Skipped.WithLabelValues(SkipLegacyAborted).Inc()
		return
	}
	legacy := ClassifyLegacy(lr.err)
	agree := Compare(legacy, res.Outcome)
	s.m.Comparisons.WithLabelValues(string(legacy), string(res.Outcome), agree).Inc()
	if agree == AgreeNo {
		// No amounts, descriptions or other journal content.
		s.log.Warn("period gate shadow disagreement",
			zap.String("journal_id", ref.JournalID),
			zap.String("legal_entity_id", ref.LegalEntityID),
			zap.String("posting_date", ref.PostingDate.String()),
			zap.String("legacy", string(legacy)),
			zap.String("gate", string(res.Outcome)),
			zap.String("gate_state", res.State),
			zap.String("gate_code", res.Code))
	}
}

func waitLegacy(ch chan legacyResult) legacyResult {
	select {
	case lr := <-ch:
		return lr
	case <-time.After(legacyWait):
		return legacyResult{}
	}
}

// Drain waits for in-flight shadow comparisons (tests / graceful shutdown).
func (s *Shadow) Drain() { s.wg.Wait() }
