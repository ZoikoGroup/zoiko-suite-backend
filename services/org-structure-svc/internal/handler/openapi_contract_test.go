package handler_test

// openapi.yaml is held to the live handlers three ways:
//
//  1. it is a valid OpenAPI document (every $ref resolves, every schema is
//     well-formed), which a generated client depends on;
//  2. the router serves exactly the routes the document declares, no more and no
//     fewer;
//  3. real responses, driven through the real router, answer with a status the
//     document declares for that operation and a body that validates against the
//     declared schema, including the error shapes.
//
// A spec that nothing checks is documentation, and documentation drifts.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/go-chi/chi/v5"

	"zoiko.io/org-structure-svc/internal/domain"
)

const specHost = "http://org.test"

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join(filepath.Dir(thisFile), "../../openapi.yaml"))
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

func TestOpenAPI_DocumentIsValid(t *testing.T) { loadSpec(t) }

func TestOpenAPI_DeclaresExactlyTheRoutesTheRouterServes(t *testing.T) {
	doc := loadSpec(t)

	declared := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			declared[method+" "+path] = true
		}
	}

	served := map[string]bool{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var missing, extra []string
	for k := range served {
		if !declared[k] {
			missing = append(missing, k)
		}
	}
	for k := range declared {
		if !served[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("served but not declared in openapi.yaml:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("declared in openapi.yaml but not served:\n  %s", strings.Join(extra, "\n  "))
	}
}

// ── live responses ─────────────────────────────────────────────────────────────

type contract struct {
	t      *testing.T
	router routers.Router
}

func newContract(t *testing.T) *contract {
	t.Helper()
	doc := loadSpec(t)
	doc.Servers = openapi3.Servers{{URL: specHost}}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("router from spec: %v", err)
	}
	return &contract{t: t, router: router}
}

// call drives one request through h, checks it answered `want`, and validates the
// response (status declared, body matches the schema). With validRequest the
// request itself is validated too: parameters, headers and body.
func (c *contract) call(h http.Handler, label, method, path, body string, hdr map[string]string, want int, validRequest bool) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, specHost+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "p1")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != want {
		c.t.Fatalf("%s: %s %s answered %d, the scenario expects %d: %s", label, method, path, rec.Code, want, rec.Body.String())
	}

	route, params, err := c.router.FindRoute(req)
	if err != nil {
		c.t.Fatalf("%s: %s %s is served but not declared: %v", label, method, path, err)
	}
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if validRequest {
		// The body was consumed by the handler; give the validator its own copy.
		req.Body = httptest.NewRequest(method, specHost+path, strings.NewReader(body)).Body
		if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil {
			c.t.Errorf("%s: the request is not valid per openapi.yaml: %v", label, firstLine(err.Error()))
		}
	}
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: rec.Code, Header: rec.Header(),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	out.SetBodyBytes(rec.Body.Bytes())
	if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
		c.t.Errorf("%s: %s %s answered %d, not as declared: %v\n  body: %s", label, method, path, rec.Code, firstLine(err.Error()), rec.Body.String())
	}
	return rec
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func idOf(t *testing.T, rec *httptest.ResponseRecorder, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if cr, ok := m["change_request"].(map[string]any); ok && field == "request_id" {
		m = cr
	}
	s, _ := m[field].(string)
	if s == "" {
		t.Fatalf("no %s in %s", field, rec.Body.String())
	}
	return s
}

func ifMatch(v string) map[string]string { return map[string]string{"If-Match": v} }

// Every org-unit operation, success and failure, answers as openapi.yaml says.
func TestOpenAPI_OrgUnitResponsesMatchTheSchemas(t *testing.T) {
	c := newContract(t)
	s := newStubStore()
	r := routerFor(s, &recordingPublisher{}, &stubAuthZ{})
	none := map[string]string(nil)

	// create, replay
	created := c.call(r, "create", "POST", "/v1/org/units", toJSON(mappedUnitBody("ROOT", "k-1", nil, "")), none, 201, true)
	root := idOf(t, created, "unit_id")
	c.call(r, "create replay", "POST", "/v1/org/units", toJSON(mappedUnitBody("ROOT", "k-1", nil, "")), none, 200, true)
	alt := idOf(t, c.call(r, "create alt", "POST", "/v1/org/units", toJSON(unitBody("ALT", "k-2", nil)), none, 201, true), "unit_id")
	fin := idOf(t, c.call(r, "create mapped", "POST", "/v1/org/units", toJSON(mappedUnitBody("FIN", "k-3", &root, "CC100")), none, 201, true), "unit_id")
	mgr := "emp-1"
	body := unitBody("OPS", "k-4", &root)
	body["manager_employee_id"] = mgr
	ops := idOf(t, c.call(r, "create with manager", "POST", "/v1/org/units", toJSON(body), none, 201, true), "unit_id")

	// reads
	c.call(r, "get", "GET", "/v1/org/units/"+root, "", none, 200, true)
	c.call(r, "get as of", "GET", "/v1/org/units/"+root+"?as_of=2026-02-01", "", none, 200, true)
	c.call(r, "list by entity", "GET", "/v1/org/units?legal_entity_id=le-us", "", none, 200, true)
	c.call(r, "list by manager", "GET", "/v1/org/units?manager_employee_id="+mgr, "", none, 200, true)
	c.call(r, "hierarchy", "GET", "/v1/org/hierarchy?legal_entity_id=le-us", "", none, 200, true)
	c.call(r, "hierarchy as of", "GET", "/v1/org/hierarchy?legal_entity_id=le-us&as_of=2026-02-01&recorded_as_of=2026-02-02T10:00:00Z&include_inactive=false", "", none, 200, true)
	c.call(r, "impact", "GET", "/v1/org/units/"+root+"/reparent-impact?effective_at=2026-03-01", "", none, 200, true)

	// lifecycle
	c.call(r, "activate", "POST", "/v1/org/units/"+alt+"/activate", "", ifMatch("1"), 200, true)

	// change manager, an unmapped reparent applied directly, a mapped one proposed
	c.call(r, "change manager", "POST", "/v1/org/units/"+ops+"/change-manager",
		toJSON(mgrMove("emp-2", "2026-03-01")), ifMatch("1"), 200, true)
	leaf := idOf(t, c.call(r, "create leaf", "POST", "/v1/org/units", toJSON(unitBody("LEAF", "k-5", &alt)), none, 201, true), "unit_id")
	c.call(r, "reparent applied", "POST", "/v1/org/units/"+leaf+"/reparent",
		toJSON(move(&root, "2026-03-01")), ifMatch("1"), 200, true)
	proposed := c.call(r, "reparent proposed", "POST", "/v1/org/units/"+fin+"/reparent",
		toJSON(move(&alt, "2026-03-01")), ifMatch("1"), 202, true)
	crID := idOf(t, proposed, "request_id")
	c.call(r, "reparent replay", "POST", "/v1/org/units/"+fin+"/reparent",
		toJSON(move(&alt, "2026-03-01")), ifMatch("1"), 200, true)

	// change requests: read, approve, reject
	c.call(r, "list requests", "GET", "/v1/org/change-requests?legal_entity_id=le-us&status=PENDING", "", none, 200, true)
	c.call(r, "get request", "GET", "/v1/org/change-requests/"+crID, "", none, 200, true)
	c.call(r, "approve", "POST", "/v1/org/change-requests/"+crID+"/approve", "", map[string]string{"X-Principal-Id": "checker"}, 200, true)

	fin2 := idOf(t, c.call(r, "create mapped 2", "POST", "/v1/org/units", toJSON(mappedUnitBody("FIN2", "k-6", &root, "CC200")), none, 201, true), "unit_id")
	cr2 := idOf(t, c.call(r, "propose 2", "POST", "/v1/org/units/"+fin2+"/reparent", toJSON(move(&alt, "2026-03-01")), ifMatch("1"), 202, true), "request_id")
	c.call(r, "reject", "POST", "/v1/org/change-requests/"+cr2+"/reject", toJSON(map[string]any{"reason": "wrong centre"}), map[string]string{"X-Principal-Id": "checker"}, 200, true)

	// retiring a unit that still has active children is refused (409): FIN now sits under ALT
	c.call(r, "deactivate with children", "POST", "/v1/org/units/"+alt+"/deactivate", "", ifMatch("2"), 409, true)
}

// Errors have a declared shape and status too.
func TestOpenAPI_OrgUnitErrorsMatchTheSchemas(t *testing.T) {
	c := newContract(t)
	s := newStubStore()
	r := routerFor(s, &recordingPublisher{}, &stubAuthZ{})
	none := map[string]string(nil)

	root := idOf(t, c.call(r, "create", "POST", "/v1/org/units", toJSON(mappedUnitBody("ROOT", "k-1", nil, "")), none, 201, true), "unit_id")
	fin := idOf(t, c.call(r, "create mapped", "POST", "/v1/org/units", toJSON(mappedUnitBody("FIN", "k-2", &root, "CC1")), none, 201, true), "unit_id")
	cr := idOf(t, c.call(r, "propose", "POST", "/v1/org/units/"+fin+"/reparent", toJSON(move(nil, "2026-03-01")), ifMatch("1"), 202, true), "request_id")

	// 400
	c.call(r, "invalid json", "POST", "/v1/org/units", "{not json", none, 400, false)
	c.call(r, "bad unit type", "POST", "/v1/org/units", toJSON(map[string]any{"legal_entity_id": "le-us", "code": "X", "name": "X", "unit_type": "COST_CENTER", "effective_from": "2026-01-01", "correlation_id": "z"}), none, 400, false)
	c.call(r, "bad as_of", "GET", "/v1/org/units/"+root+"?as_of=tomorrow", "", none, 400, false)
	c.call(r, "bad if-match", "POST", "/v1/org/units/"+root+"/activate", "", ifMatch("abc"), 400, true)
	c.call(r, "hierarchy without entity", "GET", "/v1/org/hierarchy", "", none, 400, false)
	c.call(r, "reject without reason", "POST", "/v1/org/change-requests/"+cr+"/reject", "{}", none, 400, false)
	// 401
	c.call(r, "no principal", "GET", "/v1/org/units/"+root, "", map[string]string{"X-Principal-Id": ""}, 401, false)
	// 404
	c.call(r, "unknown unit", "GET", "/v1/org/units/nope", "", none, 404, true)
	c.call(r, "unknown request", "GET", "/v1/org/change-requests/nope", "", none, 404, true)
	// 409
	c.call(r, "duplicate code", "POST", "/v1/org/units", toJSON(mappedUnitBody("ROOT", "k-9", nil, "")), none, 409, true)
	c.call(r, "stale version", "POST", "/v1/org/units/"+root+"/activate", "", ifMatch("9"), 409, true)
	c.call(r, "different change pending", "POST", "/v1/org/units/"+fin+"/reparent", toJSON(move(nil, "2026-04-01")), ifMatch("1"), 409, true)
	c.call(r, "self approval", "POST", "/v1/org/change-requests/"+cr+"/approve", "", map[string]string{"X-Principal-Id": "p1"}, 403, true)
	// 422
	c.call(r, "loop", "POST", "/v1/org/units/"+root+"/reparent", toJSON(move(&fin, "2026-03-01")), ifMatch("1"), 422, true)
	c.call(r, "missing parent", "POST", "/v1/org/units", toJSON(unitBody("KID", "k-10", strPtr("ghost"))), none, 422, true)
	// 428
	c.call(r, "no if-match", "POST", "/v1/org/units/"+root+"/activate", "", none, 428, false)

	// 403 (authorisation), 503 (a dependency)
	denied := routerFor(s, &recordingPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	c.call(denied, "forbidden", "GET", "/v1/org/units/"+root, "", none, 403, true)
	unavailable := routerFor(s, &recordingPublisher{}, &stubAuthZ{err: errors.New("authz down")})
	downEmp := routerWith(s, &stubAuthZ{}, &countingValidator{err: errors.New("connection refused")})
	c.call(downEmp, "employee service down", "POST", "/v1/org/units/"+root+"/change-manager", toJSON(mgrMove("emp-9", "2026-03-01")), ifMatch("1"), 503, true)
	c.call(unavailable, "dependency down", "GET", "/v1/org/units/"+root, "", none, 503, true)

	// 500: stored data that contains a cycle is reported, not hidden.
	a, b := "a", "b"
	cyc := newStubStore()
	cyc.units["a"] = &domain.OrgUnit{UnitID: "a", LegalEntityID: "le-us", Code: "A", ParentUnitID: &b, Status: domain.UnitStatusActive}
	cyc.units["b"] = &domain.OrgUnit{UnitID: "b", LegalEntityID: "le-us", Code: "B", ParentUnitID: &a, Status: domain.UnitStatusActive}
	c.call(routerFor(cyc, &recordingPublisher{}, &stubAuthZ{}), "cycle", "GET", "/v1/org/hierarchy?legal_entity_id=le-us", "", none, 500, true)
}

// The older routes answer as declared too.
func TestOpenAPI_LegacyResponsesMatchTheSchemas(t *testing.T) {
	c := newContract(t)
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	none := map[string]string(nil)

	dept := map[string]any{"legal_entity_id": "le-us", "name": "Finance", "code": "FIN", "cost_center_code": "CC1", "correlation_id": "d-1"}
	deptID := idOf(t, c.call(r, "create department", "POST", "/v1/org/departments", toJSON(dept), none, 201, true), "department_id")
	c.call(r, "department replay", "POST", "/v1/org/departments", toJSON(dept), none, 200, true)
	c.call(r, "list departments", "GET", "/v1/org/departments?legal_entity_id=le-us", "", none, 200, true)
	c.call(r, "get department", "GET", "/v1/org/departments/"+deptID, "", none, 200, true)

	pos := map[string]any{"legal_entity_id": "le-us", "department_id": deptID, "title": "Analyst", "code": "AN", "job_level": "L2", "max_headcount": 3, "correlation_id": "p-1"}
	posID := idOf(t, c.call(r, "create position", "POST", "/v1/org/positions", toJSON(pos), none, 201, true), "position_id")
	c.call(r, "position replay", "POST", "/v1/org/positions", toJSON(pos), none, 200, true)
	c.call(r, "list positions", "GET", "/v1/org/positions?department_id="+deptID, "", none, 200, true)
	c.call(r, "get position", "GET", "/v1/org/positions/"+posID, "", none, 200, true)

	assign := map[string]any{"employee_id": "emp-1", "department_id": deptID, "position_id": posID, "effective_from": "2026-01-01", "correlation_id": "a-1"}
	c.call(r, "assign", "POST", "/v1/org/assignments", toJSON(assign), none, 201, true)
	c.call(r, "assign replay", "POST", "/v1/org/assignments", toJSON(assign), none, 201, true)
	c.call(r, "get assignment", "GET", "/v1/org/assignments/employee/emp-1", "", none, 200, true)
}
