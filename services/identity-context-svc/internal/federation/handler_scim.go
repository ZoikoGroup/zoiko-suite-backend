package federation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"
)

const maxSCIMBody = 1 << 20

// SCIMRouter serves the SCIM 2.0 endpoints a tenant's directory calls. It is mounted OUTSIDE the platform's
// envelope and replay middleware: a directory has no platform identity to put in those headers, and its replay
// protection is the provisioning ledger. What stands in for identity is the connection's bearer token, with the
// tenant and connection named in the path, so every request is authenticated before anything is read or written.
func (h *Handler) SCIMRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)
	h.RegisterSCIM(r)
	return r
}

// RegisterSCIM mounts the SCIM routes on r. Exposed so the published contract can be checked against the real routes.
func (h *Handler) RegisterSCIM(r chi.Router) {
	r.Route("/scim/v2/{tenantID}/{connectionID}", func(r chi.Router) {
		r.Use(h.scimAuth)
		r.Get("/ServiceProviderConfig", h.scimServiceProviderConfig)
		r.Post("/Users", h.scimCreate)
		r.Get("/Users", h.scimList)
		r.Get("/Users/{id}", h.scimGet)
		r.Put("/Users/{id}", h.scimReplace)
		r.Patch("/Users/{id}", h.scimPatch)
		r.Delete("/Users/{id}", h.scimDelete)
	})
}

type scimCtxKey struct{}

func writeSCIM(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeSCIMError(w http.ResponseWriter, e *SCIMError) { writeSCIM(w, e.Status, e.Body()) }

// scimFail maps an error to a SCIM error response. Internal errors are logged, never returned.
func (h *Handler) scimFail(w http.ResponseWriter, err error) {
	var se *SCIMError
	switch {
	case errors.As(err, &se):
		writeSCIMError(w, se)
	case errors.Is(err, ErrDedupeMismatch):
		writeSCIMError(w, &SCIMError{Status: http.StatusConflict, Detail: "that event identity was already used with a different request"})
	case errors.Is(err, ErrStaleLink):
		writeSCIMError(w, &SCIMError{Status: http.StatusConflict, Detail: "the user was modified concurrently; retry"})
	case errors.Is(err, ErrUserNotFound):
		writeSCIMError(w, &SCIMError{Status: http.StatusNotFound, Detail: "no such user"})
	default:
		h.log.Error("scim request failed", zap.Error(err))
		writeSCIMError(w, &SCIMError{Status: http.StatusInternalServerError, Detail: "internal error"})
	}
}

func bearer(r *http.Request) string {
	const prefix = "bearer "
	v := r.Header.Get("Authorization")
	if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return ""
}

func (h *Handler) scimAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, err := h.prov.AuthenticateSCIM(r.Context(), chi.URLParam(r, "tenantID"), chi.URLParam(r, "connectionID"), bearer(r))
		switch {
		case errors.Is(err, ErrTokenInvalid):
			// one answer for every reason, and nothing written: an unauthenticated caller learns nothing
			w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
			writeSCIMError(w, &SCIMError{Status: http.StatusUnauthorized, Detail: "authentication required"})
			return
		case err != nil:
			h.log.Error("scim authentication could not be decided", zap.Error(err))
			writeSCIMError(w, &SCIMError{Status: http.StatusServiceUnavailable, Detail: "temporarily unavailable"})
			return
		}
		next.ServeHTTP(w, r.WithContext(withValue(r.Context(), scimCtxKey{}, t)))
	})
}

func (h *Handler) scimBase(r *http.Request) string {
	return "/scim/v2/" + chi.URLParam(r, "tenantID") + "/" + chi.URLParam(r, "connectionID")
}

func target(r *http.Request) *SCIMTarget {
	t, _ := r.Context().Value(scimCtxKey{}).(*SCIMTarget)
	return t
}

func (h *Handler) scimBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSCIMBody))
	if err != nil {
		writeSCIMError(w, &SCIMError{Status: http.StatusRequestEntityTooLarge, Type: "invalidSyntax", Detail: "the request body is too large"})
		return nil, false
	}
	return raw, true
}

func (h *Handler) scimRequest(r *http.Request, body []byte) SCIMRequest {
	event := r.Header.Get("X-Event-Id")
	if event == "" {
		event = r.Header.Get("Idempotency-Key")
	}
	corr := r.Header.Get("X-Correlation-ID")
	if corr == "" {
		corr = ulid.Make().String()
	}
	return SCIMRequest{Method: r.Method, Path: r.URL.Path, Body: body, EventID: event, CorrelationID: corr}
}

func (h *Handler) resource(r *http.Request, v *UserView) map[string]any {
	return SCIMResource(v.Link, v.DisplayName, v.Email, v.Link.Attributes, h.scimBase(r)+"/Users/"+v.Link.LinkID)
}

func (h *Handler) scimServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	writeSCIM(w, http.StatusOK, map[string]any{
		"schemas":               []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":                 map[string]any{"supported": true},
		"bulk":                  map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":                map[string]any{"supported": true, "maxResults": 100},
		"changePassword":        map[string]any{"supported": false},
		"sort":                  map[string]any{"supported": false},
		"etag":                  map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{"type": "oauthbearertoken", "name": "Bearer token", "description": "A per-connection bearer token issued by an administrator", "primary": true}},
	})
}

func (h *Handler) scimCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := h.scimBody(w, r)
	if !ok {
		return
	}
	out, err := h.prov.CreateUser(r.Context(), target(r), h.scimRequest(r, body))
	if err != nil {
		h.scimFail(w, err)
		return
	}
	status := http.StatusOK
	if out.Result != nil && out.Result.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Location", h.scimBase(r)+"/Users/"+out.View.Link.LinkID)
	writeSCIM(w, status, h.resource(r, out.View))
}

func (h *Handler) scimReplace(w http.ResponseWriter, r *http.Request) {
	body, ok := h.scimBody(w, r)
	if !ok {
		return
	}
	out, err := h.prov.ReplaceUser(r.Context(), target(r), chi.URLParam(r, "id"), h.scimRequest(r, body))
	if err != nil {
		h.scimFail(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, h.resource(r, out.View))
}

func (h *Handler) scimPatch(w http.ResponseWriter, r *http.Request) {
	body, ok := h.scimBody(w, r)
	if !ok {
		return
	}
	out, err := h.prov.PatchUser(r.Context(), target(r), chi.URLParam(r, "id"), h.scimRequest(r, body))
	if err != nil {
		h.scimFail(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, h.resource(r, out.View))
}

func (h *Handler) scimDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := h.prov.DeleteUser(r.Context(), target(r), chi.URLParam(r, "id"), h.scimRequest(r, nil)); err != nil {
		h.scimFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) scimGet(w http.ResponseWriter, r *http.Request) {
	v, err := h.prov.GetUser(r.Context(), target(r), chi.URLParam(r, "id"))
	if err != nil {
		h.scimFail(w, err)
		return
	}
	writeSCIM(w, http.StatusOK, h.resource(r, v))
}

func (h *Handler) scimList(w http.ResponseWriter, r *http.Request) {
	list, err := h.prov.ListUsers(r.Context(), target(r), r.URL.Query().Get("filter"))
	if err != nil {
		h.scimFail(w, err)
		return
	}
	res := make([]map[string]any, 0, len(list))
	for i := range list {
		res = append(res, h.resource(r, &list[i]))
	}
	writeSCIM(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(res), "startIndex": 1, "itemsPerPage": len(res), "Resources": res})
}

func withValue(ctx context.Context, key, val any) context.Context {
	return context.WithValue(ctx, key, val)
}
