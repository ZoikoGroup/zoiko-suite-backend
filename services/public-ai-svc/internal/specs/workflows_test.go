package specs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
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

