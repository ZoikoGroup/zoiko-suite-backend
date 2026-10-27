package ar05

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// callbackBody is the SIGNED payload of a provider callback.
type callbackBody struct {
	ProviderEventID   string `json:"provider_event_id"`
	ExternalAttemptID string `json:"external_attempt_id"`
	DocumentID        string `json:"document_id"`
	Status            string `json:"status"` // ACCEPTED | REJECTED | PROCESSING
	ProviderReference string `json:"provider_reference,omitempty"`
	ReceiptID         string `json:"receipt_id,omitempty"`
	ReasonCode        string `json:"reason_code,omitempty"`
	Message           string `json:"message,omitempty"`
	OccurredAt        string `json:"occurred_at"`
}

// errCallbackMismatch / errCallbackReplayed are in-transaction sentinels turned into security events after rollback.
var (
	errCallbackMismatch = errors.New("callback does not match the attempt")
	errCallbackReplayed = errors.New("callback nonce was already used")
)

// recordProviderCallback is the HTTP entry for provider callbacks (spec command RecordProviderCallback).
//
// It is NOT behind Guard: the provider has no principal and sends no tenant. Authentication is the HMAC signature alone
// (callback.go). Tenant, entity and submission come from the attempt the signed body names, resolved through
// ar05_attempt_index. A forged / unverifiable / replayed / mismatching callback is refused with a stable code, recorded
// in ar05_security_events (and in the tenant's audit trail when the attempt is known) and changes NOTHING else.
func (m *Module) recordProviderCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, kit.MaxBodyBytes))
	if err != nil {
		kit.WriteError(w, http.StatusRequestEntityTooLarge, kit.CodeValidationFailed, "request body too large")
		return
	}
	provider := r.Header.Get(HeaderProvider)
	sig, ts, nonce := r.Header.Get(HeaderSignature), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderNonce)
	bodyDigest := Sha256Hex(raw)
	deny := func(status int, code, reason string, claimed *callbackBody, route *attemptRoute, detail map[string]any) {
		if route == nil && claimed != nil && claimed.ExternalAttemptID != "" && m.deps.DB != nil {
			// Label the refusal with the tenant the (untrusted) body points at, so the tenant's own audit trail shows the attack.
			route, _ = routeAttempt(ctx, m.deps.DB, claimed.ExternalAttemptID)
		}
		m.securityEvent(r, reason, provider, claimed, route, bodyDigest, sig != "", detail)
		kit.WriteError(w, status, code, "the callback was refused")
	}
	claimed := func() *callbackBody { // UNTRUSTED: used only to label the security event
		var b callbackBody
		_ = json.Unmarshal(raw, &b)
		return &b
	}

	if !providerRe.MatchString(provider) {
		deny(http.StatusUnauthorized, CodeCallbackUnauthenticated, "unknown_provider", claimed(), nil, nil)
		return
	}
	secret, ok := m.secrets.Secret(provider)
	if !ok {
		deny(http.StatusUnauthorized, CodeCallbackUnauthenticated, "no_secret_for_provider", claimed(), nil, nil)
		return
	}
	signedAt, verr := VerifyCallback(secret, sig, ts, nonce, raw, m.now(), m.CallbackWindow)
	switch {
	case verr == nil:
	case errors.Is(verr, ErrCallbackStale):
		deny(http.StatusUnauthorized, CodeCallbackStale, "stale_timestamp", claimed(), nil, nil)
		return
	default:
		deny(http.StatusUnauthorized, CodeCallbackUnauthenticated, "bad_signature", claimed(), nil, map[string]any{"cause": verr.Error()})
		return
	}

	// Authentic from here on.
	var b callbackBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "the callback body is not valid: "+err.Error())
		return
	}
	occurred, terr := time.Parse(time.RFC3339Nano, b.OccurredAt)
	switch {
	case trimmed(b.ProviderEventID) == "" || len(b.ProviderEventID) > 128:
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "provider_event_id is required (max 128 characters)")
		return
	case mustUUID("external_attempt_id", b.ExternalAttemptID) != nil || mustUUID("document_id", b.DocumentID) != nil:
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "external_attempt_id and document_id must be UUIDs")
		return
	case b.Status != "ACCEPTED" && b.Status != "REJECTED" && b.Status != "PROCESSING":
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "status must be ACCEPTED, REJECTED or PROCESSING")
		return
	case terr != nil:
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "occurred_at must be an RFC3339 instant")
		return
	case len(b.ProviderReference) > 255 || len(b.ReceiptID) > 256 || len(b.ReasonCode) > 128 || len(b.Message) > maxText:
		kit.WriteError(w, http.StatusBadRequest, kit.CodeValidationFailed, "provider_reference (255), receipt_id (256), reason_code (128) or message is too long")
		return
	}
	route, err := routeAttempt(ctx, m.deps.DB, b.ExternalAttemptID)
	if err != nil {
		m.deps.Log.Error("ar05 callback routing failed", zap.Error(err))
		kit.WriteError(w, http.StatusServiceUnavailable, kit.CodeDependencyUnavailable, "")
		return
	}
	if route == nil {
		deny(http.StatusNotFound, kit.CodeNotFound, "unknown_attempt", &b, nil, nil)
		return
	}
	if route.Provider != provider {
		deny(http.StatusForbidden, CodeCallbackMismatch, "provider_mismatch", &b, route, map[string]any{"attempt_provider": route.Provider})
		return
	}

	var result struct {
		duplicate   bool
		disposition string
		state       State
		sub         *Submission
		applied     bool
	}
	c := kit.Caller{TenantID: route.TenantID, PrincipalID: "provider:" + provider, CorrelationID: r.Header.Get("X-Correlation-ID"), LegalEntityID: route.LegalEntityID}
	if c.CorrelationID == "" {
		c.CorrelationID = b.ProviderEventID
	}
	err = m.deps.DB.WithTx(ctx, route.TenantID, func(tx pgx.Tx) error {
		cur, err := getSubmission(ctx, tx, route.TenantID, route.SubmissionID, true)
		if err != nil {
			return err
		}
		if cur.DocumentID != b.DocumentID {
			return errCallbackMismatch
		}
		att, err := getAttempt(ctx, tx, route.TenantID, b.ExternalAttemptID)
		if err != nil {
			return err
		}
		if att.SubmissionID != cur.SubmissionID {
			return errCallbackMismatch
		}
		if ex, err := findAck(ctx, tx, route.TenantID, b.ExternalAttemptID, b.ProviderEventID); err != nil {
			return err
		} else if ex != nil {
			// Duplicate delivery of an event already recorded: idempotent, nothing changes.
			result.duplicate, result.disposition, result.state, result.sub = true, ex.Disposition, cur.State, cur
			return nil
		}
		tag, err := tx.Exec(ctx, `INSERT INTO ar05_callback_nonces (provider, nonce) VALUES ($1,$2) ON CONFLICT DO NOTHING`, provider, nonce)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errCallbackReplayed
		}
		now := m.now()
		disp, tran := "NO_CHANGE", Transition("")
		switch {
		case cur.State.Terminal():
			disp = "AFTER_TERMINAL"
		case b.Status == "PROCESSING":
			if cur.State == StateUnknown {
				disp, tran = "APPLIED", TrReceived
			}
		case b.Status == "ACCEPTED":
			switch {
			case trimmed(b.ReceiptID) == "" && trimmed(b.ProviderReference) == "":
				disp = "MISSING_RECEIPT" // no false ACCEPTED without a receipt
			case cur.State == StateUnknown || cur.State == StateSubmitted:
				disp, tran = "APPLIED", TrAccept
			default:
				disp = "ILLEGAL_TRANSITION"
			}
		case b.Status == "REJECTED":
			if cur.State == StateUnknown || cur.State == StateSubmitted {
				disp, tran = "APPLIED", TrReject
			} else {
				disp = "ILLEGAL_TRANSITION"
			}
		}
		to := cur.State
		if tran != "" {
			if to, err = Next(cur.State, tran); err != nil {
				return err
			}
		}
		sigHash := sig[len(sigPrefix):]
		ack := &Ack{SubmissionID: cur.SubmissionID, ExternalAttemptID: b.ExternalAttemptID, Provider: provider, ProviderEventID: b.ProviderEventID, Status: b.Status,
			ReceiptID: trimmed(b.ReceiptID), ProviderReference: trimmed(b.ProviderReference), ReasonCode: trimmed(b.ReasonCode), Message: trimmed(b.Message), OccurredAt: occurred.UTC(),
			ReceivedAt: now, Source: "CALLBACK", SignatureVerified: true, SignatureAlg: SigAlg, SignatureDigest: Sha256Hex([]byte(sigHash)), CallbackNonce: nonce, CallbackTimestamp: &signedAt,
			BodyDigest: bodyDigest, Disposition: disp, StateAfter: string(to), RecordedBy: c.PrincipalID}
		if err := insertAck(ctx, tx, cur, ack); err != nil {
			return err
		}
		result.disposition, result.state, result.sub = disp, to, cur
		if disp != "APPLIED" {
			// Authentic but without effect (late, illegal, no receipt): stored as evidence, nothing else changes.
			return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: cur.SubmissionID, Command: "recordProviderCallback",
				Before: map[string]any{"state": cur.State}, After: map[string]any{"state": cur.State},
				Detail: map[string]any{"disposition": disp, "provider_event_id": b.ProviderEventID, "status": b.Status, "external_attempt_id": b.ExternalAttemptID}})
		}
		outcome := OutReceived
		var events func(*Submission) []kit.Event
		switch b.Status {
		case "ACCEPTED":
			outcome = OutAccepted
			events = func(n *Submission) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: n.SubmissionID, EventType: EventAccepted,
					Payload: evPayload(c, n, map[string]any{"external_attempt_id": b.ExternalAttemptID, "receipt_id": ack.ReceiptID, "provider_reference": ack.ProviderReference, "via": "CALLBACK"})}}
			}
		case "REJECTED":
			outcome = OutRejected
			events = func(n *Submission) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: n.SubmissionID, EventType: EventRejected,
					Payload: evPayload(c, n, map[string]any{"external_attempt_id": b.ExternalAttemptID, "reason_code": ack.ReasonCode, "message": truncate(ack.Message, 500), "via": "CALLBACK"})}}
			}
		default:
			events = func(n *Submission) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: n.SubmissionID, EventType: EventSubmitted,
					Payload: evPayload(c, n, map[string]any{"external_attempt_id": b.ExternalAttemptID, "via": "CALLBACK"})}}
			}
		}
		if err := appendResult(ctx, tx, c, cur, b.ExternalAttemptID, resultWrite{Outcome: outcome, Source: SrcCallback, ProviderRef: ack.ProviderReference, At: now,
			Detail: map[string]any{"provider_event_id": b.ProviderEventID, "attempt_is_latest": att.AttemptNo == cur.AttemptCount}}); err != nil {
			return err
		}
		upd := subUpdate{State: to}
		if ack.ProviderReference != "" {
			ref := ack.ProviderReference
			upd.LastProviderRef = &ref
		}
		n, err := m.commit(ctx, tx, c, cur, change{upd: upd, events: events, h: hist{At: now, Command: "recordProviderCallback",
			Detail: map[string]any{"provider_event_id": b.ProviderEventID, "status": b.Status, "external_attempt_id": b.ExternalAttemptID, "signature_verified": true}}})
		if err != nil {
			return err
		}
		result.sub, result.applied = n, true
		return nil
	})
	switch {
	case errors.Is(err, errCallbackMismatch):
		deny(http.StatusUnprocessableEntity, CodeCallbackMismatch, "document_or_attempt_mismatch", &b, route, map[string]any{"document_id": b.DocumentID})
		return
	case errors.Is(err, errCallbackReplayed):
		deny(http.StatusUnauthorized, CodeCallbackReplayed, "nonce_replayed", &b, route, nil)
		return
	case err != nil:
		m.fail(w, kit.MapPgErr(err))
		return
	}
	if result.applied {
		if m.registry != nil && (result.sub.State == StateAccepted || result.sub.State == StateRejected) {
			m.pushRegistry(context.WithoutCancel(r.Context()), c, result.sub, string(result.sub.State), pickStr(b.ReceiptID, b.ProviderReference), "cb:"+b.ProviderEventID)
		}
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"received": true, "duplicate": result.duplicate, "applied": result.applied, "disposition": result.disposition,
		"submission_id": result.sub.SubmissionID, "state": result.state})
}

func pickStr(a, b string) string {
	if trimmed(a) != "" {
		return trimmed(a)
	}
	return trimmed(b)
}

// securityEvent records a refused callback: always in ar05_security_events (no tenant needed) and, when the attempt it
// names is known, in that tenant's audit trail. The claimed attempt id is UNTRUSTED text (truncated) used only to label.
func (m *Module) securityEvent(r *http.Request, reason, provider string, claimed *callbackBody, route *attemptRoute, bodyDigest string, sigPresent bool, detail map[string]any) {
	ctx := r.Context()
	m.deps.Log.Warn("ar05 provider callback refused", zap.String("reason", reason), zap.String("provider", truncate(provider, 64)), zap.String("remote", r.RemoteAddr))
	if m.deps.DB == nil {
		return
	}
	var attempt, tenant *string
	if claimed != nil && claimed.ExternalAttemptID != "" {
		a := truncate(claimed.ExternalAttemptID, 64)
		attempt = &a
	}
	if route != nil {
		t := route.TenantID
		tenant = &t
	}
	var d []byte
	if detail != nil {
		d, _ = json.Marshal(detail)
	}
	prov := truncate(provider, 64)
	if _, err := m.deps.DB.Pool.Exec(ctx, `INSERT INTO ar05_security_events (event_id, kind, reason, provider, claimed_attempt, tenant_id, remote_addr, body_digest, signature_present, detail)
		VALUES ($1,'CALLBACK_REFUSED',$2,NULLIF($3,''),$4,$5::uuid,$6,$7,$8,$9)`,
		uuid.NewString(), reason, prov, attempt, tenant, truncate(r.RemoteAddr, 64), bodyDigest, sigPresent, d); err != nil {
		m.deps.Log.Error("ar05 could not record a security event", zap.Error(err))
	}
	if route == nil {
		return
	}
	c := kit.Caller{TenantID: route.TenantID, PrincipalID: "provider:" + truncate(provider, 64), LegalEntityID: route.LegalEntityID, CorrelationID: r.Header.Get("X-Correlation-ID")}
	ad := map[string]any{"reason": reason, "provider": truncate(provider, 64), "body_digest": bodyDigest, "remote_addr": r.RemoteAddr}
	for k, v := range detail {
		ad[k] = v
	}
	if err := m.deps.DB.WithTx(ctx, route.TenantID, func(tx pgx.Tx) error {
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: route.SubmissionID, Command: "recordProviderCallback.refused", Detail: ad})
	}); err != nil {
		m.deps.Log.Error("ar05 could not audit a refused callback", zap.Error(err))
	}
}
