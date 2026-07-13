// Command server is a simulated GTRM regional pool for Phase 1 proof
// (docs/architecture/global-traffic-residency-manager-decision.md §10.1).
//
// It runs in one of two modes (POOL_MODE):
//
//   pool       — a region-tagged backend. Implements BACKEND REGION ASSERTION
//                (§8.2): it rejects any request whose X-Zoiko-Resolved-Region
//                does not match its own POOL_REGION, as a second line of
//                defence against compiler bugs, config drift and
//                direct-to-backend misroutes. On a match it echoes the
//                resolved routing context so tests can prove where a request
//                landed.
//
//   terminator — a residency-neutral incident/safe endpoint. Processes NO
//                tenant data; returns 503 with an incident reference. Used for
//                BLOCK-mode quarantine and the fail-closed catch-all (§8.1, §9.1).
//
// No database, no tenant data — this is a routing-proof fixture only.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	region := env("POOL_REGION", "eu")
	name := env("POOL_NAME", region+"-pool")
	mode := env("POOL_MODE", "pool") // pool | terminator
	port := env("PORT", "8080") // non-root distroless cannot bind <1024

	mux := http.NewServeMux()

	// Liveness/readiness — always 200 while the process is up. Traefik health
	// checks (used only for approved failover, §8.3) probe this.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","pool":"` + name + `"}`))
	})

	if mode == "terminator" {
		mux.HandleFunc("/", terminator(name))
	} else {
		mux.HandleFunc("/", poolHandler(name, region))
	}

	log.Printf("gtrm-pool starting: name=%s region=%s mode=%s port=%s", name, region, mode, port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// poolHandler serves a region-tagged pool with backend region assertion.
func poolHandler(name, region string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resolved := r.Header.Get("X-Zoiko-Resolved-Region")

		// Backend region assertion (§8.2): the gateway must have resolved a
		// region, and it must match this pool. Anything else is a misroute —
		// reject, log the violation, process no tenant data.
		if resolved == "" || resolved != region {
			logDecision(r, name, "region_assertion_failed", false)
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":            "region_assertion_failed",
				"pool":             name,
				"pool_region":      region,
				"resolved_region":  resolved,
				"tenant_processed": false,
			})
			return
		}

		logDecision(r, name, "tenant_residency", true)
		writeJSON(w, http.StatusOK, map[string]any{
			"pool":             name,
			"pool_region":      region,
			"resolved_region":  resolved,
			"resolved_tenant":  r.Header.Get("X-Zoiko-Resolved-Tenant"),
			"gtrm_map_version": r.Header.Get("X-Zoiko-GTRM-Map-Version"),
			"path":             r.URL.Path,
		})
	}
}

// terminator serves the residency-neutral incident/safe endpoint. No tenant
// data is processed; it never routes to a regional pool (§8.1, §9.1).
func terminator(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logDecision(r, name, "residency_unresolved_or_quarantined", false)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"incident":         true,
			"endpoint":         name,
			"reason":           "residency_unresolved_or_quarantined",
			"tenant_processed": false,
			"note":             "residency-neutral endpoint — no tenant data processed",
		})
	}
}

// logDecision emits a structured, single-line JSON route-decision log (§11
// auditability, test G). It contains only routing metadata from trusted,
// gateway-set headers — no secrets, tokens, or tenant content (log hygiene,
// test P). Fields not resolvable at the pool (policy_version,
// data_residency_policy_id) are gateway/compiler-side and intentionally omitted
// here rather than logged as blanks.
func logDecision(r *http.Request, pool, reason string, processed bool) {
	rec := map[string]any{
		"event":            "gtrm_route_decision",
		"pool":             pool,
		"resolved_tenant":  r.Header.Get("X-Zoiko-Resolved-Tenant"),
		"resolved_region":  r.Header.Get("X-Zoiko-Resolved-Region"),
		"gtrm_map_version": r.Header.Get("X-Zoiko-GTRM-Map-Version"),
		"gtrm_state":       r.Header.Get("X-Zoiko-GTRM-State"),
		"decision_reason":  reason,
		"tenant_processed": processed,
		"host":             r.Host,
		"path":             r.URL.Path,
	}
	if b, err := json.Marshal(rec); err == nil {
		log.Println(string(b))
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
