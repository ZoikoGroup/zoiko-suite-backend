package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/integration"
)

// Provider callbacks (ZS-SVC-T-001 §9.3). A callback is evidence about an attempt, never an instruction: it is
// authenticated with a credential of its own, the tenant is the connection's (never the payload's), the shape is the
// contract's exact callback schema, a repeat is acknowledged without repeating its effect, and the only transitions
// it can drive are the ones the contract's callback profile lists. A callback that contradicts what is already
// recorded is preserved as evidence and raised for reconciliation, never written over history.

// CallbackRequest is one callback as the HTTP layer hands it over. Body is already bounded by the read cap.
type CallbackRequest struct {
	ConnectorKey  string
	ConnectionID  string
	OperationID   string
	Method        string
	Header        http.Header
	Body          []byte
	RemoteAddr    string
	CorrelationID string
}

// CallbackOutcome is what the endpoint answers. Nothing here ever contains a payload value.
type CallbackOutcome struct {
	Status     int
	Receipt    *Receipt
	XIC        *integration.XICError
	Duplicate  bool
	RetryAfter int
	Detail     string
	Reason     string
}

var (
	connectorKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{2,62}$`)
	operationIDRE  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,62}$`)
)

// cbSecrets adapts the engine's credential source to the shared authentication routine, which speaks in its own
// errors: an outage is retryable, anything else is a failed authentication.
type cbSecrets struct{ s Secrets }

func (c cbSecrets) Secret(ctx context.Context, tenant, ref string) ([]byte, error) {
	b, err := c.s.Secret(ctx, tenant, ref)
	switch {
	case err == nil:
		return b, nil
	case errors.Is(err, ErrSecretUnavailable):
		return nil, inbound.ErrSecretUnavailable
	}
	return nil, inbound.ErrAuthFailed
}

func (s *Service) authFailure(reason string) *CallbackOutcome {
	s.observe("callback_refused", reason)
	return &CallbackOutcome{Status: http.StatusUnauthorized, XIC: integration.NewXIC(integration.XICAuthenticationFailed, "authentication failed"), Reason: reason}
}

func (s *Service) unavailable(reason string) *CallbackOutcome {
	s.observe("callback_refused", reason)
	return &CallbackOutcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: reason, Detail: "temporarily unavailable"}
}

// mapStatus reads a provider's status word. Anything outside the vocabulary maps to "": a callback is never applied
// from words the engine does not know.
func mapStatus(text string) string {
	t := strings.ToUpper(strings.TrimSpace(text))
	switch {
	case settledWords[t]:
		return StateSettled
	case failedWords[t]:
		return StateFailed
	case pendingWords[t]:
		return StatePending
	}
	return ""
}

// ReceiveCallback runs the pipeline for one provider callback.
func (s *Service) ReceiveCallback(ctx context.Context, req CallbackRequest) *CallbackOutcome {
	now := s.now()
	cfg := s.cfg()
	ipKey := hostOf(req.RemoteAddr)

	// 1. the envelope, before anything is parsed or any secret is fetched
	if !s.allow("cbconn:"+req.ConnectionID, cfg.CallbackConnRatePerMin, now) || !s.allow("cbip:"+ipKey, cfg.CallbackIPRatePerMin, now) {
		s.observe("callback_refused", "rate_limited")
		return &CallbackOutcome{Status: http.StatusTooManyRequests, RetryAfter: 30, Reason: "rate_limited", XIC: integration.NewXIC(integration.XICRateLimited, "too many requests")}
	}
	if int64(len(req.Body)) > cfg.CallbackMaxBodyBytes {
		s.observe("callback_refused", "too_large")
		return &CallbackOutcome{Status: http.StatusRequestEntityTooLarge, Reason: "too_large", Detail: "the request body is larger than this endpoint accepts"}
	}

	// 2. the connector operation and the candidate connection come from the trusted route
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
		return s.unavailable("dependency")
	}
	if conn.ConnectorID != req.ConnectorKey {
		return s.authFailure("connector_mismatch")
	}
	v, err := s.Conns.PinnedContract(ctx, conn)
	if err != nil {
		if errors.Is(err, xicconn.ErrContractUnavailable) {
			return s.authFailure("contract_unavailable")
		}
		return s.unavailable("dependency")
	}
	op := v.Operation(req.OperationID)
	if op == nil || op.Direction != registry.Outbound || !strings.EqualFold(req.Method, http.MethodPost) {
		return s.authFailure("unknown_operation")
	}
	cb := v.CallbackFor(op)
	if cb == nil {
		return s.authFailure("no_callback_profile")
	}

	// 3. authenticate the provider with the credential the callback profile names, independently of anything else
	info, ok := v.UsedAuthProfiles()[cb.AuthProfile]
	if !ok || (cb.Mechanism != "HMAC" && cb.Mechanism != "MTLS") || info.Mechanism != cb.Mechanism {
		return s.authFailure("mechanism_unsupported")
	}
	info.ReplayWindowSeconds = cb.ReplayWindowSeconds
	if cb.TimestampHeader != "" {
		info.TimestampHeader = cb.TimestampHeader
	}
	if cb.SignatureAlgorithm != "" {
		info.SignatureAlgorithm = cb.SignatureAlgorithm
	}
	res, aerr := inbound.Authenticate(ctx, cbSecrets{s.Secrets}, s.EdgeSecret, conn, info, req.Header, req.Body, now)
	base := &Receipt{ReceiptID: uuid.NewString(), TenantID: conn.TenantID, ConnectionID: conn.ConnectionID, ConnectorID: conn.ConnectorID, OperationID: op.ID,
		ContractVersionID: v.ContractVersionID, AuthResult: AuthOK, AuthMechanism: res.Mechanism, KeyLabel: res.KeyLabel, BodyHash: hashBytes(req.Body), BodySize: len(req.Body),
		SourceNetHash: hashBytes([]byte("net\x00" + ipKey))[:32], CorrelationID: req.CorrelationID, ReceivedAt: now}
	ictx := withTenant(ctx, conn.TenantID)
	switch {
	case errors.Is(aerr, inbound.ErrSecretUnavailable):
		return s.unavailable("secret_unavailable")
	case errors.Is(aerr, inbound.ErrAuthUnsupported):
		return s.authFailure("mechanism_unsupported")
	case errors.Is(aerr, inbound.ErrAuthFailed):
		// the sender is not authenticated: the answer is the one uniform 401, and only capped, body-free metadata is kept
		base.AuthResult, base.AuthMechanism, base.KeyLabel = AuthFailed, info.Mechanism, ""
		s.recordRejected(ictx, base, CBAuthFailed, integration.XICAuthenticationFailed, 0, true)
		return s.authFailure("bad_credential")
	case errors.Is(aerr, inbound.ErrReplayWindow):
		base.AuthResult = AuthReplay
		return s.reject(ictx, base, CBReplayWindow, integration.XICReplayWindowFailed, http.StatusUnauthorized, "the callback is outside the replay window", true)
	}
	// From here on the provider is authenticated, so what happens is durable and evidenced.

	// 4. the connection must still be live, and the shape exact
	if !conn.Live() {
		return s.reject(ictx, base, CBConnectionInactive, integration.XICConnectionNotActive, http.StatusConflict, "the connection is not active", false)
	}
	if enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))); enc != "" && enc != "identity" {
		return s.reject(ictx, base, CBEncoding, integration.XICSchemaVersionUnsupported, http.StatusUnsupportedMediaType, "compressed callbacks are not accepted", false)
	}
	sch := v.SchemaByRef(cb.Schema)
	if sch == nil || sch.MediaType != "application/json" || mediaOf(req.Header.Get("Content-Type")) != "application/json" {
		return s.reject(ictx, base, CBMediaType, integration.XICSchemaVersionUnsupported, http.StatusUnsupportedMediaType, "the media type is not supported for this callback", false)
	}
	if hv := strings.TrimSpace(req.Header.Get(registry.DefaultSchemaVersionHeader)); hv != "" && hv != cb.Schema {
		return s.reject(ictx, base, CBSchemaVersion, integration.XICSchemaVersionUnsupported, http.StatusUnprocessableEntity, "the callback schema version is not supported", false)
	}
	if int64(len(req.Body)) > sch.MaxSizeBytes {
		return s.reject(ictx, base, CBSchemaViolation, "", http.StatusRequestEntityTooLarge, "the callback exceeds the schema's size limit", false)
	}
	compiled, cerr := adapter.CompileSchema(sch.ValidationProfile)
	if cerr != nil || compiled.ValidateDocument(req.Body) != nil {
		return s.reject(ictx, base, CBSchemaViolation, "", http.StatusUnprocessableEntity, "the callback does not satisfy the contract's schema", false)
	}
	var doc any
	_ = json.Unmarshal(req.Body, &doc)

	// 5. the provider's claims about whose callback this is are corroboration; a disagreement is a security event (NP-26)
	if cb.TenantPointer != "" {
		if got := pointerString(doc, cb.TenantPointer); got != "" && got != conn.TenantID {
			return s.claimMismatch(ictx, base, "tenant")
		}
	}
	if cb.AccountPointer != "" {
		if got := pointerString(doc, cb.AccountPointer); got != "" && xicconn.HashAccountRef(conn.TenantID, conn.ConnectorID, got) != conn.ProviderAccountHash {
			return s.claimMismatch(ictx, base, "account")
		}
	}

	// 6. the dedupe key comes from the contract's callback profile; without it a repeat cannot be recognised
	val := callbackDedupeValue(cb.DedupeKeySource, req.Header, doc)
	if val == "" || len(val) > 512 {
		return s.reject(ictx, base, CBDedupeMissing, integration.XICCallbackInvalid, http.StatusUnprocessableEntity, "the callback does not carry the event identifier its contract requires", false)
	}
	dedupe := hashBytes([]byte("cb\x00" + conn.ConnectionID + "\x00" + op.ID + "\x00" + val))
	if prior, gerr := s.Repo.GetReceiptByDedupe(ictx, conn.TenantID, dedupe); gerr == nil {
		return s.duplicate(ictx, base, prior)
	} else if !errors.Is(gerr, ErrReceiptNotFound) {
		return s.unavailable("storage_unavailable")
	}

	// 7. what the callback says, in the contract's own pointers and vocabulary
	ref := pointerString(doc, cb.ReferencePointer)
	statusText := pointerString(doc, cb.StatusPointer)
	clientRef := ""
	if cb.ClientRefPointer != "" {
		clientRef = pointerString(doc, cb.ClientRefPointer)
	}
	base.ProviderReference, base.ReportedStatus = safeRef(ref), safeWord(statusText)
	target := mapStatus(statusText)
	if target == "" {
		return s.reject(ictx, base, CBStatusUnknown, integration.XICCallbackInvalid, http.StatusUnprocessableEntity, "the callback reports a status this contract does not define", false)
	}
	base.TargetState = target
	if ref == "" && clientRef == "" {
		return s.reject(ictx, base, CBReferenceMissing, integration.XICCallbackInvalid, http.StatusUnprocessableEntity, "the callback does not identify an attempt", false)
	}

	// 8. correlate the ORIGINAL attempt: by the provider's reference, else by the request reference it echoes
	a, ferr := s.Repo.FindAttempt(ictx, conn.ConnectionID, op.ID, safeRef(ref), clientRef)
	switch {
	case errors.Is(ferr, ErrNotFound):
		// nothing to apply it to (yet): a callback that beats the submit response and carries no echo of the request
		// reference is asked to come again, not guessed onto an attempt (NP-24)
		out := s.reject(ictx, base, CBUncorrelated, integration.XICCallbackInvalid, http.StatusConflict, "the callback does not match an attempt of this connection yet", false)
		out.RetryAfter = 30
		return out
	case ferr != nil:
		return s.unavailable("storage_unavailable")
	}
	base.AttemptID = a.AttemptID
	if ref != "" && a.ProviderReference != "" && a.ProviderReference != safeRef(ref) {
		return s.reject(ictx, base, CBReferenceMismatch, integration.XICCallbackInvalid, http.StatusConflict, "the callback's reference is not the attempt's", false)
	}

	// 9. apply it - or record why not - in one transaction with its receipt
	hasQuery := op.UnknownResolution.StatusQueryOperation != ""
	var final *CallbackOutcome
	_, merr := s.Repo.Mutate(ictx, a.AttemptID, func(tx *Tx) error {
		final = s.decide(tx, base, cb, target, ref, hasQuery, dedupe, now)
		return nil
	})
	switch {
	case errors.Is(merr, ErrDuplicate):
		prior, gerr := s.Repo.GetReceiptByDedupe(ictx, conn.TenantID, dedupe)
		if gerr != nil {
			return s.unavailable("storage_unavailable")
		}
		return s.duplicate(ictx, base, prior)
	case merr != nil:
		return s.unavailable("storage_unavailable")
	}
	s.observe("callback_"+strings.ToLower(final.Receipt.State), final.Receipt.ReasonCode)
	return final
}

// decide is the callback's decision about an attempt, taken inside the attempt's own transaction so it is made on
// the state the attempt really has. It either applies a transition the contract's profile allows, or records why not.
func (s *Service) decide(tx *Tx, base *Receipt, cb *registry.CallbackInfo, target, ref string, hasQuery bool, dedupe string, now time.Time) *CallbackOutcome {
	rc := *base
	cur := tx.A
	rc.AttemptStateBefore = cur.State
	note := func(kind string, detail map[string]any) {
		detail["receipt_id"] = rc.ReceiptID
		tx.Event = &Event{Kind: kind, From: cur.State, To: cur.State, Actor: "system:xic-callback", At: now, Detail: mustJSON(detail)}
	}
	invalid := func(reason string) {
		tx.Out = append(tx.Out, s.invalidEvent(&rc, reason))
	}
	rejected := func(reason string, xic integration.XICCode, status int, detail string) *CallbackOutcome {
		rc.State, rc.ReasonCode, rc.XICCode, rc.AttemptStateAfter = ReceiptRejected, reason, string(xic), cur.State
		tx.NoUpdate, tx.Receipt = true, &rc
		note("CALLBACK_REJECTED", map[string]any{"reason": reason, "reported_status": rc.ReportedStatus})
		invalid(reason)
		return &CallbackOutcome{Status: status, Receipt: &rc, XIC: integration.NewXIC(xic, detail), Reason: reason}
	}
	recorded := func() *CallbackOutcome {
		rc.State, rc.ReasonCode, rc.AttemptStateAfter = ReceiptDuplicate, CBAlreadyRecorded, cur.State
		tx.NoUpdate, tx.Receipt = true, &rc
		note("CALLBACK_CONFIRMED", map[string]any{"reported_status": rc.ReportedStatus})
		tx.Out = append(tx.Out, s.duplicateEvent(&rc))
		return &CallbackOutcome{Status: http.StatusOK, Receipt: &rc, Duplicate: true, Reason: CBAlreadyRecorded}
	}

	if !cur.Open() {
		switch {
		case cur.State == target:
			return recorded() // a late confirmation of what is already known
		case target == StatePending && (cur.State == StateSettled || cur.State == StateFailed):
			return recorded() // a late "still processing" after the outcome: superseded, not a contradiction
		}
		// a late success after a recorded failure (or the reverse, or anything about a cancelled attempt): history is
		// not rewritten; the inconsistency is raised for reconciliation (NP-25)
		rc.NeedsReconciliation = true
		out := rejected(CBLateContradicts, integration.XICCallbackInvalid, http.StatusConflict, "the callback contradicts the outcome already recorded")
		tx.Out = append(tx.Out, OutEvent{Type: "integration.callback.inconsistent", AggregateType: "outbound-attempt", AggregateID: cur.AttemptID, ActorID: "system:xic-callback",
			CorrelationID: rc.CorrelationID, Data: map[string]any{"callback_receipt_id": rc.ReceiptID, "attempt_id": cur.AttemptID, "tenant_id": rc.TenantID, "connection_id": rc.ConnectionID,
				"recorded_state": cur.State, "reported_status": rc.ReportedStatus}})
		return out
	}
	if cur.State == target {
		return recorded() // already PENDING and told PENDING again
	}
	if !containsFold(cb.Transitions[cur.State], target) {
		return rejected(CBTransition, integration.XICCallbackInvalid, http.StatusConflict, "the callback asks for a transition the contract does not allow")
	}

	// apply: the same definitions of PENDING / SETTLED / FAILED the submit path and the status query use
	switch target {
	case StateSettled:
		s.markFinal(tx, StateSettled, MethodCallback, "", 0, safeRef(ref), "", "", now)
	case StateFailed:
		s.markFinal(tx, StateFailed, MethodCallback, "CALLBACK_REPORTED_FAILED", 0, safeRef(ref), "", "", now)
	default:
		s.markPending(tx, cur.State, 0, safeRef(ref), hasQuery, now, "system:xic-callback", "CALLBACK_PENDING")
	}
	rc.State, rc.AttemptStateAfter, rc.DedupeKey = ReceiptApplied, tx.A.State, dedupe
	tx.Receipt = &rc
	tx.Out = append(tx.Out, OutEvent{Type: "integration.callback.applied", AggregateType: "outbound-attempt", AggregateID: cur.AttemptID, ActorID: "system:xic-callback",
		CorrelationID: rc.CorrelationID, Data: map[string]any{"callback_receipt_id": rc.ReceiptID, "attempt_id": cur.AttemptID, "tenant_id": rc.TenantID,
			"connection_id": rc.ConnectionID, "from_state": rc.AttemptStateBefore, "to_state": rc.AttemptStateAfter}})
	return &CallbackOutcome{Status: http.StatusOK, Receipt: &rc}
}

func (s *Service) invalidEvent(r *Receipt, reason string) OutEvent {
	return OutEvent{Type: "integration.callback.invalid", AggregateType: "callback-receipt", AggregateID: r.ReceiptID, ActorID: "system:xic-callback", CorrelationID: r.CorrelationID,
		Data: map[string]any{"callback_receipt_id": r.ReceiptID, "connection_id": r.ConnectionID, "tenant_id": r.TenantID, "failure_reason": reason,
			"source_network_meta_ref": r.SourceNetHash}}
}

func (s *Service) duplicateEvent(r *Receipt) OutEvent {
	return OutEvent{Type: "integration.callback.duplicate", AggregateType: "callback-receipt", AggregateID: r.ReceiptID, ActorID: "system:xic-callback", CorrelationID: r.CorrelationID,
		Data: map[string]any{"callback_receipt_id": r.ReceiptID, "original_receipt_id": r.OriginalReceiptID, "connection_id": r.ConnectionID, "tenant_id": r.TenantID, "attempt_id": r.AttemptID}}
}

// reject records a rejected callback (the provider is authenticated) and answers with the reason.
func (s *Service) reject(ctx context.Context, r *Receipt, reason string, xic integration.XICCode, status int, detail string, capped bool) *CallbackOutcome {
	if !s.recordRejected(ctx, r, reason, xic, status, capped) {
		return s.unavailable("storage_unavailable")
	}
	s.observe("callback_rejected", reason)
	o := &CallbackOutcome{Status: status, Receipt: r, Reason: reason, Detail: detail}
	if xic != "" {
		o.XIC = integration.NewXIC(xic, detail)
	}
	return o
}

// recordRejected persists a rejected receipt and its integration.callback.invalid event. With capped, evidence is
// kept only within the per-connection budget: an unauthenticated sender cannot make this service store without
// limit, and what is dropped is counted instead.
func (s *Service) recordRejected(ctx context.Context, r *Receipt, reason string, xic integration.XICCode, _ int, capped bool) bool {
	r.State, r.ReasonCode, r.XICCode = ReceiptRejected, reason, string(xic)
	if capped && !s.allow("cbinv:"+r.ConnectionID, s.cfg().CallbackInvalidPerMin, r.ReceivedAt) {
		s.observe("callback_evidence_dropped", reason)
		return true
	}
	_, err := s.Repo.InsertReceipt(ctx, r, []OutEvent{s.invalidEvent(r, reason)})
	return err == nil
}

func (s *Service) claimMismatch(ctx context.Context, r *Receipt, what string) *CallbackOutcome {
	_ = s.Repo.RecordSignal(ctx, r.TenantID, OutEvent{Type: "integration.security.callback-claim-mismatch", AggregateType: "callback-receipt", AggregateID: r.ReceiptID,
		ActorID: "system:xic-callback", CorrelationID: r.CorrelationID, Data: map[string]any{"tenant_id": r.TenantID, "connection_id": r.ConnectionID, "callback_receipt_id": r.ReceiptID,
			"claim": what, "reason_code": "XIC-008"}})
	return s.reject(ctx, r, CBClaimMismatch, integration.XICTenantScopeMismatch, http.StatusForbidden, "the callback names a different "+what+" than the authenticated connection", false)
}

// duplicate answers a repeat of a callback already decided: deterministically, with the original receipt, and
// without repeating any effect (NP-23). The same event id with a different body is a conflict, not a repeat.
func (s *Service) duplicate(ctx context.Context, r *Receipt, prior *Receipt) *CallbackOutcome {
	if prior.BodyHash != r.BodyHash {
		return s.reject(ctx, r, CBBodyChanged, integration.XICDuplicateExchange, http.StatusConflict, "the event identifier was used with a different body", false)
	}
	r.State, r.ReasonCode, r.OriginalReceiptID, r.AttemptID, r.DedupeKey = ReceiptDuplicate, CBDuplicateEvent, prior.ReceiptID, prior.AttemptID, ""
	r.ProviderReference, r.ReportedStatus, r.TargetState = prior.ProviderReference, prior.ReportedStatus, prior.TargetState
	if _, err := s.Repo.InsertReceipt(ctx, r, []OutEvent{s.duplicateEvent(r)}); err != nil {
		return s.unavailable("storage_unavailable")
	}
	s.observe("callback_duplicate", "event")
	return &CallbackOutcome{Status: http.StatusOK, Receipt: prior, Duplicate: true, Reason: CBDuplicateEvent}
}

// ── helpers ──────────────────────────────────────────────────────────────

func hostOf(remote string) string {
	if i := strings.LastIndex(remote, ":"); i > 0 && !strings.Contains(remote[i:], "]") {
		return remote[:i]
	}
	return remote
}

func mediaOf(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// pointerString resolves an RFC 6901 JSON pointer to a string (numbers are rendered as their JSON text).
func pointerString(doc any, ptr string) string {
	if ptr == "" || ptr[0] != '/' {
		return ""
	}
	cur := doc
	for _, raw := range strings.Split(ptr[1:], "/") {
		tok := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		if cur, ok = m[tok]; !ok {
			return ""
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		b, _ := json.Marshal(v)
		return string(b)
	}
	return ""
}

func callbackDedupeValue(source string, h http.Header, doc any) string {
	switch {
	case strings.HasPrefix(source, "header:"):
		return strings.TrimSpace(h.Get(strings.TrimPrefix(source, "header:")))
	case strings.HasPrefix(source, "body:"):
		return pointerString(doc, strings.TrimPrefix(source, "body:"))
	case source == "provider_event_id":
		if v := strings.TrimSpace(h.Get("X-Event-Id")); v != "" {
			return v
		}
		return pointerString(doc, "/event_id")
	}
	return ""
}

// safeRef and safeWord bound what a callback may put into a receipt: a reference or a status word that is not plain
// is not stored, so a receipt never carries free text from the sender.
func safeRef(v string) string {
	if v != "" && len(v) <= 128 && domainRefRE.MatchString(v) {
		return v
	}
	return ""
}

var wordRE = regexp.MustCompile(`^[A-Za-z][A-Za-z_ -]{0,38}$`)

func safeWord(v string) string {
	if wordRE.MatchString(strings.TrimSpace(v)) {
		return strings.ToUpper(strings.TrimSpace(v))
	}
	return ""
}

// ── rate limiting (a small token bucket per key) ───────────────────────────

type bucket struct {
	tokens float64
	at     time.Time
}

type limiter struct{ m map[string]*bucket }

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
		if len(s.lim.m) >= maxBuckets {
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
