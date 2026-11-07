package auth

import (
	"encoding/json"
	"net/http"

	"zoiko.io/identity-context-svc/internal/keys"
	"zoiko.io/identity-envelope/apicontract"
)

// NewJWKSHandler publishes every key in the ring (the active one and any
// being rotated in or out), so a relying party can verify every live token
// through a rotation without a restart (AU-05).
func NewJWKSHandler(ring *keys.Keyring) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		set, err := ring.JWKS()
		if err != nil {
			apicontract.WriteError(w, r, http.StatusInternalServerError, "key_set_unavailable", "key set unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")
		// Relying parties cache by kid and refetch on an unknown one; a short
		// public max-age keeps intermediaries from pinning a stale set.
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": set})
	}
}
