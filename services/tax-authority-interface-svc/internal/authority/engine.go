package authority

import (
	"context"
	"errors"
	"fmt"
	"time"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/adapter/ext"
)

// Config tunes the engine.
type Config struct {
	// StaleAfter is how long a filing may stay SUBMITTING before it is swept to PENDING_UNKNOWN. It must exceed
	// the longest profile timeout.
	StaleAfter time.Duration
	// BreakerWindow / BreakerFailures: the authority is "failing" when its last BreakerFailures calls inside the
	// window were all failures. Sending pauses (nothing is sent, nothing changes) unless the filing deadline is
	// at risk, in which case it is tried anyway and escalated.
	BreakerWindow   time.Duration
	BreakerFailures int
	// DefaultRetryAfter is the not-before applied to a rate limit that carried no Retry-After.
	DefaultRetryAfter time.Duration
	// PollInterval is how often a filing the authority holds is asked about; UrgentPollInterval applies once the
	// deadline is at risk.
	PollInterval       time.Duration
	UrgentPollInterval time.Duration
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
	if c.PollInterval <= 0 {
		c.PollInterval = time.Minute
	}
	if c.UrgentPollInterval <= 0 {
		c.UrgentPollInterval = 15 * time.Second
	}
	return c
}

// Engine runs filings against authorities.
type Engine struct {
	Store      Store
	Connectors ext.ConnectorResolver
	Creds      ext.CredentialSource
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

// Caller is who a downstream call is made for.
type Caller = ext.Caller

// Result is what an operation returns.
type Result struct {
	Filing *Filing
	// Note says, in a rule name, what happened when the filing did not move.
	Note string
}

func (e *Engine) profileFor(ctx context.Context, f *Filing) (*ProfileRecord, error) {
	v := 0
	if f.ProfileVersion != nil {
		v = *f.ProfileVersion
	}
	p, err := e.Store.GetProfile(ctx, f.ProviderKey, v)
	if err != nil {
		return nil, err
	}
	// a pinned version may have been deprecated since and stays usable for the filing pinned to it; a WITHDRAWN
	// one (found unsafe) is not
	if p.Status == ProfileWithdrawn || (v == 0 && p.Status != ProfileCertified) {
		return nil, ErrProfileNotFound
	}
	if p.Spec.Kind != "TAX" {
		return nil, fmt.Errorf("%w: not a tax profile", ErrBlocked)
	}
	return p, nil
}

func canonicalOf(f *Filing) adapter.Canonical {
	c := adapter.Canonical{
		"rail": f.Rail, "reference": f.SubmissionID, "idempotency_key": f.SubmissionID, "period": f.TaxPeriod,
		"filing_type": f.FilingType, "jurisdiction": f.Jurisdiction, "subject_ref": f.SubjectRef,
	}
	if f.AmountText != "" {
		c["amount"] = f.AmountText
	}
	return c
}

// CheckBinding is the early refusal at filing time: does a certified profile of this authority carry this
// schema version at all?
func (e *Engine) CheckBinding(ctx context.Context, providerKey, rail string) error {
	p, err := e.Store.GetProfile(ctx, providerKey, 0)
	if err != nil {
		return err
	}
	if p.Status != ProfileCertified || p.Spec.Kind != "TAX" {
		return ErrProfileNotFound
	}
	if err := p.Spec.Normalize().CheckAgainstProfile(adapter.Canonical{"rail": rail}); err != nil {
		return fmt.Errorf("%w: %v", ErrBlocked, err)
	}
	return nil
}

func (e *Engine) atRisk(f *Filing) bool {
	return f.DeadlineAt != nil && !e.now().Before(f.DeadlineAt.Add(-f.EscalateBefore))
}

func (e *Engine) nextPoll(f *Filing, cfg Config) *time.Time {
	d := cfg.PollInterval
	if e.atRisk(f) {
		d = cfg.UrgentPollInterval
	}
	t := e.now().Add(d)
	return &t
}

// escalateIfAtRisk records, once, that an unresolved filing is near its deadline. It never blocks the call.
func (e *Engine) escalateIfAtRisk(ctx context.Context, f *Filing, why, actor, corr string) {
	if f == nil || f.EscalatedAt != nil || !e.atRisk(f) {
		return
	}
	switch f.Status {
	case StatusAccepted, StatusRejected, StatusBlocked, StatusSuperseded:
		return
	}
	_ = e.Store.Escalate(ctx, f.SubmissionID, why, actor, corr)
}

// Validate checks the approved document against the authority schema and records the result: VALIDATED, or
// BLOCKED with the failing paths. It never changes the document.
func (e *Engine) Validate(ctx context.Context, id, actor, corr string) (*Result, error) {
	f, err := e.Store.LoadFiling(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.ProviderKey == "" {
		return nil, ErrNotConfigured
	}
	switch f.Status {
	case StatusValidated:
		return &Result{Filing: f, Note: "already_validated"}, nil
	case StatusPrepared:
	case StatusBlocked:
		return &Result{Filing: f, Note: "blocked"}, ErrBlocked
	default:
		return nil, ErrInFlight
	}
	prof, err := e.profileFor(ctx, f)
	if err != nil {
		return nil, err
	}
	if _, err := prof.Spec.Normalize().BuildRequestDoc(canonicalOf(f), f.Document); err != nil {
		if isRefusal(err) {
			b, berr := e.Store.Block(ctx, id, "schema validation failed: "+refusalName(err), actor, corr)
			if berr != nil {
				return nil, berr
			}
			return &Result{Filing: b, Note: err.Error()}, ErrBlocked
		}
		return nil, fmt.Errorf("%w: %v", ErrBlocked, err)
	}
	v, err := e.Store.MarkValidated(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	return &Result{Filing: v, Note: "validated"}, nil
}

// Send performs the first send (PREPARED/VALIDATED) or a retry (PENDING_UNKNOWN).
//
// Order is the safety property: everything that can fail without the authority having heard anything (profile,
// schema validation, certificate, connector, breaker, credential) happens first and changes nothing, then the
// filing is durably SUBMITTING by compare-and-set, then the call, whose outcome is recorded with a context that
// survives the caller hanging up. A retry never skips the question "what does the authority already have?".
func (e *Engine) Send(ctx context.Context, id, actor string, caller Caller) (*Result, error) {
	cfg := e.Cfg.withDefaults()
	f, err := e.Store.LoadFiling(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.ProviderKey == "" {
		return nil, ErrNotConfigured
	}
	from := f.Status
	switch from {
	case StatusPrepared, StatusValidated, StatusPendingUnknown:
	case StatusBlocked:
		return &Result{Filing: f, Note: "blocked"}, ErrBlocked
	default:
		return nil, ErrInFlight
	}
	if f.NotBefore != nil && e.now().Before(*f.NotBefore) {
		return nil, ErrNotBefore
	}
	prof, err := e.profileFor(ctx, f)
	if err != nil {
		return nil, err
	}
	spec := prof.Spec.Normalize()

	// an expired taxpayer certificate stops the send before it starts; nothing changes
	if f.CertificateNotAt != nil && !e.now().Before(*f.CertificateNotAt) {
		return nil, ErrCertificateExpired
	}

	body, err := spec.BuildRequestDoc(canonicalOf(f), f.Document)
	if err != nil {
		if isRefusal(err) && f.SendCount == 0 && (from == StatusPrepared || from == StatusValidated) {
			b, berr := e.Store.Block(ctx, id, "schema validation failed: "+refusalName(err), actor, caller.CorrelationID)
			if berr != nil {
				return nil, berr
			}
			return &Result{Filing: b, Note: err.Error()}, ErrBlocked
		}
		return nil, fmt.Errorf("%w: %v", ErrBlocked, err)
	}
	hash := adapter.HashBytes(body)
	if f.SentPayloadHash != "" && f.SentPayloadHash != hash {
		return nil, ErrPayloadChanged
	}
	if from == StatusPrepared {
		v, err := e.Store.MarkValidated(ctx, id, actor)
		if err != nil {
			return nil, err
		}
		f, from = v, StatusValidated
	}

	conn, err := e.Connectors.Resolve(ctx, f.ConnectorID, caller)
	if err != nil {
		return nil, err
	}
	if err := conn.Usable(f.LegalEntityID, spec.RequiredScope); err != nil {
		return nil, err
	}
	if e.breakerOpen(ctx, f.ProviderKey, cfg) && !e.atRisk(f) {
		return nil, ErrCircuitOpen
	}

	call, release, err := e.buildCall(ctx, spec, conn, f, body, caller)
	if err != nil {
		return nil, err
	}
	defer release()

	if from == StatusPendingUnknown {
		res, done, err := e.reconcile(ctx, f, prof, call, actor, caller, cfg)
		if err != nil || done {
			return res, err
		}
		// proven not received, and the authority honours the idempotency key: fall through to a resend
	}

	if _, err := e.Store.BeginSend(ctx, id, from, prof.Version, hash, actor); err != nil {
		return nil, err
	}
	rec, cancel := detached(ctx)
	defer cancel()
	callCtx, stop := context.WithTimeout(rec, time.Duration(spec.TimeoutSeconds+5)*time.Second)
	defer stop()
	r := e.Transport.Submit(callCtx, call)

	out := e.outcomeOf(r, from, prof, "SUBMIT", actor, cfg, f)
	nf, err := e.Store.Finish(rec, id, out, actor, caller.CorrelationID)
	if err != nil {
		return nil, err
	}
	if out.Kind == OutUnknown || out.Kind == OutRelease {
		e.escalateIfAtRisk(rec, nf, "authority did not take the filing: "+out.Failure, actor, caller.CorrelationID)
	}
	return &Result{Filing: nf, Note: noteOf(out)}, nil
}

func (e *Engine) buildCall(ctx context.Context, spec adapter.Spec, conn ext.Connector, f *Filing, body []byte, caller Caller) (adapter.Call, func(), error) {
	call := adapter.Call{Spec: spec, BaseURL: conn.EndpointURL, Body: body, IdempotencyKey: f.SubmissionID, EchoExpect: f.SubmissionID}
	release := func() {}
	switch spec.Auth.Type {
	case "bearer", "api_key":
		cred, err := e.Creds.Use(ctx, conn.CredentialRef, caller)
		if err != nil {
			return call, release, err
		}
		call.Credential = cred
		release = func() { zero(cred.Secret) }
	case "hmac":
		call.Signer = e.Creds.Sign(ctx, conn.CredentialRef, caller)
	}
	return call, release, nil
}

// reconcile asks the authority what it has for this filing. done=true means the filing was settled (or must stay
// pending) and the caller must not send; done=false means the authority proved it never received the filing and
// the profile permits a resend with the same idempotency key.
func (e *Engine) reconcile(ctx context.Context, f *Filing, prof *ProfileRecord, call adapter.Call, actor string, caller Caller, cfg Config) (*Result, bool, error) {
	spec := call.Spec
	if spec.Query == nil {
		if spec.RetrySafe {
			return nil, false, nil
		}
		return &Result{Filing: f, Note: "no_query_profile_cannot_prove"}, true, ErrUnresolved
	}
	rec, cancel := detached(ctx)
	defer cancel()
	qctx, stop := context.WithTimeout(rec, time.Duration(spec.TimeoutSeconds+5)*time.Second)
	defer stop()
	r := e.Transport.Fetch(qctx, call, f.AuthorityRef)
	entry := entryOf(r, prof, "QUERY", actor)
	from := StatusPendingUnknown
	settle := func(o Outcome, note string) (*Result, bool, error) {
		o.From, o.Transport = from, entry
		nf, err := e.Store.Finish(rec, f.SubmissionID, o, actor, caller.CorrelationID)
		if err != nil {
			return nil, false, err
		}
		return &Result{Filing: nf, Note: note}, true, nil
	}
	switch r.Class {
	case adapter.ClassAccepted:
		return settle(Outcome{Kind: OutAccepted, AuthorityRef: r.ProviderRef, Authority: r.ProviderStat, ReceiptHash: r.ResponseHash}, "accepted_at_authority")
	case adapter.ClassAcknowledged, adapter.ClassPending:
		return settle(Outcome{Kind: OutSubmitted, AuthorityRef: r.ProviderRef, Authority: r.ProviderStat, ReceiptHash: r.ResponseHash, NextPoll: e.nextPoll(f, cfg)}, "found_at_authority")
	case adapter.ClassRejected:
		return settle(Outcome{Kind: OutRejected, AuthorityRef: r.ProviderRef, Authority: r.ProviderStat, ReceiptHash: r.ResponseHash, Reason: r.Reason}, "rejected_at_authority")
	case adapter.ClassNotFound:
		if _, _, err := settle(Outcome{Kind: OutNone}, ""); err != nil {
			return nil, false, err
		}
		if spec.RetrySafe {
			return nil, false, nil
		}
		return &Result{Filing: f, Note: "not_found_but_resend_not_idempotent"}, true, ErrUnresolved
	default:
		// rate limited, unavailable, auth, unreadable, a receipt that does not echo our reference, or no answer
		if _, _, err := settle(Outcome{Kind: OutNone, Failure: string(r.Class) + ":" + r.ErrCode, NextPoll: e.nextPoll(f, cfg)}, ""); err != nil {
			return nil, false, err
		}
		e.escalateIfAtRisk(rec, f, "authority could not be asked what it holds: "+string(r.Class), actor, caller.CorrelationID)
		return &Result{Filing: f, Note: "authority_did_not_answer_the_query"}, true, ErrUnresolved
	}
}

// Poll asks the authority about a filing it holds or may hold (PollOriginalSubmission). It never sends the filing.
func (e *Engine) Poll(ctx context.Context, id, actor string, caller Caller) (*Result, error) {
	cfg := e.Cfg.withDefaults()
	f, err := e.Store.LoadFiling(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.ProviderKey == "" {
		return nil, ErrNotConfigured
	}
	if f.Status != StatusSubmitted && f.Status != StatusPendingUnknown {
		return nil, ErrInFlight
	}
	prof, err := e.profileFor(ctx, f)
	if err != nil {
		return nil, err
	}
	spec := prof.Spec.Normalize()
	if spec.Query == nil {
		return nil, ErrNoQuery
	}
	conn, err := e.Connectors.Resolve(ctx, f.ConnectorID, caller)
	if err != nil {
		return nil, err
	}
	if err := conn.Usable(f.LegalEntityID, spec.RequiredScope); err != nil {
		return nil, err
	}
	call, release, err := e.buildCall(ctx, spec, conn, f, nil, caller)
	if err != nil {
		return nil, err
	}
	defer release()

	rec, cancel := detached(ctx)
	defer cancel()
	qctx, stop := context.WithTimeout(rec, time.Duration(spec.TimeoutSeconds+5)*time.Second)
	defer stop()
	r := e.Transport.Fetch(qctx, call, f.AuthorityRef)
	entry := entryOf(r, prof, "QUERY", actor)
	finish := func(o Outcome, note string) (*Result, error) {
		o.From, o.Transport = f.Status, entry
		nf, err := e.Store.Finish(rec, id, o, actor, caller.CorrelationID)
		if err != nil {
			return nil, err
		}
		return &Result{Filing: nf, Note: note}, nil
	}
	next := e.nextPoll(f, cfg)
	switch r.Class {
	case adapter.ClassAccepted:
		return finish(Outcome{Kind: OutAccepted, AuthorityRef: r.ProviderRef, Authority: r.ProviderStat, ReceiptHash: r.ResponseHash}, "accepted_at_authority")
	case adapter.ClassRejected:
		return finish(Outcome{Kind: OutRejected, AuthorityRef: r.ProviderRef, Authority: r.ProviderStat, ReceiptHash: r.ResponseHash, Reason: r.Reason}, "rejected_at_authority")
	case adapter.ClassAcknowledged, adapter.ClassPending:
		if f.Status == StatusPendingUnknown {
			return finish(Outcome{Kind: OutSubmitted, AuthorityRef: r.ProviderRef, Authority: r.ProviderStat, ReceiptHash: r.ResponseHash, NextPoll: next}, "found_at_authority")
		}
		return finish(Outcome{Kind: OutNone, Authority: r.ProviderStat, NextPoll: next}, "still_pending_at_authority")
	case adapter.ClassNotFound:
		why := "not_received_by_authority"
		if f.Status == StatusSubmitted {
			why = "authority_has_no_record_of_a_submitted_filing"
		}
		res, err := finish(Outcome{Kind: OutNone, Failure: why, NextPoll: next}, why)
		if err == nil {
			e.escalateIfAtRisk(rec, res.Filing, why, actor, caller.CorrelationID)
		}
		return res, err
	default:
		res, err := finish(Outcome{Kind: OutNone, Failure: string(r.Class) + ":" + r.ErrCode, NextPoll: next}, "authority_did_not_answer")
		if err == nil {
			e.escalateIfAtRisk(rec, res.Filing, "authority did not answer a status poll", actor, caller.CorrelationID)
		}
		return res, err
	}
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

// Sweep turns filings stuck in SUBMITTING (the process died mid-call) into PENDING_UNKNOWN. It never resends.
func (e *Engine) Sweep(ctx context.Context) (int, error) {
	return e.Store.SweepStale(ctx, e.now().Add(-e.Cfg.withDefaults().StaleAfter), "authority-sweeper")
}

// PollDue asks about every filing whose time has come. Each runs in its own tenant's scope.
func (e *Engine) PollDue(ctx context.Context, tenantCtx func(context.Context, string) context.Context, principal string) (int, error) {
	due, err := e.Store.DuePolls(ctx, e.now(), 50)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range due {
		tctx := tenantCtx(ctx, d.TenantID)
		if _, err := e.Poll(tctx, d.SubmissionID, principal, Caller{TenantID: d.TenantID, PrincipalID: principal}); err == nil {
			n++
		} else if errors.Is(err, ErrInFlight) {
			continue
		}
	}
	return n, nil
}

// ── classification ──────────────────────────────────────────────────────────

func (e *Engine) outcomeOf(r adapter.Result, from string, prof *ProfileRecord, op, actor string, cfg Config, f *Filing) Outcome {
	entry := entryOf(r, prof, op, actor)
	o := Outcome{From: StatusSubmitting, Transport: entry, AuthorityRef: r.ProviderRef, Authority: r.ProviderStat, ReceiptHash: r.ResponseHash}
	switch r.Disposition {
	case adapter.NotSent:
		o.Kind, o.ReleaseTo, o.Failure = OutRelease, from, "not_sent:"+r.ErrCode
		return o
	case adapter.Unknown:
		o.Kind, o.Failure, o.NextPoll = OutUnknown, "no_answer_after_send", e.nextPoll(f, cfg)
		return o
	}
	switch r.Class {
	case adapter.ClassAcknowledged, adapter.ClassPending:
		o.Kind, o.NextPoll = OutSubmitted, e.nextPoll(f, cfg)
	case adapter.ClassAccepted:
		o.Kind = OutAccepted
	case adapter.ClassRejected:
		o.Kind, o.Reason = OutRejected, r.Reason
	case adapter.ClassRateLimited:
		wait := r.RetryAfter
		if wait <= 0 {
			wait = cfg.DefaultRetryAfter
		}
		nb := e.now().Add(wait)
		o.Kind, o.ReleaseTo, o.NotBefore, o.Failure = OutRelease, from, &nb, "rate_limited"
	case adapter.ClassAuth:
		o.Kind, o.ReleaseTo, o.Failure = OutRelease, from, "authority_auth_failed"
	case adapter.ClassMismatch:
		// the answer is not about our filing: it proves nothing, so the filing is unknown, never accepted
		o.Kind, o.Failure, o.NextPoll = OutUnknown, "receipt_mismatch", e.nextPoll(f, cfg)
	default:
		o.Kind, o.Failure, o.NextPoll = OutUnknown, "answer_not_conclusive:"+string(r.Class)+":"+r.ErrCode, e.nextPoll(f, cfg)
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

func noteOf(o Outcome) string {
	switch o.Kind {
	case OutSubmitted:
		return "received_by_authority"
	case OutAccepted:
		return "accepted_by_authority"
	case OutRejected:
		return "rejected_by_authority"
	case OutUnknown:
		return "outcome_unknown"
	case OutRelease:
		return o.Failure
	}
	return ""
}

func isRefusal(err error) bool {
	return errors.Is(err, adapter.ErrSchemaInvalid) || errors.Is(err, adapter.ErrRailUnsupported) ||
		errors.Is(err, adapter.ErrCurrency) || errors.Is(err, adapter.ErrLimitExceeded) || errors.Is(err, adapter.ErrDocumentMissing)
}

func refusalName(err error) string {
	switch {
	case errors.Is(err, adapter.ErrRailUnsupported):
		return "unsupported authority schema version"
	case errors.Is(err, adapter.ErrDocumentMissing):
		return "no approved document"
	}
	return err.Error()
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
