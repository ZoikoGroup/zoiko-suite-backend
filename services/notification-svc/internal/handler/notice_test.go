package handler_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
)

// stubNotices keeps notices in memory and applies the REAL domain rules for responses, so
// the handler tests exercise the same acknowledgement logic the database store does.
type stubNotices struct {
	mu      sync.Mutex
	notices map[string]*domain.Notice
	acks    map[string]*domain.NoticeAck
}

func newStubNotices() *stubNotices {
	return &stubNotices{notices: map[string]*domain.Notice{}, acks: map[string]*domain.NoticeAck{}}
}

func (s *stubNotices) CreateNotice(_ context.Context, p domain.CreateNoticeParams) (*domain.Notice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := &domain.Notice{NoticeID: uuid.NewString(), TenantID: "tenant-abc", LegalEntityID: p.LegalEntityID, LineageID: uuid.NewString(), VersionNumber: 1,
		IntentVersionID: p.IntentVersionID, RecipientPrincipalID: p.RecipientPrincipalID, RecipientCapacity: "SELF", Channel: "EMAIL", Locale: p.Locale,
		Subject: p.Subject, Body: p.Body, ContentHash: domain.NoticeContentHash(p.Subject, p.Body, p.Locale), PolicyRef: p.PolicyRef,
		EffectiveDate: p.EffectiveDate, AckRequirement: p.AckRequirement, DeadlineAt: p.DeadlineAt, Status: domain.NoticeReady,
		CreatedByPrincipalID: p.CreatedByPrincipalID, CreatedAt: time.Now()}
	if p.SupersedesNoticeID != "" {
		prior, ok := s.notices[p.SupersedesNoticeID]
		if !ok {
			return nil, domain.ErrNoticeNotFound
		}
		n.LineageID, n.VersionNumber = prior.LineageID, prior.VersionNumber+1
		n.SupersedesNoticeID, n.CorrectionReason = &prior.NoticeID, &p.CorrectionReason
		prior.SupersededByNoticeID = &n.NoticeID
	}
	s.notices[n.NoticeID] = n
	return n, nil
}

func (s *stubNotices) GetNotice(_ context.Context, id string) (*domain.Notice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.notices[id]; ok {
		c := *n
		return &c, nil
	}
	return nil, domain.ErrNoticeNotFound
}

func (s *stubNotices) BeginNoticeDispatch(_ context.Context, id, notificationID, _ string) (*domain.Notice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notices[id]
	if !ok {
		return nil, false, domain.ErrNoticeNotFound
	}
	switch {
	case n.Status == domain.NoticeReady:
		n.Status, n.NotificationID = domain.NoticeDeliveryInProgess, &notificationID
		return n, true, nil
	case n.NotificationID != nil && *n.NotificationID == notificationID:
		return n, false, nil
	}
	return nil, false, domain.ErrNoticeState
}

func (s *stubNotices) RefreshNotice(ctx context.Context, id string, _ time.Time) (*domain.Notice, error) {
	return s.GetNotice(ctx, id)
}

func (s *stubNotices) RecordNoticeAck(_ context.Context, id, actor, action, comment string, now time.Time) (*domain.Notice, *domain.NoticeAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notices[id]
	if !ok {
		return nil, nil, domain.ErrNoticeNotFound
	}
	if actor != n.RecipientPrincipalID {
		return nil, nil, domain.ErrNotNoticeRecipient
	}
	switch n.Status {
	case domain.NoticeAckPending:
	case domain.NoticeAcknowledged, domain.NoticeDeclined, domain.NoticeDisputed:
		return n, nil, domain.ErrNoticeAlreadyAnswered
	default:
		return n, nil, domain.ErrNoticeState
	}
	to, err := domain.AckOutcome(n.AckRequirement, action)
	if err != nil {
		return nil, nil, err
	}
	n.Status = to
	a := &domain.NoticeAck{AckID: uuid.NewString(), NoticeID: id, Action: action, ActorPrincipalID: actor, Method: domain.AckMethodAuthAction, Comment: comment, OccurredAt: now}
	s.acks[id] = a
	return n, a, nil
}

func (s *stubNotices) ListNoticeTransitions(context.Context, string) ([]domain.NoticeTransition, error) {
	return []domain.NoticeTransition{}, nil
}

func (s *stubNotices) GetNoticeAck(_ context.Context, id string) (*domain.NoticeAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acks[id], nil
}

// countingDeliverer records how many times the provider was reached and what it was given.
type countingDeliverer struct {
	mu    sync.Mutex
	calls int
	seen  domain.Notification
}

func (d *countingDeliverer) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.seen = n
	return domain.DeliveryOutcome{Delivered: true, ProviderResponse: "smtp accepted; message-id=<n@example.com>"}
}

type noticeRig struct {
	r       chi.Router
	notices *stubNotices
	del     *countingDeliverer
	store   *stubStore
	authz   *stubAuthZ
	ivID    string
	intent  string
}

// newNoticeRig builds the router with a published E3 (or, when e3 is false, E2) intent.
func newNoticeRig(t *testing.T, e3 bool, withNotices bool) *noticeRig {
	t.Helper()
	ints := newStubIntents()
	setup, _ := intentRouter(t, ints, &stubDeliverer{delivered: true}, nil)
	intentID := publishedIntent(t, setup, "legal.notice_of_change", func(b map[string]any) {
		b["allowed_channels"] = []string{"EMAIL"}
		if e3 {
			b["evidence_class"] = "E3"
		}
	})
	rig := &noticeRig{notices: newStubNotices(), del: &countingDeliverer{}, store: newStubStore(), authz: &stubAuthZ{}, intent: intentID}
	rig.store.events = &stubPublisher{}
	for _, v := range ints.versions {
		rig.ivID = v.VersionID
	}
	rig.r = chi.NewRouter()
	rig.r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	d := handler.Deps{Store: rig.store, AuthZ: rig.authz, Deliverer: rig.del, Recipient: &stubResolver{email: "r@example.com"},
		Intents: ints, RetryPolicy: retry.DefaultPolicy, Log: zap.NewNop()}
	if withNotices {
		d.Notices = rig.notices
	}
	handler.RegisterRoutes(rig.r, handler.New(d))
	return rig
}

func (g *noticeRig) create(t *testing.T, mutate func(map[string]any)) (int, domain.Notice) {
	t.Helper()
	body := map[string]any{"intent_id": g.intent, "recipient_principal_id": "recipient-1", "subject": "Notice of change",
		"body": "Your terms change on 1 January.", "policy_ref": "PDC-RULE-9", "ack_requirement": "RECEIPT",
		"deadline_at": time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)}
	if mutate != nil {
		mutate(body)
	}
	rr := doReq(g.r, http.MethodPost, "/v1/notices/", body, "sender-1")
	if rr.Code != http.StatusCreated {
		return rr.Code, domain.Notice{}
	}
	return rr.Code, decodeInto[domain.Notice](t, rr.Body.Bytes())
}

func TestNoticeAPI_UnavailableWithoutAStore(t *testing.T) {
	rig := newNoticeRig(t, true, false)
	if rr := doReq(rig.r, http.MethodPost, "/v1/notices/", map[string]any{"intent_id": rig.intent}, "sender-1"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestNoticeAPI_CreateValidatesAgainstTheIntent(t *testing.T) {
	rig := newNoticeRig(t, true, true)
	code, n := rig.create(t, nil)
	if code != http.StatusCreated || n.Status != domain.NoticeReady || n.IntentVersionID != rig.ivID {
		t.Fatalf("create = %d %+v", code, n)
	}
	if len(rig.authz.calls) != 1 || rig.authz.calls[0] != "NOTIFICATION_SEND" {
		t.Errorf("authorized as %v, want NOTIFICATION_SEND", rig.authz.calls)
	}
	for name, mutate := range map[string]func(map[string]any){
		"no intent":        func(b map[string]any) { delete(b, "intent_id") },
		"no policy basis":  func(b map[string]any) { b["policy_ref"] = "" },
		"no deadline":      func(b map[string]any) { delete(b, "deadline_at") },
		"deadline passed":  func(b map[string]any) { b["deadline_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) },
		"unknown ack kind": func(b map[string]any) { b["ack_requirement"] = "SIGNATURE" },
		"unknown field":    func(b map[string]any) { b["legal_service_complete"] = true },
	} {
		if code, _ := rig.create(t, mutate); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, code)
		}
	}
	rig.authz.err = domain.ErrAuthorizationDenied
	if code, _ := rig.create(t, nil); code != http.StatusForbidden {
		t.Errorf("a caller who may not send: status = %d, want 403", code)
	}
}

func TestNoticeAPI_AnIntentBelowE3IsNotARegulatedNotice(t *testing.T) {
	rig := newNoticeRig(t, false, true)
	if code, _ := rig.create(t, nil); code != http.StatusBadRequest {
		t.Fatalf("an E2 intent must be refused, got %d", code)
	}
}

func TestNoticeAPI_DispatchUsesTheGovernedSendOnceAndOnlyOnce(t *testing.T) {
	rig := newNoticeRig(t, true, true)
	_, n := rig.create(t, nil)

	rr := doReq(rig.r, http.MethodPost, "/v1/notices/"+n.NoticeID+"/dispatch", nil, "sender-1")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d %s", rr.Code, rr.Body.String())
	}
	if rig.del.calls != 1 {
		t.Fatalf("the provider was reached %d times, want 1", rig.del.calls)
	}
	seen := rig.del.seen
	if seen.Body != n.Body || seen.Subject != n.Subject || seen.RecipientAddress != "r@example.com" {
		t.Errorf("the transport was given different content or address: %+v", seen)
	}
	if seen.IntentVersionID != rig.ivID || seen.RenderedContentHash != n.ContentHash || seen.CommunicationClass != "T0" {
		t.Errorf("the delivery is not pinned to the intent version and content hash: %+v", seen)
	}
	got := decodeInto[struct{ Notice domain.Notice }](t, rr.Body.Bytes())
	if got.Notice.Status != domain.NoticeDeliveryInProgess || got.Notice.NotificationID == nil {
		t.Errorf("the notice should own its delivery now: %+v", got.Notice)
	}
	if len(rig.store.byID) != 1 {
		t.Errorf("want one delivery, have %d", len(rig.store.byID))
	}

	// The same dispatch again neither creates nor sends a second delivery.
	if rr := doReq(rig.r, http.MethodPost, "/v1/notices/"+n.NoticeID+"/dispatch", nil, "sender-1"); rr.Code != http.StatusOK {
		t.Fatalf("repeat dispatch = %d", rr.Code)
	}
	if rig.del.calls != 1 || len(rig.store.byID) != 1 {
		t.Errorf("a repeated dispatch must not send again: calls=%d deliveries=%d", rig.del.calls, len(rig.store.byID))
	}
}

func TestNoticeAPI_ASupersededUnsentNoticeCannotBeDispatched(t *testing.T) {
	rig := newNoticeRig(t, true, true)
	_, v1 := rig.create(t, nil)
	rr := doReq(rig.r, http.MethodPost, "/v1/notices/"+v1.NoticeID+"/corrections", map[string]any{"reason": "date was wrong", "subject": "Corrected", "body": "1 February."}, "sender-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("correction = %d %s", rr.Code, rr.Body.String())
	}
	v2 := decodeInto[domain.Notice](t, rr.Body.Bytes())
	if v2.VersionNumber != 2 || v2.SupersedesNoticeID == nil || *v2.SupersedesNoticeID != v1.NoticeID || v2.DeadlineAt == nil {
		t.Errorf("the correction should be version 2, name its prior and keep the deadline: %+v", v2)
	}
	if rr := doReq(rig.r, http.MethodPost, "/v1/notices/"+v1.NoticeID+"/dispatch", nil, "sender-1"); rr.Code != http.StatusConflict {
		t.Errorf("dispatching a superseded version = %d, want 409", rr.Code)
	}
	if rig.del.calls != 0 {
		t.Error("nothing may be sent for a superseded version")
	}
	if rr := doReq(rig.r, http.MethodPost, "/v1/notices/"+v1.NoticeID+"/corrections", map[string]any{"subject": "x", "body": "y"}, "sender-1"); rr.Code != http.StatusBadRequest {
		t.Errorf("a correction needs a reason: %d", rr.Code)
	}
}

func TestNoticeAPI_OnlyTheRecipientCanRespondAndNobodyCanRespondForThem(t *testing.T) {
	rig := newNoticeRig(t, true, true)
	_, n := rig.create(t, nil)
	rig.notices.notices[n.NoticeID].Status = domain.NoticeAckPending // as if delivery had been evidenced
	path := "/v1/notices/" + n.NoticeID + "/acknowledgement"

	if rr := doReq(rig.r, http.MethodPost, path, map[string]any{"action": "ACKNOWLEDGE"}, "operator-9"); rr.Code != http.StatusForbidden {
		t.Fatalf("an operator acknowledging for the recipient = %d, want 403", rr.Code)
	}
	// There is no way to name a different actor or an unauthenticated method.
	for _, body := range []map[string]any{
		{"action": "ACKNOWLEDGE", "actor_principal_id": "recipient-1"},
		{"action": "ACKNOWLEDGE", "method": "EMAIL_OPENED"},
	} {
		if rr := doReq(rig.r, http.MethodPost, path, body, "operator-9"); rr.Code != http.StatusBadRequest {
			t.Errorf("a body naming an actor or method = %d, want 400", rr.Code)
		}
	}
	if rr := doReq(rig.r, http.MethodPost, path, map[string]any{"action": "OPENED"}, "recipient-1"); rr.Code != http.StatusBadRequest {
		t.Errorf("an email-open style action = %d, want 400", rr.Code)
	}
	if rr := doReq(rig.r, http.MethodPost, path, map[string]any{"action": "ACKNOWLEDGE"}, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("no authenticated principal = %d, want 401", rr.Code)
	}

	rr := doReq(rig.r, http.MethodPost, path, map[string]any{"action": "ACKNOWLEDGE", "comment": "read and understood"}, "recipient-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("the recipient's own acknowledgement = %d %s", rr.Code, rr.Body.String())
	}
	got := decodeInto[struct {
		Notice          domain.Notice
		Acknowledgement domain.NoticeAck
	}](t, rr.Body.Bytes())
	if got.Notice.Status != domain.NoticeAcknowledged || got.Acknowledgement.ActorPrincipalID != "recipient-1" || got.Acknowledgement.Method != domain.AckMethodAuthAction {
		t.Errorf("unexpected response: %+v", got)
	}
	if rr := doReq(rig.r, http.MethodPost, path, map[string]any{"action": "ACKNOWLEDGE"}, "recipient-1"); rr.Code != http.StatusConflict {
		t.Errorf("a second response = %d, want 409", rr.Code)
	}
	if len(rig.authz.calls) != 1 {
		t.Errorf("responding to a notice is the recipient's own act and consults no role (only the creation did): %v", rig.authz.calls)
	}
}

func TestNoticeAPI_ReadingAndTheEvidenceBundle(t *testing.T) {
	rig := newNoticeRig(t, true, true)
	_, n := rig.create(t, nil)
	rig.authz.calls = nil

	// The recipient reads their own notice without any role.
	if rr := doReq(rig.r, http.MethodGet, "/v1/notices/"+n.NoticeID, nil, "recipient-1"); rr.Code != http.StatusOK {
		t.Fatalf("recipient read = %d", rr.Code)
	}
	if len(rig.authz.calls) != 0 {
		t.Errorf("the recipient's own read consulted a role: %v", rig.authz.calls)
	}
	// Anyone else needs NOTIFICATION_VIEW.
	rig.authz.err = domain.ErrAuthorizationDenied
	if rr := doReq(rig.r, http.MethodGet, "/v1/notices/"+n.NoticeID, nil, "stranger"); rr.Code != http.StatusForbidden {
		t.Errorf("a stranger = %d, want 403", rr.Code)
	}
	// The bundle is for administrators: even the recipient is not given it without the role.
	if rr := doReq(rig.r, http.MethodGet, "/v1/notices/"+n.NoticeID+"/evidence", nil, "recipient-1"); rr.Code != http.StatusForbidden {
		t.Errorf("the bundle for a caller without the role = %d, want 403", rr.Code)
	}
	rig.authz.err = nil
	rr := doReq(rig.r, http.MethodGet, "/v1/notices/"+n.NoticeID+"/evidence", nil, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("bundle = %d %s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !contains(body, "not a finding that service was legally effective") {
		t.Errorf("the bundle must state the legal boundary: %s", body)
	}
	if rr := doReq(rig.r, http.MethodGet, "/v1/notices/"+uuid.NewString(), nil, "admin-1"); rr.Code != http.StatusNotFound {
		t.Errorf("an unknown notice = %d, want 404", rr.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
