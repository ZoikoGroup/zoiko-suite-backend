package events_test

// Event contract parity: asyncapi.yaml and the code must name the same events.
//
// An AsyncAPI file that nothing checks is documentation, and documentation
// drifts. What makes ORG §9.2's contract gate mean anything here is that a
// wire name can only change in two places at once.
//
// This also pins the SPEC-name mapping. The specification names events
// TenantActivated and LegalEntityProfileAmended; the wire names are
// tenant.activated and entity.profile.amended. Both are correct, and the only
// way an auditor can check that ORG-02's five named events are all published is
// if the mapping between them is written down and verified rather than
// reconstructed by eye.

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

type asyncAPIDoc struct {
	Components struct {
		Messages map[string]struct {
			Name      string  `yaml:"name"`
			SpecEvent *string `yaml:"x-spec-event"`
			Delivery  string  `yaml:"x-delivery"`
		} `yaml:"messages"`
	} `yaml:"components"`
	Channels map[string]struct {
		Subscribe struct {
			Message struct {
				OneOf []struct {
					Ref string `yaml:"$ref"`
				} `yaml:"oneOf"`
			} `yaml:"message"`
		} `yaml:"subscribe"`
	} `yaml:"channels"`
}

func loadAsyncAPI(t *testing.T) asyncAPIDoc {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "../../asyncapi.yaml"))
	if err != nil {
		t.Fatalf("read asyncapi.yaml: %v", err)
	}
	var doc asyncAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse asyncapi.yaml: %v", err)
	}
	if len(doc.Components.Messages) == 0 {
		t.Fatal("asyncapi.yaml declares no messages — parsed, but into nothing")
	}
	return doc
}

// codeEventNames is every wire name the code can emit.
//
// Listed here rather than derived by reflection because the point is to have
// TWO independent statements of the same fact: if a constant is added and this
// list is not updated, the next test fails and someone decides deliberately
// whether the event belongs in the contract.
func codeEventNames() []string {
	return []string{
		events.EventTenantCreated,
		events.EventTenantActivated,
		events.EventTenantSuspended,
		events.EventTenantResumed,
		events.EventTenantTerminationInitiated,
		events.EventTenantTerminated,
		events.EventTenantDefaultsChanged,
		events.EventLegalEntityCreated,
		events.EventLegalEntityProfileAmended,
		events.EventLegalEntityStatusChanged,
		events.EventRegisteredOfficeChanged,
		events.EventLegalEntityNameChanged,
		events.EventRegistryConflictQuarantined,
		events.EventRegistryConflictResolved,
	}
}

func TestAsyncAPI_DeclaresEveryEventTheCodeEmits(t *testing.T) {
	doc := loadAsyncAPI(t)

	declared := map[string]bool{}
	for _, m := range doc.Components.Messages {
		declared[m.Name] = true
	}

	var missing []string
	for _, name := range codeEventNames() {
		if !declared[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("events the code emits but asyncapi.yaml does not declare:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

func TestAsyncAPI_EveryDeclaredMessageIsOnTheChannel(t *testing.T) {
	doc := loadAsyncAPI(t)

	onChannel := map[string]bool{}
	for _, ch := range doc.Channels {
		for _, ref := range ch.Subscribe.Message.OneOf {
			onChannel[ref.Ref[strings.LastIndex(ref.Ref, "/")+1:]] = true
		}
	}

	var orphaned []string
	for key := range doc.Components.Messages {
		if !onChannel[key] {
			orphaned = append(orphaned, key)
		}
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		// A message defined but not referenced by a channel is invisible to a
		// consumer generating from this contract: it reads as an event nobody
		// publishes.
		t.Fatalf("messages defined but not listed on any channel:\n  %s",
			strings.Join(orphaned, "\n  "))
	}
}

// TestAsyncAPI_CoversEveryORGSpecEvent is the auditor's check: each event the
// ORG-02/ORG-03 specification names must map to exactly one wire name.
func TestAsyncAPI_CoversEveryORGSpecEvent(t *testing.T) {
	doc := loadAsyncAPI(t)

	// spec name -> wire names claiming it
	byspec := map[string][]string{}
	for _, m := range doc.Components.Messages {
		if m.SpecEvent != nil && *m.SpecEvent != "" {
			byspec[*m.SpecEvent] = append(byspec[*m.SpecEvent], m.Name)
		}
	}

	// ORG-02 §4.2 "Events produced" and ORG-03 §4.3 "Events produced".
	for _, specEvent := range []string{
		"TenantCreated",
		"TenantActivated",
		"TenantSuspended",
		"TenantTerminationInitiated",
		"TenantTerminated",
		"LegalEntityCreated",
		"LegalEntityProfileAmended",
		"LegalEntityStatusChanged",
		"RegisteredOfficeChanged",
	} {
		wire := byspec[specEvent]
		switch len(wire) {
		case 0:
			t.Errorf("ORG spec event %q maps to no published event", specEvent)
		case 1: // correct
		default:
			// Two wire names claiming one spec event means a consumer
			// subscribing to the spec event has to guess which to listen for.
			t.Errorf("ORG spec event %q maps to %d wire names: %v", specEvent, len(wire), wire)
		}
	}
}

// TestAsyncAPI_CommandEventMappingMatchesTheCode pins events.TenantCommandEvent
// against the contract, so a command cannot quietly start emitting a different
// event than the one documented for it.
func TestAsyncAPI_CommandEventMappingMatchesTheCode(t *testing.T) {
	doc := loadAsyncAPI(t)
	declared := map[string]bool{}
	for _, m := range doc.Components.Messages {
		declared[m.Name] = true
	}

	for command, want := range map[string]string{
		"ActivateTenant":      events.EventTenantActivated,
		"SuspendTenant":       events.EventTenantSuspended,
		"ResumeTenant":        events.EventTenantResumed,
		"InitiateTermination": events.EventTenantTerminationInitiated,
		"CompleteTermination": events.EventTenantTerminated,
		"ChangeDefaultLocale": events.EventTenantDefaultsChanged,
	} {
		got := events.TenantCommandEvent(command)
		if got != want {
			t.Errorf("command %s emits %q, expected %q", command, got, want)
		}
		if !declared[got] {
			t.Errorf("command %s emits %q, which asyncapi.yaml does not declare", command, got)
		}
	}

	// A command outside ORG-02's list must not silently map to some event.
	if got := events.TenantCommandEvent("DeleteTenant"); got != "" {
		t.Errorf("unknown command mapped to event %q; it must map to nothing", got)
	}
}

// TestAsyncAPI_OutboxDeliveryIsClaimedOnlyWhereItIsTrue guards the delivery
// annotation.
//
// x-delivery: outbox is a promise that the event and the fact it attests commit
// together. Every ORG-02/ORG-03 command path makes that promise good; the
// pre-existing paths do not and are annotated `direct`. Claiming `outbox` for a
// direct path would tell a consumer it can rely on a guarantee that is not
// there.
func TestAsyncAPI_OutboxDeliveryIsClaimedOnlyWhereItIsTrue(t *testing.T) {
	doc := loadAsyncAPI(t)

	transactional := map[string]bool{
		events.EventTenantActivated:            true,
		events.EventTenantSuspended:            true,
		events.EventTenantResumed:              true,
		events.EventTenantTerminationInitiated: true,
		events.EventTenantTerminated:           true,
		events.EventTenantDefaultsChanged:      true,
		events.EventLegalEntityProfileAmended:  true,
		events.EventLegalEntityNameChanged:     true,
		events.EventRegisteredOfficeChanged:    true,
	}

	for _, m := range doc.Components.Messages {
		if m.Delivery == "" {
			t.Errorf("%s: no x-delivery annotation", m.Name)
			continue
		}
		if m.Delivery != "outbox" && m.Delivery != "direct" {
			t.Errorf("%s: unknown x-delivery %q", m.Name, m.Delivery)
			continue
		}
		if m.Delivery == "outbox" && !transactional[m.Name] {
			t.Errorf("%s claims transactional outbox delivery, but is not published from an outbox path", m.Name)
		}
		if m.Delivery == "direct" && transactional[m.Name] {
			t.Errorf("%s is published through the outbox but claims direct delivery", m.Name)
		}
	}
}
