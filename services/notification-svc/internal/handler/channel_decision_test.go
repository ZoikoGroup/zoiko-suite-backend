package handler_test

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/channeldecision"
	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
)

func cdRouter(t *testing.T, authz *stubAuthZ, res *stubResolver, supp handler.SuppressionStore, prefs handler.PreferenceStore, ints handler.IntentStore) chi.Router {
	t.Helper()
	s := newStubStore()
	s.events = &stubPublisher{}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	d := handler.Deps{Store: s, AuthZ: authz, Deliverer: &stubDeliverer{delivered: true}, Recipient: res,
		Preferences: prefs, Intents: ints, RetryPolicy: retry.DefaultPolicy, Log: zap.NewNop()}
	if supp != nil {
		d.Suppressions = supp
	}
	handler.RegisterRoutes(r, handler.New(d))
	return r
}

func TestChannelDecision_ListsEligibleAndRejectedChannels(t *testing.T) {
	authz := &stubAuthZ{}
	r := cdRouter(t, authz, &stubResolver{email: "r@example.com"}, &stubSuppressionStore{}, nil, nil)
	rr := doReq(r, http.MethodPost, "/v1/channel-decision", map[string]any{"legal_entity_id": "le-us", "recipient_principal_id": "bob"}, "alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	got := decodeInto[channeldecision.Result](t, rr.Body.Bytes())
	if len(got.Eligible) != 2 || got.Eligible[0].Channel != "EMAIL" || got.Eligible[1].Channel != "IN_APP" || len(got.Rejected) != 2 {
		t.Fatalf("unexpected decision: %+v", got)
	}
	if len(authz.calls) != 1 || authz.calls[0] != "NOTIFICATION_SEND" {
		t.Fatalf("authorized as %v, want NOTIFICATION_SEND", authz.calls)
	}
}

func TestChannelDecision_SuppressedAddressAndMissingEndpointRemoveEmail(t *testing.T) {
	supp := &stubSuppressionStore{suppressions: []*ledger.EmailSuppression{{TenantID: "tenant-abc", RecipientEmail: "r@example.com", Reason: "HARD_BOUNCE"}}}
	r := cdRouter(t, &stubAuthZ{}, &stubResolver{email: "r@example.com"}, supp, nil, nil)
	got := decodeInto[channeldecision.Result](t, doReq(r, http.MethodPost, "/v1/channel-decision",
		map[string]any{"legal_entity_id": "le-us", "recipient_principal_id": "bob"}, "alice").Body.Bytes())
	if len(got.Eligible) != 1 || got.Eligible[0].Channel != "IN_APP" {
		t.Fatalf("a suppressed address leaves only in-app: %+v", got)
	}

	// Suppression state not wired: fail closed for email, never assume it is fine.
	r = cdRouter(t, &stubAuthZ{}, &stubResolver{email: "r@example.com"}, nil, nil, nil)
	got = decodeInto[channeldecision.Result](t, doReq(r, http.MethodPost, "/v1/channel-decision",
		map[string]any{"legal_entity_id": "le-us", "recipient_principal_id": "bob", "channels": []string{"EMAIL"}}, "alice").Body.Bytes())
	if len(got.Eligible) != 0 || got.Code == "" {
		t.Fatalf("email without a readable suppression state must not be eligible: %+v", got)
	}
}

func TestChannelDecision_RefusalsAndValidation(t *testing.T) {
	ok := &stubResolver{email: "r@example.com"}
	// Not allowed to send for the entity: 403, nothing revealed.
	r := cdRouter(t, &stubAuthZ{err: domain.ErrAuthorizationDenied}, ok, &stubSuppressionStore{}, nil, nil)
	if rr := doReq(r, http.MethodPost, "/v1/channel-decision", map[string]any{"legal_entity_id": "le-us", "recipient_principal_id": "bob"}, "alice"); rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	r = cdRouter(t, &stubAuthZ{}, ok, &stubSuppressionStore{}, nil, nil)
	for name, tc := range map[string]struct {
		body map[string]any
		who  string
		want int
	}{
		"no recipient":      {map[string]any{"legal_entity_id": "le-us"}, "alice", http.StatusBadRequest},
		"no entity":         {map[string]any{"recipient_principal_id": "bob"}, "alice", http.StatusBadRequest},
		"no principal":      {map[string]any{"legal_entity_id": "le-us", "recipient_principal_id": "bob"}, "", http.StatusUnauthorized},
		"marketing refused": {map[string]any{"legal_entity_id": "le-us", "recipient_principal_id": "bob", "communication_class": "M1"}, "alice", http.StatusUnprocessableEntity},
		"unknown field":     {map[string]any{"legal_entity_id": "le-us", "recipient_principal_id": "bob", "x": 1}, "alice", http.StatusBadRequest},
	} {
		if rr := doReq(r, http.MethodPost, "/v1/channel-decision", tc.body, tc.who); rr.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s)", name, rr.Code, tc.want, rr.Body.String())
		}
	}
}

func TestChannelDecision_AnIntentDecidesClassAndChannels(t *testing.T) {
	ints := newStubIntents()
	setup, _ := intentRouter(t, ints, &stubDeliverer{delivered: true}, nil)
	intentID := publishedIntent(t, setup, "payroll.payslip_available", func(b map[string]any) { b["allowed_channels"] = []string{"EMAIL"} })

	r := cdRouter(t, &stubAuthZ{}, &stubResolver{email: "r@example.com"}, &stubSuppressionStore{}, nil, ints)
	rr := doReq(r, http.MethodPost, "/v1/channel-decision", map[string]any{"intent_id": intentID, "recipient_principal_id": "bob"}, "alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	got := decodeInto[channeldecision.Result](t, rr.Body.Bytes())
	if len(got.Eligible) != 1 || got.Eligible[0].Channel != "EMAIL" {
		t.Fatalf("the intent allows email only: %+v", got)
	}
	if rr := doReq(r, http.MethodPost, "/v1/channel-decision", map[string]any{"intent_id": intentID, "recipient_principal_id": "bob", "communication_class": "S0"}, "alice"); rr.Code != http.StatusBadRequest {
		t.Fatalf("a class that conflicts with the intent: %d", rr.Code)
	}
	if rr := doReq(r, http.MethodPost, "/v1/channel-decision", map[string]any{"intent_id": intentID, "legal_entity_id": "le-other", "recipient_principal_id": "bob"}, "alice"); rr.Code != http.StatusBadRequest {
		t.Fatalf("a different legal entity than the intent's: %d", rr.Code)
	}
}
