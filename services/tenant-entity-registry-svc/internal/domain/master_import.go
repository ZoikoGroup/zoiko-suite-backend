package domain

// ORG-9 master imports (ZS-SVC-U-001 s19: POST /v1/master-imports).
//
// A file of party master records is uploaded, validated row by row, previewed, approved by someone other than the uploader,
// imported through the ordinary party creation path, and reconciled. The pure rules here are what a row must be to be valid
// and what reconciliation must show; the state is kept by the store.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Import kinds. Only parties are imported; legal entities stay owned by ORG-03.
const ImportKindParty = "PARTY"

// Import statuses.
const (
	ImportUploaded     = "UPLOADED"
	ImportValidating   = "VALIDATING"
	ImportPreviewReady = "PREVIEW_READY"
	ImportBlocked      = "BLOCKED"
	ImportApproved     = "APPROVED"
	ImportImporting    = "IMPORTING"
	ImportCompleted    = "COMPLETED"
	ImportFailed       = "FAILED"
	ImportCancelled    = "CANCELLED"
)

// Row statuses.
const (
	RowPending  = "PENDING"
	RowValid    = "VALID"
	RowInvalid  = "INVALID"
	RowImported = "IMPORTED"
	RowConflict = "CONFLICT"
	RowFailed   = "FAILED"
)

// Bounds on a file.
const (
	MaxImportRows    = 10000
	MaxImportRowSize = 64 * 1024
)

var (
	rowRefShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#@-]{0,127}$`)
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// CreateMasterImportRequest uploads a file of rows.
type CreateMasterImportRequest struct {
	Kind          string `json:"kind"`
	SourceName    string `json:"source_name"`
	SourceVersion string `json:"source_version"`
	// ManifestSHA256 is the hash the sender states for the rows; when present it must match the rows received.
	ManifestSHA256 string      `json:"manifest_sha256,omitempty"`
	SourceClass    SourceClass `json:"source_class,omitempty"` // SYSTEM_IMPORT when absent
	EvidenceRef    string      `json:"evidence_ref"`
	// ValidFrom is when the facts in the file began to hold, unless a row says otherwise.
	ValidFrom     string          `json:"valid_from"`
	Rows          json.RawMessage `json:"rows"`
	CorrelationID string          `json:"correlation_id,omitempty"`
}

// SourceClassOrDefault is the class the file's facts carry.
func (r CreateMasterImportRequest) SourceClassOrDefault() SourceClass {
	if r.SourceClass == "" {
		return SourceSystemImport
	}
	return r.SourceClass
}

// SplitRows returns each row as received, in order.
func (r CreateMasterImportRequest) SplitRows() ([]json.RawMessage, error) {
	var rows []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(r.Rows))
	if err := dec.Decode(&rows); err != nil {
		return nil, fmt.Errorf("rows must be a JSON array of objects")
	}
	return rows, nil
}

// ContentSHA256 is the SHA-256 of the rows as received, compacted so that whitespace does not change it.
func (r CreateMasterImportRequest) ContentSHA256() string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, r.Rows); err != nil {
		return ""
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// RequestHash identifies what an upload asks for, so a retry with the same key and a different request is refused.
func (r CreateMasterImportRequest) RequestHash() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{r.Kind, r.SourceName, r.SourceVersion, r.ContentSHA256(), r.EvidenceRef, r.ValidFrom, string(r.SourceClassOrDefault())}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// Validate checks the request as a whole; each row is checked later, by validation, so the sender gets every error at once.
func (r CreateMasterImportRequest) Validate() error {
	switch {
	case r.Kind != ImportKindParty:
		return fmt.Errorf("kind must be PARTY")
	case strings.TrimSpace(r.SourceName) == "" || len(r.SourceName) > 200:
		return fmt.Errorf("source_name is required (1 to 200 characters)")
	case strings.TrimSpace(r.SourceVersion) == "" || len(r.SourceVersion) > 100:
		return fmt.Errorf("source_version is required (1 to 100 characters)")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required: the file is recorded with the source it came from")
	case !r.SourceClassOrDefault().Valid():
		return fmt.Errorf("source_class is not a known source class")
	case r.ManifestSHA256 != "" && !sha256Hex.MatchString(r.ManifestSHA256):
		return fmt.Errorf("manifest_sha256 must be a lower case SHA-256 hex digest")
	}
	if _, err := ParseDate(r.ValidFrom); err != nil {
		return fmt.Errorf("valid_from %v", err)
	}
	if len(r.Rows) == 0 {
		return fmt.Errorf("rows is required")
	}
	rows, err := r.SplitRows()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("a file holds at least one row")
	}
	if len(rows) > MaxImportRows {
		return fmt.Errorf("a file holds at most %d rows", MaxImportRows)
	}
	for i, raw := range rows {
		if len(raw) > MaxImportRowSize {
			return fmt.Errorf("row %d is larger than %d bytes", i+1, MaxImportRowSize)
		}
	}
	if r.ManifestSHA256 != "" && r.ManifestSHA256 != r.ContentSHA256() {
		return fmt.Errorf("manifest_sha256 does not match the rows received: the file was changed or damaged in transit")
	}
	return nil
}

// ApproveMasterImportRequest approves a previewed import, starting it.
type ApproveMasterImportRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	EvidenceRef     string `json:"evidence_ref"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r ApproveMasterImportRequest) Validate() error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	return nil
}

// ImportPartyRow is one party record in a file. Anything not listed here is refused, so nothing is silently dropped.
type ImportPartyRow struct {
	RowRef        string                 `json:"row_ref"`
	PartyKind     PartyKind              `json:"party_kind"`
	Names         []PartyNameInput       `json:"names"`
	Country       string                 `json:"country,omitempty"`
	Identifiers   []PartyIdentifierInput `json:"identifiers,omitempty"`
	LegalEntityID string                 `json:"legal_entity_id,omitempty"`
	ValidFrom     string                 `json:"valid_from,omitempty"`
}

// ImportDefaults are the file-level values a row inherits.
type ImportDefaults struct {
	ImportID    string
	SourceName  string
	SourceClass SourceClass
	EvidenceRef string
	ValidFrom   string
}

// CreateRequestIDFor is the idempotency key of a row's party creation: the same row of the same import always names the same
// request, so running the import again never creates a second party.
func CreateRequestIDFor(importID string, rowNo int) string {
	return fmt.Sprintf("imp:%s:%d", importID, rowNo)
}

// PartyRequest builds the ordinary party creation request for a row.
func (row ImportPartyRow) PartyRequest(d ImportDefaults, rowNo int) *CreatePartyRequest {
	validFrom := row.ValidFrom
	if validFrom == "" {
		validFrom = d.ValidFrom
	}
	return &CreatePartyRequest{
		CreateRequestID: CreateRequestIDFor(d.ImportID, rowNo),
		PartyKind:       row.PartyKind,
		Names:           row.Names,
		Country:         row.Country,
		Identifiers:     row.Identifiers,
		SourceSystem:    d.SourceName,
		SourceRef:       row.RowRef,
		SourceClass:     d.SourceClass,
		EvidenceRef:     d.EvidenceRef,
		ValidFrom:       validFrom,
		LegalEntityID:   row.LegalEntityID,
	}
}

// ParseImportRow reads a row strictly: an unknown field is an error, not something to ignore.
func ParseImportRow(raw []byte) (ImportPartyRow, error) {
	var row ImportPartyRow
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&row); err != nil {
		return ImportPartyRow{}, err
	}
	if dec.More() {
		return ImportPartyRow{}, fmt.Errorf("a row is one JSON object")
	}
	return row, nil
}

// RowIssue is one reason a row is invalid or failed.
type RowIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Issue codes.
const (
	IssueMalformed           = "MALFORMED_ROW"
	IssueRowRef              = "ROW_REF_INVALID"
	IssueDuplicateRef        = "DUPLICATE_ROW_REF"
	IssueDuplicateIdentifier = "DUPLICATE_IDENTIFIER"
	IssueInvalid             = "ROW_INVALID"
	IssueCreateFailed        = "CREATE_FAILED"
)

// RowValidation is the verdict on one row.
type RowValidation struct {
	RowNo  int        `json:"row_no"`
	RowRef string     `json:"row_ref"`
	Status string     `json:"status"` // VALID or INVALID
	Issues []RowIssue `json:"issues,omitempty"`
}

// ValidateImportRows checks every row of a file and reports every problem of every row at once. A row is invalid when it is
// malformed, carries an unknown field, has no usable row_ref, is not a valid party creation request, or repeats a row_ref or an
// identifier an earlier row of the file already has. The first row to use a reference or identifier keeps it.
func ValidateImportRows(raws [][]byte, d ImportDefaults) []RowValidation {
	out := make([]RowValidation, 0, len(raws))
	refs := map[string]int{}
	idents := map[string]int{}
	for i, raw := range raws {
		no := i + 1
		v := RowValidation{RowNo: no, Status: RowValid}
		bad := func(code, msg string) {
			v.Status = RowInvalid
			v.Issues = append(v.Issues, RowIssue{Code: code, Message: msg})
		}
		row, err := ParseImportRow(raw)
		if err != nil {
			bad(IssueMalformed, err.Error())
			// A rejected row still needs a reference to be found by; the position stands in.
			v.RowRef = fmt.Sprintf("~row-%d", no)
			out = append(out, v)
			continue
		}
		v.RowRef = strings.TrimSpace(row.RowRef)
		switch {
		case !rowRefShape.MatchString(v.RowRef):
			bad(IssueRowRef, "row_ref is the sender's own key for the record: letters, digits and . _ : / # @ - (1 to 128 characters)")
			v.RowRef = fmt.Sprintf("~row-%d", no)
		default:
			if first, dup := refs[v.RowRef]; dup {
				bad(IssueDuplicateRef, fmt.Sprintf("row_ref %s is already used by row %d", v.RowRef, first))
				v.RowRef = fmt.Sprintf("~dup-%d-%s", no, v.RowRef)
			} else {
				refs[v.RowRef] = no
			}
		}
		row.RowRef = strings.TrimSpace(row.RowRef)
		if err := row.PartyRequest(d, no).Validate(); err != nil {
			bad(IssueInvalid, err.Error())
		}
		for _, id := range row.Identifiers {
			key := strings.ToUpper(string(id.Scheme) + "|" + strings.TrimSpace(id.Scope) + "|" + strings.TrimSpace(id.Value))
			if strings.TrimSpace(id.Value) == "" {
				continue
			}
			if first, dup := idents[key]; dup && first != no {
				bad(IssueDuplicateIdentifier, fmt.Sprintf("%s identifier is already carried by row %d: the same party twice in one file", id.Scheme, first))
			} else if !dup {
				idents[key] = no
			}
		}
		out = append(out, v)
	}
	return out
}

// Counts summarises row verdicts.
func CountVerdicts(vs []RowValidation) (valid, invalid int) {
	for _, v := range vs {
		if v.Status == RowValid {
			valid++
		} else {
			invalid++
		}
	}
	return valid, invalid
}

// Reconciliation compares what a file held with what was done with it.
type Reconciliation struct {
	SourceRows int  `json:"source_rows"`
	StagedRows int  `json:"staged_rows"`
	Invalid    int  `json:"invalid"`
	Imported   int  `json:"imported"`
	Conflicts  int  `json:"conflicts"`
	Failed     int  `json:"failed"`
	Unresolved int  `json:"unresolved"`
	Matched    bool `json:"matched"`
}

// Reconcile checks that every row of the file is accounted for exactly once: the rows received equal the rows staged, and each
// is imported, a conflict, failed or invalid, with none left unresolved. Matched is false if not.
func Reconcile(sourceRows, staged, invalid, imported, conflicts, failed, unresolved int) Reconciliation {
	r := Reconciliation{SourceRows: sourceRows, StagedRows: staged, Invalid: invalid, Imported: imported, Conflicts: conflicts, Failed: failed, Unresolved: unresolved}
	r.Matched = sourceRows == staged && unresolved == 0 && imported+conflicts+failed+invalid == staged
	return r
}

// MasterImport is an import.
type MasterImport struct {
	ImportID      string `json:"import_id"`
	TenantID      string `json:"tenant_id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	SourceName    string `json:"source_name"`
	SourceVersion string `json:"source_version"`
	ContentSHA256 string `json:"content_sha256"`
	SourceClass   string `json:"source_class"`
	EvidenceRef   string `json:"evidence_ref"`
	ValidFrom     string `json:"valid_from"`
	RowCount      int    `json:"row_count"`
	ValidRows     int    `json:"valid_rows"`
	InvalidRows   int    `json:"invalid_rows"`
	ImportedRows  int    `json:"imported_rows"`
	ConflictRows  int    `json:"conflict_rows"`
	FailedRows    int    `json:"failed_rows"`
	// ScopeEntityID is the legal-entity scope the file was uploaded under.
	ScopeEntityID    string          `json:"-"`
	UploadedBy       string          `json:"uploaded_by"`
	UploadedAt       time.Time       `json:"uploaded_at"`
	ValidatedAt      *time.Time      `json:"validated_at,omitempty"`
	ApprovedBy       string          `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time      `json:"approved_at,omitempty"`
	ApprovalEvidence string          `json:"approval_evidence,omitempty"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	FinishedAt       *time.Time      `json:"finished_at,omitempty"`
	ValidateOpID     string          `json:"validate_operation_id,omitempty"`
	ImportOpID       string          `json:"import_operation_id,omitempty"`
	Reconciliation   *Reconciliation `json:"reconciliation,omitempty"`
	RecordVersion    int64           `json:"record_version"`
}

// StagedRow is a row as received, with its position.
type StagedRow struct {
	RowNo  int    `json:"row_no"`
	RowRef string `json:"row_ref"`
	Raw    []byte `json:"-"`
}

// ImportRow is a row's verdict and outcome. The row as received is not returned: it can carry identifiers.
type ImportRow struct {
	RowNo     int        `json:"row_no"`
	RowRef    string     `json:"row_ref"`
	Status    string     `json:"status"`
	Errors    []RowIssue `json:"errors,omitempty"`
	Outcome   string     `json:"outcome,omitempty"`
	PartyID   string     `json:"party_id,omitempty"`
	CaseID    string     `json:"case_id,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// RowOutcome is what importing one row did.
type RowOutcome struct {
	Status  string // IMPORTED, CONFLICT or FAILED
	Outcome string // CREATED, MATCHED_EXISTING, REVIEW_REQUIRED, or empty when it failed
	PartyID string
	CaseID  string
	Issues  []RowIssue
}

// OutcomeOf maps the answer of party creation to a row outcome. A created party is imported. A row that matched an existing
// party, or needs review, is a conflict left for stewardship: an import never overwrites and never links on its own say-so.
func OutcomeOf(res *CreatePartyResult) RowOutcome {
	if res == nil {
		return RowOutcome{Status: RowFailed, Issues: []RowIssue{{Code: IssueCreateFailed, Message: "party creation returned nothing"}}}
	}
	o := RowOutcome{Outcome: string(res.Outcome)}
	if res.Party != nil {
		o.PartyID = res.Party.PartyID
	}
	if res.Case != nil {
		o.CaseID = res.Case.CaseID
	}
	if res.Outcome == OutcomeCreated && o.PartyID != "" {
		o.Status = RowImported
		return o
	}
	o.Status = RowConflict
	if res.Match != nil && o.PartyID == "" {
		o.PartyID = res.Match.PartyID
	}
	return o
}

// ImportRowQuery selects rows of an import.
type ImportRowQuery struct {
	Status   string
	AfterRow int
	Limit    int
}

// Validate checks the query.
func (q ImportRowQuery) Validate() error {
	switch q.Status {
	case "", RowPending, RowValid, RowInvalid, RowImported, RowConflict, RowFailed:
	default:
		return fmt.Errorf("status is not a row status")
	}
	if q.AfterRow < 0 || q.Limit < 0 || q.Limit > 200 {
		return fmt.Errorf("limit is at most 200 and after_row is not negative")
	}
	return nil
}
