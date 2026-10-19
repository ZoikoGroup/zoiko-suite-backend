package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"zoiko.io/eventing/adapter"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

// Config tunes the engine.
type Config struct {
	// StaleAfter is how long an attempt may stay SUBMITTING before the sweeper turns it into PENDING_UNKNOWN.
	// It must exceed the longest profile timeout.
	StaleAfter time.Duration
	// BreakerWindow / BreakerFailures: the provider is "failing" when its last BreakerFailures calls inside
	// the window were all failures. Sending pauses (nothing is sent, no state changes) until one succeeds
	// or the window passes.
	BreakerWindow   time.Duration
	BreakerFailures int
	// DefaultRetryAfter is the not-before applied to a rate limit that carried no Retry-After.
	DefaultRetryAfter time.Duration
}

func (c Config) withDefaults() Config {
	if c.StaleAfter <= 0 {
		c.StaleAfter = 2 * time.Minute
	}
	if c.BreakerWindow <= 0 {
		c.BreakerWindow = time.Minute
	}
	if c.BreakerFailures <= 0 {
		c.BreakerFailures = 5
	}
	if c.DefaultRetryAfter <= 0 {
		c.DefaultRetryAfter = 30 * time.Second
	}
	return c
}

// Engine runs attempts against providers.
type Engine struct {
	Store      Store
	Connectors ConnectorResolver
	Creds      CredentialSource
	Transport  *adapter.Transport
	Cfg        Config
	Now        func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

// Result is what Send returns.
type Result struct {
	Attempt *domain.PaymentInitiationAttempt
	// Note says, in a rule name, what happened when the attempt did not move (for the caller's response).
	Note string
}

// Profile loads the profile an attempt is pinned to (or the latest certified one before the first send).
func (e *Engine) profileFor(ctx context.Context, d *Dispatch) (*ProfileRecord, error) {
	v := 0
	if d.ProfileVersion != nil {
		v = *d.ProfileVersion
	}
	p, err := e.Store.GetProfile(ctx, d.ProviderKey, v)
	if err != nil {
		return nil, err
	}
	// a pinned version may have been deprecated since; it stays usable for the attempt it is pinned to.
	// A version that was WITHDRAWN (found unsafe) is not.
	if p.Status == ProfileWithdrawn || (v == 0 && p.Status != ProfileCertified) {
		return nil, ErrProfileNotFound
	}
	if p.Spec.Kind != "BANK" {
		return nil, fmt.Errorf("%w: not a bank profile", ErrProfileMismatch)
	}
	return p, nil
}

func canonicalOf(d *Dispatch) adapter.Canonical {
	return adapter.Canonical{
		"rail": d.Rail, "amount": d.AmountText, "currency": d.Currency,
		"payer": d.PayerAccountRef, "payee": d.PayeeRef, "reference": d.PaymentReference,
		"end_to_end_id": d.IdempotencyKey, "idempotency_key": d.IdempotencyKey,
		"execution_date": d.ExecutionDate.UTC().Format("2006-01-02"),
	}
}

// CheckBinding is the early refusal at prepare time: does a certified profile of this provider carry this
// rail, currency and amount at all? (unsupported rail version, over the limit)
func (e *Engine) CheckBinding(ctx context.Context, providerKey, rail, currency, amountText string) error {
	p, err := e.Store.GetProfile(ctx, providerKey, 0)
	if err != nil {
		return err
	}
	if p.Status != ProfileCertified || p.Spec.Kind != "BANK" {
		return ErrProfileNotFound
	}
	err = p.Spec.Normalize().CheckAgainstProfile(adapter.Canonical{"rail": rail, "currency": currency, "amount": amountText})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProfileMismatch, err)
	}
	return nil
}

// Send performs the first send (attempt PREPARED) or a retry (attempt PENDING_UNKNOWN).
//
// Order is the safety property:
//  1. everything that can fail without the bank having heard anything (profile, request build, connector,
//     breaker, credential) happens first and changes no state;
//  2. then the attempt is made durably SUBMITTING by compare-and-set;
//  3. then the network call, whose outcome is recorded with a context that survives the caller hanging up.
//
// A retry never skips the question "what does the provider already have?".
func (e *Engine) Send(ctx context.Context, attemptID, actor string, caller Caller) (*Result, error) {
	cfg := e.Cfg.withDefaults()
	d, err := e.Store.LoadDispatch(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	if d.ProviderKey == "" {
		return nil, ErrNotConfigured
	}
	from := d.Status
	if from != domain.StatusPrepared && from != domain.StatusPendingUnknown {
		return nil, ErrInFlight
	}
	if d.NotBefore != nil && e.now().Before(*d.NotBefore) {
		return nil, ErrNotBefore
	}
	prof, err := e.profileFor(ctx, d)
	if err != nil {
		return nil, err
	}
	spec := prof.Spec.Normalize()

	body, err := spec.BuildRequest(canonicalOf(d))
	if err != nil {
		if from == domain.StatusPrepared && d.SendCount == 0 && isRefusal(err) {
			// nothing has ever been sent and the profile cannot carry this payment: a definite refusal
			a, rerr := e.Store.RejectUnsent(ctx, attemptID, "provider profile refused the payment: "+refusalName(err), actor, caller.CorrelationID)
			if rerr != nil {
				return nil, rerr
			}
			return &Result{Attempt: a, Note: "rejected_by_profile"}, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrProfileMismatch, err)
	}
	hash := adapterHash(body)
	if d.SentPayloadHash != "" && d.SentPayloadHash != hash {
		return nil, ErrPayloadChanged
	}

	conn, err := e.Connectors.Resolve(ctx, d.ConnectorID, caller)
	if err != nil {
		return nil, err
	}
	if err := conn.Usable(d.LegalEntityID, spec.RequiredScope); err != nil {
		return nil, err
	}

	if e.breakerOpen(ctx, d.ProviderKey, cfg) {
		return nil, ErrCircuitOpen
	}

	call := adapter.Call{Spec: spec, BaseURL: conn.EndpointURL, Body: body, IdempotencyKey: d.IdempotencyKey}
	switch spec.Auth.Type {
	case "bearer", "api_key":
		cred, err := e.Creds.Use(ctx, conn.CredentialRef, caller)
		if err != nil {
			return nil, err
		}
		call.Credential = cred
		defer zero(cred.Secret)
	case "hmac":
		call.Signer = e.Creds.Sign(ctx, conn.CredentialRef, caller)
	}

	if from == domain.StatusPendingUnknown {
		// Ask first. The attempt may well have reached the bank.
		res, done, err := e.reconcile(ctx, d, prof, call, actor, caller, cfg)
		if err != nil {
			return nil, err
		}
		if done {
			return res, nil
		}
		// proven not received, and the provider honours the idempotency key: fall through to a resend
	}

	if _, err := e.Store.BeginSend(ctx, attemptID, from, prof.Version, hash, actor); err != nil {
		return nil, err
	}

	// From here the attempt is SUBMITTING. Record the outcome whatever happens to the caller's request.
	rec, cancel := detached(ctx)
	defer cancel()
	callCtx, stop := context.WithTimeout(rec, time.Duration(spec.TimeoutSeconds+5)*time.Second)
	defer stop()
	r := e.Transport.Submit(callCtx, call)

	out := outcomeOf(r, from, prof, "SUBMIT", actor, cfg)
	a, err := e.Store.FinishSend(rec, attemptID, out, actor, caller.CorrelationID)
	if err != nil {
		return nil, err
	}
	return &Result{Attempt: a, Note: noteOf(out)}, nil
}

// reconcile asks the provider what it has for this attempt. done=true means the attempt was settled (or must
// stay pending) and the caller must not send; done=false means the provider proved it never received the
// request and the profile permits a resend with the same idempotency key.
func (e *Engine) reconcile(ctx context.Context, d *Dispatch, prof *ProfileRecord, call adapter.Call, actor string, caller Caller, cfg Config) (*Result, bool, error) {
	spec := call.Spec
	if spec.Query == nil {
		if spec.RetrySafe {
			// no lookup exists, but the provider deduplicates on the key: a resend cannot duplicate
			return nil, false, nil
		}
		return &Result{Note: "no_query_profile_cannot_prove"}, true, ErrUnresolved
	}
	rec, cancel := detached(ctx)
	defer cancel()
	qctx, stop := context.WithTimeout(rec, time.Duration(spec.TimeoutSeconds+5)*time.Second)
	defer stop()
	r := e.Transport.Fetch(qctx, call, d.ProviderRequestID)

	entry := entryOf(r, prof, "QUERY", actor)
	switch r.Class {
	case adapter.ClassAcknowledged, adapter.ClassPending:
		// the provider has it: record SUBMITTED and never send again
		out := SendOutcome{Kind: OutSubmitted, From: domain.StatusPendingUnknown, ProviderRef: r.ProviderRef, Provider: r.ProviderStat, Transport: entry}
		a, err := e.Store.FinishSend(rec, d.AttemptID, out, actor, caller.CorrelationID)
		if err != nil {
			return nil, false, err
		}
		return &Result{Attempt: a, Note: "found_at_provider"}, true, nil
	case adapter.ClassRejected:
		out := SendOutcome{Kind: OutRejected, From: domain.StatusPendingUnknown, Reason: r.Reason, Provider: r.ProviderStat, Transport: entry}
		a, err := e.Store.FinishSend(rec, d.AttemptID, out, actor, caller.CorrelationID)
		if err != nil {
			return nil, false, err
		}
		return &Result{Attempt: a, Note: "rejected_at_provider"}, true, nil
	case adapter.ClassNotFound:
		_, err := e.Store.FinishSend(rec, d.AttemptID, SendOutcome{Kind: OutNone, From: domain.StatusPendingUnknown, Transport: entry}, actor, caller.CorrelationID)
		if err != nil {
			return nil, false, err
		}
		if spec.RetrySafe {
			return nil, false, nil
		}
		// the provider says it has nothing, but it does not promise key-level deduplication: do not guess
		return &Result{Note: "not_found_but_resend_not_idempotent"}, true, ErrUnresolved
	default:
		// rate limited, unavailable, auth, unreadable, or no answer: still unknown
		_, err := e.Store.FinishSend(rec, d.AttemptID, SendOutcome{Kind: OutNone, From: domain.StatusPendingUnknown, Failure: string(r.Class), Transport: entry}, actor, caller.CorrelationID)
		if err != nil {
			return nil, false, err
		}
		return &Result{Note: "provider_did_not_answer_the_query"}, true, ErrUnresolved
	}
}

// Query asks the provider about an attempt without sending anything (the operator's "what happened?").
func (e *Engine) Query(ctx context.Context, attemptID, actor string, caller Caller) (*Result, error) {
	cfg := e.Cfg.withDefaults()
	d, err := e.Store.LoadDispatch(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	if d.ProviderKey == "" {
		return nil, ErrNotConfigured
	}
	if d.Status != domain.StatusPendingUnknown {
		return nil, ErrInFlight
	}
	prof, err := e.profileFor(ctx, d)
	if err != nil {
		return nil, err
	}
	spec := prof.Spec.Normalize()
	if spec.Query == nil {
		return nil, fmt.Errorf("%w: profile has no query", ErrProfileMismatch)
	}
	conn, err := e.Connectors.Resolve(ctx, d.ConnectorID, caller)
	if err != nil {
		return nil, err
	}
	if err := conn.Usable(d.LegalEntityID, spec.RequiredScope); err != nil {
		return nil, err
	}
	call := adapter.Call{Spec: spec, BaseURL: conn.EndpointURL, IdempotencyKey: d.IdempotencyKey}
	switch spec.Auth.Type {
	case "bearer", "api_key":
		cred, err := e.Creds.Use(ctx, conn.CredentialRef, caller)
		if err != nil {
			return nil, err
		}
		call.Credential = cred
		defer zero(cred.Secret)
	case "hmac":
		call.Signer = e.Creds.Sign(ctx, conn.CredentialRef, caller)
	}
	res, done, err := e.reconcile(ctx, d, prof, call, actor, caller, cfg)
	if err != nil {
		return res, err
	}
	if !done {
		// proven not received; a query does not resend: say so
		return &Result{Note: "not_received_by_provider"}, nil
	}
	return res, nil
}

func (e *Engine) breakerOpen(ctx context.Context, providerKey string, cfg Config) bool {
	calls, err := e.Store.RecentCalls(ctx, providerKey, e.now().Add(-cfg.BreakerWindow), cfg.BreakerFailures)
	if err != nil || len(calls) < cfg.BreakerFailures {
		return false
	}
	for _, healthy := range calls {
		if healthy {
			return false
		}
	}
	return true
}

// Sweep turns attempts stuck in SUBMITTING (the process died mid-call) into PENDING_UNKNOWN. It never
// resends: whether the request left is exactly what is unknown.
func (e *Engine) Sweep(ctx context.Context) (int, error) {
	cfg := e.Cfg.withDefaults()
	return e.Store.SweepStale(ctx, e.now().Add(-cfg.StaleAfter), "provider-sweeper")
}

// ── classification ──────────────────────────────────────────────────────────

func outcomeOf(r adapter.Result, from domain.AttemptStatus, prof *ProfileRecord, op, actor string, cfg Config) SendOutcome {
	entry := entryOf(r, prof, op, actor)
	o := SendOutcome{From: domain.StatusSubmitting, Transport: entry, ProviderRef: r.ProviderRef, Provider: r.ProviderStat}
	switch r.Disposition {
	case adapter.NotSent:
		// provably never left: back where it was, no harm done
		o.Kind, o.ReleaseTo, o.Failure = OutRelease, from, "not_sent:"+r.ErrCode
		return o
	case adapter.Unknown:
		o.Kind, o.Failure = OutUnknown, "no_answer_after_send"
		return o
	}
	switch r.Class {
	case adapter.ClassAcknowledged, adapter.ClassPending:
		o.Kind = OutSubmitted
	case adapter.ClassRejected:
		o.Kind, o.Reason = OutRejected, r.Reason
	case adapter.ClassRateLimited:
		// the provider says it did not process this: back where it was, with a not-before
		wait := r.RetryAfter
		if wait <= 0 {
			wait = cfg.DefaultRetryAfter
		}
		nb := time.Now().UTC().Add(wait)
		o.Kind, o.ReleaseTo, o.NotBefore, o.Failure = OutRelease, from, &nb, "rate_limited"
	case adapter.ClassAuth:
		// credentials were refused before the payment was considered
		o.Kind, o.ReleaseTo, o.Failure = OutRelease, from, "provider_auth_failed"
	default:
		// 5xx, unreadable 2xx, unmapped status: the provider may have acted on it
		o.Kind, o.Failure = OutUnknown, "answer_not_conclusive:"+string(r.Class)+":"+r.ErrCode
	}
	return o
}

func entryOf(r adapter.Result, prof *ProfileRecord, op, actor string) TransportEntry {
	return TransportEntry{
		ProviderKey: prof.ProviderKey, ProfileVersion: prof.Version, Operation: op,
		Disposition: string(r.Disposition), Class: string(r.Class), HTTPStatus: r.HTTPStatus,
		RequestHash: r.RequestHash, ResponseHash: r.ResponseHash, ProviderRef: r.ProviderRef, ErrorCode: r.ErrCode,
		DurationMS: int(r.Duration / time.Millisecond), RetryAfterS: int(r.RetryAfter / time.Second), Actor: actor,
	}
}

func noteOf(o SendOutcome) string {
	switch o.Kind {
	case OutSubmitted:
		return "acknowledged_by_provider"
	case OutRejected:
		return "rejected_by_provider"
	case OutUnknown:
		return "outcome_unknown"
	case OutRelease:
		return o.Failure
	}
	return ""
}

func isRefusal(err error) bool {
	return errors.Is(err, adapter.ErrRailUnsupported) || errors.Is(err, adapter.ErrCurrency) ||
		errors.Is(err, adapter.ErrLimitExceeded) || errors.Is(err, adapter.ErrAmountMismatch)
}

func refusalName(err error) string {
	switch {
	case errors.Is(err, adapter.ErrRailUnsupported):
		return "unsupported rail or message version"
	case errors.Is(err, adapter.ErrCurrency):
		return "unsupported currency"
	case errors.Is(err, adapter.ErrLimitExceeded):
		return "amount over the provider limit"
	case errors.Is(err, adapter.ErrAmountMismatch):
		return "translated amount differs from the authorized amount"
	}
	return strings.TrimSpace(err.Error())
}

// detached keeps the values of ctx but not its cancellation, so the outcome of a call that already left is
// recorded even if the HTTP client that asked for it hung up.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func adapterHash(b []byte) string { return adapter.HashBytes(b) }
