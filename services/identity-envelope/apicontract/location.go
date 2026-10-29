package apicontract

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

func SetLocation(w http.ResponseWriter, location string) {
	location = strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(location))
	if location == "" {
		return
	}
	w.Header().Set("Location", location)
}

func ResourceLocation(collection, id string) string {
	collection = strings.TrimRight(strings.TrimSpace(collection), "/")
	if id == "" {
		return collection
	}
	return collection + "/" + url.PathEscape(id)
}

func OperationLocation(operationID string) string {
	return "/v1/operations/" + url.PathEscape(operationID)
}

func Created(w http.ResponseWriter, location string, body any) {
	SetLocation(w, location)
	WriteJSON(w, http.StatusCreated, body)
}

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}
