package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrIdempotencyMismatch is returned when an Idempotency-Key that was already
// used is presented with a different request (method, path or body).
var ErrIdempotencyMismatch = errors.New("Idempotency-Key was already used for a different request")

// IdempotencyClaim identifies one logical command. The tenant comes from the
// request context; the record is scoped (tenant, principal, key).
type IdempotencyClaim struct {
	PrincipalID string
	Key         string
	// Operation is "CreateDocument" or "AddVersion".
	Operation string
	// RequestHash is the SHA-256 (hex) of method + path + body.
	RequestHash string
}

// IdempotencyRecord is the stored outcome of a committed command.
type IdempotencyRecord struct {
	PrincipalID    string
	Key            string
	Operation      string
	RequestHash    string
	ResponseStatus int
	ResponseBody   json.RawMessage
	CreatedAt      time.Time
}
