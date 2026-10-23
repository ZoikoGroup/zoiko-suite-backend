package registry

import "context"

// Repo is the registry's persistence boundary. Every method acts for the tenant in the request context; the
// registry's content is platform data, so reads see what the tenant may see (a tenant sees published
// contracts, the platform scope sees drafts too) and writes succeed only from the platform scope. Both
// implementations (Postgres, memory) enforce that themselves: it is not left to callers.
type Repo interface {
	// CreateConnector inserts c and its event atomically. A repeated idemKey with the same hash returns the
	// original (created=false); with a different hash it returns ErrIdempotency.
	CreateConnector(ctx context.Context, c *Connector, idemKey, idemHash string, ev Event) (created bool, err error)
	GetConnector(ctx context.Context, id string) (*Connector, error)
	ListConnectors(ctx context.Context, family, lifecycle string) ([]Connector, error)
	// MutateConnector locks the connector, applies fn and writes the result and the event it returns.
	MutateConnector(ctx context.Context, id string, fn func(c *Connector) (*Event, error)) (*Connector, error)

	// CreateVersion inserts a DRAFT version with its content and mapping rows.
	CreateVersion(ctx context.Context, v *ContractVersion, idemKey, idemHash string, ev Event) (created bool, err error)
	GetVersion(ctx context.Context, id string) (*ContractVersion, error)
	ListVersions(ctx context.Context, connectorID string) ([]ContractVersion, error)
	// Baseline returns the connector's PUBLISHED version, or nil when it has none.
	Baseline(ctx context.Context, connectorID string) (*ContractVersion, error)
	// ReplaceDraft rewrites a DRAFT's header, content and mapping rows when expectedVersion still matches.
	ReplaceDraft(ctx context.Context, v *ContractVersion, expectedVersion int64, ev Event) (*ContractVersion, error)
	// MutateVersion locks the connector and the version (in that order, so publishers of one connector are
	// serialized), applies fn and writes whatever fn changed in one transaction. fn returning an error writes
	// nothing.
	MutateVersion(ctx context.Context, id string, fn func(tx *VersionTx) error) (*ContractVersion, error)

	ListGates(ctx context.Context, versionID string) ([]Gate, error)
	ListTransitions(ctx context.Context, versionID string) ([]Transition, error)
	ListValidationRuns(ctx context.Context, versionID string) ([]ValidationRun, error)
	GetMapping(ctx context.Context, mappingVersionID string) (*MappingVersion, error)
}

// VersionTx is what MutateVersion hands to its function: the locked rows, and the effects to record.
type VersionTx struct {
	V         *ContractVersion // fn may change Status, ContentHash, compat fields, published_*, blocked_reason, validated_at
	Connector *Connector
	Gates     []Gate           // every attestation, oldest first
	Baseline  *ContractVersion // the connector's PUBLISHED version at lock time, nil when none

	// Effects.
	Transition         *Transition
	NewGate            *Gate
	NewRun             *ValidationRun
	Events             []Event
	Supersede          string // the version this publish supersedes
	ConnectorLifecycle string // a new lifecycle for the connector
}
