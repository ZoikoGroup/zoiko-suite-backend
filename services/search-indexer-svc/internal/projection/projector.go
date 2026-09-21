// Package projection turns a domain event into a searchable projection,
// subject to the published contract. It is ESR-02 §5.2's "projection rules" in
// one place.
//
// The rule that shapes every function here is that a projection is an
// ALLOWLIST, not a copy with exclusions:
//
//	"Projection includes only registered fields; unknown fields are ignored or
//	 quarantined according to contract, never opportunistically indexed."
//
// So Project starts from the contract's field list and pulls values out of the
// payload, rather than starting from the payload and dropping known-bad keys.
// The difference matters the day a source adds a field: an allowlist ignores
// it, a denylist indexes it. That is INV-08, and NP-10 ("secret field appears
// in source schema unexpectedly") is the version of it that costs the most.
package projection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
)

// ErrProhibitedFieldPresent is returned when an event payload carries a value
// at a path the contract marked SECRET_PROHIBITED.
//
// A hard failure, not a silent drop. INV-09 says these "never enter search
// indexes", and a source that has started emitting one is a contract breach
// the source owner has to know about — dropping it quietly would index every
// OTHER field of a document whose schema is no longer the one that was
// certified, and nobody would find out.
var ErrProhibitedFieldPresent = errors.New("payload carries a SECRET_PROHIBITED field")

// ErrNoTrustedTenant is returned when an event cannot supply a trusted tenant.
//
// INV-02: "every projection document carries trusted tenant identity; tenant
// is never inferred solely from user query text." A projection with no tenant
// cannot be filtered to a tenant, so it would be visible to whichever tenant's
// query reached it — the single worst failure this service can have. It is
// therefore a refusal, never a default and never a lookup.
var ErrNoTrustedTenant = errors.New("event carries no trusted tenant identity")

// Event is one consumed domain event, already unwrapped from its Kafka message.
//
// The field set is the intersection of what every producer in this estate
// actually emits, which is NOT what Doc 03 describes — the envelope shapes
// differ per service and the spec's names are not always the wire names. These
// are the wire names, taken from the producers themselves.
type Event struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	Jurisdiction  string          `json:"jurisdiction"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Result is a projection ready to write, plus the ledger row that records it.
type Result struct {
	Projection searchclient.Projection
	Record     domain.ProjectionRecord
	// Restriction is set when this event was a visibility-reducing one. The
	// caller routes it down the priority lane (§8.2) instead of the normal
	// indexing path.
	Restriction *domain.RestrictionTombstone
}

// Projector builds projections for one published contract.
type Projector struct {
	contract domain.IndexContract
	source   domain.SearchSource
	// byPath indexes fields by their payload path so Project is one pass over
	// the contract rather than a scan per field.
	byPath      map[string]domain.SearchFieldDefinition
	prohibited  []domain.SearchFieldDefinition
	idPath      string
	versionPath string
}

// New builds a Projector. Returns an error rather than a half-configured
// projector when the contract cannot produce a usable projection at all.
func New(contract domain.IndexContract, source domain.SearchSource) (*Projector, error) {
	p := &Projector{
		contract: contract,
		source:   source,
		byPath:   make(map[string]domain.SearchFieldDefinition, len(contract.Fields)),
	}

	reserved := make(map[string]bool, len(searchclient.ReservedFields()))
	for _, r := range searchclient.ReservedFields() {
		reserved[r] = true
	}

	for _, f := range contract.Fields {
		if reserved[f.FieldID] {
			// Belt and braces with the store's own check. A contract that
			// registered "tenant_id" would let a source overwrite the trusted
			// tenant on its own projection.
			return nil, fmt.Errorf("%w: %s", domain.ErrReservedField, f.FieldID)
		}
		if f.SensitivityClass == domain.SensitivitySecretProhibited {
			p.prohibited = append(p.prohibited, f)
			continue
		}
		p.byPath[f.SourcePath] = f
	}

	// The identity fields are conventions rather than contract fields: every
	// producer in this estate keys its payload on "<thing>_id", and the
	// contract's source_id path is derived from the source_type rather than
	// registered separately, so a source cannot accidentally point the
	// document id at a mutable field.
	p.idPath = source.SourceType + "_id"
	p.versionPath = "record_version"
	return p, nil
}

// Scope is the search surface this projector writes into.
func (p *Projector) Scope() string { return p.contract.ScopeName }

// Contract exposes the contract, for the generation builder's mapping.
func (p *Projector) Contract() domain.IndexContract { return p.contract }

// IsRestriction reports whether an event type is on this source's priority
// visibility-reduction lane.
func (p *Projector) IsRestriction(eventType string) bool {
	for _, t := range p.source.RestrictionEventTypes {
		if t == eventType {
			return true
		}
	}
	return false
}

// Handles reports whether this projector consumes the given event type at all.
func (p *Projector) Handles(eventType string) bool {
	for _, t := range p.source.EventTypes {
		if t == eventType {
			return true
		}
	}
	return p.IsRestriction(eventType)
}

// Project turns one event into a projection.
//
// generationID names the index this will be written into; it is stamped on the
// document so TC-02 ("every search traces to exact active index generation")
// can be answered from a hit alone rather than by inferring it from the alias
// at read time.
func (p *Projector) Project(e Event, generationID, physicalIndex string) (*Result, error) {
	tenantID := strings.TrimSpace(e.TenantID)
	if tenantID == "" {
		// Deliberately NOT resolved from legal_entity_id here.
		//
		// The previous implementation of this service did exactly that: it
		// called tenant-entity-registry-svc's GET /v1/entities/{id} with no
		// headers to turn an entity into a tenant. That endpoint scopes its
		// own query by the caller's X-Tenant-Id and answers 404 without one,
		// so the lookup could never succeed — and if it HAD succeeded it would
		// have been a privileged cross-tenant read invented for the
		// convenience of an indexer.
		//
		// The event envelope is the correct source: it was produced inside a
		// request that had already proven its tenant. A producer that omits
		// tenant_id has a producer-side gap, and the honest response is to
		// refuse the projection and say so (INV-02, INV-07).
		return nil, ErrNoTrustedTenant
	}

	var payload map[string]any
	if len(e.Payload) > 0 {
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode payload: %w", err)
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}

	// NP-10 / INV-09 first, before anything is extracted. A prohibited field
	// that is PRESENT fails the whole projection, even if every other field
	// would have been fine.
	for _, f := range p.prohibited {
		if _, present := lookupPath(payload, f.SourcePath); present {
			return nil, fmt.Errorf("%w: %s (field %s)", ErrProhibitedFieldPresent, f.SourcePath, f.FieldID)
		}
	}

	sourceID := stringAt(payload, p.idPath)
	if sourceID == "" {
		return nil, fmt.Errorf("payload has no %s — a projection needs a stable source identifier", p.idPath)
	}

	fields := make(map[string]any, len(p.byPath))
	for path, def := range p.byPath {
		raw, present := lookupPath(payload, path)
		if !present || raw == nil {
			continue
		}
		coerced, err := coerce(raw, def.Type)
		if err != nil {
			// A type mismatch is a contract breach, same class as an
			// unregistered field: the generation's mapping is strict, so
			// writing it would be rejected by the engine anyway. Failing here
			// gives the operator the field name instead of an engine error.
			return nil, fmt.Errorf("field %s (%s): %w", def.FieldID, path, err)
		}
		fields[def.FieldID] = coerced
	}

	isRestriction := p.IsRestriction(e.EventType)
	sourceVersion := versionOf(payload, p.versionPath, e.EmittedAt)

	// The epoch is what orders restrictions against each other and against
	// ordinary updates. A restriction takes the event's emission time so two
	// restrictions from the same producer order correctly; an ordinary update
	// takes 0 so it can never outrank one.
	//
	// That asymmetry is the whole of NP-11: a replayed create arriving after a
	// deletion carries epoch 0, the ledger already holds a positive epoch, and
	// the compare-and-set in the store refuses it. Visibility cannot be
	// resurrected by anything that is not itself a restriction decision.
	var epoch int64
	if isRestriction {
		epoch = e.EmittedAt.UTC().UnixMilli()
		if epoch <= 0 {
			epoch = time.Now().UTC().UnixMilli()
		}
	}

	proj := searchclient.Projection{
		DocID:            searchclient.ProjectionDocID(tenantID, p.source.SourceType, sourceID),
		TenantID:         tenantID,
		LegalEntityID:    e.LegalEntityID,
		ResidencyRegion:  p.source.ResidencyRegion,
		SourceType:       p.source.SourceType,
		SourceID:         sourceID,
		SourceVersion:    sourceVersion,
		RestrictionEpoch: epoch,
		SensitivityClass: string(p.source.SensitivityCeiling),
		RetrievalClass:   string(p.contract.RetrievalClass),
		ACLRefs:          aclRefs(e, payload),
		IndexGeneration:  generationID,
		Fields:           fields,
		Tombstoned:       isRestriction,
	}
	if isRestriction {
		proj.TombstoneReason = e.EventType
		proj.TombstoneSource = e.EventID
		// A tombstoned projection keeps its governance lineage and drops its
		// content. Keeping the document rather than deleting it is what makes
		// NP-11 enforceable at all: a deleted document has no epoch to compare
		// a late replay against, so the replay would simply re-create it.
		proj.Fields = map[string]any{}
	}
	proj.ContentHash = hashProjection(proj)

	res := &Result{
		Projection: proj,
		Record: domain.ProjectionRecord{
			TenantID:         tenantID,
			ScopeName:        p.contract.ScopeName,
			SourceType:       p.source.SourceType,
			SourceID:         sourceID,
			SourceVersion:    sourceVersion,
			RestrictionEpoch: epoch,
			ContentHash:      proj.ContentHash,
			Tombstoned:       isRestriction,
			LastEventID:      e.EventID,
		},
	}

	if isRestriction {
		effective := e.EmittedAt
		if effective.IsZero() {
			effective = time.Now().UTC()
		}
		res.Restriction = &domain.RestrictionTombstone{
			TenantID:      tenantID,
			ScopeName:     p.contract.ScopeName,
			SourceType:    p.source.SourceType,
			SourceID:      sourceID,
			Reason:        e.EventType,
			Epoch:         epoch,
			SourceEventID: e.EventID,
			EffectiveAt:   effective,
			State:         domain.PropagationPending,
		}
	}
	return res, nil
}

// aclRefs collects the resource-authorization attributes that accompany a
// projection for candidate filtering.
//
// §5.2 is explicit about what these are and are not: "ACL/resource attributes
// in the projection are hints for candidate filtering, not the final authority
// for material retrieval." They narrow the candidate set cheaply; the R1/R2
// re-authorization is what actually decides.
func aclRefs(e Event, payload map[string]any) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(prefix, v string) {
		if v == "" {
			return
		}
		ref := prefix + ":" + v
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	add("entity", e.LegalEntityID)
	add("entity", stringAt(payload, "legal_entity_id"))
	add("owner", stringAt(payload, "created_by_principal_id"))
	add("owner", stringAt(payload, "owner_principal_id"))
	add("jurisdiction", e.Jurisdiction)
	sort.Strings(out)
	return out
}

// versionOf derives a monotonic source version.
//
// record_version when the producer keeps one — several services in this estate
// do, and it is the honest answer. Otherwise the event's emission time in
// milliseconds, which is monotonic per producer and is what makes ordinary
// replay suppression work for the producers that carry no version at all.
//
// Never a counter of our own: a version this service invented would reorder
// differently on a rebuild, so the same replay would be suppressed in one
// generation and applied in the next.
func versionOf(payload map[string]any, path string, emittedAt time.Time) int64 {
	if raw, ok := lookupPath(payload, path); ok {
		switch v := raw.(type) {
		case float64:
			return int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
		}
	}
	if emittedAt.IsZero() {
		return time.Now().UTC().UnixMilli()
	}
	return emittedAt.UTC().UnixMilli()
}

// hashProjection is the deterministic field hash §5.3 asks for as completeness
// evidence: "deterministic field hash or sampled source-to-index comparison for
// material classes."
//
// Sorted keys, so the same content hashes the same however the map iterated.
// Governance lineage is included because a projection whose tenant changed is
// not the same projection even if every domain field matched.
func hashProjection(p searchclient.Projection) string {
	keys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d|%v|",
		p.TenantID, p.SourceType, p.SourceID, p.SourceVersion, p.RestrictionEpoch, p.Tombstoned)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%v;", k, p.Fields[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// lookupPath resolves a dotted path against a decoded payload.
//
// Dotted rather than flat because payloads nest — "amount.currency_code" is a
// real shape in this estate — and a flat key lookup would silently find
// nothing and project an absent field, which reads identically to a field the
// producer did not send.
func lookupPath(payload map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	parts := strings.Split(path, ".")
	var current any = payload
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func stringAt(payload map[string]any, path string) string {
	raw, ok := lookupPath(payload, path)
	if !ok {
		return ""
	}
	s, _ := raw.(string)
	return strings.TrimSpace(s)
}

// coerce converts a decoded JSON value to the contract's declared type, or
// reports that it cannot.
//
// Strict about what it accepts. A string where a number was declared IS
// accepted when it parses, because several producers serialise decimals as
// strings and refusing those would block half the finance corpus; anything
// that does not parse is an error rather than a zero, because a silently-zeroed
// amount is worse in a search index than a missing document.
func coerce(raw any, declared string) (any, error) {
	switch strings.ToUpper(declared) {
	case "TEXT", "KEYWORD":
		switch v := raw.(type) {
		case string:
			return v, nil
		case float64, bool:
			return fmt.Sprint(v), nil
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("cannot render as %s: %T", declared, raw)
			}
			return string(b), nil
		}

	case "LONG", "INTEGER":
		switch v := raw.(type) {
		case float64:
			return int64(v), nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("cannot parse %q as %s", v, declared)
			}
			return n, nil
		default:
			return nil, fmt.Errorf("cannot parse %T as %s", raw, declared)
		}

	case "DOUBLE", "DECIMAL":
		switch v := raw.(type) {
		case float64:
			return v, nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return nil, fmt.Errorf("cannot parse %q as %s", v, declared)
			}
			return f, nil
		default:
			return nil, fmt.Errorf("cannot parse %T as %s", raw, declared)
		}

	case "BOOLEAN":
		switch v := raw.(type) {
		case bool:
			return v, nil
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("cannot parse %q as BOOLEAN", v)
			}
			return b, nil
		default:
			return nil, fmt.Errorf("cannot parse %T as BOOLEAN", raw)
		}

	case "DATE":
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("cannot parse %T as DATE", raw)
		}
		// RFC3339 only. Guessing between formats is how a day/month order gets
		// silently inverted, and a date field is filterable and sortable —
		// so a misparsed one reorders results rather than just displaying oddly.
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
		if err != nil {
			// Date-only is the one other shape this estate emits.
			if t2, err2 := time.Parse("2006-01-02", strings.TrimSpace(s)); err2 == nil {
				return t2.UTC().Format(time.RFC3339), nil
			}
			return nil, fmt.Errorf("cannot parse %q as RFC3339 DATE", s)
		}
		return t.UTC().Format(time.RFC3339), nil

	default:
		return nil, fmt.Errorf("unknown declared type %q", declared)
	}
}

// FieldMappings lowers a contract's fields into engine mappings.
func FieldMappings(c domain.IndexContract) []searchclient.FieldMapping {
	out := make([]searchclient.FieldMapping, 0, len(c.Fields))
	for _, f := range c.Fields {
		if f.SensitivityClass == domain.SensitivitySecretProhibited {
			// Not mapped at all. A prohibited field has no index
			// representation, so even a projection that somehow carried one
			// would be refused by the strict mapping.
			continue
		}
		analyzer := f.AnalyzerProfile
		if analyzer == "" && strings.EqualFold(f.Type, "TEXT") {
			analyzer = c.AnalyzerProfile
		}
		out = append(out, searchclient.FieldMapping{
			Name:       f.FieldID,
			Type:       f.Type,
			Searchable: f.Searchable,
			Filterable: f.Filterable,
			Facetable:  f.Facetable,
			Sortable:   f.Sortable,
			Analyzer:   analyzer,
		})
	}
	return out
}
