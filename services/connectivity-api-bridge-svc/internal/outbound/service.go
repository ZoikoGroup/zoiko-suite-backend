package outbound

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/egress"
)

// Connections is what the engine needs of the connection layer (XIC-02).
type Connections interface {
	Connection(ctx context.Context, id string) (*xicconn.Connection, error)
	PinnedContract(ctx context.Context, c *xicconn.Connection) (*registry.ContractVersion, error)
	Operable(ctx context.Context, tenant, connectionID, operationID string) (*xicconn.Decision, error)
}

// Secrets releases the material of a bound credential reference for one call. The caller zeroes it after.
type Secrets interface {
	Secret(ctx context.Context, tenantID, credentialRef string) ([]byte, error)
}

// ErrSecretUnavailable is what a Secrets returns when the vault could not answer (retry), as opposed to refusing.
var ErrSecretUnavailable = errors.New("credential material is unavailable")

// Config holds the engine's policy knobs.
type Config struct {
	BodyRetention     time.Duration // how long an open attempt's request body may be kept. Zero keeps none.
	BodyMaxBytes      int           // a body larger than this is never stored
	BodyDenyClasses   []string      // data classes whose bodies are never stored
	RequestTimeout    time.Duration // the whole provider call
	SubmitLease       time.Duration // a SUBMITTING attempt older than this is treated as crashed
	ResolveBackoff    time.Duration // delay before the first status query; doubles per query
	ResolveBackoffMax time.Duration
	MaxUnknownAge     time.Duration // OD-13: an attempt open this long is escalated once
	MaxResponseBytes  int64
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{BodyRetention: 7 * 24 * time.Hour, BodyMaxBytes: 256 << 10, BodyDenyClasses: []string{"PERSONAL", "RESTRICTED"},
		RequestTimeout: 20 * time.Second, SubmitLease: 2 * time.Minute, ResolveBackoff: 30 * time.Second, ResolveBackoffMax: 15 * time.Minute,
		MaxUnknownAge: 24 * time.Hour, MaxResponseBytes: 64 << 10}
}

// Service is the outbound engine.
type Service struct {
	Repo    Repo
	Conns   Connections
	Secrets Secrets
	Egress  egress.Policy
	Cfg     Config
	Clock   func() time.Time
	// HTTP overrides the provider client. It exists for tests; production leaves it nil and every call goes through
	// the egress policy's pinned dialer.
	HTTP *http.Client
	// Check overrides the destination check, for tests that cannot resolve a public name.
	Check func(ctx context.Context, url string) (string, error)
	// Observe receives a short reason for every refusal and outcome, for metrics. It must not block.
	Observe func(kind, reason string)
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) cfg() Config {
	if s.Cfg.RequestTimeout == 0 {
		return DefaultConfig()
	}
	return s.Cfg
}

func (s *Service) observe(kind, reason string) {
	if s.Observe != nil {
		s.Observe(kind, reason)
	}
}

func (s *Service) checkDest(ctx context.Context, u string) (string, error) {
	if s.Check != nil {
		return s.Check(ctx, u)
	}
	return s.Egress.CheckDestination(ctx, u)
}

func (s *Service) client() *http.Client {
	var c *http.Client
	if s.HTTP != nil {
		cp := *s.HTTP
		c = &cp
	} else {
		c = s.Egress.NewHTTPClient(s.cfg().RequestTimeout)
	}
	// a redirect is never followed: the next host was not approved, and the credential must not follow it (I-20)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// Backoff is the delay before the nth status query of an attempt.
func (s *Service) Backoff(n int) time.Duration {
	cfg := s.cfg()
	d := cfg.ResolveBackoff
	for i := 1; i < n && d < cfg.ResolveBackoffMax; i++ {
		d *= 2
	}
	if d > cfg.ResolveBackoffMax {
		d = cfg.ResolveBackoffMax
	}
	return d
}

var (
	domainRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#@=+-]{0,127}$`)
	uuidRE      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func dflt(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// ReserveInput is what a domain supplies to reserve a logical outbound operation.
type ReserveInput struct {
	ConnectionID     string          `json:"connection_id"`
	OperationID      string          `json:"operation_id"`
	DomainRequestRef string          `json:"domain_request_ref"`
	Payload          json.RawMessage `json:"payload"`
}

// supportedAuth lists the mechanisms an outbound call can carry. OAuth and mTLS fail closed at reservation: nothing
// is reserved for an operation that could never be submitted.
func supportedAuth(m string) bool { return m == "API_KEY_HEADER" || m == "HMAC" }

// Reserve durably records the logical outbound operation BEFORE any network call (I-13). It validates the request
// against the pinned contract, the connection's state and the payload schema, takes the payload hash, fixes the
// provider idempotency reference for the life of the attempt, and stores the request body only if data policy allows
// and only while the attempt is open. Reserving the same domain request again returns the original attempt; the same
// request with a different payload is a conflict, never a second operation.
func (s *Service) Reserve(ctx context.Context, actor Actor, in ReserveInput) (*Attempt, bool, error) {
	tenant := tenantOf(ctx)
	var is []string
	if !uuidRE.MatchString(in.ConnectionID) {
		is = append(is, "connection_id")
	}
	if strings.TrimSpace(in.OperationID) == "" || len(in.OperationID) > 63 {
		is = append(is, "operation_id")
	}
	if !domainRefRE.MatchString(in.DomainRequestRef) {
		is = append(is, "domain_request_ref")
	}
	if len(bytes.TrimSpace(in.Payload)) == 0 || string(bytes.TrimSpace(in.Payload)) == "null" {
		is = append(is, "payload")
	}
	if len(is) > 0 {
		return nil, false, fmt.Errorf("%w: %s", ErrBadRequest, strings.Join(is, ", "))
	}
	d, err := s.Conns.Operable(ctx, tenant, in.ConnectionID, in.OperationID)
	if err != nil {
		return nil, false, err
	}
	op := d.Operation
	if op.Direction != registry.Outbound {
		return nil, false, ErrOperationDirection
	}
	conn, err := s.Conns.Connection(ctx, in.ConnectionID)
	if err != nil {
		return nil, false, err
	}
	v, err := s.Conns.PinnedContract(ctx, conn)
	if err != nil {
		return nil, false, err
	}
	auth, ok := v.UsedAuthProfiles()[op.AuthProfile]
	if !ok || !supportedAuth(auth.Mechanism) {
		return nil, false, ErrAuthUnsupported
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, in.Payload); err != nil {
		return nil, false, fmt.Errorf("%w: payload is not valid JSON", ErrBadRequest)
	}
	body := compact.Bytes()
	if op.MaxPayloadBytes > 0 && int64(len(body)) > op.MaxPayloadBytes {
		return nil, false, fmt.Errorf("%w: payload exceeds the operation's size limit", ErrBadRequest)
	}
	if op.RequestSchema != "" {
		sch := v.SchemaByRef(op.RequestSchema)
		if sch == nil || sch.MediaType != "application/json" {
			return nil, false, fmt.Errorf("%w: the operation's request schema is not usable for a JSON call", ErrBadRequest)
		}
		if int64(len(body)) > sch.MaxSizeBytes {
			return nil, false, fmt.Errorf("%w: payload exceeds the schema's size limit", ErrBadRequest)
		}
		compiled, err := adapter.CompileSchema(sch.ValidationProfile)
		if err != nil {
			return nil, false, fmt.Errorf("%w: the contract's request schema cannot be applied", ErrBadRequest)
		}
		if err := compiled.ValidateDocument(body); err != nil {
			return nil, false, fmt.Errorf("%w: the payload does not satisfy the contract's request schema", ErrBadRequest)
		}
	}

	now := s.now()
	cfg := s.cfg()
	a := &Attempt{AttemptID: uuid.NewString(), TenantID: tenant, ConnectionID: in.ConnectionID, ConnectorID: conn.ConnectorID, OperationID: in.OperationID,
		ContractVersionID: v.ContractVersionID, DomainRequestRef: in.DomainRequestRef, PayloadProfile: op.RequestSchema, PayloadHash: hashBytes(body),
		PayloadSize: len(body), State: StateReserved, RequestedBy: actor.PrincipalID, CorrelationID: actor.CorrelationID, Version: 1, CreatedAt: now, UpdatedAt: now}
	if op.ProviderIdempotency.Supported {
		a.ProviderIdemRefHash = hashBytes([]byte(a.AttemptID))
		a.ResubmitAllowed = op.ResubmitAfterUnknown
	}
	if cfg.BodyRetention > 0 && len(body) <= cfg.BodyMaxBytes && !denied(op.DataClasses, cfg.BodyDenyClasses) {
		exp := now.Add(cfg.BodyRetention)
		a.Body, a.BodyExpiresAt, a.BodyRetained = body, &exp, true
	}
	ev := Event{Kind: "RESERVED", To: StateReserved, Actor: actor.PrincipalID, At: now,
		Detail: mustJSON(map[string]any{"operation_id": a.OperationID, "payload_hash": a.PayloadHash, "body_stored": a.BodyRetained})}
	out := []OutEvent{{Type: "integration.outbound.reserved", AggregateType: "outbound-attempt", AggregateID: a.AttemptID, ActorID: actor.PrincipalID, CorrelationID: a.CorrelationID,
		Data: map[string]any{"attempt_id": a.AttemptID, "connection_id": a.ConnectionID, "operation_id": a.OperationID, "tenant_id": tenant,
			"domain_request_ref": a.DomainRequestRef, "payload_hash": a.PayloadHash}}}
	existing, err := s.Repo.Reserve(ctx, a, ev, out)
	switch {
	case err == nil:
		s.observe("reserved", op.ID)
		return a, true, nil
	case errors.Is(err, ErrDuplicate):
		if existing.PayloadHash != a.PayloadHash {
			return nil, false, ErrPayloadConflict
		}
		return existing, false, nil
	}
	return nil, false, err
}

func denied(classes, deny []string) bool {
	for _, c := range classes {
		if containsFold(deny, c) {
			return true
		}
	}
	return false
}

// prepared is everything a call needs, checked right before it is made.
type prepared struct {
	d      *xicconn.Decision
	v      *registry.ContractVersion
	op     registry.Operation
	auth   registry.AuthInfo
	url    string
	secret []byte
	media  string
}

func (p *prepared) wipe() {
	for i := range p.secret {
		p.secret[i] = 0
	}
}

// prepare re-validates authority at execution (NP-15), builds the destination from the connection's governed
// endpoint binding, checks it against egress policy BEFORE any credential is fetched, and then fetches the credential.
// Any failure here means nothing was sent.
func (s *Service) prepare(ctx context.Context, a *Attempt, operationID string, query url.Values) (*prepared, error) {
	d, err := s.Conns.Operable(ctx, a.TenantID, a.ConnectionID, operationID)
	if err != nil {
		return nil, err
	}
	conn, err := s.Conns.Connection(ctx, a.ConnectionID)
	if err != nil {
		return nil, err
	}
	v, err := s.Conns.PinnedContract(ctx, conn)
	if err != nil {
		return nil, err
	}
	op := d.Operation
	if op.Direction != registry.Outbound {
		return nil, ErrOperationDirection
	}
	auth, ok := v.UsedAuthProfiles()[op.AuthProfile]
	if !ok || !supportedAuth(auth.Mechanism) {
		return nil, ErrAuthUnsupported
	}
	ep, ok := v.UsedEndpointProfiles()[op.EndpointProfile]
	if !ok || d.Host == "" || !containsFold(ep.Hosts, d.Host) {
		return nil, ErrBlocked
	}
	port := 443
	if len(ep.Ports) > 0 {
		port = ep.Ports[0]
	}
	hostport := d.Host
	if port != 443 {
		hostport = net.JoinHostPort(d.Host, strconv.Itoa(port))
	}
	u := "https://" + hostport + op.EndpointTemplate
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	if _, err := s.checkDest(ctx, u); err != nil {
		_ = s.Repo.RecordSignal(ctx, a.TenantID, OutEvent{Type: "integration.security.outbound-egress-blocked", AggregateType: "outbound-attempt", AggregateID: a.AttemptID,
			ActorID: "system:xic-outbound", CorrelationID: a.CorrelationID, Data: map[string]any{"tenant_id": a.TenantID, "attempt_id": a.AttemptID, "reason_code": "XIC-009"}})
		return nil, ErrBlocked
	}
	p := &prepared{d: d, v: v, op: op, auth: auth, url: u, media: "application/json"}
	if op.RequestSchema != "" {
		if sch := v.SchemaByRef(op.RequestSchema); sch != nil && sch.MediaType != "" {
			p.media = sch.MediaType
		}
	}
	var lastErr error = ErrBlocked
	for _, c := range d.CredentialRefs {
		sec, err := s.Secrets.Secret(ctx, a.TenantID, c.Ref)
		if err == nil {
			p.secret = sec
			return p, nil
		}
		if errors.Is(err, ErrSecretUnavailable) {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		lastErr = ErrBlocked
	}
	return nil, lastErr
}

func macFor(algorithm string) func() hash.Hash {
	switch algorithm {
	case "HMAC-SHA384":
		return sha512.New384
	case "HMAC-SHA512":
		return sha512.New
	}
	return sha256.New
}

// authorize attaches the credential to the request. Secrets are used here and nowhere else.
func (p *prepared) authorize(req *http.Request, body []byte, now time.Time) {
	switch p.auth.Mechanism {
	case "API_KEY_HEADER":
		req.Header.Set(p.auth.APIKeyHeader, string(p.secret))
	case "HMAC":
		ts := strconv.FormatInt(now.Unix(), 10)
		if p.auth.TimestampFormat == "RFC3339" {
			ts = now.Format(time.RFC3339)
		}
		content := body
		if p.auth.SignedContent == "TIMESTAMP_DOT_BODY" {
			content = append([]byte(ts+"."), body...)
		}
		m := hmac.New(macFor(p.auth.SignatureAlgorithm), p.secret)
		m.Write(content)
		sum := m.Sum(nil)
		sig := hex.EncodeToString(sum)
		if p.auth.SignatureEncoding == "base64" {
			sig = base64.StdEncoding.EncodeToString(sum)
		}
		req.Header.Set(p.auth.TimestampHeader, ts)
		req.Header.Set(p.auth.SignatureHeader, sig)
	}
}

// providerKey is the provider idempotency key of an attempt: stable for every submission of the same logical
// attempt (I-15), and never derived from anything a caller controls.
func providerKey(a *Attempt) string { return a.AttemptID }

// response is what a provider call produced. sent is false when the failure happened before any request bytes could
// have left (refused destination, failed dial), in which case nothing can have reached the provider.
type response struct {
	status int
	body   []byte
	err    error
	sent   bool
}

func (s *Service) call(ctx context.Context, p *prepared, a *Attempt, method string, body []byte) response {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg().RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, p.url, rd)
	if err != nil {
		return response{err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", p.media)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-Id", a.AttemptID)
	if p.op.ProviderIdempotency.Supported && p.op.ProviderIdempotency.Header != "" {
		req.Header.Set(p.op.ProviderIdempotency.Header, providerKey(a))
	}
	p.authorize(req, body, s.now())
	resp, err := s.client().Do(req)
	if err != nil {
		return response{err: err, sent: !notSent(err)}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, s.cfg().MaxResponseBytes))
	return response{status: resp.StatusCode, body: b, sent: true}
}

// notSent reports an error that happened before the request could have reached the provider: the egress policy's
// refusal or a failed dial. Everything else after the request began is ambiguous.
func notSent(err error) bool {
	if errors.Is(err, egress.ErrDenied) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

func tenantOf(ctx context.Context) string { return tenantFromCtx(ctx) }

// bodyFor returns the request body of an attempt: the stored one, or the one supplied with the submit and proved
// against the payload hash.
func (s *Service) bodyFor(a *Attempt, supplied []byte) ([]byte, error) {
	if len(supplied) > 0 {
		var c bytes.Buffer
		if err := json.Compact(&c, supplied); err != nil {
			return nil, fmt.Errorf("%w: body is not valid JSON", ErrBadRequest)
		}
		if hashBytes(c.Bytes()) != a.PayloadHash {
			return nil, ErrBodyMismatch
		}
		return c.Bytes(), nil
	}
	if len(a.Body) > 0 {
		return a.Body, nil
	}
	return nil, ErrBodyNotHeld
}

var errInFlight = errors.New("in flight")

// Submit submits, or where the contract allows resumes, the ORIGINAL attempt (§9.1). It never creates a second
// logical attempt. The attempt is moved to SUBMITTING and that state is committed before the network call, so a
// crash after the call leaves a durable trace. A terminal attempt answers with its state and sends nothing; an
// attempt already submitting is in flight and is left alone; an UNKNOWN or PENDING attempt is submitted again only
// when the contract made that safe (same provider key), and is otherwise to be resolved, not resent (I-14).
func (s *Service) Submit(ctx context.Context, actor Actor, id string, supplied []byte) (*Attempt, error) {
	a, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !a.Open() || a.State == StateSubmitting {
		return a, nil
	}
	if (a.State == StateUnknown || a.State == StatePending) && !a.ResubmitAllowed {
		return nil, ErrUnknownMustBeResolved
	}
	body, err := s.bodyFor(a, supplied)
	if err != nil {
		return nil, err
	}
	p, err := s.prepare(ctx, a, a.OperationID, nil)
	if err != nil {
		s.recordBlocked(ctx, a, err)
		return nil, err
	}
	defer p.wipe()
	if p.d.ContractVersionID != a.ContractVersionID {
		// the connection moved to another contract version since the reservation: the reserved payload was
		// validated against the old one, so the attempt must not go out under the new one
		s.recordBlocked(ctx, a, ErrBlocked)
		return nil, &xicconn.Refusal{Code: xicconn.CodeContractMismatch, Detail: "the connection moved to another contract version after the attempt was reserved", ConnectionID: a.ConnectionID}
	}

	now := s.now()
	prev := a.State
	_, err = s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		st := tx.A.State
		if st != StateReserved && !((st == StateUnknown || st == StatePending) && tx.A.ResubmitAllowed) {
			return errInFlight
		}
		prev = st
		lease := now.Add(s.cfg().SubmitLease)
		tx.A.State, tx.A.SubmitCount, tx.A.LastSubmitAt, tx.A.NextResolutionAt = StateSubmitting, tx.A.SubmitCount+1, &now, &lease
		tx.Event = &Event{Kind: "SUBMIT_STARTED", From: st, To: StateSubmitting, Actor: actor.PrincipalID, At: now,
			Detail: mustJSON(map[string]any{"submit_count": tx.A.SubmitCount, "resubmission": st != StateReserved})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.submitted", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: actor.PrincipalID,
			CorrelationID: tx.A.CorrelationID, Data: map[string]any{"attempt_id": tx.A.AttemptID, "connection_id": tx.A.ConnectionID, "operation_id": tx.A.OperationID,
				"tenant_id": tx.A.TenantID, "provider_idempotency_ref_hash": tx.A.ProviderIdemRefHash, "submit_count": tx.A.SubmitCount}})
		return nil
	})
	if errors.Is(err, errInFlight) {
		return s.Repo.Get(ctx, id)
	}
	if err != nil {
		return nil, err
	}

	method := dflt(p.op.Method, http.MethodPost)
	r := s.call(ctx, p, a, method, body)
	// the answer is recorded even if the caller went away: the call may have reached the provider
	rctx := context.WithoutCancel(ctx)
	return s.recordSubmit(rctx, a, p, prev, r)
}

// classify turns a provider answer to a SUBMIT into a state. Only a clear success or a clear failure is final; any
// answer that does not prove whether the provider acted is UNKNOWN (§9.1, I-14).
func classify(status int) (state, reason string) {
	switch {
	case status >= 200 && status < 300:
		return StateSettled, ""
	case status >= 300 && status < 400:
		return StateFailed, ReasonRedirectRefused
	case status == 401 || status == 403:
		return StateFailed, ReasonAuthRejected
	case status == 400 || status == 404 || status == 405 || status == 410 || status == 415 || status == 422:
		return StateFailed, ReasonProviderRejected
	case status == 408:
		return StateUnknown, ReasonTimeout
	case status == 429:
		return StateUnknown, ReasonRateLimited
	case status >= 500:
		return StateUnknown, ReasonProviderUnavail
	}
	return StateUnknown, ReasonUnclassified
}

var refKeys = []string{"provider_reference", "payment_id", "reference", "ref", "id", "object_id"}

func providerRef(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	for _, k := range refKeys {
		if v, ok := m[k].(string); ok && v != "" && len(v) <= 128 && domainRefRE.MatchString(v) {
			return v
		}
	}
	return ""
}

func (s *Service) recordSubmit(ctx context.Context, a *Attempt, p *prepared, prev string, r response) (*Attempt, error) {
	now := s.now()
	hasQuery := p.op.UnknownResolution.StatusQueryOperation != ""
	if r.err != nil {
		if !r.sent {
			return s.revert(ctx, a, prev, now)
		}
		reason := ReasonConnectionLost
		var ne net.Error
		if errors.As(r.err, &ne) && ne.Timeout() || errors.Is(r.err, context.DeadlineExceeded) {
			reason = ReasonTimeout
		}
		return s.toUnknown(ctx, a, reason, 0, "", hasQuery, now)
	}
	state, reason := classify(r.status)
	ref := providerRef(r.body)
	switch state {
	case StateSettled:
		if p.op.CallbackProfile != "" || r.status == http.StatusAccepted {
			return s.toPending(ctx, a, r.status, ref, hasQuery, now)
		}
		return s.toFinal(ctx, a, StateSettled, MethodSubmit, "", r.status, ref, "", "", now)
	case StateFailed:
		if reason == ReasonRedirectRefused {
			_ = s.Repo.RecordSignal(ctx, a.TenantID, OutEvent{Type: "integration.security.outbound-redirect-refused", AggregateType: "outbound-attempt", AggregateID: a.AttemptID,
				ActorID: "system:xic-outbound", CorrelationID: a.CorrelationID, Data: map[string]any{"tenant_id": a.TenantID, "attempt_id": a.AttemptID, "reason_code": "XIC-009"}})
		}
		return s.toFinal(ctx, a, StateFailed, MethodSubmit, reason, r.status, ref, "", "", now)
	}
	return s.toUnknown(ctx, a, reason, r.status, ref, hasQuery, now)
}

func (s *Service) nextResolution(a *Attempt, hasQuery bool, now time.Time) *time.Time {
	if !hasQuery {
		return nil
	}
	t := now.Add(s.Backoff(a.ResolveCount + 1))
	return &t
}

var errNoChange = errors.New("no change")

func (s *Service) mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Attempt, error) {
	a, err := s.Repo.Mutate(ctx, id, fn)
	if errors.Is(err, errNoChange) {
		return s.Repo.Get(ctx, id)
	}
	return a, err
}

// revert puts an attempt back to the state it had before a submission whose request provably never left.
func (s *Service) revert(ctx context.Context, a *Attempt, prev string, now time.Time) (*Attempt, error) {
	out, err := s.mutate(ctx, a.AttemptID, func(tx *Tx) error {
		if tx.A.State != StateSubmitting {
			return errNoChange
		}
		tx.A.State = prev
		if prev == StateReserved {
			tx.A.NextResolutionAt = nil
		}
		tx.Event = &Event{Kind: "NOT_SENT", From: StateSubmitting, To: prev, Actor: "system:xic-outbound", At: now,
			Detail: mustJSON(map[string]any{"reason": ReasonBlocked})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.blocked", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: "system:xic-outbound",
			CorrelationID: tx.A.CorrelationID, Data: map[string]any{"attempt_id": tx.A.AttemptID, "tenant_id": tx.A.TenantID, "reason_code": ReasonBlocked}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.observe("not_sent", ReasonBlocked)
	return out, ErrBlocked
}

// recordBlocked records that a submission was refused before anything was sent. The attempt keeps its state.
func (s *Service) recordBlocked(ctx context.Context, a *Attempt, cause error) {
	now := s.now()
	code := "BLOCKED"
	var ref *xicconn.Refusal
	if errors.As(cause, &ref) {
		code = string(ref.Code)
	}
	_, _ = s.mutate(ctx, a.AttemptID, func(tx *Tx) error {
		if !tx.A.Open() {
			return errNoChange
		}
		tx.Event = &Event{Kind: "SUBMIT_BLOCKED", From: tx.A.State, To: tx.A.State, Actor: "system:xic-outbound", At: now, Detail: mustJSON(map[string]any{"reason_code": code})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.blocked", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: "system:xic-outbound",
			CorrelationID: tx.A.CorrelationID, Data: map[string]any{"attempt_id": tx.A.AttemptID, "tenant_id": tx.A.TenantID, "reason_code": code}})
		return nil
	})
	s.observe("blocked", code)
}

func (s *Service) toUnknown(ctx context.Context, a *Attempt, reason string, status int, ref string, hasQuery bool, now time.Time) (*Attempt, error) {
	out, err := s.mutate(ctx, a.AttemptID, func(tx *Tx) error {
		if tx.A.State != StateSubmitting && tx.A.State != StatePending && tx.A.State != StateUnknown {
			return errNoChange
		}
		from := tx.A.State
		tx.A.State, tx.A.AmbiguityReason, tx.A.ProviderStatus = StateUnknown, reason, status
		if ref != "" && tx.A.ProviderReference == "" {
			tx.A.ProviderReference = ref
		}
		tx.A.NextResolutionAt = s.nextResolution(tx.A, hasQuery, now)
		tx.Event = &Event{Kind: "UNKNOWN", From: from, To: StateUnknown, Actor: "system:xic-outbound", At: now,
			Detail: mustJSON(map[string]any{"ambiguity_reason": reason, "provider_status": status})}
		data := map[string]any{"attempt_id": tx.A.AttemptID, "tenant_id": tx.A.TenantID, "connection_id": tx.A.ConnectionID, "ambiguity_reason": reason, "provider_reference": tx.A.ProviderReference}
		if tx.A.NextResolutionAt != nil {
			data["next_resolution_at"] = tx.A.NextResolutionAt.Format(time.RFC3339)
		}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.unknown", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: "system:xic-outbound",
			CorrelationID: tx.A.CorrelationID, Data: data})
		return nil
	})
	if err == nil {
		s.observe("unknown", reason)
	}
	return out, err
}

func (s *Service) toPending(ctx context.Context, a *Attempt, status int, ref string, hasQuery bool, now time.Time) (*Attempt, error) {
	out, err := s.mutate(ctx, a.AttemptID, func(tx *Tx) error {
		if tx.A.State != StateSubmitting && tx.A.State != StateUnknown && tx.A.State != StatePending {
			return errNoChange
		}
		from := tx.A.State
		tx.A.State, tx.A.AmbiguityReason, tx.A.ProviderStatus = StatePending, "", status
		if ref != "" && tx.A.ProviderReference == "" {
			tx.A.ProviderReference = ref
		}
		tx.A.NextResolutionAt = s.nextResolution(tx.A, hasQuery, now)
		tx.Event = &Event{Kind: "PENDING", From: from, To: StatePending, Actor: "system:xic-outbound", At: now, Detail: mustJSON(map[string]any{"provider_status": status})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.pending", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: "system:xic-outbound",
			CorrelationID: tx.A.CorrelationID, Data: map[string]any{"attempt_id": tx.A.AttemptID, "tenant_id": tx.A.TenantID, "connection_id": tx.A.ConnectionID, "provider_reference": tx.A.ProviderReference}})
		return nil
	})
	if err == nil {
		s.observe("pending", "accepted")
	}
	return out, err
}

// toFinal records SETTLED or FAILED with how it was reached. The stored body is dropped: it is no longer needed.
func (s *Service) toFinal(ctx context.Context, a *Attempt, state, method, reason string, status int, ref, resolvedBy, evidence string, now time.Time) (*Attempt, error) {
	out, err := s.mutate(ctx, a.AttemptID, func(tx *Tx) error {
		if !tx.A.Open() || tx.A.State == StateReserved {
			return errNoChange
		}
		from := tx.A.State
		tx.A.State, tx.A.AmbiguityReason, tx.A.ProviderStatus = state, reason, status
		if ref != "" && tx.A.ProviderReference == "" {
			tx.A.ProviderReference = ref
		}
		tx.A.ResolutionMethod, tx.A.ResolvedBy, tx.A.EvidenceRef = method, resolvedBy, evidence
		tx.A.FinalOutcomeRef = mustJSON(map[string]any{"transport_state": state, "provider_status": status, "provider_reference": tx.A.ProviderReference, "reason": reason, "method": method})
		tx.A.NextResolutionAt, tx.A.Body, tx.A.BodyRetained = nil, nil, false
		tx.Event = &Event{Kind: "RESOLVED", From: from, To: state, Actor: dflt(resolvedBy, "system:xic-outbound"), At: now,
			Detail: mustJSON(map[string]any{"method": method, "provider_status": status, "reason": reason})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.resolved", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: dflt(resolvedBy, "system:xic-outbound"),
			CorrelationID: tx.A.CorrelationID, Data: map[string]any{"attempt_id": tx.A.AttemptID, "tenant_id": tx.A.TenantID, "connection_id": tx.A.ConnectionID,
				"final_transport_state": state, "resolution_method": method, "provider_reference": tx.A.ProviderReference, "reason_code": reason}})
		return nil
	})
	if err == nil {
		s.observe("resolved", state)
	}
	return out, err
}

// Cancel cancels an attempt that was reserved and never submitted. Once any submission has started the original
// must be resolved instead: cancelling would deny that the provider may already have acted.
func (s *Service) Cancel(ctx context.Context, actor Actor, id, reason string) (*Attempt, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 255 {
		return nil, fmt.Errorf("%w: reason", ErrBadRequest)
	}
	now := s.now()
	return s.mutate(ctx, id, func(tx *Tx) error {
		if tx.A.State == StateCancelled {
			return errNoChange
		}
		if tx.A.State != StateReserved || tx.A.SubmitCount > 0 {
			return ErrTransition
		}
		tx.A.State, tx.A.AmbiguityReason, tx.A.Body, tx.A.BodyRetained, tx.A.NextResolutionAt = StateCancelled, ReasonOperatorCancelled, nil, false, nil
		tx.Event = &Event{Kind: "CANCELLED", From: StateReserved, To: StateCancelled, Actor: actor.PrincipalID, At: now, Detail: mustJSON(map[string]any{"reason": reason})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.cancelled", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: actor.PrincipalID,
			CorrelationID: tx.A.CorrelationID, Data: map[string]any{"attempt_id": tx.A.AttemptID, "tenant_id": tx.A.TenantID, "connection_id": tx.A.ConnectionID}})
		return nil
	})
}
