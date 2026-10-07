package handler_test

import (
	"net/http"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// expected_version on the protected updates is optional and, when given,
// reaches the store's guarded write; a stale one is 409.

func TestExpectedVersion_PassedToStore(t *testing.T) {
	s := ownRoleStore()
	if w := postAsAdmin(s, "/v1/admin/roles/r-1/retire", `{"expected_version":7}`); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotExpectedVersion != 7 {
		t.Errorf("store got expected_version %d, want 7", s.gotExpectedVersion)
	}
}

func TestExpectedVersion_OmittedIsUnchecked(t *testing.T) {
	for _, body := range []string{``, `{}`} {
		s := ownRoleStore()
		if w := postAsAdmin(s, "/v1/admin/roles/r-1/retire", body); w.Code != http.StatusOK {
			t.Fatalf("body %q: want 200 (existing callers send no version), got %d", body, w.Code)
		}
		if s.gotExpectedVersion != 0 {
			t.Errorf("body %q: store got expected_version %d, want 0 (unchecked)", body, s.gotExpectedVersion)
		}
	}
}

func TestExpectedVersion_StaleIs409(t *testing.T) {
	s := ownRoleStore()
	s.setActiveErr = domain.ErrVersionConflict
	w := postAsAdmin(s, "/v1/admin/roles/r-1/retire", `{"expected_version":1}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale expected_version: want 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExpectedVersion_InvalidBodyIs400(t *testing.T) {
	for _, body := range []string{`{"expected_version":-1}`, `{"expected_version":"x"}`} {
		if w := postAsAdmin(ownRoleStore(), "/v1/admin/roles/r-1/retire", body); w.Code != http.StatusBadRequest {
			t.Errorf("body %s: want 400, got %d", body, w.Code)
		}
	}
}
