package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrRegistryUnavailable means the contract registry could not answer. A gate
// that needs a contract must fail closed on it: "could not check" is not
// "compatible" (INT-CTRL-005).
var ErrRegistryUnavailable = errors.New("contract registry unavailable")

// maxSchemaBytes bounds what the client will read from the registry.
const maxSchemaBytes = 1 << 20

// Schema is one registered payload contract (schema-registry-svc EventSchema).
type Schema struct {
	EventName         string          `json:"event_name"`
	Version           int             `json:"version"`
	JSONSchema        json.RawMessage `json:"json_schema"`
	CompatibilityMode string          `json:"compatibility_mode"`
	OwningService     string          `json:"owning_service"`
}

// Registry resolves a registered contract. version 0 means "latest".
type Registry interface {
	Schema(ctx context.Context, eventName string, version int) (*Schema, error)
}

// HTTPRegistry reads contracts from schema-registry-svc over HTTP.
//
// Reads there require a resolved principal, so the caller supplies the
// workload identity the registry should see. Every call has an explicit
// timeout (§8: no infinite waits).
type HTTPRegistry struct {
	BaseURL   string
	Principal string
	HTTP      *http.Client
}

// NewHTTPRegistry builds a client with a bounded timeout.
func NewHTTPRegistry(baseURL, principal string, timeout time.Duration) (*HTTPRegistry, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("registry base URL must be absolute")
	}
	if strings.TrimSpace(principal) == "" {
		return nil, errors.New("registry principal is required")
	}
	if timeout <= 0 {
		return nil, errors.New("registry timeout must be positive")
	}
	return &HTTPRegistry{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Principal: principal,
		HTTP:      &http.Client{Timeout: timeout},
	}, nil
}

// Schema implements Registry. 404 becomes INT-SCHEMA-004 (no such contract);
// everything else that is not a 200 is ErrRegistryUnavailable.
func (r *HTTPRegistry) Schema(ctx context.Context, eventName string, version int) (*Schema, error) {
	if eventName == "" || strings.ContainsAny(eventName, "/?#") {
		return nil, New(CodeSchemaMismatch, "event name is not a valid contract name")
	}
	path := "/v1/schemas/" + url.PathEscape(eventName) + "/versions/latest"
	if version > 0 {
		path = "/v1/schemas/" + url.PathEscape(eventName) + "/versions/" + strconv.Itoa(version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegistryUnavailable, err)
	}
	req.Header.Set("X-Principal-Id", r.Principal)
	if c, ok := FromContext(ctx); ok {
		req.Header.Set(HeaderCorrelationID, c.CorrelationID)
		req.Header.Set(HeaderTenantID, c.TenantID)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegistryUnavailable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, Newf(CodeSchemaMismatch, "no registered contract %s (version %s)", eventName, versionLabel(version))
	default:
		return nil, fmt.Errorf("%w: status %d", ErrRegistryUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSchemaBytes+1))
	if err != nil || len(body) > maxSchemaBytes {
		return nil, fmt.Errorf("%w: unreadable or oversized response", ErrRegistryUnavailable)
	}
	var s Schema
	if err := json.Unmarshal(body, &s); err != nil || s.Version < 1 || len(s.JSONSchema) == 0 {
		return nil, fmt.Errorf("%w: malformed response", ErrRegistryUnavailable)
	}
	if version > 0 && s.Version != version {
		return nil, fmt.Errorf("%w: registry returned version %d, asked for %d", ErrRegistryUnavailable, s.Version, version)
	}
	return &s, nil
}

func versionLabel(v int) string {
	if v <= 0 {
		return "latest"
	}
	return strconv.Itoa(v)
}

// ---- payload validation ------------------------------------------------------

// schemaNode is the part of JSON Schema the registry understands and enforces
// for compatibility: type, properties, required, items.
type schemaNode struct {
	Type       string                `json:"type"`
	Properties map[string]schemaNode `json:"properties"`
	Required   []string              `json:"required"`
	Items      *schemaNode           `json:"items"`
}

// ValidatePayload checks payload against the registered schema subset and
// returns one violation per problem (empty means valid). Fields the schema does
// not mention are allowed: contracts evolve by adding optional fields. A schema
// or payload that cannot be parsed is itself a violation, never a pass.
func ValidatePayload(schema, payload json.RawMessage) []string {
	var root schemaNode
	if err := json.Unmarshal(schema, &root); err != nil {
		return []string{"schema is not a readable contract"}
	}
	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return []string{"payload is not valid JSON"}
	}
	var out []string
	validateNode(&out, "$", root, doc)
	return out
}

func validateNode(out *[]string, path string, n schemaNode, v any) {
	if n.Type != "" && !typeMatches(n.Type, v) {
		*out = append(*out, fmt.Sprintf("%s: expected %s, got %s", path, n.Type, jsonType(v)))
		return
	}
	switch t := v.(type) {
	case map[string]any:
		for _, req := range n.Required {
			if _, ok := t[req]; !ok {
				*out = append(*out, fmt.Sprintf("%s.%s: required field missing", path, req))
			}
		}
		for name, child := range n.Properties {
			if val, ok := t[name]; ok {
				validateNode(out, path+"."+name, child, val)
			}
		}
	case []any:
		if n.Items != nil {
			for i, el := range t {
				validateNode(out, fmt.Sprintf("%s[%d]", path, i), *n.Items, el)
			}
		}
	}
}

func typeMatches(want string, v any) bool {
	switch want {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	}
	return false // an unknown declared type cannot be satisfied
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// CheckPayload fetches the contract and validates payload against it. A
// mismatch is INT-SCHEMA-004; an unreachable registry is returned as
// ErrRegistryUnavailable so the caller can fail closed or retry without
// pretending the payload was checked.
func CheckPayload(ctx context.Context, reg Registry, eventName string, version int, payload json.RawMessage) (*Schema, error) {
	s, err := reg.Schema(ctx, eventName, version)
	if err != nil {
		return nil, err
	}
	if v := ValidatePayload(s.JSONSchema, payload); len(v) > 0 {
		return s, Newf(CodeSchemaMismatch, "payload does not satisfy %s v%d: %s", s.EventName, s.Version, strings.Join(v, "; "))
	}
	return s, nil
}
