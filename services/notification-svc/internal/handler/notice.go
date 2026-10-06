package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/identity"
	"zoiko.io/notification-svc/internal/telemetry"
)

// ZS-SVC-Y-001 NCD-05 API: regulated notices (sections 8.1 to 8.5).
//
//	POST /v1/notices                       prepare a notice (validated to READY)
//	POST /v1/notices/{id}/dispatch         submit it for delivery
//	GET  /v1/notices/{id}                  the notice, its history and any response
//	POST /v1/notices/{id}/acknowledgement  the recipient's own response
//	POST /v1/notices/{id}/corrections      a new version that supersedes this one
//	GET  /v1/notices/{id}/evidence         the evidence bundle
//
// The service proves what it prepared, attempted, transmitted, observed and received as
// acknowledgement. It never says legal service is complete (8.5); that is for policy
// outside this service. Delivery is the ordinary governed send (kill switch, suppression,
// privacy, preferences, retries and attempt records all apply), reached through the same
// code as POST /v1/notifications.

// NoticeStore is the notice persistence boundary.
type NoticeStore interface {
	CreateNotice(ctx context.Context, p domain.CreateNoticeParams) (*domain.Notice, error)
	GetNotice(ctx context.Context, noticeID string) (*domain.Notice, error)
	BeginNoticeDispatch(ctx context.Context, noticeID, notificationID, actor string) (*domain.Notice, bool, error)
	RefreshNotice(ctx context.Context, noticeID string, now time.Time) (*domain.Notice, error)
	RecordNoticeAck(ctx context.Context, noticeID, actor, action, comment string, now time.Time) (*domain.Notice, *domain.NoticeAck, error)
	ListNoticeTransitions(ctx context.Context, noticeID string) ([]domain.NoticeTransition, error)
	GetNoticeAck(ctx context.Context, noticeID string) (*domain.NoticeAck, error)
}

func registerNoticeRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/notices", func(r chi.Router) {
		r.Post("/", h.CreateNotice)
		r.Get("/{noticeID}", h.GetNotice)
		r.Post("/{noticeID}/dispatch", h.DispatchNotice)
		r.Post("/{noticeID}/acknowledgement", h.AcknowledgeNotice)
		r.Post("/{noticeID}/corrections", h.CorrectNotice)
		r.Get("/{noticeID}/evidence", h.NoticeEvidence)
	})
}

func (h *Handler) noticeGate(w http.ResponseWriter, r *http.Request) (principalID, tenantID string, ok bool) {
	if h.notices == nil || h.intents == nil {
		writeError(w, http.StatusServiceUnavailable, "notices_unavailable", "regulated notices are not configured")
		return "", "", false
	}
	if principalID, ok = h.requirePrincipal(w, r); !ok {
		return "", "", false
	}
	if tenantID, ok = h.requireTenant(w, r); !ok {
		return "", "", false
	}
	return principalID, tenantID, true
}

func (h *Handler) writeNoticeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNoticeNotFound):
		writeError(w, http.StatusNotFound, "notice_not_found", err.Error())
	case errors.Is(err, domain.ErrNoticeInvalid):
		writeError(w, http.StatusBadRequest, "notice_invalid", err.Error())
	case errors.Is(err, domain.ErrNotNoticeRecipient):
		writeError(w, http.StatusForbidden, "operator_acknowledgement_refused",
			"only the notice's recipient, acting as themselves, may respond to it; nobody can acknowledge on their behalf")
	case errors.Is(err, domain.ErrNoticeAlreadyAnswered):
		writeError(w, http.StatusConflict, "notice_already_answered", err.Error())
	case errors.Is(err, domain.ErrAckNotRequired):
		writeError(w, http.StatusConflict, "acknowledgement_not_required", err.Error())
	case errors.Is(err, domain.ErrNoticeState):
		writeError(w, http.StatusConflict, "notice_state", err.Error())
	default:
		h.log.Error("notice store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

type createNoticeRequest struct {
	IntentID             string     `json:"intent_id"`
	RecipientPrincipalID string     `json:"recipient_principal_id"`
	Locale               string     `json:"locale"`
	Subject              string     `json:"subject"`
	Body                 string     `json:"body"`
	PolicyRef            string     `json:"policy_ref"`
	EffectiveDate        *string    `json:"effective_date"`
	AckRequirement       string     `json:"ack_requirement"`
	DeadlineAt           *time.Time `json:"deadline_at"`
}

// CreateNotice prepares a notice and validates it into READY.
func (h *Handler) CreateNotice(w http.ResponseWriter, r *http.Request) {
	principalID, _, ok := h.noticeGate(w, r)
	if !ok {
		return
	}
	var req createNoticeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IntentID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "intent_id is required: a regulated notice is sent under a communication intent")
		return
	}
	now := time.Now().UTC()
	iv, err := h.intents.EffectiveIntentVersion(r.Context(), req.IntentID, now, now)
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, iv.LegalEntityID, actionSend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.Locale == "" {
		req.Locale = "en"
	}
	p := domain.CreateNoticeParams{LegalEntityID: iv.LegalEntityID, IntentVersionID: iv.VersionID, RecipientPrincipalID: req.RecipientPrincipalID,
		Locale: req.Locale, Subject: req.Subject, Body: req.Body, PolicyRef: req.PolicyRef, EffectiveDate: req.EffectiveDate,
		AckRequirement: req.AckRequirement, DeadlineAt: req.DeadlineAt, CreatedByPrincipalID: principalID}
	if err := domain.ValidateNotice(p, iv, now); err != nil {
		h.writeNoticeError(w, err)
		return
	}
	n, err := h.notices.CreateNotice(r.Context(), p)
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

type correctNoticeRequest struct {
	Reason         string     `json:"reason"`
	Subject        string     `json:"subject"`
	Body           string     `json:"body"`
	Locale         string     `json:"locale"`
	PolicyRef      string     `json:"policy_ref"`
	EffectiveDate  *string    `json:"effective_date"`
	AckRequirement string     `json:"ack_requirement"`
	DeadlineAt     *time.Time `json:"deadline_at"`
}

// CorrectNotice creates a new version that supersedes this one. The version already served
// is not touched and stays visible (8.4); whether a clock restarts is the caller's explicit
// decision, never an assumption: omit deadline_at to keep the existing one.
func (h *Handler) CorrectNotice(w http.ResponseWriter, r *http.Request) {
	principalID, _, ok := h.noticeGate(w, r)
	if !ok {
		return
	}
	var req correctNoticeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	prior, err := h.notices.GetNotice(r.Context(), chi.URLParam(r, "noticeID"))
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, prior.LegalEntityID, actionSend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.Reason == "" || req.Subject == "" || req.Body == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason, subject and body are required: a correction states why it exists and carries the full corrected content")
		return
	}
	iv, err := h.intents.GetIntentVersion(r.Context(), prior.IntentVersionID)
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	p := domain.CreateNoticeParams{LegalEntityID: prior.LegalEntityID, IntentVersionID: prior.IntentVersionID,
		RecipientPrincipalID: prior.RecipientPrincipalID, Locale: prior.Locale, Subject: req.Subject, Body: req.Body,
		PolicyRef: prior.PolicyRef, EffectiveDate: prior.EffectiveDate, AckRequirement: prior.AckRequirement, DeadlineAt: prior.DeadlineAt,
		CreatedByPrincipalID: principalID, SupersedesNoticeID: prior.NoticeID, CorrectionReason: req.Reason}
	if req.Locale != "" {
		p.Locale = req.Locale
	}
	if req.PolicyRef != "" {
		p.PolicyRef = req.PolicyRef
	}
	if req.EffectiveDate != nil {
		p.EffectiveDate = req.EffectiveDate
	}
	if req.AckRequirement != "" {
		p.AckRequirement = req.AckRequirement
	}
	if req.DeadlineAt != nil {
		p.DeadlineAt = req.DeadlineAt
	}
	if p.AckRequirement == domain.AckNone {
		p.DeadlineAt = nil
	}
	if err := domain.ValidateNotice(p, iv, time.Now().UTC()); err != nil {
		h.writeNoticeError(w, err)
		return
	}
	n, err := h.notices.CreateNotice(r.Context(), p)
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

// discardWriter lets the notice dispatch reuse the send path's delivery and attempt
// recording, which answer through an http.ResponseWriter, without replying to the caller
// twice. The status it saw tells the dispatch whether the send path hit a store failure.
type discardWriter struct {
	header http.Header
	code   int
}

func (d *discardWriter) Header() http.Header {
	if d.header == nil {
		d.header = http.Header{}
	}
	return d.header
}
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(code int)        { d.code = code }

// noticeNotificationID makes the delivery's identity a pure function of the notice, so a
// dispatch interrupted between creating the delivery and linking it can be repeated without
// creating a second delivery.
func noticeNotificationID(noticeID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("notice:"+noticeID)).String()
}

// DispatchNotice submits a READY notice for delivery.
func (h *Handler) DispatchNotice(w http.ResponseWriter, r *http.Request) {
	principalID, tenantID, ok := h.noticeGate(w, r)
	if !ok {
		return
	}
	notice, err := h.notices.GetNotice(r.Context(), chi.URLParam(r, "noticeID"))
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, notice.LegalEntityID, actionSend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	// Only a READY notice is sent, once. A repeat of the same dispatch returns the notice as it is.
	if notice.Status != domain.NoticeReady {
		if notice.NotificationID != nil {
			h.respondNotice(w, r, notice.NoticeID, http.StatusOK)
			return
		}
		h.writeNoticeError(w, domain.ErrNoticeState)
		return
	}
	if notice.SupersededByNoticeID != nil {
		writeError(w, http.StatusConflict, "notice_state", "a newer version of this notice exists and supersedes it; dispatch that version")
		return
	}
	iv, err := h.intents.GetIntentVersion(r.Context(), notice.IntentVersionID)
	if err != nil {
		h.writeIntentError(w, err)
		return
	}

	address, source, resolveErr := h.resolveRecipient(r.Context(), tenantID, principalID, domain.SendNotificationRequest{
		RecipientPrincipalID: notice.RecipientPrincipalID, Channel: domain.ChannelEmail})
	notificationID := noticeNotificationID(notice.NoticeID)
	notification := &domain.Notification{
		NotificationID: notificationID, TenantID: tenantID, LegalEntityID: notice.LegalEntityID,
		RecipientPrincipalID: notice.RecipientPrincipalID, RecipientAddress: address, RecipientAddressSource: source,
		Channel: domain.ChannelEmail, Subject: notice.Subject, Body: notice.Body, Status: "PENDING",
		SourceEventType: "notice.dispatch", SourceReference: notice.NoticeID, CorrelationID: "notice-" + notice.NoticeID,
		CreatedByPrincipalID: principalID, CreatedAt: time.Now().UTC(), RenderedContentHash: notice.ContentHash,
		PurposeContext: "regulated-notice", CommunicationClass: iv.PurposeClass, IntentVersionID: iv.VersionID,
	}
	created, err := h.store.CreateNotification(r.Context(), notification)
	if err != nil {
		h.log.Error("failed to create the notice's delivery", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	// Link before attempting: from here the notice owns the delivery, even if this process dies.
	if _, _, err := h.notices.BeginNoticeDispatch(r.Context(), notice.NoticeID, notificationID, principalID); err != nil {
		h.writeNoticeError(w, err)
		return
	}
	if created {
		dw := &discardWriter{}
		var outcome domain.DeliveryOutcome
		if resolveErr != nil {
			outcome.Reason = "recipient resolution failed: " + resolveErr.Error()
			outcome.Retryable = !identity.IsSettled(resolveErr)
		} else {
			var delivered bool
			if outcome, delivered = h.submitAndDeliver(dw, r, notification, tenantID, telemetry.OriginRequest); !delivered {
				// The send path could not mark the submission, so the provider was not called.
				// The notification stays in flight and the stranded sweep revives it.
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", "the delivery could not be started; it will be retried")
				return
			}
		}
		h.recordAttemptOutcome(dw, r, notification, outcome, domain.AttemptMeta{
			Origin: domain.AttemptOriginRequest, ProviderName: outcome.ProviderName, Retryable: outcome.Retryable,
			PrivacyDecisionID: outcome.PrivacyDecisionID, PrivacyResult: outcome.PrivacyResult, ActorPrincipalID: principalID,
		}, "notice-"+notice.NoticeID, tenantID, http.StatusCreated)
	}
	h.respondNotice(w, r, notice.NoticeID, http.StatusAccepted)
}

// canReadNotice: the notice's recipient may always read their own notice (they cannot respond
// to one they cannot see); anyone else needs NOTIFICATION_VIEW on its legal entity.
func (h *Handler) canReadNotice(w http.ResponseWriter, r *http.Request, principalID string, n *domain.Notice) bool {
	if principalID == n.RecipientPrincipalID {
		return true
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, n.LegalEntityID, actionView); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// respondNotice refreshes the notice from the delivery facts and answers with it.
func (h *Handler) respondNotice(w http.ResponseWriter, r *http.Request, noticeID string, code int) {
	n, err := h.notices.RefreshNotice(r.Context(), noticeID, time.Now().UTC())
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	history, err := h.notices.ListNoticeTransitions(r.Context(), noticeID)
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	ack, err := h.notices.GetNoticeAck(r.Context(), noticeID)
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	writeJSON(w, code, map[string]any{"notice": n, "history": history, "acknowledgement": ack})
}

// GetNotice returns the notice as it stands now, its full history and any response.
func (h *Handler) GetNotice(w http.ResponseWriter, r *http.Request) {
	principalID, _, ok := h.noticeGate(w, r)
	if !ok {
		return
	}
	n, err := h.notices.GetNotice(r.Context(), chi.URLParam(r, "noticeID"))
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	if !h.canReadNotice(w, r, principalID, n) {
		return
	}
	h.respondNotice(w, r, n.NoticeID, http.StatusOK)
}

type acknowledgeRequest struct {
	Action  string `json:"action"`
	Comment string `json:"comment"`
}

// AcknowledgeNotice records the recipient's own response to this exact notice version.
//
// The actor is the authenticated caller and must be the recipient: there is deliberately no
// way for an operator, or anyone else, to acknowledge for them (8.3). Opening an email or
// following a link is not this call and never counts.
func (h *Handler) AcknowledgeNotice(w http.ResponseWriter, r *http.Request) {
	principalID, _, ok := h.noticeGate(w, r)
	if !ok {
		return
	}
	var req acknowledgeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Comment) > 500 {
		writeError(w, http.StatusBadRequest, "notice_invalid", "comment is at most 500 characters")
		return
	}
	n, _, err := h.notices.RecordNoticeAck(r.Context(), chi.URLParam(r, "noticeID"), principalID, req.Action, req.Comment, time.Now().UTC())
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	h.respondNotice(w, r, n.NoticeID, http.StatusOK)
}

// NoticeEvidence returns the evidence bundle: what was prepared, every attempt, the provider
// facts, the lifecycle history and the response, with the limits of each.
func (h *Handler) NoticeEvidence(w http.ResponseWriter, r *http.Request) {
	principalID, _, ok := h.noticeGate(w, r)
	if !ok {
		return
	}
	n, err := h.notices.GetNotice(r.Context(), chi.URLParam(r, "noticeID"))
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	// The bundle is evidence for those who administer the notice, not for the recipient.
	if err := h.authz.CheckAllowed(r.Context(), principalID, n.LegalEntityID, actionView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	n, err = h.notices.RefreshNotice(r.Context(), n.NoticeID, time.Now().UTC())
	if err != nil {
		h.writeNoticeError(w, err)
		return
	}
	history, _ := h.notices.ListNoticeTransitions(r.Context(), n.NoticeID)
	ack, _ := h.notices.GetNoticeAck(r.Context(), n.NoticeID)
	bundle := map[string]any{
		"notice": n, "history": history, "acknowledgement": ack,
		"attempts": []domain.DeliveryAttempt{}, "delivery_evidence": []domain.DeliveryEvidence{},
		"notice_text": "This bundle records what the platform prepared, attempted, transmitted, observed and received as acknowledgement. " +
			"It is not a finding that service was legally effective; that is decided by the applicable policy outside this service.",
	}
	if n.NotificationID != nil {
		if attempts, err := h.store.ListAttempts(r.Context(), *n.NotificationID); err == nil && attempts != nil {
			bundle["attempts"] = attempts
		}
		if h.evidence != nil {
			if facts, err := h.evidence.ListDeliveryEvidence(r.Context(), *n.NotificationID); err == nil {
				bundle["delivery_evidence"] = facts
			}
		}
	}
	writeJSON(w, http.StatusOK, bundle)
}
