package batch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
)

// The manifest is what the sender says it sent (§10.1). Its format is the platform's, not the contract's: a
// connector cannot make "batch accepted" mean something weaker by describing a weaker manifest. The contract's BATCH
// profile sets the limits and the file naming; the manifest names the batch, the schema, the counts and, for every
// file, its size and SHA-256.

// Manifest is the sender's description of a batch.
type Manifest struct {
	BatchID           string         `json:"batch_id"`
	SourceSystem      string         `json:"source_system"`
	Schema            string         `json:"schema"`
	FileCount         int            `json:"file_count"`
	RecordCount       *int           `json:"record_count,omitempty"`
	Period            *ManifestRange `json:"period,omitempty"`
	ChecksumAlgorithm string         `json:"checksum_algorithm"`
	Files             []ManifestFile `json:"files"`
}

// ManifestRange is the source period the batch covers.
type ManifestRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ManifestFile describes one delivered file.
type ManifestFile struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Compression string `json:"compression,omitempty"`
	Records     *int   `json:"records,omitempty"`
}

var (
	batchIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#@=+-]{0,127}$`)
	sha256RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sourceRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,63}$`)
)

const maxManifestBytes = 1 << 20

// ParseManifest decodes a manifest strictly: an unknown field, a trailing document or an oversized manifest is not a
// manifest. It returns a reason for a manifest that does not meet the platform's format.
func ParseManifest(raw []byte) (*Manifest, string) {
	if len(raw) == 0 || len(raw) > maxManifestBytes {
		return nil, ReasonManifestInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil || dec.More() {
		return nil, ReasonManifestInvalid
	}
	switch {
	case !batchIDRE.MatchString(m.BatchID), !sourceRE.MatchString(m.SourceSystem), strings.TrimSpace(m.Schema) == "":
		return nil, ReasonManifestInvalid
	case m.ChecksumAlgorithm != "SHA-256":
		return nil, ReasonManifestInvalid
	case m.FileCount < 1 || m.FileCount != len(m.Files):
		return nil, ReasonManifestInvalid
	case m.RecordCount != nil && *m.RecordCount < 0:
		return nil, ReasonManifestInvalid
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if f.Name == "" || len(f.Name) > 255 || strings.ContainsAny(f.Name, "/\\\x00") || f.Name == "." || f.Name == ".." || seen[f.Name] ||
			f.Size < 0 || !sha256RE.MatchString(f.SHA256) || (f.Compression != "" && f.Compression != "none" && f.Compression != "gzip") ||
			(f.Records != nil && *f.Records < 0) {
			return nil, ReasonManifestInvalid
		}
		seen[f.Name] = true
	}
	return &m, ""
}

// ContentHash identifies what a batch CONTAINS: its identity, its schema and every file name with its digest. Two
// deliveries with the same batch id and the same content hash are the same batch (NP-37); the same id with another
// content hash is a conflict (NP-38).
func ContentHash(m *Manifest) string {
	parts := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		parts = append(parts, f.Name+"="+f.SHA256)
	}
	sort.Strings(parts)
	return sha("content", m.BatchID, m.Schema, strings.Join(parts, "\n"))
}

func sha(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DeliveredFile is one file as it arrived.
type DeliveredFile struct {
	Name string
	Data []byte
}

// Verdict is the outcome of checking a delivery against its manifest and its contract before any row is read.
type Verdict struct {
	OK     bool
	Reason string
	Detail string
	// Files are the verified files in manifest order (set even when verification stopped on the first failure, with
	// what was checked so far, so a refused batch still has file-level evidence).
	Files []File
}

// Verify checks a delivery against its manifest and the contract's BATCH profile: every declared file arrived exactly
// once under an allowed name and size, with the declared length and SHA-256, and nothing undeclared came with it
// (I-21, NP-33). It touches no row.
func Verify(m *Manifest, delivered []DeliveredFile, bi *registry.BatchInfo, serviceMaxFileBytes int64) Verdict {
	v := Verdict{}
	byName := map[string][]byte{}
	for _, f := range delivered {
		if _, dup := byName[f.Name]; dup {
			v.Reason, v.Detail = ReasonFileUnexpected, "a file was delivered twice"
			return v
		}
		byName[f.Name] = f.Data
	}
	if len(m.Files) > bi.MaxFiles {
		v.Reason, v.Detail = ReasonTooManyFiles, "the manifest lists more files than the contract allows"
		return v
	}
	declared := map[string]bool{}
	for i, mf := range m.Files {
		declared[mf.Name] = true
		f := File{Name: mf.Name, DeclaredSHA256: mf.SHA256, DeclaredSize: mf.Size, Compression: dflt(mf.Compression, "none"), DeclaredRecords: mf.Records, State: FileReceived, Ordinal: i}
		v.Files = append(v.Files, f)
	}
	for name := range byName {
		if !declared[name] {
			v.Reason, v.Detail = ReasonFileUnexpected, "a delivered file is not in the manifest"
			return v
		}
	}
	for i, mf := range m.Files {
		f := &v.Files[i]
		if !bi.FileNamePattern.MatchString(mf.Name) {
			f.State, f.ReasonCode = FileRejected, ReasonFileName
			v.Reason, v.Detail = ReasonFileName, "a file name is not allowed by the contract"
			return v
		}
		if mf.Compression == "gzip" && !bi.Gzip() {
			f.State, f.ReasonCode = FileRejected, ReasonCompression
			v.Reason, v.Detail = ReasonCompression, "the contract does not admit compressed files"
			return v
		}
		data, ok := byName[mf.Name]
		if !ok {
			f.State, f.ReasonCode = FileRejected, ReasonFileMissing
			v.Reason, v.Detail = ReasonFileMissing, "a file listed in the manifest was not delivered"
			return v
		}
		f.ActualSize, f.ActualSHA256 = int64(len(data)), hashBytes(data)
		if f.ActualSize > bi.MaxFileBytes || f.ActualSize > serviceMaxFileBytes || mf.Size > bi.MaxFileBytes || mf.Size > serviceMaxFileBytes {
			f.State, f.ReasonCode = FileRejected, ReasonFileTooLarge
			v.Reason, v.Detail = ReasonFileTooLarge, "a file is larger than the contract allows"
			return v
		}
		if f.ActualSize != mf.Size || f.ActualSHA256 != mf.SHA256 {
			f.State, f.ReasonCode = FileRejected, ReasonIntegrity
			v.Reason, v.Detail = ReasonIntegrity, "a file does not match the size and checksum the manifest declares"
			return v
		}
		f.State = FileVerified
	}
	if bi.RequireRecordCount && m.RecordCount == nil {
		v.Reason, v.Detail = ReasonRecordCount, "the contract requires the manifest to declare its record count"
		return v
	}
	if m.RecordCount != nil && *m.RecordCount > bi.MaxRows {
		v.Reason, v.Detail = ReasonTooManyRows, "the manifest declares more rows than the contract allows"
		return v
	}
	v.OK = true
	return v
}

func dflt(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

var _ = fmt.Sprintf
