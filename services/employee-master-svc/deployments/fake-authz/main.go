// fake-authz is a minimal stand-in for authorization-svc used solely by the
// limited, self-contained local compose for employee-master-svc.
//
// employee-master-svc is written to be authorization-gated: every handler
// (reads and writes) calls POST /v1/authorize on authorization-svc and fails
// closed (503 authz_unavailable) if it is unreachable. It has no permit-all
// local fallback, so a legitimate local run needs an authorization service
// to answer.
//
// This shim answers every authorize call with a GRANTED decision so all
// employee-master endpoints work without standing up the real
// authorization-svc and its dependencies (its own Postgres, Kafka, MTLS). It
// MUST NOT be used anywhere but a local dev compose.
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

type authorizeResponse struct {
	DecisionOutcome  string `json:"decision_outcome"`
	DecisionBasis    string `json:"decision_basis"`
	AccessDecisionID string `json:"access_decision_id"`
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("POST /v1/authorize", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authorizeResponse{
			DecisionOutcome:  "GRANTED",
			DecisionBasis:    "static-fake-authz-local-dev-only",
			AccessDecisionID: "fake-" + "granted",
		})
	})

	log.Println("fake-authz listening on :8089")
	if err := http.ListenAndServe(":8089", mux); err != nil {
		log.Fatal(err)
	}
}