package handler_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
)

// stubPrefs keeps profiles by principal and applies the REAL validation.
type stubPrefs struct {
	byPrincipal map[string]*domain.RecipientPreferences
}

func (s *stubPrefs) GetPreferences(_ context.Context, id string) (*domain.RecipientPreferences, error) {
	if p, ok := s.byPrincipal[id]; ok {
		return p, nil
	}
	return nil, domain.ErrPreferencesNotFound
}

func (s *stubPrefs) SetPreferences(_ context.Context, p domain.SetPreferencesParams) (*domain.RecipientPreferences, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	out := &domain.RecipientPreferences{TenantID: "tenant-abc", PrincipalID: p.PrincipalID, TimeZone: p.TimeZone,
		QuietStart: p.QuietStart, QuietEnd: p.QuietEnd, MutedChannels: p.MutedChannels, Version: 1, UpdatedBy: p.UpdatedBy, UpdatedAt: time.Now()}
	if out.MutedChannels == nil {
		out.MutedChannels = []string{}
	}
	if s.byPrincipal == nil {
		s.byPrincipal = map[string]*domain.RecipientPreferences{}
	}
	s.byPrincipal[p.PrincipalID] = out
	return out, nil
}

func prefRouter(prefs handler.PreferenceStore) chi.Router {
	s := newStubStore()
	s.events = &stubPublisher{}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterRoutes(r, handler.New(handler.Deps{Store: s, AuthZ: &stubAuthZ{}, Deliverer: &stubDeliverer{delivered: true},
		Recipient: &stubResolver{email: "r@example.com"}, Preferences: prefs, RetryPolicy: retry.DefaultPolicy, Log: zap.NewNop()}))
	return r
}

func TestPreferencesAPI_AnUnconfiguredServiceAnswers503(t *testing.T) {
	rr := doReq(prefRouter(nil), http.MethodGet, "/v1/preferences/", nil, "alice")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestPreferencesAPI_APersonSetsAndReadsOnlyTheirOwnProfile(t *testing.T) {
	store := &stubPrefs{}
	r := prefRouter(store)

	if rr := doReq(r, http.MethodGet, "/v1/preferences/", nil, "alice"); rr.Code != http.StatusNotFound {
		t.Fatalf("no profile yet: status = %d, want 404", rr.Code)
	}
	if rr := doReq(r, http.MethodGet, "/v1/preferences/", nil, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: status = %d, want 401", rr.Code)
	}

	// The principal is the authenticated caller and never part of the body: a body that
	// names one is refused outright, so nobody can write another person's profile.
	if rr := doReq(r, http.MethodPost, "/v1/preferences/", map[string]any{"time_zone": "UTC", "principal_id": "bob"}, "alice"); rr.Code != http.StatusBadRequest {
		t.Fatalf("a body naming a principal: status = %d, want 400", rr.Code)
	}
	if _, ok := store.byPrincipal["bob"]; ok {
		t.Fatal("a caller must not be able to write another principal's profile")
	}

	rr := doReq(r, http.MethodPost, "/v1/preferences/", map[string]any{
		"time_zone": "Europe/London", "quiet_start": "22:00", "quiet_end": "07:00", "muted_channels": []string{"SMS"}}, "alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeInto[domain.RecipientPreferences](t, rr.Body.Bytes())
	if got.PrincipalID != "alice" || got.UpdatedBy != "alice" {
		t.Fatalf("profile belongs to the caller, got %+v", got)
	}

	if rr := doReq(r, http.MethodGet, "/v1/preferences/", nil, "alice"); rr.Code != http.StatusOK {
		t.Fatalf("read back: %d", rr.Code)
	}
	if rr := doReq(r, http.MethodGet, "/v1/preferences/", nil, "bob"); rr.Code != http.StatusNotFound {
		t.Fatalf("bob has no profile: %d", rr.Code)
	}
}

func TestPreferencesAPI_InvalidProfilesAreRefused(t *testing.T) {
	r := prefRouter(&stubPrefs{})
	for name, body := range map[string]map[string]any{
		"no zone":        {"quiet_start": "22:00", "quiet_end": "07:00"},
		"unknown zone":   {"time_zone": "Mars/Olympus"},
		"half a window":  {"time_zone": "UTC", "quiet_start": "22:00"},
		"bad clock":      {"time_zone": "UTC", "quiet_start": "10pm", "quiet_end": "07:00"},
		"unknown muting": {"time_zone": "UTC", "muted_channels": []string{"FAX"}},
	} {
		if rr := doReq(r, http.MethodPost, "/v1/preferences/", body, "alice"); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, rr.Code, rr.Body.String())
		}
	}
}
