package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/handler"
	svcmiddleware "zoiko.io/configuration-feature-flag-svc/internal/middleware"
	"zoiko.io/configuration-feature-flag-svc/internal/vaultref"
)

type fakeVault struct {
	governed map[string]bool
	err      error
	asked    []string
	tenant   string
}

func (f *fakeVault) Governed(_ context.Context, tenantID, _ string, path string) (bool, error) {
	f.asked = append(f.asked, path)
	f.tenant = tenantID
	return f.governed[path], f.err
}

func refRouter(s *stubStore, v vaultref.Verifier) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, testAuthz(), testAuthzScopeID, testMetrics(), zap.NewNop())
	if v != nil {
		h.WithSecretReferences(v)
	}
	handler.RegisterRoutes(r, h)
	return r
}

func postConfig(r chi.Router, value string) *httptest.ResponseRecorder {
	body := `{"key":"mail.smtp","value":` + value + `,"environment":"production","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// S3-B3: a secret reference is accepted only when the vault governs its path; nothing is stored otherwise, and the
// vault is never asked for a value.
func TestSecretRefs_AReferenceMustNameAPathTheVaultGoverns(t *testing.T) {
	store := &stubStore{configEntry: &domain.ConfigEntry{ConfigID: "c-1", Key: "mail.smtp"}, configEntryCreated: true}
	v := &fakeVault{governed: map[string]bool{"kv/smtp": true}}
	r := refRouter(store, v)

	if w := postConfig(r, `"secret://production/kv/smtp"`); w.Code != http.StatusCreated {
		t.Fatalf("a governed reference is accepted: %d %s", w.Code, w.Body.String())
	}
	if len(v.asked) != 1 || v.asked[0] != "kv/smtp" || v.tenant != testTenant {
		t.Fatalf("the vault is asked about the path, in the caller's tenant: %v %q", v.asked, v.tenant)
	}

	store.gotUpsertConfig = domain.UpsertConfigEntryParams{}
	w := postConfig(r, `"secret://production/kv/missing"`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "secret_reference_unresolved") {
		t.Fatalf("a dangling reference is refused: %d %s", w.Code, w.Body.String())
	}
	if store.gotUpsertConfig.Key != "" {
		t.Fatal("a refused reference must not reach the store")
	}
}

func TestSecretRefs_AnOutageRefusesTheWriteAndNestedReferencesAreChecked(t *testing.T) {
	store := &stubStore{configEntry: &domain.ConfigEntry{ConfigID: "c-1"}, configEntryCreated: true}
	down := &fakeVault{err: vaultref.ErrUnavailable}
	w := postConfig(refRouter(store, down), `"secret://production/kv/smtp"`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "secret_vault_unavailable") {
		t.Fatalf("an unreachable vault is not permission: %d %s", w.Code, w.Body.String())
	}
	if w = postConfig(refRouter(store, &fakeVault{err: errors.New("x")}), `"secret://production/kv/smtp"`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("any failure refuses: %d", w.Code)
	}

	v := &fakeVault{governed: map[string]bool{"kv/a": true, "kv/b": true}}
	if w = postConfig(refRouter(store, v), `{"user":"x","password":"secret://production/kv/a","extra":["secret://production/kv/b","plain"]}`); w.Code != http.StatusCreated {
		t.Fatalf("nested references are accepted when governed: %d %s", w.Code, w.Body.String())
	}
	if len(v.asked) != 2 {
		t.Fatalf("every nested reference is checked once: %v", v.asked)
	}
	v = &fakeVault{governed: map[string]bool{"kv/a": true}}
	if w = postConfig(refRouter(store, v), `{"password":"secret://production/kv/a","token":"secret://production/kv/zzz"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("one dangling nested reference refuses the write: %d", w.Code)
	}
}

func TestSecretRefs_PlainValuesAreNeverSentToTheVaultAndNoVerifierMeansShapeOnly(t *testing.T) {
	store := &stubStore{configEntry: &domain.ConfigEntry{ConfigID: "c-1"}, configEntryCreated: true}
	v := &fakeVault{}
	if w := postConfig(refRouter(store, v), `100`); w.Code != http.StatusCreated || len(v.asked) != 0 {
		t.Fatalf("a plain value needs no vault: %d asked=%v", w.Code, v.asked)
	}
	if w := postConfig(refRouter(store, nil), `"secret://production/kv/anything"`); w.Code != http.StatusCreated {
		t.Fatalf("with no verifier wired the reference is checked for shape only: %d", w.Code)
	}
	raw, _ := json.Marshal("secret://production/kv/x")
	_ = raw
}
