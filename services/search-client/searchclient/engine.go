// Engine is the control-plane half of this package: everything ESR-02
// (secure indexing/projection) and ESR-05 (index lifecycle) need that a plain
// "index a document, search for it" client does not have.
//
// It exists because ZS-SVC-AB-001 makes two demands the original Client
// interface cannot express at all:
//
//   - INV-21 / NP-42 — "Reindexing is non-destructive and cannot replace the
//     active generation until completeness and security validation pass."
//     Serving traffic through an ALIAS that points at one of several concrete
//     generations is the only way to build a replacement while the old one
//     still serves, and to cut over atomically. Client's IndexName is a
//     literal index name with no alias layer, so there was nowhere to build.
//
//   - INV-08 / NP-51 — "Fields not explicitly registered as searchable/
//     exposable are absent from searchable projections", and "source sends
//     unknown new field marked searchable by vendor dynamic mapping → dynamic
//     mapping disabled/ignored; contract publication required." OpenSearch's
//     default is dynamic:true, which does the exact opposite: it maps and
//     indexes whatever arrives. EnsureGeneration writes dynamic:"strict", so
//     an unregistered field is REJECTED at write time rather than silently
//     indexed — the difference between a contract and a suggestion.
//
// The original Client interface is left alone. It is the narrow read/write
// surface domain code should keep using, and its integration tests still pin
// it. Engine embeds it, so one concrete *client serves both.
package searchclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// ── Generation naming ────────────────────────────────────────────────────────

// GenerationIndex is the concrete index name for one generation of a scope.
//
// Callers never hand a raw index name to a query: queries address the ALIAS
// (Alias below), which the control plane repoints. That indirection is what
// INV-03 ("user-supplied index/alias names cannot select a broader tenant or
// residency partition") and NP-02 rest on — the physical name is server-owned
// and never appears in a request envelope.
func GenerationIndex(scope IndexName, generationID string) string {
	return string(scope) + "-g" + strings.ToLower(generationID)
}

// Alias is the stable name queries address. It never moves; what it points at
// does.
func Alias(scope IndexName) string { return string(scope) }

// ── Field contracts ──────────────────────────────────────────────────────────

// FieldMapping is one registered field from a published IndexContract, lowered
// into an OpenSearch mapping property.
//
// Type is the ESR field type, not the engine's: KEYWORD / TEXT / DATE / LONG /
// DOUBLE / BOOLEAN. The translation lives in osType so a contract stays
// vendor-neutral, which §17's implementation note requires: the vendor's
// field-level features are "enforcement mechanisms, not the canonical business
// authority".
type FieldMapping struct {
	Name       string
	Type       string
	Searchable bool
	Filterable bool
	Facetable  bool
	Sortable   bool

	// Analyzer pins tokenization per §4.3: changing tokenization,
	// normalization or synonym sets "creates a new contract/index generation
	// where results can materially change". Empty means the engine default,
	// which is only acceptable for non-text types.
	Analyzer string
}

// osType maps an ESR field type onto an OpenSearch field type.
func (f FieldMapping) osType() string {
	switch strings.ToUpper(f.Type) {
	case "TEXT":
		return "text"
	case "DATE":
		return "date"
	case "LONG", "INTEGER":
		return "long"
	case "DOUBLE", "DECIMAL":
		return "double"
	case "BOOLEAN":
		return "boolean"
	default:
		// KEYWORD is the safe default: exact-match, not analyzed, not
		// tokenized. A field whose type nobody declared must not become
		// full-text searchable by accident.
		return "keyword"
	}
}

// property renders this field as an OpenSearch mapping property.
func (f FieldMapping) property() map[string]any {
	t := f.osType()
	p := map[string]any{"type": t}

	if t == "text" {
		if f.Analyzer != "" {
			p["analyzer"] = f.Analyzer
		}
		// index:false when not searchable. A registered-but-unsearchable text
		// field is stored for hydration and snippet policy but must not be
		// matchable, or INV-08 leaks through relevance instead of through
		// _source.
		if !f.Searchable {
			p["index"] = false
		}
		if f.Sortable || f.Facetable || f.Filterable {
			// A text field cannot be sorted or aggregated directly; the
			// keyword sub-field is what makes that legal. Registering the
			// sub-field here rather than letting a caller guess at
			// "field.keyword" keeps the contract the only place field
			// capabilities are declared.
			p["fields"] = map[string]any{
				"keyword": map[string]any{"type": "keyword", "ignore_above": 8191},
			}
		}
		return p
	}

	if !f.Searchable && !f.Filterable && !f.Facetable && !f.Sortable {
		p["index"] = false
	}
	return p
}

// GovernanceProperties are the fields EVERY projection carries regardless of
// contract, because the security model is built on them rather than on the
// domain's own schema. All keyword (exact-match) where they are filtered on: a
// tenant filter that went through an analyzer would match on a token, and a
// token match is not a filter.
func GovernanceProperties() map[string]any {
	return map[string]any{
		"tenant_id":              map[string]any{"type": "keyword"},
		"legal_entity_id":        map[string]any{"type": "keyword"},
		"residency_region":       map[string]any{"type": "keyword"},
		"source_type":            map[string]any{"type": "keyword"},
		"source_id":              map[string]any{"type": "keyword"},
		"source_version":         map[string]any{"type": "long"},
		"projection_version":     map[string]any{"type": "long"},
		"restriction_epoch":      map[string]any{"type": "long"},
		"sensitivity_class":      map[string]any{"type": "keyword"},
		"retrieval_class":        map[string]any{"type": "keyword"},
		"acl_refs":               map[string]any{"type": "keyword"},
		"index_generation":       map[string]any{"type": "keyword"},
		"indexed_at":             map[string]any{"type": "date"},
		"content_hash":           map[string]any{"type": "keyword"},
		"tombstoned":             map[string]any{"type": "boolean"},
		"tombstone_reason":       map[string]any{"type": "keyword"},
		"tombstone_source_event": map[string]any{"type": "keyword"},
	}
}

// ReservedFields lists the projection field names the control plane owns. A
// contract that registered one of these would let a source overwrite its own
// tenant or its own restriction epoch — INV-02 and INV-18 defeated in one move.
func ReservedFields() []string {
	props := GovernanceProperties()
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrAliasDrift is returned when an alias points at more than one concrete
// index. §8.3 calls for freezing automated cutover and opening an incident
// rather than guessing which generation is authoritative.
var ErrAliasDrift = errors.New("searchclient: alias drift")

// ErrStrictMappingRejected is returned when OpenSearch refuses a document
// because it carries a field the generation's strict mapping does not declare.
// The caller quarantines rather than retries: a contract violation does not
// become valid on the second attempt (NP-10, NP-51).
var ErrStrictMappingRejected = errors.New("searchclient: document rejected by strict mapping")

// ── Lifecycle ────────────────────────────────────────────────────────────────

// EnsureGeneration creates one concrete generation index with a STRICT mapping
// built from the published contract's fields. Idempotent: an existing index is
// left exactly as it is, because a generation's mapping is immutable by
// definition (§8.1 — a mapping change is a new generation, never a patch).
func (c *client) EnsureGeneration(ctx context.Context, physicalIndex string, fields []FieldMapping) error {
	exists, err := c.indexExists(ctx, physicalIndex)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	props := GovernanceProperties()
	for _, f := range fields {
		if f.Name == "" {
			continue
		}
		if _, reserved := props[f.Name]; reserved {
			return fmt.Errorf(
				"searchclient: field %q is a reserved governance field and cannot be registered by a contract", f.Name)
		}
		props[f.Name] = f.property()
	}

	body, err := json.Marshal(map[string]any{
		"settings": map[string]any{
			"index": map[string]any{
				// One shard locally. The partitioning that matters here is
				// logical (tenant / residency filters), not physical; OD-02
				// leaves the physical strategy open and this is the
				// single-node development answer to it.
				"number_of_shards":   1,
				"number_of_replicas": 0,
			},
		},
		"mappings": map[string]any{
			// NP-51. Not "false" (ignore silently) but "strict" (reject the
			// write): a source that starts emitting an unregistered field
			// must produce a loud projection failure that routes to the
			// quarantine path, not a document quietly missing data.
			"dynamic":    "strict",
			"properties": props,
		},
	})
	if err != nil {
		return fmt.Errorf("searchclient: EnsureGeneration marshal mapping: %w", err)
	}

	resp, err := c.os.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: physicalIndex,
		Body:  bytes.NewReader(body),
	})
	if err != nil {
		// A concurrent creator that won the race is success, not failure —
		// two replicas booting together must not fight over a generation.
		if strings.Contains(err.Error(), "resource_already_exists_exception") {
			return nil
		}
		return fmt.Errorf("searchclient: EnsureGeneration create %s: %w", physicalIndex, err)
	}
	raw, isErr := drain(resp.Inspect().Response)
	if isErr {
		if strings.Contains(raw, "resource_already_exists_exception") {
			return nil
		}
		return fmt.Errorf("searchclient: EnsureGeneration create %s failed: %s", physicalIndex, raw)
	}
	return nil
}

// ActivateGeneration atomically repoints alias at physicalIndex, removing it
// from whatever it pointed at before, and returns the previous target.
//
// ONE _aliases call, not a remove followed by an add. OpenSearch applies the
// whole action list atomically, so there is no instant at which the alias
// resolves to nothing — which would surface to callers as an
// index_not_found_exception, i.e. a hard search outage, in the middle of a
// routine cutover (§8.1's "ACTIVE by controlled alias/router switch").
func (c *client) ActivateGeneration(ctx context.Context, alias, physicalIndex string) (string, error) {
	current, err := c.ActiveGeneration(ctx, alias)
	if err != nil {
		return "", err
	}
	if current == physicalIndex {
		return current, nil
	}

	actions := make([]map[string]any, 0, 2)
	if current != "" {
		actions = append(actions, map[string]any{
			"remove": map[string]any{"index": current, "alias": alias},
		})
	}
	actions = append(actions, map[string]any{
		"add": map[string]any{"index": physicalIndex, "alias": alias},
	})

	body, err := json.Marshal(map[string]any{"actions": actions})
	if err != nil {
		return "", fmt.Errorf("searchclient: ActivateGeneration marshal: %w", err)
	}

	resp, err := c.os.Aliases(ctx, opensearchapi.AliasesReq{Body: bytes.NewReader(body)})
	if err != nil {
		return "", fmt.Errorf("searchclient: ActivateGeneration %s -> %s: %w", alias, physicalIndex, err)
	}
	if raw, isErr := drain(resp.Inspect().Response); isErr {
		return "", fmt.Errorf("searchclient: ActivateGeneration %s -> %s failed: %s", alias, physicalIndex, raw)
	}
	return current, nil
}

// ActiveGeneration returns the concrete index the alias currently resolves to,
// or "" when the alias does not exist yet.
//
// More than one target is an ERROR, not a pick-the-first. A scope serving two
// generations at once means TC-02 ("every search traces to exact active index
// generation(s)") cannot be satisfied, and NP-42's alias-drift incident is
// precisely this state.
func (c *client) ActiveGeneration(ctx context.Context, alias string) (string, error) {
	resp, err := c.os.Indices.Alias.Get(ctx, opensearchapi.AliasGetReq{Alias: []string{alias}})
	if err != nil {
		if isMissing(err) {
			return "", nil
		}
		return "", fmt.Errorf("searchclient: ActiveGeneration %s: %w", alias, err)
	}
	if r := resp.Inspect().Response; r != nil {
		if r.StatusCode == 404 {
			_, _ = drain(r)
			return "", nil
		}
		_, _ = drain(r)
	}

	targets := make([]string, 0, 1)
	for index := range resp.Indices {
		targets = append(targets, index)
	}
	switch len(targets) {
	case 0:
		return "", nil
	case 1:
		return targets[0], nil
	default:
		sort.Strings(targets)
		return "", fmt.Errorf("%w: alias %s resolves to %d indices (%s)",
			ErrAliasDrift, alias, len(targets), strings.Join(targets, ", "))
	}
}

// DropIndex removes a retired generation. Never called on an ACTIVE one — the
// caller checks control-plane state first, because this is unrecoverable from
// the engine's side. §8.3 is explicit that the recovery path is a rebuild from
// source, not a restore: "indexes are disposable projections, not the sole
// backup".
func (c *client) DropIndex(ctx context.Context, physicalIndex string) error {
	resp, err := c.os.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{Indices: []string{physicalIndex}})
	if err != nil {
		if isMissing(err) {
			return nil
		}
		return fmt.Errorf("searchclient: DropIndex %s: %w", physicalIndex, err)
	}
	if raw, isErr := drain(resp.Inspect().Response); isErr {
		if strings.Contains(raw, "index_not_found") {
			return nil
		}
		return fmt.Errorf("searchclient: DropIndex %s failed: %s", physicalIndex, raw)
	}
	return nil
}

func (c *client) indexExists(ctx context.Context, physicalIndex string) (bool, error) {
	resp, err := c.os.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{Indices: []string{physicalIndex}})

	// THE RESPONSE IS READ BEFORE THE ERROR, deliberately.
	//
	// Indices.Exists answers 404 for a missing index, and the typed client
	// turns any non-2xx into an error — so the error is how "it does not
	// exist" arrives, which is the ANSWER to this question rather than a
	// failure. It still returns the response alongside, and the status code on
	// that response is an exact, stable fact where the error's text is neither.
	//
	// It was matched on text, and the text was wrong: the client renders the
	// status as `status: [404 Not Found]`, not `status: 404`. Every
	// EnsureGeneration therefore reported a real failure for the ordinary case
	// of an index that does not exist yet — which is to say, for every
	// generation build. Found only by running it: the unit tests exercise the
	// mapping and plan compilation, and nothing below this line is reachable
	// without a cluster.
	if resp != nil {
		status := resp.StatusCode
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		switch status {
		case 200:
			return true, nil
		case 404:
			return false, nil
		}
	}
	if err != nil {
		if isMissing(err) {
			return false, nil
		}
		return false, fmt.Errorf("searchclient: indexExists %s: %w", physicalIndex, err)
	}
	return false, nil
}

// isMissing recognises a "not found" from an error whose response was not
// available to read a status code from.
//
// The bracketed form is what opensearch-go actually produces —
// `status: [404 Not Found]` — and the bare `status: 404` form is kept because
// a future version rendering it that way should not silently reintroduce the
// bug this comment describes.
func isMissing(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "index_not_found") ||
		strings.Contains(s, "alias_not_found") ||
		strings.Contains(s, "status: 404") ||
		strings.Contains(s, "status: [404") ||
		strings.Contains(s, "404 not found")
}
