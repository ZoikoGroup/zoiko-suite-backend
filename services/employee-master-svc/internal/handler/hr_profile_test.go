package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/employee-master-svc/internal/domain"
)

// fullProfileBody is a create payload exercising every HR profile field.
func fullProfileBody() map[string]any {
	return map[string]any{
		"legal_entity_id": "le-us",
		"employee_number": "EMP-7001",
		"first_name":      "Asha",
		"last_name":       "Iyer",
		"email":           "asha.iyer@example.com",
		"worker_type":     "FULL_TIME",
		"hire_date":       "2024-03-01",

		"date_of_birth":       "1992-07-19",
		"gender":              "FEMALE",
		"profile_picture_url": "https://cdn.example.com/a/asha.png",
		"personal_email":      "asha@personal.example.com",
		"work_email":          "a.iyer@corp.example.com",

		"current_address":   "12 Residency Road",
		"permanent_address": "44 Hill View",
		"city":              "Bengaluru",
		"state":             "Karnataka",
		"country":           "India",
		"postal_code":       "560025",

		"company":           "Zoiko Technologies",
		"business_unit":     "Platform",
		"division":          "Engineering",
		"team":              "Identity",
		"designation_id":    "desig-sse",
		"confirmation_date": "2024-09-01",
	}
}

func TestCreateEmployee_FullHRProfile_RoundTrips(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/employees/", fullProfileBody(), "hr-manager")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var created domain.Employee
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	rrGet := doReq(r, http.MethodGet, "/v1/employees/"+created.EmployeeID, nil, "hr-manager")
	if rrGet.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrGet.Code, rrGet.Body.String())
	}

	var got domain.Employee
	if err := json.NewDecoder(rrGet.Body).Decode(&got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}

	for _, c := range []struct {
		field string
		got   *string
		want  string
	}{
		{"date_of_birth", got.DateOfBirth, "1992-07-19"},
		{"gender", got.Gender, "FEMALE"},
		{"profile_picture_url", got.ProfilePictureURL, "https://cdn.example.com/a/asha.png"},
		{"personal_email", got.PersonalEmail, "asha@personal.example.com"},
		{"work_email", got.WorkEmail, "a.iyer@corp.example.com"},
		{"current_address", got.CurrentAddress, "12 Residency Road"},
		{"permanent_address", got.PermanentAddress, "44 Hill View"},
		{"city", got.City, "Bengaluru"},
		{"state", got.State, "Karnataka"},
		{"country", got.Country, "India"},
		{"postal_code", got.PostalCode, "560025"},
		{"company", got.Company, "Zoiko Technologies"},
		{"business_unit", got.BusinessUnit, "Platform"},
		{"division", got.Division, "Engineering"},
		{"team", got.Team, "Identity"},
		{"designation_id", got.DesignationID, "desig-sse"},
		{"confirmation_date", got.ConfirmationDate, "2024-09-01"},
	} {
		if c.got == nil {
			t.Errorf("%s: expected %q got nil", c.field, c.want)
			continue
		}
		if *c.got != c.want {
			t.Errorf("%s: expected %q got %q", c.field, c.want, *c.got)
		}
	}
}

func TestCreateEmployee_InvalidGender_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	body := fullProfileBody()
	body["gender"] = "F"

	rr := doReq(r, http.MethodPost, "/v1/employees/", body, "hr-manager")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEmployee_OmittedGender_Accepted(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	body := fullProfileBody()
	delete(body, "gender")

	rr := doReq(r, http.MethodPost, "/v1/employees/", body, "hr-manager")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEmployee_MalformedDate_Rejected(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"date_of_birth", "19-07-1992"},
		{"date_of_birth", "1992-13-01"},
		{"confirmation_date", "not-a-date"},
	} {
		t.Run(tc.field+"_"+tc.value, func(t *testing.T) {
			r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
			body := fullProfileBody()
			body[tc.field] = tc.value

			rr := doReq(r, http.MethodPost, "/v1/employees/", body, "hr-manager")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestCreateEmployee_DuplicateWorkEmail_Returns409(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	if rr := doReq(r, http.MethodPost, "/v1/employees/", fullProfileBody(), "hr-manager"); rr.Code != http.StatusCreated {
		t.Fatalf("seed: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	// Different person, different primary email and employee number, same work email.
	second := fullProfileBody()
	second["employee_number"] = "EMP-7002"
	second["email"] = "raj.menon@example.com"
	second["first_name"] = "Raj"
	second["last_name"] = "Menon"

	rr := doReq(r, http.MethodPost, "/v1/employees/", second, "hr-manager")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
}

// Two employees with no work email must not collide: the uniqueness index is
// partial for exactly this reason.
func TestCreateEmployee_MultipleNullWorkEmails_Allowed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	first := fullProfileBody()
	delete(first, "work_email")
	if rr := doReq(r, http.MethodPost, "/v1/employees/", first, "hr-manager"); rr.Code != http.StatusCreated {
		t.Fatalf("seed: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	second := fullProfileBody()
	delete(second, "work_email")
	second["employee_number"] = "EMP-7003"
	second["email"] = "dev.rao@example.com"

	rr := doReq(r, http.MethodPost, "/v1/employees/", second, "hr-manager")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateEmployee_PatchesHRProfileFields(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rrCreate := doReq(r, http.MethodPost, "/v1/employees/", fullProfileBody(), "hr-manager")
	var emp domain.Employee
	_ = json.NewDecoder(rrCreate.Body).Decode(&emp)

	rr := doReq(r, http.MethodPut, "/v1/employees/"+emp.EmployeeID, map[string]any{
		"division": "Infrastructure",
		"team":     "Storage",
		"city":     "Chennai",
	}, "hr-manager")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	var got domain.Employee
	_ = json.NewDecoder(rr.Body).Decode(&got)

	if got.Division == nil || *got.Division != "Infrastructure" {
		t.Errorf("division not patched: %v", got.Division)
	}
	if got.Team == nil || *got.Team != "Storage" {
		t.Errorf("team not patched: %v", got.Team)
	}
	if got.City == nil || *got.City != "Chennai" {
		t.Errorf("city not patched: %v", got.City)
	}
	// Untouched fields must survive the patch.
	if got.BusinessUnit == nil || *got.BusinessUnit != "Platform" {
		t.Errorf("business_unit should be unchanged, got %v", got.BusinessUnit)
	}
	if got.Gender == nil || *got.Gender != "FEMALE" {
		t.Errorf("gender should be unchanged, got %v", got.Gender)
	}
}

func TestUpdateEmployee_InvalidGender_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rrCreate := doReq(r, http.MethodPost, "/v1/employees/", fullProfileBody(), "hr-manager")
	var emp domain.Employee
	_ = json.NewDecoder(rrCreate.Body).Decode(&emp)

	rr := doReq(r, http.MethodPut, "/v1/employees/"+emp.EmployeeID, map[string]any{
		"gender": "unknown",
	}, "hr-manager")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

// A directory listing must not carry personal data, however privileged the caller.
func TestListEmployees_OmitsPersonalData(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	if rr := doReq(r, http.MethodPost, "/v1/employees/", fullProfileBody(), "hr-manager"); rr.Code != http.StatusCreated {
		t.Fatalf("seed: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodGet, "/v1/employees/?legal_entity_id=le-us", nil, "hr-manager")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	for _, leaked := range []string{
		"1992-07-19",
		"asha@personal.example.com",
		"12 Residency Road",
		"44 Hill View",
		"560025",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("listing leaked personal data %q: %s", leaked, body)
		}
	}

	// Directory-appropriate fields must still be present.
	for _, want := range []string{"a.iyer@corp.example.com", "Platform", "Engineering"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing missing %q: %s", want, body)
		}
	}
}

func TestListEmployees_FiltersByOrgPlacement(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	if rr := doReq(r, http.MethodPost, "/v1/employees/", fullProfileBody(), "hr-manager"); rr.Code != http.StatusCreated {
		t.Fatalf("seed: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	other := fullProfileBody()
	other["employee_number"] = "EMP-7004"
	other["email"] = "sam.pai@example.com"
	other["work_email"] = "s.pai@corp.example.com"
	other["division"] = "Finance"
	other["business_unit"] = "Corporate"
	if rr := doReq(r, http.MethodPost, "/v1/employees/", other, "hr-manager"); rr.Code != http.StatusCreated {
		t.Fatalf("seed 2: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodGet, "/v1/employees/?legal_entity_id=le-us&division=Finance", nil, "hr-manager")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	var list []domain.Employee
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 employee in Finance, got %d", len(list))
	}
	if list[0].EmployeeNumber != "EMP-7004" {
		t.Errorf("wrong employee returned: %s", list[0].EmployeeNumber)
	}
}
