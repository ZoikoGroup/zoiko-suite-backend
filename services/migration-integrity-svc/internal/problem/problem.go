// Package problem writes RFC 9457 "Problem Details for HTTP APIs" error
// responses (Content-Type application/problem+json). Every error this
// service returns from its own code goes through Write, so clients can rely
// on one shape: type, title, status, detail, instance and a stable code.
package problem

import (
	"encoding/json"
	"net/http"
)

// typeBase prefixes every problem type URI; the suffix is the stable code.
const typeBase = "https://api.zoikosuite.com/problems/migration-integrity/"

// Details is one RFC 9457 problem document.
type Details struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	// Code is a stable, machine-readable extension member (e.g.
	// "segregation-of-duties"); Type ends with the same value.
	Code string `json:"code"`
}

// Write sends a problem response. code is a short kebab-case identifier;
// title is the human summary of that code; detail explains this occurrence.
func Write(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	body := Details{
		Type:     typeBase + code,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
		Code:     code,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
