package health

import (
	"encoding/json"
	"net/http"
)

type Response struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Response{Status: "UP", Service: "esignature-integration-svc"})
	}
}
