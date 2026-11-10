package specs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Doc 08 §07: every third-party action is pinned to a full commit SHA and
// every container image to a digest, so a moved tag cannot change CI.
func TestWorkflowsArePinned(t *testing.T) {
	files, err := filepath.Glob("../../../../.github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	uses := regexp.MustCompile(`^\s*(?:-\s*)?uses:\s*([^\s#]+)`)
	image := regexp.MustCompile(`^\s*image:\s*([^\s#]+)`)
	dockerRun := regexp.MustCompile(`docker run\b.*?\s([a-z0-9./-]+/[a-z0-9._-]+:[^\s@]+(?:@sha256:[0-9a-f]{64})?)\s`)
	sha := regexp.MustCompile(`@[0-9a-f]{40}$`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var wf struct {
			Jobs map[string]any `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(b, &wf); err != nil || len(wf.Jobs) == 0 {
			t.Errorf("%s: not a valid workflow: %v", filepath.Base(f), err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			where := filepath.Base(f) + ":" + strconv.Itoa(i+1)
			if m := uses.FindStringSubmatch(line); m != nil && !strings.HasPrefix(m[1], "./") && !sha.MatchString(m[1]) {
				t.Errorf("%s: action %s is not pinned to a commit SHA", where, m[1])
			}
			if m := image.FindStringSubmatch(line); m != nil && !strings.Contains(m[1], "@sha256:") {
				t.Errorf("%s: image %s is not pinned to a digest", where, m[1])
			}
			if m := dockerRun.FindStringSubmatch(line); m != nil && !strings.Contains(m[1], "@sha256:") {
				t.Errorf("%s: docker run image %s is not pinned to a digest", where, m[1])
			}
		}
	}
}

// SEC-T048: the secret scan proves itself on every run: a generated token is
// planted and the job fails unless gitleaks reports it.
func TestSecretScanSelfTest(t *testing.T) {
	b, err := os.ReadFile("../../../../.github/workflows/secret-scan.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	for _, want := range []string{"gitleaks self-test", "dir /scan", "ghp_", "did not flag a planted", "exit 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("secret-scan.yml lacks %q", want)
		}
	}
	if strings.Index(s, "gitleaks self-test") > strings.Index(s, "- name: gitleaks\n") {
		t.Error("the self-test must run before the real scan")
	}
}
