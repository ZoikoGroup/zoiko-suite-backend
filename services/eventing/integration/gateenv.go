package integration

import (
	"os"
	"time"
)

// GateFromEnv builds a producer's schema gate from the environment, so every relay adopts it the same way:
//
//	SCHEMA_REGISTRY_URL             the registry; empty leaves the gate off and returns nil, nil
//	SCHEMA_GATE_PRINCIPAL           the workload identity the registry sees (default: service)
//	SCHEMA_GATE_ALLOW_UNREGISTERED  "false" refuses events that have no registered contract (default true, for rollout)
//
// A malformed registry URL is an error, never a silently ungated relay. The caller must check for nil before handing the
// result to a relay: a nil *SchemaGate stored in an interface is not a nil interface.
func GateFromEnv(service string) (*SchemaGate, error) {
	url := os.Getenv("SCHEMA_REGISTRY_URL")
	if url == "" {
		return nil, nil
	}
	principal := os.Getenv("SCHEMA_GATE_PRINCIPAL")
	if principal == "" {
		principal = service
	}
	reg, err := NewHTTPRegistry(url, principal, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return &SchemaGate{Registry: reg, AllowUnregistered: os.Getenv("SCHEMA_GATE_ALLOW_UNREGISTERED") != "false"}, nil
}
