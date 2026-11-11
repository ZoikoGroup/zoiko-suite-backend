package catalogue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const asyncapiDir = "../asyncapi"

// The committed documents must equal what the catalogue generates, so the
// catalogue and its AsyncAPI contracts cannot drift (ZS-EVENT-001 section 21).
func TestAsyncAPIFilesAreCurrent(t *testing.T) {
	domains, err := AsyncAPIDomains()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range domains {
		want, err := AsyncAPI(d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(asyncapiDir, d+".asyncapi.json"))
		if err != nil {
			t.Fatalf("%s: %v\nrun: go generate ./catalogue (in services/eventing)", d, err)
		}
		// CRLF checkouts must not read as drift.
		if strings.ReplaceAll(string(got), "\r\n", "\n") != string(want) {
			t.Errorf("%s.asyncapi.json is out of date; run: go generate ./catalogue (in services/eventing)", d)
		}
	}
	files, _ := filepath.Glob(filepath.Join(asyncapiDir, "*.asyncapi.json"))
	if len(files) != len(domains) {
		t.Errorf("%d files for %d domains: remove stale documents", len(files), len(domains))
	}
}

// Every catalogued type appears in exactly one message, on a channel whose
// address follows the section 5.2 convention.
func TestAsyncAPICoversEveryTypeOnce(t *testing.T) {
	all, _ := All()
	seen := map[string]int{}
	domains, _ := AsyncAPIDomains()
	for _, d := range domains {
		b, err := AsyncAPI(d)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			AsyncAPI string `json:"asyncapi"`
			Channels map[string]struct {
				Address string `json:"address"`
			} `json:"channels"`
			Components struct {
				Messages map[string]struct {
					Name string `json:"name"`
				} `json:"messages"`
			} `json:"components"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: not valid JSON: %v", d, err)
		}
		if doc.AsyncAPI != "3.1.0" {
			t.Errorf("%s: asyncapi %q", d, doc.AsyncAPI)
		}
		for _, c := range doc.Channels {
			if !strings.HasPrefix(c.Address, "zoikosuite."+d+".") || !strings.HasSuffix(c.Address, ".v1") {
				t.Errorf("%s: bad channel address %q", d, c.Address)
			}
		}
		for _, m := range doc.Components.Messages {
			seen[m.Name]++
		}
	}
	for _, e := range all {
		if seen[e.Type] != 1 {
			t.Errorf("%s appears in %d messages, want 1", e.Type, seen[e.Type])
		}
	}
}

func TestAsyncAPIUnknownDomain(t *testing.T) {
	if _, err := AsyncAPI("nope"); err == nil {
		t.Fatal("an empty domain must be an error")
	}
}
