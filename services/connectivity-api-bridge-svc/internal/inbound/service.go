package inbound

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/mapping"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/integration"
)

// Connections is what the gateway needs of the connection layer (XIC-02).
type Connections interface {
	Connection(ctx context.Context, id string) (*xicconn.Connection, error)
	PinnedContract(ctx context.Context, c *xicconn.Connection) (*registry.ContractVersion, error)
	Operable(ctx context.Context, tenant, connectionID, operationID string) (*xicconn.Decision, error)
}

// Config holds the gateway's policy knobs.
type Config struct {
	MaxBodyBytes       int64         // the read cap before any parsing; also bounds signature work
	RawRetention       time.Duration // OD-05: how long a raw body is kept. Zero keeps none.
	RawMaxBytes        int           // a body larger than this is never retained raw
	RawDenyClasses     []string      // data classes whose raw bodies are never retained
	HandoffMaxAttempts int           // attempts after which a stalled handoff is escalated
	HandoffBackoffBase time.Duration
	HandoffBackoffMax  time.Duration
	HandoffLease       time.Duration // how long a claimed handoff is left alone before it is retried
	ConnRatePerMin     int           // requests per connection per minute (0 = unlimited)
	IPRatePerMin       int           // requests per source address per minute (0 = unlimited)
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{MaxBodyBytes: 5 << 20, RawRetention: 30 * 24 * time.Hour, RawMaxBytes: 1 << 20,
		RawDenyClasses: []string{"PERSONAL", "RESTRICTED"}, HandoffMaxAttempts: 8, HandoffBackoffBase: 5 * time.Second,
		HandoffBackoffMax: 15 * time.Minute, HandoffLease: time.Minute, ConnRatePerMin: 600, IPRatePerMin: 1200}
}

// Service is the gateway.
type Service struct {
	Repo       Repo
	Conns      Connections
	Secrets    Secrets
	EdgeSecret []byte
	Cfg        Config
	Clock      func() time.Time
	// Handoff holds the delivery collaborators: egress policy, allowed regions, the HTTP client and workload identity.
	Handoff HandoffDeps
	// Observe receives a short reason for every refusal and outcome, for metrics. It must not block.
	Observe func(kind, reason string)

	limMu sync.Mutex
	lim   *limiter
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) cfg() Config {
	if s.Cfg.MaxBodyBytes == 0 {
		return DefaultConfig()
	}
	return s.Cfg
}

func (s *Service) observe(kind, reason string) {
	if s.Observe != nil {
		s.Observe(kind, reason)
	}
}

// Request is one delivery as the HTTP layer hands it over. Body is already bounded by the read cap.
type Request struct {
	ConnectorKey  string
	ConnectionID  string
	OperationID   string
	Method        string
	Header        http.Header
	Body          []byte
	RemoteAddr    string
	CorrelationID string
}

// Outcome is what the gateway answers. A refusal carries a stable XIC error; an accepted delivery carries the
// exchange. Nothing here ever contains a payload value.
type Outcome struct {
	Status     int
	Exchange   *Exchange
	XIC        *integration.XICError
	Duplicate  bool
	RetryAfter int
	// Detail is a short, value-free sentence for the response of non-XIC refusals.
	Detail string
	// Reason is the stable machine reason, also stored on the exchange.
	Reason string
}

var (
	connectorKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{2,62}$`)
	operationIDRE  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,62}$`)
)

func sha(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// authFailure is the one uniform answer to anything that goes wrong before the sender is authenticated: unknown
// connection, wrong connector or operation, wrong method, missing or bad credential. The caller learns nothing
// about which, and nothing durable is written for it (§8.1 step 3, INT: unauthenticated input writes nothing).
func (s *Service) authFailure(reason string) *Outcome {
	s.observe("refused", reason)
	return &Outcome{Status: http.StatusUnauthorized, XIC: integration.NewXIC(integration.XICAuthenticationFailed, "authentication failed"), Reason: reason}
}

// Receive runs the pipeline for one delivery.
func (s *Service) Receive(ctx context.Context, req Request) *Outcome {
	now := s.now()
	cfg := s.cfg()
	ipKey := hostOf(req.RemoteAddr)

	// 1. the envelope, before anything is parsed or any secret is fetched
	if !s.allow("conn:"+req.ConnectionID, cfg.ConnRatePerMin, now) || !s.allow("ip:"+ipKey, cfg.IPRatePerMin, now) {
		s.observe("refused", "rate_limited")
		return &Outcome{Status: http.StatusTooManyRequests, RetryAfter: 30, Reason: "rate_limited",
			XIC: integration.NewXIC(integration.XICRateLimited, "too many requests")}
	}
	if int64(len(req.Body)) > cfg.MaxBodyBytes {
		s.observe("refused", "too_large")
		return &Outcome{Status: http.StatusRequestEntityTooLarge, Reason: "too_large", Detail: "the request body is larger than this endpoint accepts"}
	}

	// 2. resolve the connector operation and the candidate connection from the trusted route
	if !connectorKeyRE.MatchString(req.ConnectorKey) || !operationIDRE.MatchString(req.OperationID) {
		return s.authFailure("bad_route")
	}
	if _, err := uuid.Parse(req.ConnectionID); err != nil {
		return s.authFailure("bad_route")
	}
	conn, err := s.Conns.Connection(ctx, req.ConnectionID)
	if err != nil {
		if errors.Is(err, xicconn.ErrNotFound) {
			return s.authFailure("unknown_connection")
		}
		s.observe("refused", "dependency")
		return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "dependency", Detail: "temporarily unavailable"}
	}
	if conn.ConnectorID != req.ConnectorKey {
		return s.authFailure("connector_mismatch")
	}
	v, err := s.Conns.PinnedContract(ctx, conn)
	if err != nil {
		if errors.Is(err, xicconn.ErrContractUnavailable) {
			return s.authFailure("contract_unavailable")
		}
		s.observe("refused", "dependency")
		return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "dependency", Detail: "temporarily unavailable"}
	}
	op := v.Operation(req.OperationID)
	if op == nil || op.Direction != registry.Inbound || (op.Channel != "WEBHOOK" && op.Channel != "API") || !strings.EqualFold(op.Method, req.Method) {
		return s.authFailure("unknown_operation")
	}

	// 3. authenticate the sender: signature, key or attested client certificate
	res, aerr := s.authenticate(ctx, conn, v, op, req, now)
	switch {
	case errors.Is(aerr, ErrSecretUnavailable):
		s.observe("refused", "secret_unavailable")
		return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "secret_unavailable", Detail: "temporarily unavailable"}
	case errors.Is(aerr, ErrAuthUnsupported):
		return s.authFailure("mechanism_unsupported")
	case errors.Is(aerr, ErrAuthFailed):
		return s.authFailure("bad_credential")
	}

	// From here on the sender is authenticated, so what happens is durable and evidenced.
	base := &Exchange{ExchangeID: uuid.NewString(), TenantID: conn.TenantID, ConnectionID: conn.ConnectionID, ConnectorID: conn.ConnectorID,
		OperationID: op.ID, ContractVersionID: v.ContractVersionID, DomainOutcome: DomainNA, BodyHash: hashBytes(req.Body),
		BodySize: len(req.Body), ContentType: mediaType(req.Header.Get("Content-Type")), AuthMechanism: res.Mechanism,
		SourceNetHash: sha("net", ipKey)[:32], CorrelationID: req.CorrelationID, AttemptNo: 1, Version: 1, ReceivedAt: now, UpdatedAt: now}
	ictx := withTenant(ctx, conn.TenantID)

	// 4. tenant and replay: the tenant is the connection's, never the payload's; a stale timestamp on a valid
	//    signature is a replay attempt
	if errors.Is(aerr, ErrReplayWindow) {
		return s.reject(ictx, base, "REPLAY_WINDOW", integration.XICReplayWindowFailed, http.StatusUnauthorized, "the request is outside the replay window", res.KeyLabel)
	}
	if dec, err := s.Conns.Operable(ctx, conn.TenantID, conn.ConnectionID, op.ID); err != nil {
		var ref *xicconn.Refusal
		if errors.As(err, &ref) {
			return s.reject(ictx, base, "CONNECTION_"+string(ref.Code), integration.XICCode(ref.Code), statusFor(integration.XICCode(ref.Code)), ref.Detail, res.KeyLabel)
		}
		s.observe("refused", "dependency")
		return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "dependency", Detail: "temporarily unavailable"}
	} else {
		_ = dec
	}

	// 5. transport shape: no compressed bodies (a decompression bomb is refused, not inflated), exact media type
	if enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))); enc != "" && enc != "identity" {
		return s.reject(ictx, base, "UNSUPPORTED_ENCODING", integration.XICSchemaVersionUnsupported, http.StatusUnsupportedMediaType, "compressed request bodies are not accepted", res.KeyLabel)
	}
	if int64(len(req.Body)) > op.MaxPayloadBytes {
		return s.reject(ictx, base, "PAYLOAD_TOO_LARGE", "", http.StatusRequestEntityTooLarge, "the payload is larger than this operation allows", res.KeyLabel)
	}
	schemaHeader := dflt(op.SchemaVersionHeader, registry.DefaultSchemaVersionHeader)
	sourceRef := strings.TrimSpace(req.Header.Get(schemaHeader))
	if sourceRef == "" {
		sourceRef = op.RequestSchema
	}
	base.SourceSchema = sourceRef

	// 6. idempotency: the key comes from the contract's profile; without one a delivery cannot be recognized (I-08)
	idem := v.IdempotencyOf(op)
	if idem == nil {
		return s.reject(ictx, base, "NO_IDEMPOTENCY_PROFILE", "", http.StatusUnprocessableEntity, "the operation declares no idempotency profile", res.KeyLabel)
	}
	values, ok := idempotencyValues(idem, req.Header, req.Body)
	if !ok {
		return s.reject(ictx, base, "DEDUPE_KEY_MISSING", "", http.StatusUnprocessableEntity, "the delivery does not carry the idempotency key its contract requires", res.KeyLabel)
	}
	scope := []string{"conn", conn.ConnectionID, op.ID}
	if idem.Scope == "TENANT" {
		scope = []string{"tenant", conn.TenantID, conn.ConnectorID}
	}
	base.DedupeKey = sha(append(scope, values...)...)
	base.SourceRefHash = sha(append([]string{"ref"}, values...)...)

	// 7. media type, schema, claims, mapping, eligibility
	base.Raw = req.Body
	dec := s.evaluate(ctx, conn, v, op, sourceRef, base.ContentType, req.Body)
	if dec.Signal != nil {
		_ = s.Repo.RecordSignal(ictx, conn.TenantID, *dec.Signal(base))
	}
	s.finish(base, dec, op, v, now)

	// 8. durable, deduplicated persistence in one step
	ev, out := s.evidence(base, dec, res.KeyLabel)
	existing, err := s.Repo.Insert(ictx, base, ev, out)
	switch {
	case err == nil:
		return s.outcomeFor(base, dec)
	case errors.Is(err, ErrDedupe):
		return s.duplicate(ictx, base, existing, idem, res.KeyLabel)
	default:
		s.observe("refused", "dependency")
		return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "storage_unavailable", Detail: "temporarily unavailable"}
	}
}

func statusFor(c integration.XICCode) int {
	switch c {
	case integration.XICConnectionNotActive, integration.XICCertificationStale:
		return http.StatusConflict
	case integration.XICContractVersionMismatch:
		return http.StatusUnprocessableEntity
	case integration.XICResidencyRouteBlocked, integration.XICTenantScopeMismatch:
		return http.StatusForbidden
	}
	return c.Info().Status
}

func hostOf(remote string) string {
	if i := strings.LastIndex(remote, ":"); i > 0 && !strings.Contains(remote[i:], "]") {
		return remote[:i]
	}
	return remote
}

func mediaType(ct string) string {
	if ct == "" {
		return ""
	}
	m, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(ct))
	}
	return m
}

func dflt(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// ── authentication ───────────────────────────────────────────────────────────

func (s *Service) authenticate(ctx context.Context, conn *xicconn.Connection, v *registry.ContractVersion, op *registry.Operation, req Request, now time.Time) (AuthResult, error) {
	info, ok := v.UsedAuthProfiles()[op.AuthProfile]
	b := conn.BindingFor(op.AuthProfile)
	if !ok || b == nil || b.Status == xicconn.BindingRevoked || b.Status == xicconn.BindingExpired || (b.ExpiresAt != nil && !b.ExpiresAt.After(now)) {
		return AuthResult{}, ErrAuthFailed
	}
	refs := []KeyUse{{Ref: b.CurrentRef}}
	if b.PreviousRef != "" && b.PreviousGraceUntil != nil && b.PreviousGraceUntil.After(now) {
		refs = append(refs, KeyUse{Ref: b.PreviousRef, Grace: true})
	}
	switch info.Mechanism {
	case "HMAC", "API_KEY_HEADER", "MTLS":
	default:
		return AuthResult{}, ErrAuthUnsupported
	}
	var keys [][]byte
	var labels []string
	defer func() { zero(keys) }()
	for _, k := range refs {
		secret, err := s.Secrets.Secret(ctx, conn.TenantID, k.Ref)
		switch {
		case err == nil:
			keys = append(keys, secret)
			labels = append(labels, map[bool]string{false: "current", true: "previous"}[k.Grace])
		case errors.Is(err, ErrSecretUnavailable):
			return AuthResult{}, ErrSecretUnavailable
		}
	}
	if len(keys) == 0 {
		return AuthResult{}, ErrAuthFailed
	}
	switch info.Mechanism {
	case "HMAC":
		return VerifyHMAC(info, req.Header, req.Body, keys, labels, now)
	case "API_KEY_HEADER":
		return VerifyAPIKey(info, req.Header, keys, labels)
	default:
		return VerifyMTLS(req.Header, s.EdgeSecret, keys, labels, now)
	}
}

// ── idempotency ──────────────────────────────────────────────────────────────

// idempotencyValues resolves every key source of the profile. All must be present: a delivery that cannot present
// its key cannot be recognized as a repeat, so it is refused rather than guessed (I-08).
func idempotencyValues(p *registry.IdempotencyInfo, h http.Header, body []byte) ([]string, bool) {
	var doc any
	parsed := false
	var out []string
	for _, src := range p.KeySources {
		var val string
		switch {
		case strings.HasPrefix(src, "header:"):
			val = strings.TrimSpace(h.Get(strings.TrimPrefix(src, "header:")))
		case src == "provider_event_id":
			val = strings.TrimSpace(h.Get("X-Event-Id"))
			if val == "" {
				if !parsed {
					doc, parsed = parseJSON(body), true
				}
				val = pointerString(doc, "/event_id")
			}
		case strings.HasPrefix(src, "body:"):
			if !parsed {
				doc, parsed = parseJSON(body), true
			}
			val = pointerString(doc, strings.TrimPrefix(src, "body:"))
		}
		if val == "" || len(val) > 512 {
			return nil, false
		}
		out = append(out, src+"="+val)
	}
	return out, len(out) > 0
}

func parseJSON(b []byte) any {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

// pointerString reads an RFC 6901 pointer as a string. Only scalars count; an object or array is no identifier.
func pointerString(doc any, ptr string) string {
	if doc == nil || !strings.HasPrefix(ptr, "/") {
		return ""
	}
	cur := doc
	for _, seg := range strings.Split(ptr[1:], "/") {
		seg = strings.NewReplacer("~1", "/", "~0", "~").Replace(seg)
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[seg]
			if !ok {
				return ""
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return ""
			}
			cur = t[i]
		default:
			return ""
		}
	}
	switch t := cur.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// ── evaluation: schema, claims, mapping, eligibility ─────────────────────────

type decision struct {
	State             string
	Class             string
	Reason            string
	XIC               integration.XICCode
	Status            int
	Detail            string
	SourceSchema      string
	MappingVersionID  string
	CanonicalContract string
	Canonical         []byte
	CanonicalHash     string
	Lineage           []byte
	Issues            []mapping.Issue
	// Signal, when set, builds a security-signal event to record even though the exchange is refused.
	Signal func(e *Exchange) *OutEvent
}

func quarantine(class, reason string, xic integration.XICCode, status int, detail string) decision {
	return decision{State: StateQuarantined, Class: class, Reason: reason, XIC: xic, Status: status, Detail: detail}
}

// evaluate runs everything that decides what a delivery IS: the exact source schema, the schema's own validation,
// the sender's tenant and account claims against the authenticated connection, the deterministic mapping and the
// existence of a domain to hand the result to. It never guesses: an unknown version, an unmappable value or a
// missing target holds the delivery.
func (s *Service) evaluate(ctx context.Context, conn *xicconn.Connection, v *registry.ContractVersion, op *registry.Operation, sourceRef, contentType string, body []byte) decision {
	sch := v.SchemaByRef(sourceRef)
	if sch == nil || sch.Direction != registry.Inbound || v.MappingFor(sourceRef) == nil {
		return quarantine(ClassSchema, "SCHEMA_VERSION_UNSUPPORTED", integration.XICSchemaVersionUnsupported, http.StatusUnprocessableEntity, "the source schema version is not supported")
	}
	if sch.MediaType != "application/json" || contentType != "application/json" {
		return quarantine(ClassSchema, "MEDIA_TYPE_UNSUPPORTED", integration.XICSchemaVersionUnsupported, http.StatusUnsupportedMediaType, "the media type is not supported on this channel")
	}
	if int64(len(body)) > sch.MaxSizeBytes {
		return quarantine(ClassSchema, "SCHEMA_SIZE_EXCEEDED", "", http.StatusRequestEntityTooLarge, "the payload exceeds the schema's size limit")
	}
	compiled, err := adapter.CompileSchema(sch.ValidationProfile)
	if err != nil {
		return quarantine(ClassSchema, "SCHEMA_UNUSABLE", integration.XICContractVersionMismatch, http.StatusUnprocessableEntity, "the contract's schema cannot be applied")
	}
	if err := compiled.ValidateDocument(body); err != nil {
		return quarantine(ClassSchema, "SCHEMA_VIOLATION", "", http.StatusUnprocessableEntity, "the payload does not satisfy the contract's schema")
	}
	doc := parseJSON(body)

	// the sender's claims about who it is are corroboration; a disagreement after authentication is a security event
	if op.PayloadTenantPointer != "" {
		if got := pointerString(doc, op.PayloadTenantPointer); got != "" && got != conn.TenantID {
			d := quarantine(ClassAuthContext, "TENANT_CLAIM_MISMATCH", integration.XICTenantScopeMismatch, http.StatusForbidden, "the payload names a different tenant than the authenticated connection")
			d.Signal = func(e *Exchange) *OutEvent {
				return &OutEvent{Type: "integration.security.inbound-tenant-claim-mismatch", AggregateType: "inbound-exchange", AggregateID: e.ExchangeID,
					ActorID: "system:xic-inbound-gateway", CorrelationID: e.CorrelationID,
					Data: map[string]any{"tenant_id": e.TenantID, "connection_id": e.ConnectionID, "exchange_id": e.ExchangeID, "reason_code": "XIC-008"}}
			}
			return d
		}
	}
	if op.PayloadAccountPointer != "" {
		if got := pointerString(doc, op.PayloadAccountPointer); got != "" && xicconn.HashAccountRef(conn.TenantID, conn.ConnectorID, got) != conn.ProviderAccountHash {
			d := quarantine(ClassAuthContext, "ACCOUNT_CLAIM_MISMATCH", integration.XICTenantScopeMismatch, http.StatusForbidden, "the payload names a different provider account than the authenticated connection")
			d.Signal = func(e *Exchange) *OutEvent {
				return &OutEvent{Type: "integration.security.inbound-account-claim-mismatch", AggregateType: "inbound-exchange", AggregateID: e.ExchangeID,
					ActorID: "system:xic-inbound-gateway", CorrelationID: e.CorrelationID,
					Data: map[string]any{"tenant_id": e.TenantID, "connection_id": e.ConnectionID, "exchange_id": e.ExchangeID, "reason_code": "XIC-008"}}
			}
			return d
		}
	}

	mv := v.MappingFor(sourceRef)
	comp, err := mapping.CompileJSON(mv.Spec)
	if err != nil {
		return quarantine(ClassMapping, "MAPPING_UNUSABLE", integration.XICMappingFailed, http.StatusUnprocessableEntity, "the contract's mapping cannot be applied")
	}
	r := comp.Evaluate(body, mv.MappingVersionID, int(op.MaxPayloadBytes))
	if !r.OK() {
		class, reason := ClassMapping, r.Issues[0].Code
		for _, is := range r.Issues { // the most serious class wins
			if rank(string(is.Class)) > rank(class) {
				class, reason = string(is.Class), is.Code
			}
		}
		d := quarantine(class, strings.ToUpper(reason), integration.XICMappingFailed, http.StatusUnprocessableEntity, "the payload could not be mapped to the canonical contract")
		d.Issues, d.MappingVersionID, d.SourceSchema = r.Issues, mv.MappingVersionID, sourceRef
		return d
	}
	target, err := s.Repo.ActiveTarget(withTenant(ctx, conn.TenantID), mv.CanonicalContract)
	if err != nil || target == nil {
		d := quarantine(ClassDomainPrecondition, "NO_HANDOFF_TARGET", "", http.StatusUnprocessableEntity, "no domain is registered to receive this canonical contract")
		d.MappingVersionID, d.SourceSchema, d.CanonicalContract = mv.MappingVersionID, sourceRef, mv.CanonicalContract
		d.Canonical, d.CanonicalHash = []byte(r.Canonical), r.CanonicalHash
		return d
	}
	lin, _ := json.Marshal(r.Lineage)
	return decision{State: StateValidated, Status: http.StatusAccepted, SourceSchema: sourceRef, MappingVersionID: mv.MappingVersionID,
		CanonicalContract: mv.CanonicalContract, Canonical: []byte(r.Canonical), CanonicalHash: r.CanonicalHash, Lineage: lin}
}

func rank(class string) int {
	switch class {
	case ClassAuthContext:
		return 4
	case ClassDataPolicy:
		return 3
	case ClassSchema:
		return 2
	}
	return 1
}

// finish copies a decision onto the exchange and applies the raw-retention policy.
func (s *Service) finish(e *Exchange, d decision, op *registry.Operation, v *registry.ContractVersion, now time.Time) {
	e.State, e.QuarantineClass, e.ReasonCode, e.XICCode = d.State, d.Class, d.Reason, string(d.XIC)
	e.SourceSchema = dflt(d.SourceSchema, e.SourceSchema)
	e.MappingVersionID, e.CanonicalContract, e.CanonicalHash = d.MappingVersionID, d.CanonicalContract, d.CanonicalHash
	e.Canonical, e.Lineage = nil, d.Lineage
	if d.State == StateValidated || (d.State == StateQuarantined && d.Class == ClassDomainPrecondition && len(d.Canonical) > 0) {
		e.Canonical = d.Canonical
	}
	if d.State == StateValidated {
		e.DomainOutcome = DomainPending
		t := now
		e.NextAttemptAt = &t
	}
	// the raw body is kept only when policy allows and the delivery is one that could be corrected and replayed
	_ = v
	if !s.retainRaw(op, d, len(e.Raw)) {
		e.Raw = nil
	}
	// bodies are kept for a bounded time only: the raw one when policy allows, the canonical one while it may still
	// need delivering or replaying. Either way the purge worker removes them after this date.
	if e.Raw != nil || e.Canonical != nil {
		keep := s.cfg().RawRetention
		if keep <= 0 {
			keep = 7 * 24 * time.Hour
		}
		exp := now.Add(keep)
		e.RawExpiresAt = &exp
	}
}

func (s *Service) retainRaw(op *registry.Operation, d decision, size int) bool {
	cfg := s.cfg()
	if cfg.RawRetention <= 0 || size == 0 || size > cfg.RawMaxBytes {
		return false
	}
	if d.State == StateQuarantined && (d.Class == ClassAuthContext || d.Class == ClassDataPolicy) {
		return false // foreign or prohibited data is never kept
	}
	for _, c := range op.DataClasses {
		for _, deny := range cfg.RawDenyClasses {
			if strings.EqualFold(c, deny) {
				return false
			}
		}
	}
	return true
}

// ── evidence, outcomes ───────────────────────────────────────────────────────

func (s *Service) evidence(e *Exchange, d decision, keyLabel string) (Event, []OutEvent) {
	issues := make([]map[string]string, 0, len(d.Issues))
	for _, is := range d.Issues {
		issues = append(issues, map[string]string{"class": string(is.Class), "code": is.Code, "field": is.Field})
	}
	detail, _ := json.Marshal(map[string]any{"reason": e.ReasonCode, "class": e.QuarantineClass, "issues": issues, "key": keyLabel,
		"source_schema": e.SourceSchema, "attempt": e.AttemptNo})
	kind := map[string]string{StateValidated: "ACCEPTED", StateQuarantined: "QUARANTINED", StateRejected: "REJECTED"}[e.State]
	ev := Event{Kind: kind, To: e.State, Detail: detail, Actor: "system:xic-inbound-gateway", At: e.ReceivedAt}
	base := map[string]any{"exchange_id": e.ExchangeID, "connection_id": e.ConnectionID, "tenant_id": e.TenantID, "operation_id": e.OperationID}
	var out []OutEvent
	switch e.State {
	case StateValidated:
		data := cp(base)
		data["source_ref_hash"], data["source_schema_version"], data["mapping_version_id"] = e.SourceRefHash, e.SourceSchema, e.MappingVersionID
		out = append(out, OutEvent{Type: "integration.inbound.accepted", AggregateType: "inbound-exchange", AggregateID: e.ExchangeID, ActorID: "system:xic-inbound-gateway", CorrelationID: e.CorrelationID, Data: data})
	case StateQuarantined:
		data := cp(base)
		data["reason_code"], data["schema_version"], data["remediation_class"] = e.ReasonCode, e.SourceSchema, e.QuarantineClass
		out = append(out, OutEvent{Type: "integration.inbound.quarantined", AggregateType: "inbound-exchange", AggregateID: e.ExchangeID, ActorID: "system:xic-inbound-gateway", CorrelationID: e.CorrelationID, Data: data})
	}
	return ev, out
}

func cp(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+4)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Service) outcomeFor(e *Exchange, d decision) *Outcome {
	switch e.State {
	case StateValidated:
		s.observe("accepted", "validated")
		return &Outcome{Status: http.StatusAccepted, Exchange: e, Reason: "validated"}
	default:
		s.observe("quarantined", e.ReasonCode)
		o := &Outcome{Status: d.Status, Exchange: e, Reason: e.ReasonCode, Detail: d.Detail}
		if d.XIC != "" {
			o.XIC = integration.NewXIC(d.XIC, d.Detail)
		}
		return o
	}
}

// reject records an authenticated delivery that was refused before it could be evaluated, and answers it.
func (s *Service) reject(ctx context.Context, e *Exchange, reason string, xic integration.XICCode, status int, detail, keyLabel string) *Outcome {
	e.State, e.ReasonCode, e.XICCode = StateRejected, reason, string(xic)
	d, _ := json.Marshal(map[string]any{"reason": reason, "key": keyLabel})
	ev := Event{Kind: "REJECTED", To: StateRejected, Detail: d, Actor: "system:xic-inbound-gateway", At: e.ReceivedAt}
	data := map[string]any{"exchange_id": e.ExchangeID, "connection_id": e.ConnectionID, "tenant_id": e.TenantID, "reason_code": reason}
	out := []OutEvent{{Type: "integration.inbound.rejected", AggregateType: "inbound-exchange", AggregateID: e.ExchangeID, ActorID: "system:xic-inbound-gateway", CorrelationID: e.CorrelationID, Data: data}}
	e.DedupeKey, e.Raw, e.Canonical = "", nil, nil
	if _, err := s.Repo.Insert(ctx, e, ev, out); err != nil {
		s.observe("refused", "storage_unavailable")
		return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "storage_unavailable", Detail: "temporarily unavailable"}
	}
	s.observe("rejected", reason)
	o := &Outcome{Status: status, Exchange: e, Reason: reason, Detail: detail}
	if xic != "" {
		o.XIC = integration.NewXIC(xic, detail)
	}
	return o
}

// duplicate answers a delivery whose dedupe key an earlier exchange holds. The same body is a repeat and returns
// the original; a different body under the same key is never "the same event" (NP-07).
func (s *Service) duplicate(ctx context.Context, e *Exchange, original *Exchange, idem *registry.IdempotencyInfo, keyLabel string) *Outcome {
	if original == nil {
		return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "storage_unavailable", Detail: "temporarily unavailable"}
	}
	basis := "idempotency-key"
	if e.BodyHash == original.BodyHash {
		dup := *e
		dup.State, dup.DedupeKey, dup.OriginalExchangeID = StateDuplicate, "", original.ExchangeID
		dup.DomainOutcome, dup.Raw, dup.Canonical = DomainNA, nil, nil
		dup.ReasonCode = "DUPLICATE"
		d, _ := json.Marshal(map[string]any{"original": original.ExchangeID, "basis": basis, "key": keyLabel})
		ev := Event{Kind: "DUPLICATE", To: StateDuplicate, Detail: d, Actor: "system:xic-inbound-gateway", At: e.ReceivedAt}
		out := []OutEvent{{Type: "integration.inbound.duplicate", AggregateType: "inbound-exchange", AggregateID: dup.ExchangeID, ActorID: "system:xic-inbound-gateway", CorrelationID: e.CorrelationID,
			Data: map[string]any{"exchange_id": dup.ExchangeID, "original_exchange_id": original.ExchangeID, "dedupe_basis": basis, "connection_id": e.ConnectionID, "tenant_id": e.TenantID}}}
		if err := s.Repo.InsertDuplicate(ctx, &dup, ev, out); err != nil {
			return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "storage_unavailable", Detail: "temporarily unavailable"}
		}
		s.observe("duplicate", "same_body")
		// the original reference, never a second effect (XIC-006)
		return &Outcome{Status: http.StatusOK, Exchange: original, Duplicate: true, Reason: "DUPLICATE"}
	}
	// same key, changed body
	e.DedupeKey, e.Raw, e.Canonical = "", nil, nil
	if idem != nil && idem.OnBodyChange == "QUARANTINE" {
		e.State, e.QuarantineClass, e.ReasonCode, e.XICCode = StateQuarantined, ClassIdempotency, "BODY_CHANGED_UNDER_SAME_KEY", string(integration.XICDuplicateExchange)
		d, _ := json.Marshal(map[string]any{"original": original.ExchangeID, "reason": e.ReasonCode})
		ev := Event{Kind: "QUARANTINED", To: StateQuarantined, Detail: d, Actor: "system:xic-inbound-gateway", At: e.ReceivedAt}
		out := []OutEvent{{Type: "integration.inbound.quarantined", AggregateType: "inbound-exchange", AggregateID: e.ExchangeID, ActorID: "system:xic-inbound-gateway", CorrelationID: e.CorrelationID,
			Data: map[string]any{"exchange_id": e.ExchangeID, "connection_id": e.ConnectionID, "tenant_id": e.TenantID, "reason_code": e.ReasonCode, "remediation_class": ClassIdempotency, "schema_version": e.SourceSchema}}}
		if _, err := s.Repo.Insert(ctx, e, ev, out); err != nil {
			return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: "storage_unavailable", Detail: "temporarily unavailable"}
		}
		s.observe("quarantined", e.ReasonCode)
		return &Outcome{Status: http.StatusConflict, Exchange: e, Reason: e.ReasonCode, Detail: "the key was used with a different body",
			XIC: integration.NewXIC(integration.XICDuplicateExchange, "the key was used with a different body")}
	}
	o := s.reject(ctx, e, "BODY_CHANGED_UNDER_SAME_KEY", integration.XICDuplicateExchange, http.StatusConflict, "the key was used with a different body", keyLabel)
	return o
}

// ── rate limiting ────────────────────────────────────────────────────────────

type bucket struct {
	tokens float64
	at     time.Time
}

type limiter struct {
	m map[string]*bucket
}

const maxBuckets = 20000

func (s *Service) allow(key string, perMin int, now time.Time) bool {
	if perMin <= 0 {
		return true
	}
	s.limMu.Lock()
	defer s.limMu.Unlock()
	if s.lim == nil {
		s.lim = &limiter{m: map[string]*bucket{}}
	}
	b, ok := s.lim.m[key]
	if !ok {
		if len(s.lim.m) >= maxBuckets { // bounded: drop the idle ones rather than grow without limit
			for k, v := range s.lim.m {
				if now.Sub(v.at) > time.Minute {
					delete(s.lim.m, k)
				}
			}
			if len(s.lim.m) >= maxBuckets {
				return false // fail closed under a flood of distinct keys
			}
		}
		b = &bucket{tokens: float64(perMin), at: now}
		s.lim.m[key] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * float64(perMin) / 60
	if b.tokens > float64(perMin) {
		b.tokens = float64(perMin)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

var _ = fmt.Sprintf

func newID() string { return uuid.NewString() }
