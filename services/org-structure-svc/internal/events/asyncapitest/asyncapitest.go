// Package asyncapitest reads asyncapi.yaml for the tests that keep it honest.
//
// It is a normal package (not a _test file) so that tests in other packages can
// share it; nothing outside tests imports it.
package asyncapitest

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Spec is the parsed asyncapi.yaml.
type Spec struct {
	raw map[string]any
}

// Load reads asyncapi.yaml from the service root.
func Load(t testing.TB) *Spec {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "../../../asyncapi.yaml"))
	if err != nil {
		t.Fatalf("read asyncapi.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse asyncapi.yaml: %v", err)
	}
	s := &Spec{raw: doc}
	if len(s.messages()) == 0 {
		t.Fatal("asyncapi.yaml declares no messages: parsed, but into nothing")
	}
	return s
}

func get(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func (s *Spec) messages() map[string]any {
	m, _ := get(s.raw, "components", "messages").(map[string]any)
	return m
}

// Message is one declared message.
type Message struct {
	Key       string // the components.messages key
	Name      string // the wire name
	SpecEvent string // "" when x-spec-event is null
	Delivery  string
	payload   any
}

// Messages returns every declared message, ordered by wire name.
func (s *Spec) Messages() []Message {
	var out []Message
	for key, v := range s.messages() {
		m, _ := v.(map[string]any)
		name, _ := m["name"].(string)
		spec, _ := m["x-spec-event"].(string)
		delivery, _ := m["x-delivery"].(string)
		out = append(out, Message{Key: key, Name: name, SpecEvent: spec, Delivery: delivery, payload: m["payload"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ByName finds a message by wire name.
func (s *Spec) ByName(name string) (Message, bool) {
	for _, m := range s.Messages() {
		if m.Name == name {
			return m, true
		}
	}
	return Message{}, false
}

// ChannelKeys are the message keys the channel's oneOf lists.
func (s *Spec) ChannelKeys() []string {
	var keys []string
	channels, _ := get(s.raw, "channels").(map[string]any)
	for _, c := range channels {
		oneOf, _ := get(c.(map[string]any), "subscribe", "message", "oneOf").([]any)
		for _, e := range oneOf {
			if ref, _ := e.(map[string]any)["$ref"].(string); ref != "" {
				keys = append(keys, ref[strings.LastIndex(ref, "/")+1:])
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// Schema is a schema flattened across allOf and $ref: every property and every
// required name that applies.
type Schema struct {
	Properties map[string]any
	Required   []string
}

func (s *Spec) flatten(node any) Schema {
	out := Schema{Properties: map[string]any{}}
	m, ok := node.(map[string]any)
	if !ok {
		return out
	}
	if ref, _ := m["$ref"].(string); ref != "" {
		target, _ := get(s.raw, append([]string{"components"}, strings.Split(strings.TrimPrefix(ref, "#/components/"), "/")...)...).(map[string]any)
		return s.flatten(target)
	}
	if all, ok := m["allOf"].([]any); ok {
		for _, part := range all {
			sub := s.flatten(part)
			for k, v := range sub.Properties {
				out.Properties[k] = merge(out.Properties[k], v)
			}
			out.Required = append(out.Required, sub.Required...)
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for k, v := range props {
			out.Properties[k] = merge(out.Properties[k], v)
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				out.Required = append(out.Required, name)
			}
		}
	}
	sort.Strings(out.Required)
	return out
}

// merge combines two declarations of one property (the envelope says
// `payload: {type: object}`, an allOf member narrows it).
func merge(a, b any) any {
	if a == nil {
		return b
	}
	return map[string]any{"allOf": []any{a, b}}
}

// Envelope is the message's top-level schema.
func (s *Spec) Envelope(m Message) Schema { return s.flatten(m.payload) }

// Payload is the schema of the message's `payload` member.
func (s *Spec) Payload(m Message) Schema { return s.flatten(s.Envelope(m).Properties["payload"]) }

// Details is the schema of the payload's `details` member.
func (s *Spec) Details(m Message) Schema { return s.flatten(s.Payload(m).Properties["details"]) }

// Component returns a components.schemas entry, flattened.
func (s *Spec) Component(name string) Schema {
	return s.flatten(get(s.raw, "components", "schemas", name))
}
