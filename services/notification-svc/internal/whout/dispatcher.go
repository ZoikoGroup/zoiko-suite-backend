package whout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/eventing/egress"
	"zoiko.io/eventing/signature"
)

// Header names on every webhook request.
const (
	HeaderEventID    = "Zoiko-Event-Id"
	HeaderEventType  = "Zoiko-Event-Type"
	HeaderDeliveryID = "Zoiko-Delivery-Id"
	HeaderAttempt    = "Zoiko-Delivery-Attempt"
)

// DispatcherConfig tunes delivery.
type DispatcherConfig struct {
	Workers        int           // concurrent deliveries across all destinations
	BatchSize      int           // deliveries claimed per poll
	PollInterval   time.Duration // idle wait between polls
	Lease          time.Duration // how long a claimed delivery is held before it can be re-claimed
	RequestTimeout time.Duration // the whole HTTP exchange
	Retry          RetryPolicy

	// Per-destination protection (INT-04 NFR: bulkhead, circuit breaker, no retry storm).
	PerDestinationConcurrency int
	BreakerThreshold          int           // consecutive failures that open the breaker
	BreakerCooldown           time.Duration // how long it stays open
	DisableAfterDeadLetters   int           // consecutive dead letters that disable the subscription

	MaxResponseBytes int64
	MaxRetryAfter    time.Duration
}

// DefaultDispatcherConfig is a sound starting point.
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		Workers: 8, BatchSize: 20, PollInterval: 2 * time.Second, Lease: 2 * time.Minute, RequestTimeout: 10 * time.Second,
		Retry: DefaultRetry, PerDestinationConcurrency: 2, BreakerThreshold: 5, BreakerCooldown: 5 * time.Minute,
		DisableAfterDeadLetters: 5, MaxResponseBytes: 64 << 10, MaxRetryAfter: time.Hour,
	}
}

// Observer receives dispatch telemetry. The zero value of Dispatcher uses a no-op.
type Observer interface {
	Attempt(outcome AttemptOutcome)
	DeadLetter(reason string)
	ControlFailure()
	BreakerDeferred()
}

type nopObserver struct{}

func (nopObserver) Attempt(AttemptOutcome) {}
func (nopObserver) DeadLetter(string)      {}
func (nopObserver) ControlFailure()        {}
func (nopObserver) BreakerDeferred()       {}

// Dispatcher delivers queued webhooks.
type Dispatcher struct {
	store          Store
	signing        Signing
	policy         egress.Policy
	client         *http.Client
	allowedRegions map[string]bool
	cfg            DispatcherConfig
	log            *zap.Logger
	obs            Observer
	now            func() time.Time
	jitter         func() float64

	mu         sync.Mutex
	inflight   map[string]int // subscription id -> deliveries in flight (the bulkhead)
	retryAfter func(string) time.Duration
}

// DispatcherDeps are the collaborators a Dispatcher needs.
type DispatcherDeps struct {
	Store          Store
	Signing        Signing
	Egress         egress.Policy
	Client         *http.Client // nil builds the policy-enforcing client
	AllowedRegions []string
	Log            *zap.Logger
	Observer       Observer
	Now            func() time.Time
	Jitter         func() float64
}

// NewDispatcher builds a Dispatcher.
func NewDispatcher(d DispatcherDeps, cfg DispatcherConfig) (*Dispatcher, error) {
	if d.Store == nil || d.Signing == nil {
		return nil, errors.New("whout: a store and a signing service are required")
	}
	def := DefaultDispatcherConfig()
	if cfg.Workers <= 0 {
		cfg.Workers = def.Workers
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = def.BatchSize
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = def.PollInterval
	}
	if cfg.Lease <= 0 {
		cfg.Lease = def.Lease
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = def.RequestTimeout
	}
	if cfg.Retry.Base <= 0 || cfg.Retry.Cap <= 0 {
		cfg.Retry = def.Retry
	}
	if cfg.PerDestinationConcurrency <= 0 {
		cfg.PerDestinationConcurrency = def.PerDestinationConcurrency
	}
	if cfg.BreakerThreshold <= 0 {
		cfg.BreakerThreshold = def.BreakerThreshold
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = def.BreakerCooldown
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = def.MaxResponseBytes
	}
	if cfg.MaxRetryAfter <= 0 {
		cfg.MaxRetryAfter = def.MaxRetryAfter
	}
	if cfg.Lease <= cfg.RequestTimeout+30*time.Second {
		cfg.Lease = cfg.RequestTimeout + 30*time.Second // the lease must outlive the request, or a slow call is re-claimed mid-flight
	}
	if d.Log == nil {
		d.Log = zap.NewNop()
	}
	if d.Observer == nil {
		d.Observer = nopObserver{}
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Jitter == nil {
		d.Jitter = rand.Float64
	}
	if d.Client == nil {
		d.Client = d.Egress.NewHTTPClient(cfg.RequestTimeout)
	}
	regions := map[string]bool{}
	for _, r := range d.AllowedRegions {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			regions[r] = true
		}
	}
	return &Dispatcher{store: d.Store, signing: d.Signing, policy: d.Egress, client: d.Client, allowedRegions: regions,
		cfg: cfg, log: d.Log, obs: d.Observer, now: d.Now, jitter: d.Jitter, inflight: map[string]int{}}, nil
}

// Run delivers until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("webhook dispatcher started", zap.Int("workers", d.cfg.Workers), zap.Duration("poll", d.cfg.PollInterval))
	for {
		n, err := d.RunOnce(ctx)
		if ctx.Err() != nil {
			d.log.Info("webhook dispatcher stopped")
			return
		}
		if err != nil {
			d.log.Warn("webhook dispatcher: claim failed", zap.Error(err))
		}
		if n == 0 || err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d.cfg.PollInterval):
			}
		}
	}
}

// RunOnce claims one batch and delivers it, returning how many deliveries it handled.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	claims, err := d.store.ClaimDue(ctx, d.now(), d.cfg.BatchSize, d.cfg.Lease)
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, d.cfg.Workers)
	var wg sync.WaitGroup
	for _, c := range claims {
		sem <- struct{}{}
		wg.Add(1)
		go func(c Claim) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					d.log.Error("webhook dispatcher: delivery panicked", zap.Any("panic", r))
				}
			}()
			d.process(ctx, c)
		}(c)
	}
	wg.Wait()
	return len(claims), nil
}

func (d *Dispatcher) acquire(sub string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[sub] >= d.cfg.PerDestinationConcurrency {
		return false
	}
	d.inflight[sub]++
	return true
}

func (d *Dispatcher) release(sub string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[sub] <= 1 {
		delete(d.inflight, sub)
		return
	}
	d.inflight[sub]--
}

func (d *Dispatcher) process(ctx context.Context, c Claim) {
	del, sub, err := d.store.LoadForSend(ctx, c.TenantID, c.DeliveryID)
	if err != nil {
		d.log.Warn("webhook dispatcher: cannot load delivery", zap.String("delivery_id", c.DeliveryID), zap.Error(err))
		return
	}
	if del.Status == DeliverySucceeded || del.Status == DeliveryDeadLettered {
		return // a stale claim
	}
	now := d.now()

	// Only an ACTIVE subscription is sent to. A suspended, disabled or changed (back to
	// DRAFT) subscription keeps its queue; nothing leaves until it is live again, which
	// is what makes "endpoint changed after approval" safe.
	if sub.Status != StatusActive {
		_ = d.store.Defer(ctx, c.TenantID, c.DeliveryID, now.Add(time.Minute))
		return
	}
	if sub.BreakerOpenUntil != nil && sub.BreakerOpenUntil.After(now) {
		d.obs.BreakerDeferred()
		until := sub.BreakerOpenUntil.Add(time.Duration(d.jitter() * float64(10*time.Second)))
		_ = d.store.Defer(ctx, c.TenantID, c.DeliveryID, until)
		return
	}
	if !d.acquire(sub.SubscriptionID) {
		_ = d.store.Defer(ctx, c.TenantID, c.DeliveryID, now.Add(time.Second))
		return
	}
	defer d.release(sub.SubscriptionID)

	host := egress.HostOf(sub.DestinationURL)
	used := del.AttemptCount - del.BudgetBase
	attemptNo, err := d.store.BeginAttempt(ctx, c.TenantID, del, host, now, d.cfg.Lease)
	if err != nil {
		d.log.Warn("webhook dispatcher: cannot begin attempt", zap.String("delivery_id", c.DeliveryID), zap.Error(err))
		return
	}
	fin := FinishInput{TenantID: c.TenantID, DeliveryID: c.DeliveryID, AttemptNo: attemptNo}
	finish := func(f FinishInput) {
		f.TenantID, f.DeliveryID, f.AttemptNo = c.TenantID, c.DeliveryID, attemptNo
		f.Now = d.now()
		f.BreakerThreshold, f.BreakerCooldown, f.DisableAfterDeadLetters = d.cfg.BreakerThreshold, d.cfg.BreakerCooldown, d.cfg.DisableAfterDeadLetters
		d.obs.Attempt(f.Outcome)
		if f.NewStatus == DeliveryDeadLettered {
			d.obs.DeadLetter(f.DeadLetterReason)
		}
		if f.ControlFailure {
			d.obs.ControlFailure()
		}
		if err := d.store.FinishAttempt(ctx, f); err != nil {
			d.log.Error("webhook dispatcher: cannot record the attempt outcome", zap.String("delivery_id", c.DeliveryID), zap.Error(err))
		}
	}
	_ = fin

	// A budget already spent (a crash left attempts whose outcome was lost) ends the
	// delivery without another send.
	if used >= sub.MaxAttempts {
		finish(FinishInput{Outcome: OutcomeBlocked, ErrorClass: "budget_exhausted", NewStatus: DeliveryDeadLettered,
			DeadLetterReason: ReasonRetriesExhausted, NextAttemptAt: now, Failure: true})
		return
	}

	// INT-NP-017: the body is checked against the CURRENT approved scope immediately
	// before it leaves. A violation is a control failure, never retried, never sent.
	if err := VerifyBody(del.Body, sub.AllowedFields); err != nil {
		d.log.Error("webhook dispatcher: body blocked before send", zap.String("delivery_id", c.DeliveryID), zap.Error(err))
		finish(FinishInput{Outcome: OutcomeBlocked, ErrorClass: "scope_violation", NewStatus: DeliveryDeadLettered,
			DeadLetterReason: ReasonMinimizationBlock, NextAttemptAt: now, ControlFailure: true})
		return
	}

	// INT-NP-018: residency and SSRF are re-checked on every attempt. The dialer
	// re-validates the address it actually connects to, so a DNS answer that changes
	// between this check and the connection is refused as well.
	if err := d.checkEgress(ctx, sub); err != nil {
		finish(FinishInput{Outcome: OutcomeEgressBlocked, ErrorClass: "egress_denied", NewStatus: DeliveryDeadLettered,
			DeadLetterReason: ReasonEgressDenied, NextAttemptAt: now, Failure: true})
		return
	}

	ts := d.now()
	signers := []signature.Signer{SignerFor(d.signing, c.TenantID, sub.SigningKeyPath,
		del.DeliveryID+":"+strconv.Itoa(attemptNo)+":0", del.CorrelationID)}
	keyPaths := []string{sub.SigningKeyPath}
	if sub.PreviousSigningKeyPath != "" && sub.PreviousKeyValidUntil != nil && sub.PreviousKeyValidUntil.After(ts) {
		signers = append(signers, SignerFor(d.signing, c.TenantID, sub.PreviousSigningKeyPath,
			del.DeliveryID+":"+strconv.Itoa(attemptNo)+":1", del.CorrelationID))
		keyPaths = append(keyPaths, sub.PreviousSigningKeyPath)
	}
	signCtx, cancelSign := context.WithTimeout(ctx, d.cfg.RequestTimeout)
	header, err := signature.Sign(signCtx, ts, del.EventID, del.Body, signers...)
	cancelSign()
	unix := ts.Unix()
	if err != nil {
		// Never sent unsigned. The vault being down is not the destination's fault, so it
		// does not count against the destination's health; the delivery is retried.
		d.log.Warn("webhook dispatcher: signing failed", zap.String("delivery_id", c.DeliveryID), zap.Error(err))
		d.finishRetry(finish, FinishInput{Outcome: OutcomeSigningFailed, ErrorClass: "signing_failed"}, attemptNo-del.BudgetBase, sub.MaxAttempts, 0)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.DestinationURL, bytes.NewReader(del.Body))
	if err != nil {
		finish(FinishInput{Outcome: OutcomeBlocked, ErrorClass: "bad_request", NewStatus: DeliveryDeadLettered,
			DeadLetterReason: ReasonEgressDenied, NextAttemptAt: now, Failure: true})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Zoiko-Webhooks/1")
	req.Header.Set(signature.HeaderName, header)
	req.Header.Set(HeaderEventID, del.EventID)
	req.Header.Set(HeaderEventType, del.EventType)
	req.Header.Set(HeaderDeliveryID, del.DeliveryID)
	req.Header.Set(HeaderAttempt, strconv.Itoa(attemptNo))

	resp, err := d.client.Do(req)
	base := FinishInput{SignatureTimestamp: &unix, SignatureKeyPaths: keyPaths}
	if err != nil {
		res := classifyError(err)
		base.Outcome, base.ErrorClass = res.Outcome, res.ErrorClass
		d.finishRetry(finish, base, attemptNo-del.BudgetBase, sub.MaxAttempts, 0)
		return
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, _ := io.Copy(h, io.LimitReader(resp.Body, d.cfg.MaxResponseBytes))
	code := resp.StatusCode
	nn := int(n)
	base.ResponseCode, base.ResponseBodyBytes = &code, &nn
	base.ResponseBodyHash = hex.EncodeToString(h.Sum(nil))

	res := ClassifyStatus(code, parseRetryAfter(resp.Header.Get("Retry-After"), d.cfg.MaxRetryAfter))
	base.Outcome, base.ErrorClass = res.Outcome, res.ErrorClass
	switch {
	case res.Outcome == OutcomeSucceeded:
		base.NewStatus, base.NextAttemptAt, base.Success = DeliverySucceeded, now, true
		finish(base)
	case res.Permanent:
		base.NewStatus, base.DeadLetterReason, base.NextAttemptAt, base.Failure = DeliveryDeadLettered, ReasonDestinationGone, now, true
		finish(base)
	default:
		d.finishRetry(finish, base, attemptNo-del.BudgetBase, sub.MaxAttempts, res.RetryAfter)
	}
}

// finishRetry records a failed attempt and either schedules the next one or, when the
// budget is spent, dead-letters the delivery. A Retry-After longer than the computed
// backoff is honoured (up to a cap) so a rate-limited destination is not hammered.
func (d *Dispatcher) finishRetry(finish func(FinishInput), f FinishInput, attemptsUsed, max int, retryAfter time.Duration) {
	now := d.now()
	f.Failure = f.Outcome != OutcomeSigningFailed
	if attemptsUsed >= max {
		f.NewStatus, f.DeadLetterReason, f.NextAttemptAt = DeliveryDeadLettered, ReasonRetriesExhausted, now
		finish(f)
		return
	}
	delay := d.cfg.Retry.Delay(attemptsUsed, d.jitter)
	if retryAfter > delay {
		delay = retryAfter
	}
	f.NewStatus, f.NextAttemptAt = DeliveryRetrying, now.Add(delay)
	finish(f)
}

func (d *Dispatcher) checkEgress(ctx context.Context, sub *Subscription) error {
	if !d.allowedRegions[sub.DestinationRegion] {
		return egress.ErrDenied
	}
	_, err := d.policy.CheckDestination(ctx, sub.DestinationURL)
	return err
}

func parseRetryAfter(v string, cap time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > cap {
			d = cap
		}
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > cap {
			d = cap
		}
		if d > 0 {
			return d
		}
	}
	return 0
}

// classifyError maps a transport error to an outcome.
func classifyError(err error) Result {
	var (
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		certInvalid x509.CertificateInvalidError
		verifyErr   *tls.CertificateVerificationError
		recordErr   tls.RecordHeaderError
		netErr      net.Error
	)
	switch {
	case errors.Is(err, egress.ErrDenied):
		return Result{Outcome: OutcomeEgressBlocked, ErrorClass: "egress_denied"}
	case errors.As(err, &unknownAuth), errors.As(err, &hostErr), errors.As(err, &certInvalid),
		errors.As(err, &verifyErr), errors.As(err, &recordErr):
		return Result{Outcome: OutcomeTLSFailure, Retryable: true, ErrorClass: "tls_failure"}
	case errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err), (errors.As(err, &netErr) && netErr.Timeout()):
		return Result{Outcome: OutcomeTimeout, Retryable: true, ErrorClass: "timeout"}
	case strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:"):
		return Result{Outcome: OutcomeTLSFailure, Retryable: true, ErrorClass: "tls_failure"}
	}
	return Result{Outcome: OutcomeNetworkError, Retryable: true, ErrorClass: "network_error"}
}
