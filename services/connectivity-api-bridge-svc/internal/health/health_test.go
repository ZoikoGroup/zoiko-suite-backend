package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestLivenessIsOK(t *testing.T) {
	w := httptest.NewRecorder()
	Handler()(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz = %d %s", w.Code, w.Body.String())
	}
}

func TestReadinessReflectsDB(t *testing.T) {
	w := httptest.NewRecorder()
	ReadyHandler(fakePinger{})(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("readyz with healthy db = %d", w.Code)
	}
	w = httptest.NewRecorder()
	ReadyHandler(fakePinger{err: errors.New("down")})(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"status":"unavailable"`) {
		t.Fatalf("readyz with failing db = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	ReadyHandler(nil)(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with no pool = %d", w.Code)
	}
}
