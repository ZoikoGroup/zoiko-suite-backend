package events

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// asyncapi.yaml and the code agree on which events the service raises: every event type written in the source is a message of the
// document, and every message of the document is raised somewhere.
func TestAsyncAPIMatchesTheEventsTheCodeRaises(t *testing.T) {
	b, err := os.ReadFile("../../asyncapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		AsyncAPI string `yaml:"asyncapi"`
		Channels map[string]struct {
			Subscribe struct {
				Message struct {
					OneOf []map[string]string `yaml:"oneOf"`
				} `yaml:"message"`
			} `yaml:"subscribe"`
		} `yaml:"channels"`
		Components struct {
			Messages map[string]struct {
				Title   string `yaml:"title"`
				Summary string `yaml:"summary"`
			} `yaml:"messages"`
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("asyncapi.yaml is not valid YAML: %v", err)
	}
	if doc.AsyncAPI != "2.6.0" {
		t.Fatalf("asyncapi %q", doc.AsyncAPI)
	}
	ch, ok := doc.Channels["zoiko.general-ledger.events"]
	if !ok {
		t.Fatal("the events topic is not a channel")
	}
	referenced := map[string]bool{}
	for _, m := range ch.Subscribe.Message.OneOf {
		referenced[strings.TrimPrefix(m["$ref"], "#/components/messages/")] = true
	}
	documented := map[string]string{}
	for name, m := range doc.Components.Messages {
		if !referenced[name] {
			t.Errorf("message %s is not offered on the channel", name)
		}
		if m.Title == "" || m.Summary == "" {
			t.Errorf("message %s has no title or summary", name)
		}
		documented[m.Title] = name
	}
	for name := range referenced {
		if _, ok := doc.Components.Messages[name]; !ok {
			t.Errorf("the channel offers %s, which is not a message", name)
		}
	}
	if _, ok := doc.Components.Schemas["Envelope"]; !ok {
		t.Error("the envelope schema is missing")
	}

	// the event types the source writes
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`Fact:\s*"([a-z]+(?:-[a-z]+)*\.[a-z]+(?:-[a-z]+)*)"`),
		regexp.MustCompile(`fact\s*:?=\s*"([a-z]+(?:-[a-z]+)*\.[a-z]+(?:-[a-z]+)*)"`),
		regexp.MustCompile(`(?:ruleOut|\.event|raise)\("([a-z]+(?:-[a-z]+)*\.[a-z]+(?:-[a-z]+)*)"`),
	}
	journal := []*regexp.Regexp{
		regexp.MustCompile(`enqueueJournalEvent\(ctx, tx, "([a-z]+)"`),
		regexp.MustCompile(`LegacyType:\s*"journal\.([a-z]+)"`),
		regexp.MustCompile(`, "(proposed)", map\[string\]any`),
	}
	raised := map[string]bool{}
	err = filepath.Walk("..", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				if !strings.HasPrefix(m[1], "content.") {
					raised["com.zoikosuite.accounting.kernel."+m[1]] = true
				}
			}
		}
		for _, re := range journal {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				raised["com.zoikosuite.accounting.journal."+m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// the conditions of the rule packs name facts too, and are not events
	for typ := range raised {
		if strings.HasPrefix(typ, "com.zoikosuite.accounting.kernel.") && !strings.Contains(strings.TrimPrefix(typ, "com.zoikosuite.accounting.kernel."), ".") {
			delete(raised, typ)
		}
	}

	var notDocumented, notRaised []string
	for typ := range raised {
		if _, ok := documented[typ]; !ok {
			notDocumented = append(notDocumented, typ)
		}
	}
	for typ := range documented {
		if !raised[typ] {
			notRaised = append(notRaised, typ)
		}
	}
	sort.Strings(notDocumented)
	sort.Strings(notRaised)
	if len(notDocumented) > 0 || len(notRaised) > 0 {
		t.Fatalf("asyncapi.yaml is out of step with the code\nraised, not documented: %v\ndocumented, not raised: %v", notDocumented, notRaised)
	}
	if len(raised) < 50 {
		t.Fatalf("only %d event types found in the source; the check is not seeing the code", len(raised))
	}
}
