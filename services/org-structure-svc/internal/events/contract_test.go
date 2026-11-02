package events_test

// Event contract parity: asyncapi.yaml and the code must name the same events.
//
// An AsyncAPI file that nothing checks is documentation, and documentation
// drifts. What makes it a contract is that a wire name, a spec name or a
// required field can only change in two places at once.

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"zoiko.io/org-structure-svc/internal/events"
	"zoiko.io/org-structure-svc/internal/events/asyncapitest"
)

// The ORG-05 events are exactly the outbox-delivered messages.
func TestAsyncAPI_DocumentsExactlyTheEventsTheCodeEmits(t *testing.T) {
	spec := asyncapitest.Load(t)

	documented := map[string]bool{}
	for _, m := range spec.Messages() {
		if m.Delivery == "outbox" {
			documented[m.Name] = true
		}
	}
	for _, name := range events.OrgEventNames {
		if !documented[name] {
			t.Errorf("code emits %s but asyncapi.yaml does not declare it as an outbox message", name)
		}
		delete(documented, name)
	}
	for name := range documented {
		t.Errorf("asyncapi.yaml declares %s but the code never emits it", name)
	}
}

// Every message is offered on the channel, and nothing on the channel is
// undeclared, so a consumer generated from the file sees every event.
func TestAsyncAPI_ChannelListsEveryMessage(t *testing.T) {
	spec := asyncapitest.Load(t)

	var declared []string
	for _, m := range spec.Messages() {
		declared = append(declared, m.Key)
	}
	sort.Strings(declared)
	if got := spec.ChannelKeys(); strings.Join(got, ",") != strings.Join(declared, ",") {
		t.Fatalf("the channel must list exactly the declared messages:\n channel:  %v\n declared: %v", got, declared)
	}
}

// The spec names are a mapping, not a coincidence: each wire name's spec name in
// the file is the one the code records, and the four events s4.5 names for
// ORG-05 are all there.
func TestAsyncAPI_SpecEventMappingMatchesTheCode(t *testing.T) {
	spec := asyncapitest.Load(t)

	for _, wire := range events.OrgEventNames {
		want, known := events.SpecEvent[wire]
		if !known {
			t.Errorf("%s has no entry in events.SpecEvent", wire)
			continue
		}
		m, ok := spec.ByName(wire)
		if !ok {
			continue // reported by the test above
		}
		if m.SpecEvent != want {
			t.Errorf("%s: asyncapi.yaml says x-spec-event %q, code says %q", wire, m.SpecEvent, want)
		}
	}

	have := map[string]bool{}
	for _, m := range spec.Messages() {
		if m.SpecEvent != "" {
			have[m.SpecEvent] = true
		}
	}
	for _, name := range []string{"OrgUnitCreated", "OrgUnitReparented", "OrgUnitManagerChanged", "OrgUnitDeactivated"} {
		if !have[name] {
			t.Errorf("ZS-SVC-I-001 s4.5 produces %s, but no message maps to it", name)
		}
	}
}

// The spec's event minimum (s7) is required of every ORG-05 payload, and every
// event documents its own details.
func TestAsyncAPI_PayloadRequiresTheSpecMinimum(t *testing.T) {
	spec := asyncapitest.Load(t)

	base := spec.Component("OrgUnitPayload")
	required := map[string]bool{}
	for _, r := range base.Required {
		required[r] = true
	}
	for _, field := range []string{"tenant_id", "object_id", "object_version", "effective_at", "recorded_at", "actor", "correlation_id"} {
		if !required[field] {
			t.Errorf("OrgUnitPayload must require %q (ZS-SVC-I-001 s7)", field)
		}
		if _, ok := base.Properties[field]; !ok {
			t.Errorf("OrgUnitPayload does not declare %q", field)
		}
	}
	if _, ok := base.Properties["evidence_ref"]; !ok {
		t.Error("evidence_ref must be declared (carried where material)")
	}
	if required["evidence_ref"] {
		t.Error("evidence_ref is only carried where material, so it must not be required")
	}

	for _, name := range events.OrgEventNames {
		m, ok := spec.ByName(name)
		if !ok {
			continue
		}
		payload := spec.Payload(m)
		for _, field := range base.Required {
			if _, ok := payload.Properties[field]; !ok {
				t.Errorf("%s: payload does not include the common field %q", name, field)
			}
		}
		details := spec.Details(m)
		if len(details.Required) == 0 {
			t.Errorf("%s documents no required details", name)
		}
		for _, r := range details.Required {
			if _, ok := details.Properties[r]; !ok {
				t.Errorf("%s: required detail %q is not declared as a property", name, r)
			}
		}
	}
}

// The older events are still published, so they stay declared, and are marked
// best-effort so nobody mistakes them for outbox events.
func TestAsyncAPI_LegacyEventsAreDeclaredAsBestEffort(t *testing.T) {
	spec := asyncapitest.Load(t)

	_, thisFile, _, _ := runtime.Caller(0)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "publisher.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []string{"org.structure.changed", "position.created", "employee.assigned"} {
		if !strings.Contains(string(src), `"`+wire+`"`) {
			t.Errorf("publisher.go no longer emits %s: remove it from asyncapi.yaml too", wire)
		}
		m, ok := spec.ByName(wire)
		if !ok {
			t.Errorf("publisher.go emits %s but asyncapi.yaml does not declare it", wire)
			continue
		}
		if m.Delivery != "best-effort" {
			t.Errorf("%s is published directly after commit, so it is best-effort, not %q", wire, m.Delivery)
		}
	}
}
