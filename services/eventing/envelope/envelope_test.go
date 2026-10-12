package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validSpec() Spec {
	v := int64(3)
	return Spec{
		Type:             "com.zoikosuite.accounting.journal.posted",
		LegacyType:       "journal.posted",
		Service:          "general-ledger-svc",
		SchemaVersion:    "1.0.0",
		OccurredAt:       time.Date(2026, 10, 7, 9, 15, 30, 0, time.UTC),
		TenantID:         "tenant-1",
		LegalEntityID:    "le-1",
		AggregateType:    "journal",
		AggregateID:      "j-1",
		AggregateVersion: &v,
		CorrelationID:    "corr-1",
		ActorID:          "user-1",
		ResidencyRegion:  "uk",
		Classification:   Confidential,
		Data:             map[string]any{"journal_id": "j-1", "note": "a<b & c>d"},
	}
}

func render(t *testing.T, e *Envelope) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func str(t *testing.T, m map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("member %q missing", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("member %q is not a string: %s", key, raw)
	}
	return s
}

func TestNew_RejectsEveryMissingRequiredAttribute(t *testing.T) {
	cases := map[string]func(*Spec){
		"type not canonical":       func(s *Spec) { s.Type = "journal.posted" },
		"type with version suffix": func(s *Spec) { s.Type = "com.zoikosuite.accounting.journal.posted.v1" },
		"service missing":          func(s *Spec) { s.Service = "" },
		"schema version not semver": func(s *Spec) {
			s.SchemaVersion = "1.0"
		},
		"time missing":                   func(s *Spec) { s.OccurredAt = time.Time{} },
		"tenant missing":                 func(s *Spec) { s.TenantID = " " },
		"region missing":                 func(s *Spec) { s.ResidencyRegion = "" },
		"classification unknown":         func(s *Spec) { s.Classification = "secret" },
		"aggregate id without type":      func(s *Spec) { s.AggregateType = "" },
		"aggregate version no aggregate": func(s *Spec) { s.AggregateType, s.AggregateID = "", "" },
		"negative aggregate version": func(s *Spec) {
			v := int64(-1)
			s.AggregateVersion = &v
		},
		"data missing":           func(s *Spec) { s.Data = nil },
		"data not an object":     func(s *Spec) { s.Data = []int{1, 2} },
		"data does not marshal":  func(s *Spec) { s.Data = map[string]any{"c": make(chan int)} },
		"aggregate type unsafe":  func(s *Spec) { s.AggregateType = "Journal Entry" },
		"service not kebab-case": func(s *Spec) { s.Service = "GeneralLedger" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mutate(&s)
			_, err := New(s)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestNew_CanonicalAttributes(t *testing.T) {
	e, err := New(validSpec())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id, err := uuid.Parse(e.ID)
	if err != nil || id.Version() != 7 {
		t.Fatalf("id %q must be a UUIDv7", e.ID)
	}
	m := render(t, e)
	want := map[string]string{
		"specversion":     "1.0",
		"source":          "urn:zoikosuite:service:general-ledger-svc",
		"type":            "com.zoikosuite.accounting.journal.posted",
		"subject":         "urn:zoikosuite:journal:j-1",
		"datacontenttype": "application/json",
		"dataschema":      "urn:zoikosuite:schema:accounting.journal.posted:1.0.0",
		"tenantid":        "tenant-1",
		"legalentityid":   "le-1",
		"residencyregion": "uk",
		"classification":  "confidential",
		"schemaversion":   "1.0.0",
		"id":              e.ID,
	}
	for k, v := range want {
		if got := str(t, m, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if string(m["aggregateversion"]) != "3" {
		t.Errorf("aggregateversion = %s, want 3", m["aggregateversion"])
	}
	if _, ok := m["publishedat"]; ok {
		t.Error("publishedat must be stamped by the dispatcher, not the producer")
	}
}

// The integrity reference must verify against the data bytes a consumer
// actually receives — including characters encoding/json escapes.
func TestPayloadHash_VerifiesAgainstRenderedData(t *testing.T) {
	e, err := New(validSpec())
	if err != nil {
		t.Fatal(err)
	}
	m := render(t, e)
	if got := PayloadHash(m["data"]); got != str(t, m, "payloadhash") {
		t.Fatalf("payloadhash %s does not verify against rendered data (%s)", str(t, m, "payloadhash"), got)
	}
	if !bytes.Equal(m["data"], m["payload"]) {
		t.Fatal("legacy payload must carry identical bytes to data")
	}
}

func TestLegacyFields_OnByDefault_OffOnRequest(t *testing.T) {
	e, _ := New(validSpec())
	m := render(t, e)
	for k, v := range map[string]string{
		"event_id":        e.ID,
		"event_type":      "journal.posted",
		"source_service":  "general-ledger-svc",
		"tenant_id":       "tenant-1",
		"legal_entity_id": "le-1",
		"actor_id":        "user-1",
		"correlation_id":  "corr-1",
		"schema_version":  "1.0.0",
	} {
		if got := str(t, m, k); got != v {
			t.Errorf("legacy %s = %q, want %q", k, got, v)
		}
	}
	emitted, err := time.Parse(time.RFC3339Nano, str(t, m, "emitted_at"))
	if err != nil || !emitted.Equal(e.Time) {
		t.Errorf("legacy emitted_at %q must equal time", str(t, m, "emitted_at"))
	}

	s := validSpec()
	s.OmitLegacyFields = true
	e2, _ := New(s)
	m2 := render(t, e2)
	for _, k := range []string{"event_id", "event_type", "emitted_at", "tenant_id", "payload", "source_service"} {
		if _, ok := m2[k]; ok {
			t.Errorf("legacy member %q present with OmitLegacyFields", k)
		}
	}
}

func TestLegacyType_DefaultsToCanonicalType(t *testing.T) {
	s := validSpec()
	s.LegacyType = ""
	e, _ := New(s)
	if got := str(t, render(t, e), "event_type"); got != s.Type {
		t.Fatalf("event_type = %q, want %q", got, s.Type)
	}
}

func TestWithPublishedAt_PreservesEveryOtherByte(t *testing.T) {
	e, _ := New(validSpec())
	rendered, _ := json.Marshal(e)
	at := time.Date(2026, 10, 7, 9, 15, 31, 0, time.UTC)

	out, err := WithPublishedAt(rendered, at)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(out, rendered[1:]) {
		t.Fatal("stamping publishedat must not alter the rest of the envelope")
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("stamped envelope is not valid JSON: %v", err)
	}
	if got := str(t, m, "publishedat"); got != "2026-10-07T09:15:31Z" {
		t.Fatalf("publishedat = %q", got)
	}
	if PayloadHash(m["data"]) != str(t, m, "payloadhash") {
		t.Fatal("payloadhash no longer verifies after stamping")
	}

	if _, err := WithPublishedAt(out, at); err == nil {
		t.Fatal("stamping twice must fail rather than emit a duplicate member")
	}
	if _, err := WithPublishedAt([]byte(`[1]`), at); err == nil {
		t.Fatal("a non-object must be rejected")
	}
	empty, err := WithPublishedAt([]byte(`{}`), at)
	if err != nil || !json.Valid(empty) {
		t.Fatalf("empty object: %s, %v", empty, err)
	}
}

func TestPartitionKey_StablePerAggregateAndOpaque(t *testing.T) {
	a, _ := New(validSpec())
	b, _ := New(validSpec())
	if a.ID == b.ID {
		t.Fatal("each envelope needs its own id")
	}
	if a.PartitionKey() != b.PartitionKey() {
		t.Fatal("events for one aggregate must share a partition key")
	}
	s := validSpec()
	s.AggregateID = "j-2"
	c, _ := New(s)
	if c.PartitionKey() == a.PartitionKey() {
		t.Fatal("different aggregates should not share a key")
	}
	if strings.Contains(a.PartitionKey(), "tenant-1") || strings.Contains(a.PartitionKey(), "j-1") {
		t.Fatal("partition key must not expose business identifiers")
	}
}
