package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ClientRegistry authenticates the OAuth clients a person may log in through
// (Group 1 register R-1).
//
// Before it existed /v1/authenticate minted tokens with no azp, so
// IDP_CLIENT_CHANNELS had nothing to match and every human session's source
// channel was the client's own assertion. A client named in the request body
// would be no better than that header, so azp is set only for a CONFIDENTIAL
// client that proves a secret: the console's server tier holds it, the browser
// never does.
//
// Secrets are high-entropy machine credentials, so they are stored as SHA-256
// digests and compared in constant time; argon2 exists to slow guessing of
// low-entropy human passwords and would only add cost here.
type ClientRegistry struct {
	digests  map[string][]byte
	required bool
}

// ErrClientInvalid is an unknown client id or a wrong secret.
var ErrClientInvalid = errors.New("invalid client credentials")

// ErrClientRequired is a login without a client where one is mandatory.
var ErrClientRequired = errors.New("client authentication required")

// LoadClientRegistry reads path, a JSON object {"<client_id>":"sha256:<hex>"}.
// An empty path yields an empty registry. required makes a login without a
// client an error.
func LoadClientRegistry(path string, required bool) (*ClientRegistry, error) {
	r := &ClientRegistry{digests: map[string][]byte{}, required: required}
	if path == "" {
		return r, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read IDP_CLIENT_SECRETS_FILE: %w", err)
	}
	var entries map[string]string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse IDP_CLIENT_SECRETS_FILE: %w", err)
	}
	for id, enc := range entries {
		hexDigest, ok := strings.CutPrefix(strings.TrimSpace(enc), "sha256:")
		if !ok {
			return nil, fmt.Errorf("client %q: secret must be \"sha256:<hex>\"", id)
		}
		d, err := hex.DecodeString(hexDigest)
		if err != nil || len(d) != sha256.Size {
			return nil, fmt.Errorf("client %q: not a SHA-256 hex digest", id)
		}
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("IDP_CLIENT_SECRETS_FILE has an empty client id")
		}
		r.digests[strings.TrimSpace(id)] = d
	}
	if required && len(r.digests) == 0 {
		return nil, errors.New("IDP_CLIENT_AUTH=required but IDP_CLIENT_SECRETS_FILE registers no clients")
	}
	return r, nil
}

// NewClientRegistry builds a registry from plaintext secrets (tests and tools).
func NewClientRegistry(secrets map[string]string, required bool) *ClientRegistry {
	r := &ClientRegistry{digests: map[string][]byte{}, required: required}
	for id, s := range secrets {
		d := sha256.Sum256([]byte(s))
		r.digests[id] = d[:]
	}
	return r
}

// Authenticate returns the verified client id, "" when no client was presented
// and none is required, or an error. A presented client that fails is always
// an error: it is never downgraded to "no client".
func (r *ClientRegistry) Authenticate(clientID, secret string) (string, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		if secret != "" {
			return "", ErrClientInvalid
		}
		if r != nil && r.required {
			return "", ErrClientRequired
		}
		return "", nil
	}
	if r == nil {
		return "", ErrClientInvalid
	}
	want, ok := r.digests[clientID]
	got := sha256.Sum256([]byte(secret))
	if !ok {
		// Compare anyway so an unknown id costs what a known one does.
		subtle.ConstantTimeCompare(got[:], got[:])
		return "", ErrClientInvalid
	}
	if secret == "" || subtle.ConstantTimeCompare(want, got[:]) != 1 {
		return "", ErrClientInvalid
	}
	return clientID, nil
}

// Count is how many clients are registered (for the startup log).
func (r *ClientRegistry) Count() int {
	if r == nil {
		return 0
	}
	return len(r.digests)
}
