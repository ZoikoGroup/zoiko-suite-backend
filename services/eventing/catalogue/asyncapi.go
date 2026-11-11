package catalogue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// AsyncAPIVersion is the AsyncAPI release ZS-EVENT-001 section 12 names as the baseline.
const AsyncAPIVersion = "3.1.0"

// ChannelAddress is the logical channel of an aggregate stream (section 5.2):
// zoikosuite.<domain>.<stream>.v<major>. A channel is a contract family, not one
// topic per event type, so every fact about an aggregate shares it.
func ChannelAddress(e Entry) string {
	return "zoikosuite." + e.Domain() + "." + e.Aggregate() + ".v1"
}

// AsyncAPIDomains returns the domains present in the catalogue, sorted.
func AsyncAPIDomains() ([]string, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range all {
		seen[e.Domain()] = true
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// AsyncAPI renders the AsyncAPI 3.1.0 document of one domain from the catalogue.
// The output is deterministic (encoding/json sorts map keys), so a committed
// copy can be compared byte for byte in CI.
//
// What it does and does not say. It states channels, messages, the producing
// operation and the common CloudEvents-aligned envelope. It does NOT state the
// per-event payload (`data`): payload schemas live in schema-registry-svc and are
// referenced by the envelope's dataschema attribute, so a message's data is an
// open object here until its schema is registered. An honest placeholder is
// better than an invented schema.
func AsyncAPI(domain string) ([]byte, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	channels := map[string]any{}
	messages := map[string]any{}
	chanMessages := map[string]map[string]any{}
	chanProducers := map[string]map[string]bool{}
	var count int

	for _, e := range all {
		if e.Domain() != domain {
			continue
		}
		count++
		addr := ChannelAddress(e)
		chanKey := e.Aggregate()
		msgKey := e.FactPath(true)
		origin := "ZS-EVENT-001 section 19"
		if e.Origin != "" {
			origin = "extension (emitted by a service, not listed in section 19)"
		}
		messages[msgKey] = map[string]any{
			"name":               e.Type,
			"title":              strings.ReplaceAll(e.FactPath(true), ".", " "),
			"summary":            fmt.Sprintf("%s: %s.", e.Subject, e.Fact()),
			"description":        fmt.Sprintf("Producer: %s. Primary consumers / intent: %s. Family: %s. Origin: %s.", e.Producer, e.Consumers, e.Family, origin),
			"contentType":        "application/json",
			"payload":            map[string]any{"$ref": "#/components/schemas/CloudEventEnvelope"},
			"x-zoiko-event-type": e.Type,
			"x-zoiko-origin":     originValue(e),
		}
		if chanMessages[chanKey] == nil {
			chanMessages[chanKey] = map[string]any{}
			chanProducers[chanKey] = map[string]bool{}
		}
		chanMessages[chanKey][e.FactPath(false)] = map[string]any{"$ref": "#/components/messages/" + msgKey}
		chanProducers[chanKey][e.Producer] = true
		channels[chanKey] = map[string]any{"address": addr, "messages": chanMessages[chanKey]}
	}
	if count == 0 {
		return nil, fmt.Errorf("catalogue: no events in domain %q", domain)
	}

	ops := map[string]any{}
	for chanKey := range channels {
		var producers []string
		for p := range chanProducers[chanKey] {
			producers = append(producers, p)
		}
		sort.Strings(producers)
		var refs []any
		var facts []string
		for fact := range chanMessages[chanKey] {
			facts = append(facts, fact)
		}
		sort.Strings(facts)
		for _, f := range facts {
			refs = append(refs, map[string]any{"$ref": "#/channels/" + chanKey + "/messages/" + f})
		}
		ops["publish."+chanKey] = map[string]any{
			"action":      "send",
			"channel":     map[string]any{"$ref": "#/channels/" + chanKey},
			"summary":     "Publish " + chanKey + " facts after the authoritative commit (transactional outbox).",
			"description": "Producer(s): " + strings.Join(producers, ", ") + ". Only the owning domain may publish (ZS-EVENT-001 E-02).",
			"messages":    refs,
		}
	}

	doc := map[string]any{
		"asyncapi": AsyncAPIVersion,
		"info": map[string]any{
			"title":   "ZoikoSuite " + domain + " domain events",
			"version": "1.0.0",
			"description": "Generated from services/eventing/catalogue/catalogue.json (ZS-EVENT-001 section 19). " +
				"Do not edit by hand: run `go generate ./catalogue` in services/eventing.",
		},
		"channels":   channels,
		"operations": ops,
		"components": map[string]any{
			"messages": messages,
			"schemas":  map[string]any{"CloudEventEnvelope": envelopeSchema()},
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func originValue(e Entry) string {
	if e.Origin == "" {
		return "spec"
	}
	return e.Origin
}

// envelopeSchema is the section 4 canonical envelope. `data` is open (see AsyncAPI).
func envelopeSchema() map[string]any {
	str := map[string]any{"type": "string"}
	return map[string]any{
		"type": "object",
		"required": []string{"specversion", "id", "source", "type", "time", "datacontenttype", "dataschema",
			"tenantid", "publishedat", "residencyregion", "classification", "schemaversion", "data"},
		"properties": map[string]any{
			"specversion":      map[string]any{"type": "string", "const": "1.0"},
			"id":               map[string]any{"type": "string", "description": "UUIDv7, immutable event fact id."},
			"source":           map[string]any{"type": "string", "description": "urn:zoikosuite:service:<service>"},
			"type":             map[string]any{"type": "string", "pattern": `^com\.zoikosuite\.[a-z0-9-]+\.[a-z0-9-]+(\.[a-z0-9-]+)+$`},
			"subject":          str,
			"time":             map[string]any{"type": "string", "format": "date-time"},
			"datacontenttype":  map[string]any{"type": "string", "const": "application/json"},
			"dataschema":       map[string]any{"type": "string", "description": "Registered payload schema (schema-registry-svc)."},
			"tenantid":         str,
			"legalentityid":    str,
			"bookid":           str,
			"aggregateid":      str,
			"aggregateversion": map[string]any{"type": "integer", "minimum": 0},
			"correlationid":    str,
			"causationid":      str,
			"requestid":        str,
			"effectiveat":      map[string]any{"type": "string", "format": "date-time"},
			"publishedat":      map[string]any{"type": "string", "format": "date-time"},
			"residencyregion":  str,
			"classification":   map[string]any{"type": "string", "enum": []string{"public", "internal", "confidential", "restricted"}},
			"schemaversion":    map[string]any{"type": "string", "pattern": `^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`},
			"payloadhash":      map[string]any{"type": "string", "pattern": "^sha256:"},
			"traceparent":      str,
			"data":             map[string]any{"type": "object", "description": "Event-specific fact payload; its schema is registered in schema-registry-svc."},
		},
	}
}
