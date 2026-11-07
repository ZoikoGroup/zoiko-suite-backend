package specs

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/events"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/store"
)

func load(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// at walks a decoded YAML document by keys.
func at(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("not a map at %q in %v", k, keys)
		}
		v = m[k]
	}
	return v
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	sort.Strings(out)
	return out
}

func keys(v any) []string {
	var out []string
	for k := range v.(map[string]any) {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonFields lists a struct's JSON names; required are those without omitempty.
func jsonFields(typ reflect.Type) (all, required []string) {
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		all = append(all, name)
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	sort.Strings(all)
	sort.Strings(required)
	return
}

func sameSet(t *testing.T, what string, spec, code []string) {
	t.Helper()
	if !reflect.DeepEqual(spec, code) {
		t.Errorf("%s: spec %v, code %v", what, spec, code)
	}
}

func mapKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestOpenAPIMatchesTurnEnvelope(t *testing.T) {
	doc := load(t, "../../api/openapi.yaml")
	schemas := at(t, doc, "components", "schemas")
	tr := at(t, schemas, "TurnResponse")
	all, required := jsonFields(reflect.TypeOf(contract.Envelope{}))
	sameSet(t, "TurnResponse properties", keys(at(t, tr, "properties")), all)
	sameSet(t, "TurnResponse required", strs(at(t, tr, "required")), required)
	sameSet(t, "status_label enum", strs(at(t, tr, "properties", "status_label", "enum")), mapKeys(contract.StatusLabels))
	sameSet(t, "PageContext enum", strs(at(t, schemas, "PageContext", "enum")), mapKeys(contract.PageContexts))

	// Every Reason constant in contract.go is in the reason enum.
	src, err := os.ReadFile("../contract/contract.go")
	if err != nil {
		t.Fatal(err)
	}
	enum := map[string]bool{}
	for _, r := range strs(at(t, tr, "properties", "reason", "enum")) {
		enum[r] = true
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*Reason[A-Za-z]+\s*=\s*"([A-Z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		if !enum[m[1]] {
			t.Errorf("reason %s is emitted but missing from the TurnResponse reason enum", m[1])
		}
	}

	// /turns requires a UUID Idempotency-Key (it must equal client_turn_id).
	turnKey := at(t, doc, "components", "parameters", "TurnIdempotencyKey", "schema", "format")
	if turnKey != "uuid" {
		t.Errorf("TurnIdempotencyKey format = %v, want uuid", turnKey)
	}
}

func TestOpenAPIProblemCodes(t *testing.T) {
	doc := load(t, "../../api/openapi.yaml")
	src, err := os.ReadFile("../problem/problem.go")
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, m := range regexp.MustCompile(`(?m)^\s*[A-Za-z]+\s+Code = "([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		code = append(code, "publicai."+m[1])
	}
	sort.Strings(code)
	sameSet(t, "Problem code enum", strs(at(t, doc, "components", "schemas", "Problem", "properties", "code", "enum")), code)
}

// Every public route answers the shared middleware problems, so each
// operation must declare them.
func TestOpenAPIDeclaresMiddlewareProblems(t *testing.T) {
	doc := load(t, "../../api/openapi.yaml")
	for path, item := range at(t, doc, "paths").(map[string]any) {
		for method, op := range item.(map[string]any) {
			if path == "/turns/{turn_id}/events" || path == "/health/public" {
				continue // fixed 404 / coarse health
			}
			codes := map[string]bool{}
			for _, c := range keys(at(t, op, "responses")) {
				codes[c] = true
			}
			need := []string{"403", "429"}
			if method == "post" {
				need = append(need, "400", "413", "415")
			}
			for _, c := range need {
				if !codes[c] {
					t.Errorf("%s %s does not declare %s", strings.ToUpper(method), path, c)
				}
			}
		}
	}
}

func TestAsyncAPIMatchesEventPayloads(t *testing.T) {
	doc := load(t, "../../api/asyncapi.yaml")
	at0 := time.Unix(0, 0)
	build := map[string]func() (store.OutboxEvent, error){
		"turnCompleted": func() (store.OutboxEvent, error) {
			return events.TurnCompleted("r", "I01", "ANSWERED", time.Second, "kb", at0)
		},
		"handoffClicked": func() (store.OutboxEvent, error) { return events.HandoffClicked("demo", "", false, at0) },
		"securitySignal": func() (store.OutboxEvent, error) {
			return events.SecuritySignal(orchestrator.SignalInjection, "r", "MEDIUM", at0)
		},
		"killSwitchChanged": func() (store.OutboxEvent, error) {
			return events.KillSwitchChanged("chat_enabled", "a", "b", "dev", at0)
		},
		"releasePromoted": func() (store.OutboxEvent, error) {
			return events.ReleasePromotedFor("kb", "kb-1", "d", []string{"s"}, at0)
		},
		"sourceRevoked": func() (store.OutboxEvent, error) { return events.SourceRevoked("s-v1", "WITHDRAWN", at0) },
		// Every optional field set, so each must be in the schema.
		"analytics": func() (store.OutboxEvent, error) {
			return events.AnalyticsEvent(events.Analytics{Name: events.AnswerDelivered, RouteID: "home", Locale: "en-US",
				ReleaseVersion: "r", IntentID: "I01", ConfidenceBucket: "high", SafetyDisposition: "ALLOW", SlotName: "market_code",
				ReasonCode: "ANSWERED", AnswerClass: "VERIFIED_FACT", SourceCount: events.IntPtr(0), StatusClass: "ANSWERED",
				LatencyBucket: "<2s", LinkID: "pricing", ActionID: "demo", HandoffCategory: "demo", RouteAvailable: events.BoolPtr(false),
				Helpfulness: "useful", FeedbackReason: "unclear", SeverityBucket: "medium", ConflictIDs: []string{"c"}, OwnerQueueID: "q"}, at0)
		},
	}
	msgs := at(t, doc, "components", "messages").(map[string]any)
	sameSet(t, "AsyncAPI messages", keys(msgs), keys(map[string]any{
		"turnCompleted": 0, "handoffClicked": 0, "securitySignal": 0, "killSwitchChanged": 0, "releasePromoted": 0, "sourceRevoked": 0,
		"analytics": 0}))
	for name, mk := range build {
		ev, err := mk()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := at(t, msgs, name, "name"); got != ev.EventType {
			t.Errorf("%s: name %v, emitted type %s", name, got, ev.EventType)
		}
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(ev.Payload, &env); err != nil {
			t.Fatal(err)
		}
		data := at(t, at(t, msgs, name, "payload", "allOf").([]any)[1], "properties", "data")
		props := keys(at(t, data, "properties"))
		for k := range env.Data {
			if i := sort.SearchStrings(props, k); i == len(props) || props[i] != k {
				t.Errorf("%s: emitted field %q is not in the schema", name, k)
			}
		}
		req, ok := at(t, data, "required").([]any)
		if !ok {
			t.Errorf("%s: data schema has no required list", name)
			continue
		}
		for _, r := range req {
			if _, ok := env.Data[r.(string)]; !ok {
				t.Errorf("%s: required field %q is not always emitted", name, r)
			}
		}
	}
	enum := map[string]bool{}
	for _, r := range strs(at(t, msgs, "securitySignal", "payload", "allOf").([]any)[1].(map[string]any)["properties"].(map[string]any)["data"].(map[string]any)["properties"].(map[string]any)["reason_class"].(map[string]any)["enum"]) {
		enum[r] = true
	}
	for _, s := range []string{orchestrator.SignalInjection, orchestrator.SignalSensitive, orchestrator.SignalSecurity, orchestrator.SignalToolInstruction} {
		if !enum[s] {
			t.Errorf("security signal %s is emitted but missing from reason_class", s)
		}
	}
}

// The analytics name enum in AsyncAPI is exactly the code's allowlist.
func TestAsyncAPIAnalyticsNames(t *testing.T) {
	doc := load(t, "../../api/asyncapi.yaml")
	data := at(t, at(t, doc, "components", "messages", "analytics", "payload", "allOf").([]any)[1], "properties", "data")
	var code []string
	for n := range events.AnalyticsNames {
		code = append(code, n)
	}
	sort.Strings(code)
	sameSet(t, "analytics names", strs(at(t, data, "properties", "name", "enum")), code)
}
