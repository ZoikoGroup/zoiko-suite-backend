package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/eventing/integration"
	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/whout"
)

// Authorization actions for INT-04 outbound webhooks. They are new: each must be
// granted in the estate's RBAC seed before the matching command can be used.
const (
	ActionWebhookSubscriptionCreate  = "WEBHOOK_SUBSCRIPTION_CREATE"
	ActionWebhookSubscriptionUpdate  = "WEBHOOK_SUBSCRIPTION_UPDATE"
	ActionWebhookSubscriptionSubmit  = "WEBHOOK_SUBSCRIPTION_SUBMIT"
	ActionWebhookSubscriptionApprove = "WEBHOOK_SUBSCRIPTION_APPROVE"
	ActionWebhookSubscriptionRotate  = "WEBHOOK_SUBSCRIPTION_ROTATE_KEY"
	ActionWebhookSubscriptionSuspend = "WEBHOOK_SUBSCRIPTION_SUSPEND"
	ActionWebhookSubscriptionResume  = "WEBHOOK_SUBSCRIPTION_RESUME"
	ActionWebhookSubscriptionRevoke  = "WEBHOOK_SUBSCRIPTION_REVOKE"
	ActionWebhookSubscriptionRead    = "WEBHOOK_SUBSCRIPTION_READ"
	ActionWebhookDeliveryRead        = "WEBHOOK_DELIVERY_READ"
	ActionWebhookDeliveryReplay      = "WEBHOOK_DELIVERY_REPLAY"
	ActionWebhookReplayApprove       = "WEBHOOK_DELIVERY_REPLAY_APPROVE"
)

// WebhookHandler serves the INT-04 subscription and delivery API.
type WebhookHandler struct {
	svc   *whout.Service
	authz AuthZClient
	log   *zap.Logger
}

// NewWebhookHandler wires the handler.
func NewWebhookHandler(svc *whout.Service, authz AuthZClient, log *zap.Logger) *WebhookHandler {
	if log == nil {
		log = zap.NewNop()
	}
	return &WebhookHandler{svc: svc, authz: authz, log: log}
}

// RegisterWebhookRoutes mounts the API under /v1.
func RegisterWebhookRoutes(r chi.Router, h *WebhookHandler) {
	r.Group(func(r chi.Router) {
		r.Post("/v1/webhook-subscriptions", h.create)
		r.Get("/v1/webhook-subscriptions", h.list)
		r.Get("/v1/webhook-subscriptions/{id}", h.get)
		r.Patch("/v1/webhook-subscriptions/{id}", h.update)
		r.Post("/v1/webhook-subscriptions/{id}/submit", h.command(ActionWebhookSubscriptionSubmit, h.submit))
		r.Post("/v1/webhook-subscriptions/{id}/approve", h.command(ActionWebhookSubscriptionApprove, h.approve))
		r.Post("/v1/webhook-subscriptions/{id}/reject", h.command(ActionWebhookSubscriptionApprove, h.reject))
		r.Post("/v1/webhook-subscriptions/{id}/rotate-signing-key", h.command(ActionWebhookSubscriptionRotate, h.rotate))
		r.Post("/v1/webhook-subscriptions/{id}/suspend", h.command(ActionWebhookSubscriptionSuspend, h.suspend))
		r.Post("/v1/webhook-subscriptions/{id}/resume", h.command(ActionWebhookSubscriptionResume, h.resume))
		r.Post("/v1/webhook-subscriptions/{id}/revoke", h.command(ActionWebhookSubscriptionRevoke, h.revoke))
		r.Get("/v1/webhook-subscriptions/{id}/log", h.readSub(h.log_))
		r.Get("/v1/webhook-subscriptions/{id}/deliveries", h.readSub(h.deliveries))
		r.Get("/v1/webhook-subscriptions/{id}/health", h.readSub(h.health))
		r.Get("/v1/webhook-dead-letters", h.deadLetters)
		r.Get("/v1/webhook-deliveries/{id}", h.evidence)
		r.Post("/v1/webhook-deliveries/{id}/replay", h.replay)
	})
}

// ── problems ────────────────────────────────────────────────────────────────

type whOutcome struct {
	status int
	code   integration.Code
	detail string
	retry  int
}

// whClassify maps an error to its presentation: an HTTP status, a stable INT-* code and
// a detail that names the rule and never echoes caller input or key material.
func whClassify(err error) whOutcome {
	switch {
	case errors.Is(err, domain.ErrAuthorizationDenied):
		return whOutcome{http.StatusForbidden, integration.CodeAuthDenied, "authorization denied", 0}
	case errors.Is(err, domain.ErrAuthzServiceUnavailable):
		return whOutcome{http.StatusServiceUnavailable, "", "authorization service unavailable", 5}
	case errors.Is(err, domain.ErrIdentityMissing):
		return whOutcome{http.StatusUnauthorized, integration.CodeAuthDenied, "X-Principal-Id header is required", 0}
	case errors.Is(err, whout.ErrNotFound):
		return whOutcome{http.StatusNotFound, "", "webhook subscription not found", 0}
	case errors.Is(err, whout.ErrDeliveryNotFound):
		return whOutcome{http.StatusNotFound, "", "webhook delivery not found", 0}
	case errors.Is(err, whout.ErrVersionConflict):
		return whOutcome{http.StatusConflict, integration.CodeIdempotencyConflict, err.Error(), 0}
	case errors.Is(err, whout.ErrSelfApproval), errors.Is(err, whout.ErrReplayUnapproved):
		return whOutcome{http.StatusForbidden, integration.CodeAuthDenied, err.Error(), 0}
	case errors.Is(err, whout.ErrInvalidState), errors.Is(err, whout.ErrNotApproved), errors.Is(err, whout.ErrReplayNotAllowed):
		return whOutcome{http.StatusConflict, integration.CodeScopeDenied, err.Error(), 0}
	case errors.Is(err, whout.ErrRestrictedField), errors.Is(err, whout.ErrOutsideScope):
		return whOutcome{http.StatusUnprocessableEntity, integration.CodeScopeDenied, err.Error(), 0}
	case errors.Is(err, whout.ErrEgressDenied):
		return whOutcome{http.StatusForbidden, integration.CodeResidencyDenied, err.Error(), 0}
	case errors.Is(err, whout.ErrKeyUnavailable):
		return whOutcome{http.StatusUnprocessableEntity, integration.CodeCredentialInvalid, err.Error(), 0}
	case errors.Is(err, whout.ErrDependency):
		return whOutcome{http.StatusServiceUnavailable, "", "a dependency needed to check this change is unavailable; retry", 5}
	case errors.Is(err, whout.ErrValidation), errors.Is(err, errWebhookBadBody), errors.Is(err, errWebhookBadVersion):
		return whOutcome{http.StatusBadRequest, integration.CodeSchemaMismatch, err.Error(), 0}
	}
	return whOutcome{http.StatusInternalServerError, "", "internal error", 0}
}

var (
	errWebhookBadBody    = errors.New("the request body is not valid JSON for this operation")
	errWebhookBadVersion = errors.New("expected_version must be supplied (body field or X-Expected-Version header) and be a positive integer")
)

// writeWebhookProblem answers with RFC 9457 problem details, keeping the service's
// pre-existing {"error_code","error_message"} members beside them so existing clients
// keep reading errors where they always did.
func (h *WebhookHandler) problem(w http.ResponseWriter, r *http.Request, err error) {
	o := whClassify(err)
	if o.status >= 500 && o.status != http.StatusServiceUnavailable {
		h.log.Error("webhook API: internal error", zap.String("path", r.URL.Path), zap.Error(err))
	}
	body := map[string]any{
		"type": "about:blank", "title": http.StatusText(o.status), "status": o.status, "detail": o.detail,
		"instance": r.URL.Path, "error_message": o.detail,
	}
	if c := r.Header.Get("X-Correlation-ID"); c != "" {
		body["correlation_id"] = c
	}
	if o.code != "" {
		info := o.code.Info()
		body["type"] = integration.ProblemTypePrefix + string(o.code)
		body["title"] = info.Title
		body["code"] = string(o.code)
		body["error_code"] = string(o.code)
		body["retryable"] = info.Retryable
	}
	if o.retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(o.retry))
	}
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(o.status)
	_ = json.NewEncoder(w).Encode(body)
}

// ── plumbing ────────────────────────────────────────────────────────────────

func (h *WebhookHandler) actor(r *http.Request) (whout.Actor, error) {
	p := r.Header.Get("X-Principal-Id")
	t := svcmiddleware.TenantFromContext(r.Context())
	if p == "" || t == "" {
		return whout.Actor{}, domain.ErrIdentityMissing
	}
	return whout.Actor{TenantID: t, PrincipalID: p, CorrelationID: getCorrelationID(r)}, nil
}

func (h *WebhookHandler) allow(r *http.Request, principal, legalEntityID, action string) error {
	if legalEntityID == "" {
		return whout.ErrValidation
	}
	return h.authz.CheckAllowed(r.Context(), principal, legalEntityID, action)
}

const maxWebhookBody = 64 << 10

func decodeWebhookBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return errWebhookBadBody
	}
	return nil
}

// expectedVersion reads the optimistic-lock version from X-Expected-Version or the
// body; if both are present they must agree.
func expectedVersion(r *http.Request, body int) (int, error) {
	hv := 0
	if s := r.Header.Get("X-Expected-Version"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return 0, errWebhookBadVersion
		}
		hv = n
	}
	switch {
	case hv > 0 && body > 0 && hv != body:
		return 0, errWebhookBadVersion
	case hv > 0:
		return hv, nil
	case body > 0:
		return body, nil
	}
	return 0, errWebhookBadVersion
}

// ── DTOs ────────────────────────────────────────────────────────────────────

type subscriptionDTO struct {
	SubscriptionID         string     `json:"subscription_id"`
	LegalEntityID          string     `json:"legal_entity_id"`
	Name                   string     `json:"name"`
	DestinationURL         string     `json:"destination_url"`
	EventTypes             []string   `json:"event_types"`
	AllowedFields          []string   `json:"allowed_fields"`
	APIVersion             string     `json:"api_version"`
	SigningKeyPath         string     `json:"signing_key_path"`
	PreviousSigningKeyPath string     `json:"previous_signing_key_path,omitempty"`
	PreviousKeyValidUntil  *time.Time `json:"previous_key_valid_until,omitempty"`
	DestinationRegion      string     `json:"destination_region"`
	MaxAttempts            int        `json:"max_attempts"`
	Status                 string     `json:"status"`
	StatusReason           string     `json:"status_reason,omitempty"`
	Version                int        `json:"version"`
	ConfigFingerprint      string     `json:"config_fingerprint"`
	ApprovedFingerprint    string     `json:"approved_fingerprint,omitempty"`
	CreatedBy              string     `json:"created_by"`
	LastChangedBy          string     `json:"last_changed_by"`
	ApprovedBy             string     `json:"approved_by,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

func toSubDTO(s *whout.Subscription) subscriptionDTO {
	return subscriptionDTO{
		SubscriptionID: s.SubscriptionID, LegalEntityID: s.LegalEntityID, Name: s.Name, DestinationURL: s.DestinationURL,
		EventTypes: nonNil(s.EventTypes), AllowedFields: nonNil(s.AllowedFields), APIVersion: s.APIVersion, SigningKeyPath: s.SigningKeyPath,
		PreviousSigningKeyPath: s.PreviousSigningKeyPath, PreviousKeyValidUntil: s.PreviousKeyValidUntil,
		DestinationRegion: s.DestinationRegion, MaxAttempts: s.MaxAttempts, Status: string(s.Status), StatusReason: s.StatusReason,
		Version: s.Version, ConfigFingerprint: s.ConfigFingerprint, ApprovedFingerprint: s.ApprovedFingerprint, CreatedBy: s.CreatedBy,
		LastChangedBy: s.LastChangedBy, ApprovedBy: s.ApprovedBy, ApprovedAt: s.ApprovedAt, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

type deliveryDTO struct {
	DeliveryID       string     `json:"delivery_id"`
	SubscriptionID   string     `json:"subscription_id"`
	EventID          string     `json:"event_id"`
	EventType        string     `json:"event_type"`
	EventOccurredAt  time.Time  `json:"event_occurred_at"`
	PayloadHash      string     `json:"payload_hash"`
	Status           string     `json:"status"`
	AttemptCount     int        `json:"attempt_count"`
	NextAttemptAt    time.Time  `json:"next_attempt_at"`
	LastAttemptAt    *time.Time `json:"last_attempt_at,omitempty"`
	LastResponseCode *int       `json:"last_response_code,omitempty"`
	LastErrorClass   string     `json:"last_error_class,omitempty"`
	DeadLetterReason string     `json:"dead_letter_reason,omitempty"`
	DeadLetteredAt   *time.Time `json:"dead_lettered_at,omitempty"`
	SucceededAt      *time.Time `json:"succeeded_at,omitempty"`
	ReplayCount      int        `json:"replay_count"`
	CreatedAt        time.Time  `json:"created_at"`
}

// toDeliveryDTO omits the body: the register shows its hash, and a payload read is a
// separate authorization this API does not offer.
func toDeliveryDTO(d *whout.Delivery) deliveryDTO {
	return deliveryDTO{
		DeliveryID: d.DeliveryID, SubscriptionID: d.SubscriptionID, EventID: d.EventID, EventType: d.EventType,
		EventOccurredAt: d.EventOccurredAt, PayloadHash: d.BodyHash, Status: string(d.Status), AttemptCount: d.AttemptCount,
		NextAttemptAt: d.NextAttemptAt, LastAttemptAt: d.LastAttemptAt, LastResponseCode: d.LastResponseCode,
		LastErrorClass: d.LastErrorClass, DeadLetterReason: d.DeadLetterReason, DeadLetteredAt: d.DeadLetteredAt,
		SucceededAt: d.SucceededAt, ReplayCount: d.ReplayCount, CreatedAt: d.CreatedAt,
	}
}

type attemptDTO struct {
	AttemptNo          int        `json:"attempt_no"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	Outcome            string     `json:"outcome"`
	ResponseCode       *int       `json:"response_code,omitempty"`
	ResponseBodyHash   string     `json:"response_body_hash,omitempty"`
	ResponseBodyBytes  *int       `json:"response_body_bytes,omitempty"`
	ErrorClass         string     `json:"error_class,omitempty"`
	DestinationHost    string     `json:"destination_host"`
	PayloadHash        string     `json:"payload_hash"`
	SignatureProfile   string     `json:"signature_profile"`
	SignatureTimestamp *int64     `json:"signature_timestamp,omitempty"`
	SignatureKeyPaths  []string   `json:"signature_key_paths"`
}

func toAttemptDTO(a whout.Attempt) attemptDTO {
	return attemptDTO{
		AttemptNo: a.AttemptNo, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Outcome: string(a.Outcome), ResponseCode: a.ResponseCode,
		ResponseBodyHash: a.ResponseBodyHash, ResponseBodyBytes: a.ResponseBodyBytes, ErrorClass: a.ErrorClass,
		DestinationHost: a.DestinationHost, PayloadHash: a.PayloadHash, SignatureProfile: a.SignatureProfile,
		SignatureTimestamp: a.SignatureTimestamp, SignatureKeyPaths: nonNil(a.SignatureKeyPaths),
	}
}

// ── handlers ────────────────────────────────────────────────────────────────

type createRequest struct {
	LegalEntityID     string   `json:"legal_entity_id"`
	Name              string   `json:"name"`
	DestinationURL    string   `json:"destination_url"`
	EventTypes        []string `json:"event_types"`
	AllowedFields     []string `json:"allowed_fields"`
	APIVersion        string   `json:"api_version"`
	SigningKeyPath    string   `json:"signing_key_path"`
	DestinationRegion string   `json:"destination_region"`
	MaxAttempts       int      `json:"max_attempts"`
}

func (h *WebhookHandler) create(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	var req createRequest
	if err := decodeWebhookBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.allow(r, a.PrincipalID, req.LegalEntityID, ActionWebhookSubscriptionCreate); err != nil {
		h.problem(w, r, err)
		return
	}
	s, err := h.svc.Create(r.Context(), a, whout.Spec{
		LegalEntityID: req.LegalEntityID, Name: req.Name, DestinationURL: req.DestinationURL, EventTypes: req.EventTypes,
		AllowedFields: req.AllowedFields, APIVersion: req.APIVersion, SigningKeyPath: req.SigningKeyPath,
		DestinationRegion: req.DestinationRegion, MaxAttempts: req.MaxAttempts,
	})
	if err != nil {
		h.problem(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/webhook-subscriptions/"+s.SubscriptionID)
	writeJSON(w, http.StatusCreated, toSubDTO(s))
}

// listScope resolves the legal entity a collection read is authorized against: the
// caller's verified X-Legal-Entity-Id.
func (h *WebhookHandler) listScope(r *http.Request, a whout.Actor, action string) error {
	return h.allow(r, a.PrincipalID, r.Header.Get("X-Legal-Entity-Id"), action)
}

func limitParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

func (h *WebhookHandler) list(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.listScope(r, a, ActionWebhookSubscriptionRead); err != nil {
		h.problem(w, r, err)
		return
	}
	subs, err := h.svc.List(r.Context(), a.TenantID, whout.Status(r.URL.Query().Get("status")), limitParam(r))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	out := make([]subscriptionDTO, 0, len(subs))
	for i := range subs {
		out = append(out, toSubDTO(&subs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

// load fetches the subscription and authorizes the caller against ITS legal entity.
func (h *WebhookHandler) load(w http.ResponseWriter, r *http.Request, action string) (whout.Actor, *whout.Subscription, bool) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return a, nil, false
	}
	s, err := h.svc.Get(r.Context(), a.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.problem(w, r, err)
		return a, nil, false
	}
	if err := h.allow(r, a.PrincipalID, s.LegalEntityID, action); err != nil {
		h.problem(w, r, err)
		return a, nil, false
	}
	return a, s, true
}

func (h *WebhookHandler) get(w http.ResponseWriter, r *http.Request) {
	if _, s, ok := h.load(w, r, ActionWebhookSubscriptionRead); ok {
		writeJSON(w, http.StatusOK, toSubDTO(s))
	}
}

type updateRequest struct {
	ExpectedVersion   int       `json:"expected_version"`
	Name              *string   `json:"name"`
	DestinationURL    *string   `json:"destination_url"`
	EventTypes        *[]string `json:"event_types"`
	AllowedFields     *[]string `json:"allowed_fields"`
	MaxAttempts       *int      `json:"max_attempts"`
	DestinationRegion *string   `json:"destination_region"`
}

func (h *WebhookHandler) update(w http.ResponseWriter, r *http.Request) {
	a, s, ok := h.load(w, r, ActionWebhookSubscriptionUpdate)
	if !ok {
		return
	}
	var req updateRequest
	if err := decodeWebhookBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	v, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	out, err := h.svc.Update(r.Context(), a, s.SubscriptionID, v, whout.Patch{
		Name: req.Name, DestinationURL: req.DestinationURL, EventTypes: req.EventTypes, AllowedFields: req.AllowedFields,
		MaxAttempts: req.MaxAttempts, Region: req.DestinationRegion,
	})
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSubDTO(out))
}

type commandRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
	NewSigningKey   string `json:"new_signing_key_path"`
	GraceSeconds    int    `json:"grace_period_seconds"`
}

type commandFn func(r *http.Request, a whout.Actor, s *whout.Subscription, version int, req commandRequest) (*whout.Subscription, error)

// command wraps a lifecycle command: identity, load, authorize against the
// subscription's legal entity, decode, run.
func (h *WebhookHandler) command(action string, fn commandFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, s, ok := h.load(w, r, action)
		if !ok {
			return
		}
		var req commandRequest
		if err := decodeWebhookBody(w, r, &req); err != nil {
			h.problem(w, r, err)
			return
		}
		v, err := expectedVersion(r, req.ExpectedVersion)
		if err != nil {
			h.problem(w, r, err)
			return
		}
		out, err := fn(r, a, s, v, req)
		if err != nil {
			h.problem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toSubDTO(out))
	}
}

func (h *WebhookHandler) submit(r *http.Request, a whout.Actor, s *whout.Subscription, v int, _ commandRequest) (*whout.Subscription, error) {
	return h.svc.Submit(r.Context(), a, s.SubscriptionID, v)
}
func (h *WebhookHandler) approve(r *http.Request, a whout.Actor, s *whout.Subscription, v int, q commandRequest) (*whout.Subscription, error) {
	return h.svc.Approve(r.Context(), a, s.SubscriptionID, v, q.Reason)
}
func (h *WebhookHandler) reject(r *http.Request, a whout.Actor, s *whout.Subscription, v int, q commandRequest) (*whout.Subscription, error) {
	return h.svc.Reject(r.Context(), a, s.SubscriptionID, v, q.Reason)
}
func (h *WebhookHandler) rotate(r *http.Request, a whout.Actor, s *whout.Subscription, v int, q commandRequest) (*whout.Subscription, error) {
	return h.svc.RotateSigningKey(r.Context(), a, s.SubscriptionID, v, q.NewSigningKey, time.Duration(q.GraceSeconds)*time.Second)
}
func (h *WebhookHandler) suspend(r *http.Request, a whout.Actor, s *whout.Subscription, v int, q commandRequest) (*whout.Subscription, error) {
	return h.svc.Suspend(r.Context(), a, s.SubscriptionID, v, q.Reason)
}
func (h *WebhookHandler) resume(r *http.Request, a whout.Actor, s *whout.Subscription, v int, q commandRequest) (*whout.Subscription, error) {
	return h.svc.Resume(r.Context(), a, s.SubscriptionID, v, q.Reason)
}
func (h *WebhookHandler) revoke(r *http.Request, a whout.Actor, s *whout.Subscription, v int, q commandRequest) (*whout.Subscription, error) {
	return h.svc.Revoke(r.Context(), a, s.SubscriptionID, v, q.Reason)
}

// readSub wraps a read of something that belongs to one subscription.
func (h *WebhookHandler) readSub(fn func(w http.ResponseWriter, r *http.Request, a whout.Actor, s *whout.Subscription)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a, s, ok := h.load(w, r, ActionWebhookDeliveryRead); ok {
			fn(w, r, a, s)
		}
	}
}

func (h *WebhookHandler) log_(w http.ResponseWriter, r *http.Request, a whout.Actor, s *whout.Subscription) {
	entries, err := h.svc.Log(r.Context(), a.TenantID, s.SubscriptionID, limitParam(r))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	type entry struct {
		Kind          string    `json:"kind"`
		Actor         string    `json:"actor"`
		Approver      string    `json:"approver,omitempty"`
		ApprovalRef   string    `json:"approval_ref,omitempty"`
		FromStatus    string    `json:"from_status,omitempty"`
		ToStatus      string    `json:"to_status,omitempty"`
		Version       int       `json:"version"`
		Fingerprint   string    `json:"fingerprint,omitempty"`
		DeliveryID    string    `json:"delivery_id,omitempty"`
		Reason        string    `json:"reason,omitempty"`
		CorrelationID string    `json:"correlation_id,omitempty"`
		CreatedAt     time.Time `json:"created_at"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entry{string(e.Kind), e.Actor, e.Approver, e.ApprovalRef, string(e.FromStatus), string(e.ToStatus),
			e.Version, e.Fingerprint, e.DeliveryID, e.Reason, e.CorrelationID, e.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": out})
}

func (h *WebhookHandler) deliveries(w http.ResponseWriter, r *http.Request, a whout.Actor, s *whout.Subscription) {
	ds, err := h.svc.Deliveries(r.Context(), a.TenantID, s.SubscriptionID, whout.DeliveryStatus(r.URL.Query().Get("status")), limitParam(r))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	out := make([]deliveryDTO, 0, len(ds))
	for i := range ds {
		out = append(out, toDeliveryDTO(&ds[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

func (h *WebhookHandler) health(w http.ResponseWriter, r *http.Request, a whout.Actor, s *whout.Subscription) {
	hl, err := h.svc.Health(r.Context(), a.TenantID, s.SubscriptionID)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subscription_id": hl.SubscriptionID, "status": string(hl.Status), "breaker_open": hl.BreakerOpen,
		"breaker_open_until": hl.BreakerOpenUntil, "consecutive_failures": hl.ConsecutiveFailures,
		"consecutive_dead_letters": hl.ConsecutiveDeadLetters, "last_success_at": hl.LastSuccessAt, "last_failure_at": hl.LastFailureAt,
		"pending": hl.Pending, "retrying": hl.Retrying, "dead_lettered": hl.DeadLettered, "succeeded": hl.Succeeded,
	})
}

func (h *WebhookHandler) deadLetters(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.listScope(r, a, ActionWebhookDeliveryRead); err != nil {
		h.problem(w, r, err)
		return
	}
	ds, err := h.svc.DeadLetters(r.Context(), a.TenantID, r.URL.Query().Get("subscription_id"), limitParam(r))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	out := make([]deliveryDTO, 0, len(ds))
	for i := range ds {
		out = append(out, toDeliveryDTO(&ds[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"dead_letters": out})
}

// deliverySub loads a delivery and the subscription it belongs to, and authorizes the
// caller against that subscription's legal entity.
func (h *WebhookHandler) deliverySub(w http.ResponseWriter, r *http.Request, a whout.Actor, action string) (*whout.Evidence, *whout.Subscription, bool) {
	ev, err := h.svc.Evidence(r.Context(), a.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.problem(w, r, err)
		return nil, nil, false
	}
	s, err := h.svc.Get(r.Context(), a.TenantID, ev.Delivery.SubscriptionID)
	if err != nil {
		h.problem(w, r, err)
		return nil, nil, false
	}
	if err := h.allow(r, a.PrincipalID, s.LegalEntityID, action); err != nil {
		h.problem(w, r, err)
		return nil, nil, false
	}
	return ev, s, true
}

func (h *WebhookHandler) evidence(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	ev, _, ok := h.deliverySub(w, r, a, ActionWebhookDeliveryRead)
	if !ok {
		return
	}
	attempts := make([]attemptDTO, 0, len(ev.Attempts))
	for _, at := range ev.Attempts {
		attempts = append(attempts, toAttemptDTO(at))
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivery": toDeliveryDTO(ev.Delivery), "attempts": attempts})
}

type replayRequest struct {
	ApproverPrincipalID string `json:"approver_principal_id"`
	ApprovalRef         string `json:"approval_ref"`
	Reason              string `json:"reason"`
}

func (h *WebhookHandler) replay(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	var req replayRequest
	if err := decodeWebhookBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	_, s, ok := h.deliverySub(w, r, a, ActionWebhookDeliveryReplay)
	if !ok {
		return
	}
	// The named approver must themselves be entitled to approve a replay for this
	// subscription's legal entity; naming a person is not enough.
	if req.ApproverPrincipalID != "" && req.ApproverPrincipalID != a.PrincipalID {
		if err := h.allow(r, req.ApproverPrincipalID, s.LegalEntityID, ActionWebhookReplayApprove); err != nil {
			h.problem(w, r, err)
			return
		}
	}
	d, err := h.svc.Replay(r.Context(), a, chi.URLParam(r, "id"), whout.ReplayApproval{
		Approver: req.ApproverPrincipalID, ApprovalRef: req.ApprovalRef, Reason: req.Reason})
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toDeliveryDTO(d))
}
