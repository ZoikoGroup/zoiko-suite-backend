package projection

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-indexer-svc/internal/domain"
)

func testSource() domain.SearchSource {
	return domain.SearchSource{
		SourceID:              "11111111-1111-1111-1111-111111111111",
		OwnerService:          "obligations-svc",
		SourceType:            "obligation",
		TenantScope:           "TENANT_SHARDED",
		ResidencyRegion:       "EU",
		SensitivityCeiling:    domain.SensitivityFinancial,
		EventTopic:            "zoiko.obligations.events",
		EventTypes:            []string{"obligation.created", "obligation.updated"},
		RestrictionEventTypes: []string{"obligation.deleted"},
		FreshnessClass:        "S1",
	}
}

func testContract(fields ...domain.SearchFieldDefinition) domain.IndexContract {
	if len(fields) == 0 {
		fields = []domain.SearchFieldDefinition{
			{FieldID: "obligation_code", SourcePath: "obligation_code", Type: "TEXT",
				Searchable: true, Returnable: true, SnippetAllowed: true, SensitivityClass: domain.SensitivityInternal},
			{FieldID: "obligation_status", SourcePath: "obligation_status", Type: "KEYWORD",
				Filterable: true, Facetable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal},
			{FieldID: "due_date", SourcePath: "due_date", Type: "DATE",
				Sortable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal},
		}
	}
	return domain.IndexContract{
		ContractID:     "22222222-2222-2222-2222-222222222222",
		SourceID:       "11111111-1111-1111-1111-111111111111",
		ScopeName:      "obligation",
		Version:        1,
		State:          domain.ContractPublished,
		RetrievalClass: domain.RetrievalR1,
		AuthzAction:    "OBLIGATION_READ",
		Fields:         fields,
	}
}

func payload(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

// INV-02. The single most important refusal in this service: a projection
// with no trusted tenant cannot be filtered to a tenant, so it would be
// visible to whichever tenant's query reached it.
//
// The previous implementation resolved this by calling
// tenant-entity-registry-svc, headerless, which could only ever 404 — and
// would have been a privileged cross-tenant read if it had worked. This test
// pins the refusal so nobody reintroduces the lookup.
func TestProject_RefusesEventWithNoTrustedTenant(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)

	_, err = p.Project(Event{
		EventID:   "evt-1",
		EventType: "obligation.created",
		// legal_entity_id present, tenant_id absent — exactly the shape
		// obligations-svc emitted before its publisher was fixed.
		LegalEntityID: "33333333-3333-3333-3333-333333333333",
		EmittedAt:     time.Now().UTC(),
		Payload:       payload(t, map[string]any{"obligation_id": "ob-1", "obligation_code": "GST-Q4"}),
	}, "gen-1", "idx-1")

	require.ErrorIs(t, err, ErrNoTrustedTenant)
}

// INV-09 / NP-10. A payload carrying a SECRET_PROHIBITED field fails the WHOLE
// projection — it is not dropped and the rest indexed, because a source that
// has started emitting one is no longer running the schema that was certified.
func TestProject_RefusesPayloadCarryingProhibitedField(t *testing.T) {
	contract := testContract(
		domain.SearchFieldDefinition{FieldID: "obligation_code", SourcePath: "obligation_code",
			Type: "TEXT", Searchable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal},
		domain.SearchFieldDefinition{FieldID: "api_secret", SourcePath: "credentials.api_secret",
			Type: "KEYWORD", SensitivityClass: domain.SensitivitySecretProhibited},
	)
	p, err := New(contract, testSource())
	require.NoError(t, err)

	_, err = p.Project(Event{
		EventID: "evt-2", EventType: "obligation.created",
		TenantID: "t-1", EmittedAt: time.Now().UTC(),
		Payload: payload(t, map[string]any{
			"obligation_id":   "ob-1",
			"obligation_code": "GST-Q4",
			"credentials":     map[string]any{"api_secret": "sk_live_abc123"},
		}),
	}, "gen-1", "idx-1")

	require.ErrorIs(t, err, ErrProhibitedFieldPresent)
}

// The same contract must project cleanly when the prohibited field is simply
// absent — the refusal is about the VALUE arriving, not about the field being
// declared. Declaring it is what teaches the projector to look.
func TestProject_ProhibitedFieldDeclaredButAbsentIsFine(t *testing.T) {
	contract := testContract(
		domain.SearchFieldDefinition{FieldID: "obligation_code", SourcePath: "obligation_code",
			Type: "TEXT", Searchable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal},
		domain.SearchFieldDefinition{FieldID: "api_secret", SourcePath: "credentials.api_secret",
			Type: "KEYWORD", SensitivityClass: domain.SensitivitySecretProhibited},
	)
	p, err := New(contract, testSource())
	require.NoError(t, err)

	res, err := p.Project(Event{
		EventID: "evt-3", EventType: "obligation.created",
		TenantID: "t-1", EmittedAt: time.Now().UTC(),
		Payload: payload(t, map[string]any{"obligation_id": "ob-1", "obligation_code": "GST-Q4"}),
	}, "gen-1", "idx-1")

	require.NoError(t, err)
	assert.NotContains(t, res.Projection.Fields, "api_secret")
}

// INV-08. The projection is an ALLOWLIST: a field the contract never
// registered is absent, however enthusiastically the producer sends it.
func TestProject_UnregisteredFieldsAreNotIndexed(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)

	res, err := p.Project(Event{
		EventID: "evt-4", EventType: "obligation.created",
		TenantID: "t-1", EmittedAt: time.Now().UTC(),
		Payload: payload(t, map[string]any{
			"obligation_id":       "ob-1",
			"obligation_code":     "GST-Q4",
			"internal_note":       "the CFO is under investigation",
			"assignee_home_email": "someone@example.com",
		}),
	}, "gen-1", "idx-1")

	require.NoError(t, err)
	assert.Equal(t, "GST-Q4", res.Projection.Fields["obligation_code"])
	assert.NotContains(t, res.Projection.Fields, "internal_note")
	assert.NotContains(t, res.Projection.Fields, "assignee_home_email")
}

// A contract may not register a governance field. Letting a source write
// "tenant_id" into its own projection would defeat INV-02 from the other
// direction — the trusted tenant would be overwritten by an asserted one.
func TestNew_RefusesContractRegisteringAReservedField(t *testing.T) {
	contract := testContract(domain.SearchFieldDefinition{
		FieldID: "tenant_id", SourcePath: "tenant_id", Type: "KEYWORD",
		Filterable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal,
	})
	_, err := New(contract, testSource())
	require.ErrorIs(t, err, domain.ErrReservedField)
}

// INV-02 again, from the merge-order side: a payload field literally named
// tenant_id cannot displace the envelope's value even if some future contract
// slipped past the check above, because governance keys are merged LAST.
func TestProject_PayloadCannotOverrideTrustedTenant(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)

	res, err := p.Project(Event{
		EventID: "evt-5", EventType: "obligation.created",
		TenantID: "trusted-tenant", EmittedAt: time.Now().UTC(),
		Payload: payload(t, map[string]any{
			"obligation_id":   "ob-1",
			"obligation_code": "GST-Q4",
			"tenant_id":       "attacker-tenant",
		}),
	}, "gen-1", "idx-1")

	require.NoError(t, err)
	assert.Equal(t, "trusted-tenant", res.Projection.TenantID)
	assert.NotContains(t, res.Projection.Fields, "tenant_id")
}

// NP-11 / NP-48. A restriction carries a positive epoch; an ordinary update
// carries zero. That asymmetry is what makes a replayed create unable to
// outrank a deletion — the store's compare-and-set does the refusing, but only
// because the epochs arrive this way.
func TestProject_RestrictionCarriesEpochAndOrdinaryUpdateDoesNot(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)
	emitted := time.Now().UTC()

	created, err := p.Project(Event{
		EventID: "evt-create", EventType: "obligation.created",
		TenantID: "t-1", EmittedAt: emitted,
		Payload: payload(t, map[string]any{"obligation_id": "ob-1", "obligation_code": "GST-Q4"}),
	}, "gen-1", "idx-1")
	require.NoError(t, err)
	assert.Zero(t, created.Projection.RestrictionEpoch,
		"an ordinary update must carry epoch 0 so it can never outrank a restriction")
	assert.False(t, created.Projection.Tombstoned)
	assert.Nil(t, created.Restriction)

	deleted, err := p.Project(Event{
		EventID: "evt-delete", EventType: "obligation.deleted",
		TenantID: "t-1", EmittedAt: emitted,
		Payload: payload(t, map[string]any{"obligation_id": "ob-1", "obligation_code": "GST-Q4"}),
	}, "gen-1", "idx-1")
	require.NoError(t, err)
	assert.Positive(t, deleted.Projection.RestrictionEpoch)
	assert.True(t, deleted.Projection.Tombstoned)
	require.NotNil(t, deleted.Restriction)
	assert.Equal(t, domain.PropagationPending, deleted.Restriction.State)
}

// A tombstoned projection keeps its lineage and DROPS its content. Keeping
// the document is what lets a late replay be compared against an epoch;
// keeping the content would mean the restricted data is still in the index
// with only a filter in front of it.
func TestProject_TombstoneDropsContentAndKeepsLineage(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)

	res, err := p.Project(Event{
		EventID: "evt-del", EventType: "obligation.deleted",
		TenantID: "t-1", LegalEntityID: "le-1", EmittedAt: time.Now().UTC(),
		Payload: payload(t, map[string]any{
			"obligation_id": "ob-1", "obligation_code": "CONFIDENTIAL-MATTER-2026",
		}),
	}, "gen-1", "idx-1")

	require.NoError(t, err)
	assert.Empty(t, res.Projection.Fields, "a tombstone must carry no content")
	assert.Equal(t, "t-1", res.Projection.TenantID)
	assert.Equal(t, "ob-1", res.Projection.SourceID)
	assert.Equal(t, "obligation.deleted", res.Projection.TombstoneReason)
	assert.Equal(t, "evt-del", res.Projection.TombstoneSource)
}

// The document id is tenant-first, so a mis-set source type or a colliding
// source id across tenants cannot overwrite another tenant's document.
func TestProjectionDocID_IsTenantScoped(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)

	a, err := p.Project(Event{
		EventID: "e1", EventType: "obligation.created", TenantID: "tenant-a",
		EmittedAt: time.Now().UTC(),
		Payload:   payload(t, map[string]any{"obligation_id": "shared-id"}),
	}, "gen-1", "idx-1")
	require.NoError(t, err)

	b, err := p.Project(Event{
		EventID: "e2", EventType: "obligation.created", TenantID: "tenant-b",
		EmittedAt: time.Now().UTC(),
		Payload:   payload(t, map[string]any{"obligation_id": "shared-id"}),
	}, "gen-1", "idx-1")
	require.NoError(t, err)

	assert.NotEqual(t, a.Projection.DocID, b.Projection.DocID,
		"two tenants sharing a source id must not share a document")
}

// A value that does not match its declared type is a contract breach, not a
// zero. A silently-zeroed amount in a search index is worse than a missing
// document, because it looks like data.
func TestProject_RefusesValueThatDoesNotMatchDeclaredType(t *testing.T) {
	contract := testContract(domain.SearchFieldDefinition{
		FieldID: "amount", SourcePath: "amount", Type: "DOUBLE",
		Filterable: true, Returnable: true, SensitivityClass: domain.SensitivityFinancial,
	})
	p, err := New(contract, testSource())
	require.NoError(t, err)

	_, err = p.Project(Event{
		EventID: "e", EventType: "obligation.created", TenantID: "t-1",
		EmittedAt: time.Now().UTC(),
		Payload:   payload(t, map[string]any{"obligation_id": "ob-1", "amount": "not-a-number"}),
	}, "gen-1", "idx-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount")
}

// A decimal serialised as a string IS accepted, because several producers in
// this estate emit them that way and refusing would block the finance corpus.
func TestProject_AcceptsNumericStringForDeclaredNumber(t *testing.T) {
	contract := testContract(domain.SearchFieldDefinition{
		FieldID: "amount", SourcePath: "amount", Type: "DOUBLE",
		Filterable: true, Returnable: true, SensitivityClass: domain.SensitivityFinancial,
	})
	p, err := New(contract, testSource())
	require.NoError(t, err)

	res, err := p.Project(Event{
		EventID: "e", EventType: "obligation.created", TenantID: "t-1",
		EmittedAt: time.Now().UTC(),
		Payload:   payload(t, map[string]any{"obligation_id": "ob-1", "amount": "1234.56"}),
	}, "gen-1", "idx-1")

	require.NoError(t, err)
	assert.InDelta(t, 1234.56, res.Projection.Fields["amount"], 0.0001)
}

// A dotted source path resolves into nested payloads. A flat lookup would
// find nothing and project an absent field, which is indistinguishable from
// a field the producer did not send.
func TestProject_ResolvesNestedSourcePaths(t *testing.T) {
	contract := testContract(domain.SearchFieldDefinition{
		FieldID: "currency", SourcePath: "amount.currency_code", Type: "KEYWORD",
		Filterable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal,
	})
	p, err := New(contract, testSource())
	require.NoError(t, err)

	res, err := p.Project(Event{
		EventID: "e", EventType: "obligation.created", TenantID: "t-1",
		EmittedAt: time.Now().UTC(),
		Payload: payload(t, map[string]any{
			"obligation_id": "ob-1",
			"amount":        map[string]any{"value": 100, "currency_code": "EUR"},
		}),
	}, "gen-1", "idx-1")

	require.NoError(t, err)
	assert.Equal(t, "EUR", res.Projection.Fields["currency"])
}

// The content hash is deterministic: the same content hashes the same
// regardless of map iteration order. §5.3 relies on it as completeness
// evidence, which is worthless if it varies run to run.
func TestProject_ContentHashIsDeterministic(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)
	emitted := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	ev := Event{
		EventID: "e", EventType: "obligation.created", TenantID: "t-1",
		EmittedAt: emitted,
		Payload: payload(t, map[string]any{
			"obligation_id": "ob-1", "obligation_code": "GST-Q4",
			"obligation_status": "OPEN", "record_version": 3,
		}),
	}
	first, err := p.Project(ev, "gen-1", "idx-1")
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		again, err := p.Project(ev, "gen-1", "idx-1")
		require.NoError(t, err)
		require.Equal(t, first.Projection.ContentHash, again.Projection.ContentHash)
	}
}

// record_version is preferred over emission time when the producer keeps one,
// so replay suppression follows the source's own ordering rather than ours.
func TestProject_PrefersRecordVersionOverEmissionTime(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)

	res, err := p.Project(Event{
		EventID: "e", EventType: "obligation.created", TenantID: "t-1",
		EmittedAt: time.Now().UTC(),
		Payload:   payload(t, map[string]any{"obligation_id": "ob-1", "record_version": 7}),
	}, "gen-1", "idx-1")

	require.NoError(t, err)
	assert.Equal(t, int64(7), res.Projection.SourceVersion)
}

// FieldMappings never lowers a prohibited field into the engine mapping, so
// the strict mapping refuses it even if a projection somehow carried one.
func TestFieldMappings_OmitsProhibitedFields(t *testing.T) {
	contract := testContract(
		domain.SearchFieldDefinition{FieldID: "obligation_code", Type: "TEXT",
			Searchable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal},
		domain.SearchFieldDefinition{FieldID: "api_secret", Type: "KEYWORD",
			SensitivityClass: domain.SensitivitySecretProhibited},
	)
	mappings := FieldMappings(contract)

	names := map[string]bool{}
	for _, m := range mappings {
		names[m.Name] = true
	}
	assert.True(t, names["obligation_code"])
	assert.False(t, names["api_secret"], "a prohibited field must have no index representation at all")
}

// A projector only claims the event types its source registered, so one
// topic carrying several domains' events does not produce cross-domain
// projections.
func TestHandles_OnlyRegisteredEventTypes(t *testing.T) {
	p, err := New(testContract(), testSource())
	require.NoError(t, err)

	assert.True(t, p.Handles("obligation.created"))
	assert.True(t, p.Handles("obligation.deleted"), "restriction types are handled too")
	assert.False(t, p.Handles("invoice.posted"))
}
