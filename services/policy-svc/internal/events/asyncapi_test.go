package events_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The event contract (asyncapi.yaml) and the code agree: every event type the
// code can publish or consume is documented, and the document names none the
// code does not have.
func TestAsyncAPI_MatchesTheCode(t *testing.T) {
	doc, err := os.ReadFile("../../asyncapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`\{name: ([a-z_.]+),`).FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]] = true
	}
	literal := regexp.MustCompile(`"((?:accounting\.)?policy\.[a-z_]+(?:\.[a-z_]+)*|entity\.created|role\.updated|authority\.delegated)"`)
	inCode := map[string]bool{}
	err = filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			inCode[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing, extra []string
	for e := range inCode {
		if !documented[e] {
			missing = append(missing, e)
		}
	}
	for e := range documented {
		if !inCode[e] {
			extra = append(extra, e)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(inCode) < 25 || len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("asyncapi.yaml out of step with the code (%d types in code): undocumented %v, not in code %v", len(inCode), missing, extra)
	}
}
