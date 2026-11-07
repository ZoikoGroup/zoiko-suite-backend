package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/httpapi"
	"zoiko.io/public-ai-svc/internal/session"
)

// docsCase is one visitor message quoted (or closely paraphrased) in Docs
// 01-11, with the outcome the document requires (testdata/docs-questions.json).
type docsCase struct {
	ID      string   `json:"id"`
	Refs    []string `json:"refs"`
	Before  []string `json:"before"`
	Message string   `json:"message"`
	Expect  struct {
		Outcome string   `json:"outcome"`
		Intent  string   `json:"intent"`
		Must    []string `json:"must"`
		MustNot []string `json:"must_not"`
		Note    string   `json:"note"`
		// Also lists outcomes the document itself accepts, each with the
		// phrases the reply must then contain and why it is acceptable.
		Also []struct {
			Outcome string   `json:"outcome"`
			Must    []string `json:"must"`
			Why     string   `json:"why"`
		} `json:"also"`
	} `json:"expect"`
}

// docsOutcome maps a document's expected outcome to the envelope statuses
// that satisfy it.
var docsOutcome = map[string][]contract.Status{
	"ANSWER":  {contract.Answered},
	"CLARIFY": {contract.Clarify},
	"ABSTAIN": {contract.Abstain},
	"REFUSE":  {contract.RefusedAction},
	"HANDOFF": {contract.Handoff},
}

func loadDocsCases(t *testing.T) []docsCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "docs-questions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []docsCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.Cases
}

// publicKBServer is the HTTP API over the dev knowledge base the console uses.
func publicKBServer(t *testing.T) *h {
	t.Helper()
	env, err := fixtureenv.NewPublicKB(ctx, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AllowedOrigins: []string{origin}, MaxBodyBytes: 16 * 1024, MaxActiveTurnsPerSession: 1,
		PolicyVersion: "pub-policy-1.0", ModelTimeout: time.Second, Environment: "dev"}
	srv := httpapi.New(cfg, nil, httpapi.Deps{Store: env.Store, Sessions: session.New(env.Store, time.Hour), Answerer: env.Pipeline,
		Config: env.Releases, Routes: env.Releases, Ready: func() bool { return true }})
	return &h{t: t, env: env, srv: srv.Router()}
}

// TestDocsQuestions runs every visitor question in Docs 01-11 (including the
// earlier turns of multi-turn scripts) and checks the governed outcome each
// document requires. DOCS_REPORT=1 prints every case.
func TestDocsQuestions(t *testing.T) {
	cases := loadDocsCases(t)
	x := publicKBServer(t)
	report := os.Getenv("DOCS_REPORT") != ""
	var fails []string
	byOutcome := map[string][2]int{}
	for i, c := range cases {
		s := x.session(fmt.Sprintf("docs-q-session-%04d", i))
		conv := &conversation{x: x, s: s}
		var e contract.Envelope
		for _, b := range c.Before {
			conv.say(b)
		}
		e = conv.say(c.Message)
		lower := strings.ToLower(e.Text)
		ok := statusIn(e.Status, c.Expect.Outcome)
		for _, a := range c.Expect.Also {
			if !ok && statusIn(e.Status, a.Outcome) && containsAll(lower, a.Must) {
				ok = true
			}
		}
		var notes []string
		for _, m := range c.Expect.MustNot {
			if len(m) > 3 && assertsPhrase(lower, strings.ToLower(m)) {
				ok = false
				notes = append(notes, "contains forbidden "+m)
			}
		}
		n := byOutcome[c.Expect.Outcome]
		n[1]++
		if ok {
			n[0]++
		}
		byOutcome[c.Expect.Outcome] = n
		line := fmt.Sprintf("%-4s %-16s want %-7s got %-14s %-4s %3dw | %q", map[bool]string{true: "ok", false: "FAIL"}[ok], c.ID,
			c.Expect.Outcome, e.Status, e.IntentID, len(strings.Fields(e.Text)), c.Message)
		if len(notes) > 0 {
			line += " [" + strings.Join(notes, "; ") + "]"
		}
		if !ok {
			fails = append(fails, line+"\n        doc: "+c.Expect.Note+"\n        got: "+truncate(e.Text, 160))
		} else if report {
			t.Log(line)
		}
	}
	keys := make([]string, 0, len(byOutcome))
	for k := range byOutcome {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-7s %d/%d", k, byOutcome[k][0], byOutcome[k][1])
	}
	for _, f := range fails {
		t.Log(f)
	}
	if len(fails) > 0 {
		t.Errorf("%d of %d document questions do not get the governed outcome their document requires", len(fails), len(cases))
	}
}

func statusIn(st contract.Status, outcome string) bool {
	for _, want := range docsOutcome[outcome] {
		if st == want {
			return true
		}
	}
	return false
}

func containsAll(text string, phrases []string) bool {
	for _, p := range phrases {
		if !strings.Contains(text, strings.ToLower(p)) {
			return false
		}
	}
	return true
}

// assertsPhrase reports a forbidden phrase that is not negated: "I haven't
// booked anything" or "cannot confirm an uptime guarantee" assert nothing.
func assertsPhrase(text, phrase string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], phrase)
		if i < 0 {
			return false
		}
		i += from
		start := i - 40
		if start < 0 {
			start = 0
		}
		if !negationRe.MatchString(text[start:i]) {
			return true
		}
		from = i + len(phrase)
	}
}

var negationRe = regexp.MustCompile(`\b(?:not|no|never|cannot|can't|haven't|hasn't|isn't|aren't|won't|don't|do not|without)\b`)

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ = http.MethodPost
