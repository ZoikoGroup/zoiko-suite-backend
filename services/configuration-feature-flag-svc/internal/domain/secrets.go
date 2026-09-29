package domain

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// ── Secrets: references only, bound to their environment (INV-09/10/11/26) ────

// ParseSecretReference splits a reference of the form
// secret://<environment>/<path> into its environment and path. ok is false
// for anything else — including the bare secret://<path> form, which names no
// environment and so cannot be checked against the one being written.
func ParseSecretReference(value json.RawMessage) (environment, path string, ok bool) {
	var s string
	if err := json.Unmarshal(value, &s); err != nil || !strings.HasPrefix(s, SecretReferencePrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, SecretReferencePrefix)
	i := strings.IndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	environment, path = rest[:i], rest[i+1:]
	if !secretRefEnvironment.MatchString(environment) {
		return "", "", false
	}
	return environment, path, true
}

var secretRefEnvironment = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// secretMaterialPatterns match credential material with a recognisable shape.
// Deliberately narrow: each is a format with a fixed prefix or structure, so a
// legitimate configuration value does not trip it by accident.
var secretMaterialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*(PRIVATE KEY|CERTIFICATE REQUEST|OPENSSH)[A-Z ]*-----`),
	regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),                                     // AWS access key id
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),                                  // GitHub token
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),                                  // Slack token
	regexp.MustCompile(`\b(sk|rk)_live_[A-Za-z0-9]{16,}`),                                 // Stripe live key
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWT
	regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`),                   // credentials in a URI
}

// credentialFieldNames are object keys whose string value is a credential by
// name. A structured value carrying one must carry a reference, not material.
var credentialFieldNames = map[string]bool{
	"password": true, "passwd": true, "secret": true, "client_secret": true,
	"api_key": true, "apikey": true, "access_token": true, "refresh_token": true,
	"private_key": true, "token": true, "auth_token": true,
}

// LooksLikeSecretMaterial reports whether value carries credential material:
// a string with a recognisable credential shape anywhere in it, or a
// credential-named field holding anything other than a secret reference.
// INV-09 applies to every key — a key not declared SECRET_REFERENCE_ONLY is
// not thereby licensed to hold a password.
func LooksLikeSecretMaterial(value json.RawMessage) bool {
	var v any
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return false
	}
	return materialIn(v)
}

func materialIn(v any) bool {
	switch t := v.(type) {
	case string:
		for _, re := range secretMaterialPatterns {
			if re.MatchString(t) {
				return true
			}
		}
	case []any:
		for _, el := range t {
			if materialIn(el) {
				return true
			}
		}
	case map[string]any:
		for k, el := range t {
			if s, ok := el.(string); ok && s != "" && credentialFieldNames[strings.ToLower(k)] &&
				!strings.HasPrefix(s, SecretReferencePrefix) {
				return true
			}
			if materialIn(el) {
				return true
			}
		}
	}
	return false
}
