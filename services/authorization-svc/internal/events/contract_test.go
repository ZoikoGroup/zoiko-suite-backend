package events

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// asyncapi.yaml is checked against the code in both directions (DoD gate 1):
// every event the service produces or consumes is declared, and nothing is
// declared that the code does not produce or consume.

type asyncDoc struct {
	Channels map[string]struct {
		Publish *struct {
			Message struct {
				OneOf []struct {
					Ref string `yaml:"$ref"`
				} `yaml:"oneOf"`
			} `yaml:"message"`
		} `yaml:"publish"`
		Subscribe *struct {
			Message struct {
				OneOf []struct {
					Ref string `yaml:"$ref"`
				} `yaml:"oneOf"`
			} `yaml:"message"`
		} `yaml:"subscribe"`
	} `yaml:"channels"`
	Components struct {
		Messages map[string]struct {
			Name string `yaml:"name"`
		} `yaml:"messages"`
	} `yaml:"components"`
}

func serviceRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func loadAsyncAPI(t *testing.T) (produced, consumed map[string]bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(serviceRoot(t), "asyncapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc asyncDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("asyncapi.yaml does not parse: %v", err)
	}
	resolve := func(ref string) string {
		key := strings.TrimPrefix(ref, "#/components/messages/")
		m, ok := doc.Components.Messages[key]
		if !ok || m.Name == "" {
			t.Fatalf("asyncapi.yaml: %s resolves to no named message", ref)
		}
		return m.Name
	}
	produced, consumed = map[string]bool{}, map[string]bool{}
	for _, ch := range doc.Channels {
		if ch.Publish != nil {
			for _, o := range ch.Publish.Message.OneOf {
				produced[resolve(o.Ref)] = true
			}
		}
		if ch.Subscribe != nil {
			for _, o := range ch.Subscribe.Message.OneOf {
				consumed[resolve(o.Ref)] = true
			}
		}
	}
	return produced, consumed
}

func diff(want, got map[string]bool) (missing, extra []string) {
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func TestAsyncAPI_ProducedMatchesTheCode(t *testing.T) {
	produced, _ := loadAsyncAPI(t)
	if missing, extra := diff(ProducedEventTypes, produced); len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("asyncapi.yaml publish out of step with ProducedEventTypes — undeclared: %v, not produced: %v", missing, extra)
	}
}

func TestAsyncAPI_ConsumedMatchesTheConsumers(t *testing.T) {
	_, consumed := loadAsyncAPI(t)
	if missing, extra := diff(AllConsumedEventTypes(), consumed); len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("asyncapi.yaml subscribe out of step with the consumers — undeclared: %v, not consumed: %v", missing, extra)
	}
}

// Every event type written as a literal — in Go (emit, NewEvent, workload
// events, the decision outbox) or in a migration (authz_emit_event, the
// exception trigger's CASE) — is in ProducedEventTypes. Dynamic names built
// from a prefix in SQL are listed there by hand; this catches the literal ones
// that drift.
func TestProducedEventTypes_CoversEveryLiteralEmitter(t *testing.T) {
	root := serviceRoot(t)
	goPatterns := []*regexp.Regexp{
		regexp.MustCompile(`emit(?:Tenant)?\(ctx, "([a-z_]+\.[a-z_.]+)"`),
		regexp.MustCompile(`NewEvent\("([a-z_]+\.[a-z_.]+)"`),
		regexp.MustCompile(`workloadEvent\(r, "([a-z_]+\.[a-z_.]+)"`),
		regexp.MustCompile(`eventType\s*:?=\s*"([a-z_]+\.[a-z_.]+)"`),
	}
	sqlPatterns := []*regexp.Regexp{
		regexp.MustCompile(`authz_emit_event\('([a-z_]+\.[a-z_.]+)'`),
		regexp.MustCompile(`THEN '([a-z_]+\.[a-z_]+\.[a-z_]+)'`),
	}
	found := map[string]string{}
	scan := func(dir, suffix string, pats []*regexp.Regexp) {
		_ = filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, suffix) || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, _ := os.ReadFile(path)
			for _, p := range pats {
				for _, m := range p.FindAllStringSubmatch(string(b), -1) {
					found[m[1]] = filepath.Base(path)
				}
			}
			return nil
		})
	}
	scan("internal", ".go", goPatterns)
	scan(filepath.Join("deployments", "migrations"), ".up.sql", sqlPatterns)
	if len(found) < 10 {
		t.Fatalf("scanner found only %d literal event types — the patterns no longer match the code", len(found))
	}
	var missing []string
	for ev, file := range found {
		if !ProducedEventTypes[ev] {
			missing = append(missing, ev+" ("+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("emitted but not in ProducedEventTypes (and so not checked against asyncapi.yaml): %v", missing)
	}
}
