package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/vaultref"
)

// WithSecretReferences makes every config write that carries a secret://<environment>/<path> reference check, before
// anything is stored, that the vault governs the path (S3-B3). Without one the reference is checked for shape only.
func (h *Handler) WithSecretReferences(v vaultref.Verifier) *Handler {
	h.secretRefs = v
	return h
}

const maxSecretRefsPerWrite = 20

// secretReferencesIn returns the distinct well-formed secret references found anywhere in the values, as vault paths.
func secretReferencesIn(values ...json.RawMessage) []string {
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if strings.HasPrefix(t, domain.SecretReferencePrefix) {
				raw, _ := json.Marshal(t)
				if _, path, ok := domain.ParseSecretReference(raw); ok {
					seen[path] = true
				}
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	for _, raw := range values {
		var v any
		if len(raw) > 0 && json.Unmarshal(raw, &v) == nil {
			walk(v)
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// verifySecretRefs refuses the write (and answers) when a reference names a path the vault does not govern, or when the
// vault cannot say. It returns true when the write may go on.
func (h *Handler) verifySecretRefs(w http.ResponseWriter, r *http.Request, tenantID, correlationID string, values ...json.RawMessage) bool {
	if h.secretRefs == nil {
		return true
	}
	paths := secretReferencesIn(values...)
	if len(paths) > maxSecretRefsPerWrite {
		writeError(w, r, http.StatusBadRequest, "too_many_secret_references", "a write carries at most 20 secret references")
		return false
	}
	for _, p := range paths {
		ok, err := h.secretRefs.Governed(r.Context(), tenantID, correlationID, p)
		switch {
		case errors.Is(err, vaultref.ErrUnavailable):
			writeError(w, r, http.StatusServiceUnavailable, "secret_vault_unavailable",
				"the vault could not confirm the secret reference; nothing was stored")
			return false
		case err != nil:
			writeError(w, r, http.StatusServiceUnavailable, "secret_vault_unavailable", "")
			return false
		case !ok:
			writeError(w, r, http.StatusUnprocessableEntity, "secret_reference_unresolved",
				"no active vault policy governs secret path "+p)
			return false
		}
	}
	return true
}
