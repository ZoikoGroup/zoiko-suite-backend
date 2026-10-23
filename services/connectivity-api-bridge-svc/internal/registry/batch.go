package registry

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// BATCH profiles (ZS-SVC-T-001 §10): what a file delivery must satisfy before any row of it is processed. The
// platform defines the manifest format; the contract defines the limits and the shape of the files.

var (
	hostKeyRE = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}=?$`)
	columnRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,62}$`)
)

// Platform ceilings a BATCH profile may not exceed.
const (
	MaxBatchFiles        = 1000
	MaxBatchRows         = 10_000_000
	MaxBatchFileBytes    = 1 << 30 // 1 GiB, whatever the contract asks: the service's own cap is lower
	DefaultExpansionRate = 20
)

type batchSpec struct {
	FileNamePattern      string   `json:"file_name_pattern"`
	Compression          []string `json:"compression,omitempty"`
	MaxFiles             int      `json:"max_files"`
	MaxRows              int      `json:"max_rows"`
	MaxFileBytes         int64    `json:"max_file_bytes,omitempty"`
	MaxDecompressedBytes int64    `json:"max_decompressed_bytes,omitempty"`
	MaxExpansionRatio    int      `json:"max_expansion_ratio,omitempty"`
	ChecksumAlgorithm    string   `json:"checksum_algorithm"`
	RequireRecordCount   bool     `json:"require_record_count,omitempty"`
	Encoding             string   `json:"encoding,omitempty"`
}

func validateBatch(at string, raw json.RawMessage, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + ".spec" + p, r}) }
	var b batchSpec
	if err := strictDecode(raw, &b); err != nil {
		add("", "not a valid BATCH specification")
		return
	}
	if p := b.FileNamePattern; p == "" || len(p) > 256 || !strings.HasPrefix(p, "^") || !strings.HasSuffix(p, "$") {
		add(".file_name_pattern", "an anchored pattern (^...$): a file is named by its contract, never by free choice")
	} else if _, err := regexp.Compile(p); err != nil {
		add(".file_name_pattern", "not a valid pattern")
	}
	if b.MaxFiles < 1 || b.MaxFiles > MaxBatchFiles {
		add(".max_files", fmt.Sprintf("between 1 and %d", MaxBatchFiles))
	}
	if b.MaxRows < 1 || b.MaxRows > MaxBatchRows {
		add(".max_rows", fmt.Sprintf("between 1 and %d", MaxBatchRows))
	}
	if b.MaxFileBytes < 0 || b.MaxFileBytes > MaxBatchFileBytes {
		add(".max_file_bytes", "within the platform ceiling")
	}
	if b.ChecksumAlgorithm != "SHA-256" {
		add(".checksum_algorithm", "SHA-256")
	}
	if b.Encoding != "" && b.Encoding != "UTF-8" {
		add(".encoding", "UTF-8")
	}
	for _, c := range b.Compression {
		if c != "gzip" {
			add(".compression", "none or gzip: no other archive format is opened")
		}
	}
	if len(b.Compression) > 0 {
		if b.MaxDecompressedBytes < 1 || b.MaxDecompressedBytes > MaxBatchFileBytes {
			add(".max_decompressed_bytes", "a compressed delivery declares the most it may expand to")
		}
		if b.MaxExpansionRatio != 0 && (b.MaxExpansionRatio < 1 || b.MaxExpansionRatio > 200) {
			add(".max_expansion_ratio", "between 1 and 200")
		}
	} else if b.MaxDecompressedBytes != 0 || b.MaxExpansionRatio != 0 {
		add(".max_decompressed_bytes", "only meaningful for a compressed delivery")
	}
}

// validateBatchOperation checks how a file operation uses the BATCH profile and the row schema.
func validateBatchOperation(at string, o *Operation, schemas map[string]*Schema, byKind map[string]map[string]*Profile, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + p, r}) }
	file := o.Direction == Inbound && (o.Channel == "SFTP" || o.Channel == "OBJECT")
	if !file {
		if o.BatchProfile != "" {
			add(".batch_profile", "only an inbound SFTP or OBJECT operation carries a batch profile")
		}
		return
	}
	if _, ok := byKind[ProfileBatch][o.BatchProfile]; !ok {
		add(".batch_profile", "a file operation names the BATCH profile its deliveries must satisfy")
	}
	s, ok := schemas[o.RequestSchema]
	if !ok || (s.MediaType != "text/csv" && s.MediaType != "application/x-ndjson") {
		add(".request_schema", "a file operation names its row schema (text/csv or application/x-ndjson)")
	}
	// a row has no headers: its identity, and so its idempotency, can only come from its own content
	if ip, ok := byKind[ProfileIdempotency][o.IdempotencyProfile]; ok {
		var spec idemSpec
		if json.Unmarshal(ip.Spec, &spec) == nil {
			for _, src := range spec.KeySources {
				if !strings.HasPrefix(src, "body:") {
					add(".idempotency_profile", "a file operation recognises a row by its content: key sources are body:/pointer")
				}
			}
		}
	}
}

// BatchInfo is the BATCH profile of a file operation, decoded, with the documented defaults applied.
type BatchInfo struct {
	Name                 string
	FileNamePattern      *regexp.Regexp
	Compression          []string
	MaxFiles             int
	MaxRows              int
	MaxFileBytes         int64
	MaxDecompressedBytes int64
	MaxExpansionRatio    int
	RequireRecordCount   bool
}

// Gzip reports whether the contract admits gzip-compressed files.
func (b *BatchInfo) Gzip() bool {
	for _, c := range b.Compression {
		if c == "gzip" {
			return true
		}
	}
	return false
}

// BatchFor returns the BATCH profile of a file operation, or nil.
func (v *ContractVersion) BatchFor(o *Operation) *BatchInfo {
	if o == nil || o.BatchProfile == "" {
		return nil
	}
	p := profileOf(&v.Content, ProfileBatch, o.BatchProfile)
	if p == nil {
		return nil
	}
	var s batchSpec
	if json.Unmarshal(p.Spec, &s) != nil {
		return nil
	}
	re, err := regexp.Compile(s.FileNamePattern)
	if err != nil {
		return nil
	}
	out := &BatchInfo{Name: p.Name, FileNamePattern: re, Compression: s.Compression, MaxFiles: s.MaxFiles, MaxRows: s.MaxRows, MaxFileBytes: s.MaxFileBytes,
		MaxDecompressedBytes: s.MaxDecompressedBytes, MaxExpansionRatio: dfltInt(s.MaxExpansionRatio, DefaultExpansionRate), RequireRecordCount: s.RequireRecordCount}
	if out.MaxFileBytes == 0 {
		out.MaxFileBytes = o.MaxPayloadBytes
	}
	return out
}

// CSVInfo is the layout of a text/csv row schema.
type CSVInfo struct {
	Columns   []string
	Delimiter rune
	Header    bool
	RowSchema json.RawMessage
}

// CSVLayout decodes a text/csv schema. The delimiter defaults to a comma and a header row is expected unless the
// schema says otherwise.
func (s *Schema) CSVLayout() (*CSVInfo, bool) {
	if s == nil || s.MediaType != "text/csv" {
		return nil, false
	}
	var p struct {
		Columns   []string        `json:"columns"`
		Delimiter string          `json:"delimiter"`
		Header    *bool           `json:"header"`
		RowSchema json.RawMessage `json:"row_schema"`
	}
	if json.Unmarshal(s.ValidationProfile, &p) != nil || len(p.Columns) == 0 {
		return nil, false
	}
	info := &CSVInfo{Columns: p.Columns, Delimiter: ',', Header: p.Header == nil || *p.Header, RowSchema: p.RowSchema}
	if p.Delimiter != "" {
		info.Delimiter = rune(p.Delimiter[0])
	}
	return info, true
}
