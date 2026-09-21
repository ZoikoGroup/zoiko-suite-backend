package searchclient

import "context"

// Engine is the full control-plane surface: the narrow Client, plus index
// lifecycle, contract-shaped projections and compiled query execution.
//
// Split from Client rather than folded into it so the two audiences stay
// separate. A domain service that only wants to index its own records and
// search them depends on Client, which cannot repoint an alias or drop a
// generation however it is misused. search-indexer-svc — the one service whose
// job IS the lifecycle — depends on Engine.
type Engine interface {
	Client

	// ── ESR-05: generations and aliases ──────────────────────────────────
	EnsureGeneration(ctx context.Context, physicalIndex string, fields []FieldMapping) error
	ActivateGeneration(ctx context.Context, alias, physicalIndex string) (previous string, err error)
	ActiveGeneration(ctx context.Context, alias string) (string, error)
	DropIndex(ctx context.Context, physicalIndex string) error

	// ── ESR-02: projections and tombstones ───────────────────────────────
	IndexProjection(ctx context.Context, physicalIndex string, p Projection) error
	DeleteProjection(ctx context.Context, physicalIndex, docID string) error
	GetProjection(ctx context.Context, physicalIndex, docID string) (map[string]any, bool, error)
	CountProjections(ctx context.Context, physicalIndex string, terms map[string]string) (int64, error)

	// ── ESR-03: compiled query execution ─────────────────────────────────
	ExecutePlan(ctx context.Context, target string, plan ExecutionPlan) (Result, error)

	// Ping reports whether the cluster is reachable. Readiness only — never a
	// gate on a query, because a cluster that answers a search is reachable by
	// definition and a health probe in front of every query is one more thing
	// to time out.
	Ping(ctx context.Context) error
}

// NewEngine builds the full control-plane client.
func NewEngine(cfg Config) (Engine, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return c.(*client), nil
}

// Ping issues the cheapest call that proves the cluster answers.
func (c *client) Ping(ctx context.Context) error {
	resp, err := c.os.Cluster.Health(ctx, nil)
	if err != nil {
		return err
	}
	if raw, isErr := drain(resp.Inspect().Response); isErr {
		return &pingError{body: raw}
	}
	return nil
}

type pingError struct{ body string }

func (e *pingError) Error() string { return "searchclient: cluster health failed: " + e.body }
