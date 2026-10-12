// fake-svcs is a minimal two-in-one stand-in, used solely by the limited,
 // self-contained local compose for general-ledger-svc.
 //
 // general-ledger-svc ships fail-closed on two dependencies for every journal
 // mutation:
 //
 //  1. authorization-svc  — every write calls POST /v1/authorize and refuses the
 //     action unless the decision is GRANTED. There is no permit-all fallback.
 //  2. financial-close-svc — create/post/reverse also call
 //     GET /v1/close/periods/status and refuse unless the period is open.
 //
 // This shim answers authorize with GRANTED and periods/status with OPEN so all
 // journal endpoints work without standing up either real service plus their
 // dependencies. It listens on two ports — 8089 (authz) and 8104 (close) —
 // matching the two base URLs general-ledger is pointed at. It MUST NOT be used
 // anywhere but a local dev compose.
 package main
 
 import (
 	"encoding/json"
 	"log"
 	"net/http"
 	"time"
 )
 
 type authorizeResponse struct {
 	DecisionOutcome  string `json:"decision_outcome"`
 	DecisionBasis    string `json:"decision_basis"`
 	AccessDecisionID string `json:"access_decision_id"`
 }
 
 type periodStatusResponse struct {
 	CloseStatus     string     `json:"close_status"`
 	PeriodState     string     `json:"period_state"`
 	PostingPolicy   string     `json:"posting_policy"`
 	ReopenExpiresAt *time.Time `json:"reopen_expires_at,omitempty"`
 }
 
 func handler() http.Handler {
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
 			DecisionBasis:    "static-fake-svcs-local-dev-only",
 			AccessDecisionID: "fake-granted",
 		})
 	})
 
 	mux.HandleFunc("GET /v1/close/periods/status", func(w http.ResponseWriter, r *http.Request) {
 		w.Header().Set("Content-Type", "application/json")
 		// Allow testing different period states via query params
 		periodState := r.URL.Query().Get("period_state")
 		postingPolicy := r.URL.Query().Get("posting_policy")
 		reopenExpiresAt := r.URL.Query().Get("reopen_expires_at")
 
 		resp := periodStatusResponse{
 			CloseStatus: "OPEN",
 			PeriodState: "OPEN",
 			PostingPolicy: "OPEN",
 		}
 
 		switch periodState {
 		case "SOFT_CLOSE":
 			resp.CloseStatus = "LOCKED"
 			resp.PeriodState = "SOFT_CLOSE"
 			resp.PostingPolicy = "RESTRICTED"
 		case "CLOSE_REVIEW":
 			resp.CloseStatus = "LOCKED"
 			resp.PeriodState = "CLOSE_REVIEW"
 			resp.PostingPolicy = "CLOSE_JOURNALS_ONLY"
 		case "HARD_CLOSED":
 			resp.CloseStatus = "LOCKED"
 			resp.PeriodState = "HARD_CLOSED"
 			resp.PostingPolicy = "CLOSED"
 		case "AUTHORIZED_REOPEN":
 			resp.CloseStatus = "OPEN"
 			resp.PeriodState = "AUTHORIZED_REOPEN"
 			resp.PostingPolicy = "REOPENED"
 			if reopenExpiresAt != "" {
 				if t, err := time.Parse(time.RFC3339, reopenExpiresAt); err == nil {
 					resp.ReopenExpiresAt = &t
 				}
 			} else {
 				// Default to 1 hour from now
 				t := time.Now().Add(time.Hour)
 				resp.ReopenExpiresAt = &t
 			}
 		case "RECLOSED":
 			resp.CloseStatus = "LOCKED"
 			resp.PeriodState = "RECLOSED"
 			resp.PostingPolicy = "CLOSED"
 		}
 
 		_ = json.NewEncoder(w).Encode(resp)
 	})
 
 	return mux
 }

func serve(name, addr string, h http.Handler) {
	log.Printf("fake-svcs %s listening on %s", name, addr)
	if err := http.ListenAndServe(addr, h); err != nil {
		log.Fatalf("fake-svcs %s: %v", name, err)
	}
}

func main() {
	h := handler()
	go serve("authorize-shim(8089)", ":8089", h)
	serve("close-shim(8104)", ":8104", h)
}