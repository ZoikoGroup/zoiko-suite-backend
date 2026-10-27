package imports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/eventing/scan"
)

// ErrReconciliationFailed means what was sent and what the owning domain accepted do not agree.
var ErrReconciliationFailed = errors.New("the import does not reconcile")

// ContentScanner is the malware and content gate. *scan.Chain satisfies it.
type ContentScanner interface {
	Name() string
	ScanOpen(ctx context.Context, open func() (io.ReadCloser, error), declaredType string) (scan.Verdict, error)
}

// Config tunes the engine.
type Config struct {
	MaxBatchBytes   int64         // the most content one batch may hold
	MaxChunkBytes   int           // the most one chunk may carry
	SampleSize      int           // rows shown per sample in a preview
	ItemInsertBatch int           // items written per transaction while parsing
	ItemMaxAttempts int           // delivery attempts per item before it is FAILED
	ItemRetryBase   time.Duration // first retry delay (doubles each attempt)
	ItemRetryCap    time.Duration
	ItemLease       time.Duration // how long a claimed item is held
	ExecWorkers     int           // concurrent deliveries to owning domains
	ClaimBatch      int           // items claimed per round
}

// DefaultConfig is a sound starting point.
func DefaultConfig() Config {
	return Config{
		MaxBatchBytes: 64 << 20, MaxChunkBytes: 4 << 20, SampleSize: 20, ItemInsertBatch: 500,
		ItemMaxAttempts: 5, ItemRetryBase: 5 * time.Second, ItemRetryCap: 5 * time.Minute, ItemLease: 2 * time.Minute,
		ExecWorkers: 8, ClaimBatch: 50,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxBatchBytes <= 0 {
		c.MaxBatchBytes = d.MaxBatchBytes
	}
	if c.MaxChunkBytes <= 0 {
		c.MaxChunkBytes = d.MaxChunkBytes
	}
	if c.MaxChunkBytes > 8<<20 {
		c.MaxChunkBytes = 8 << 20 // the column's own bound
	}
	if c.SampleSize <= 0 {
		c.SampleSize = d.SampleSize
	}
	if c.ItemInsertBatch <= 0 {
		c.ItemInsertBatch = d.ItemInsertBatch
	}
	if c.ItemMaxAttempts <= 0 {
		c.ItemMaxAttempts = d.ItemMaxAttempts
	}
	if c.ItemRetryBase <= 0 {
		c.ItemRetryBase = d.ItemRetryBase
	}
	if c.ItemRetryCap <= 0 {
		c.ItemRetryCap = d.ItemRetryCap
	}
	if c.ItemLease <= 0 {
		c.ItemLease = d.ItemLease
	}
	if c.ExecWorkers <= 0 {
		c.ExecWorkers = d.ExecWorkers
	}
	if c.ClaimBatch <= 0 {
		c.ClaimBatch = d.ClaimBatch
	}
	return c
}

// Service runs the INT-06 commands. It contains no authorization (the handler authorizes each
// command against the batch's legal entity before calling it); what it enforces is everything
// that must hold however it is called: the lifecycle, separation of duties, the hash checks, the
// fingerprint match between preview, approval and execution, and the refusal to run without a
// scanner.
type Service struct {
	repo    Repo
	scanner ContentScanner
	targets TargetClient
	cfg     Config
	now     func() time.Time
	newID   func() string
}

// Deps wires a Service.
type Deps struct {
	Repo    Repo
	Scanner ContentScanner // nil means imports are refused: there is no silent "clean" default
	Targets TargetClient
	Config  Config
	Now     func() time.Time
	NewID   func() string
}

// NewService builds a Service.
func NewService(d Deps) (*Service, error) {
	if d.Repo == nil || d.Targets == nil {
		return nil, errors.New("imports: a repository and a target client are required")
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.NewID == nil {
		d.NewID = uuid.NewString
	}
	return &Service{repo: d.Repo, scanner: d.Scanner, targets: d.Targets, cfg: d.Config.withDefaults(), now: d.Now, newID: d.NewID}, nil
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ── profiles ────────────────────────────────────────────────────────────────

// PublishProfile creates the next version of a profile in DRAFT. Profiles are platform
// reference data, published from the platform scope only.
func (s *Service) PublishProfile(ctx context.Context, a Actor, key string, spec Spec) (*Profile, error) {
	if a.TenantID != PlatformScopeID {
		return nil, ErrNotPlatformScope
	}
	if err := ValidateProfileKey(key); err != nil {
		return nil, err
	}
	spec = spec.Normalize()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	existing, err := s.repo.ListProfiles(ctx, key)
	if err != nil {
		return nil, err
	}
	next := 1
	for _, p := range existing {
		if p.Version >= next {
			next = p.Version + 1
		}
	}
	return s.repo.PutProfile(ctx, &Profile{Key: key, Version: next, Status: ProfileDraft, Spec: spec, SpecHash: spec.Hash(), CreatedBy: a.PrincipalID})
}

// CertifyProfile makes a draft usable. The certifier must be someone other than the author, and
// the deployment must actually have the target the profile names.
func (s *Service) CertifyProfile(ctx context.Context, a Actor, key string, version int) (*Profile, error) {
	if a.TenantID != PlatformScopeID {
		return nil, ErrNotPlatformScope
	}
	p, err := s.repo.GetProfile(ctx, key, version)
	if err != nil {
		return nil, err
	}
	if p.Status != ProfileDraft {
		return nil, fmt.Errorf("%w: only a draft can be certified", ErrInvalidState)
	}
	if strings.EqualFold(p.CreatedBy, a.PrincipalID) {
		return nil, ErrProfileSelfCertify
	}
	if !s.targets.Has(p.Spec.Target.Key) {
		return nil, ErrTargetNotConfigured
	}
	return s.repo.CertifyProfile(ctx, key, version, a.PrincipalID, s.now())
}

// RetireProfile deprecates or withdraws a profile. Batches already created keep their pinned version.
func (s *Service) RetireProfile(ctx context.Context, a Actor, key string, version int, to ProfileStatus) (*Profile, error) {
	if a.TenantID != PlatformScopeID {
		return nil, ErrNotPlatformScope
	}
	if to != ProfileDeprecated && to != ProfileWithdrawn {
		return nil, fmt.Errorf("%w: a profile can only be deprecated or withdrawn", ErrValidation)
	}
	return s.repo.SetProfileStatus(ctx, key, version, to)
}

// GetProfile reads a profile (version 0 is the latest certified).
func (s *Service) GetProfile(ctx context.Context, key string, version int) (*Profile, error) {
	return s.repo.GetProfile(ctx, key, version)
}

// ListProfiles lists every version of a profile.
func (s *Service) ListProfiles(ctx context.Context, key string) ([]Profile, error) {
	return s.repo.ListProfiles(ctx, key)
}

// ── batches ─────────────────────────────────────────────────────────────────

// CreateRequest declares a batch.
type CreateRequest struct {
	LegalEntityID   string
	ProfileKey      string
	ProfileVersion  int // 0 means the latest certified
	SourceSystem    string
	Jurisdiction    string
	FileName        string
	ContentType     string
	SHA256          string
	DeclaredRows    *int
	DeclaredControl string
}

// CreateBatch declares an import. The content hash is required up front, so a duplicate file is
// recognised before any content moves and the uploaded bytes can be checked against what the
// sender said they were sending.
func (s *Service) CreateBatch(ctx context.Context, a Actor, req CreateRequest, claim domain.IdempotencyClaim) (*Batch, error) {
	bad := func(m string) error { return fmt.Errorf("%w: %s", ErrValidation, m) }
	if s.scanner == nil {
		return nil, ErrNoScanner
	}
	req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
	switch {
	case strings.TrimSpace(req.LegalEntityID) == "":
		return nil, bad("legal_entity_id is required")
	case strings.TrimSpace(req.SourceSystem) == "":
		return nil, bad("source_system is required")
	case !sha256Re.MatchString(req.SHA256):
		return nil, bad("sha256 of the whole file is required (64 lowercase hex characters)")
	case strings.TrimSpace(req.ContentType) == "":
		return nil, bad("content_type is required")
	case req.DeclaredRows != nil && *req.DeclaredRows < 0:
		return nil, bad("declared_rows cannot be negative")
	case len(req.FileName) > 255 || len(req.SourceSystem) > 120:
		return nil, bad("file_name or source_system is too long")
	}
	p, err := s.repo.GetProfile(ctx, req.ProfileKey, req.ProfileVersion)
	if err != nil {
		return nil, err
	}
	if p.Status != ProfileCertified {
		return nil, ErrProfileNotCertified
	}
	if req.DeclaredControl != "" {
		if _, err := ParseDecimal(req.DeclaredControl); err != nil {
			return nil, bad("declared_control_total is not a plain decimal")
		}
	}
	// Control totals are demanded when the batch is declared, not discovered missing after the
	// upload: a material import without one never starts.
	if p.Spec.Control != nil && p.Spec.Control.Required && req.DeclaredControl == "" {
		return nil, fmt.Errorf("%w: this profile requires a declared control total", ErrValidation)
	}
	if p.Spec.Control != nil && p.Spec.Control.Required && req.DeclaredRows == nil {
		return nil, fmt.Errorf("%w: this profile requires a declared row count", ErrValidation)
	}
	b := &Batch{
		BatchID: "imb_" + s.newID(), TenantID: a.TenantID, LegalEntityID: strings.TrimSpace(req.LegalEntityID), ProfileKey: p.Key,
		ProfileVersion: p.Version, Material: p.Spec.Material, SourceSystem: strings.TrimSpace(req.SourceSystem), FileName: req.FileName, JurisdictionContext: strings.TrimSpace(req.Jurisdiction),
		DeclaredContentType: strings.TrimSpace(req.ContentType), DeclaredSHA256: req.SHA256, DeclaredRows: req.DeclaredRows,
		DeclaredControl: req.DeclaredControl, Status: StatusCreated, Version: 1, CreatedBy: a.PrincipalID, CorrelationID: a.CorrelationID,
	}
	claim.ResourceID = b.BatchID // a replay of this command answers with the batch it created
	return s.repo.CreateBatch(ctx, b, claim)
}

// Get reads a batch.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Batch, error) {
	return s.repo.GetBatch(ctx, tenantID, id)
}

// List lists batches.
func (s *Service) List(ctx context.Context, tenantID string, status Status, limit int) ([]Batch, error) {
	return s.repo.ListBatches(ctx, tenantID, status, limit)
}

// Log reads a batch's history.
func (s *Service) Log(ctx context.Context, tenantID, id string, limit int) ([]LogEntry, error) {
	if _, err := s.repo.GetBatch(ctx, tenantID, id); err != nil {
		return nil, err
	}
	return s.repo.ListLog(ctx, tenantID, id, limit)
}

func (s *Service) transition(ctx context.Context, a Actor, b *Batch, from []Status, to Status, kind, reason string, patch Patch, events ...Event) (*Batch, error) {
	return s.repo.Transition(ctx, Transition{
		TenantID: b.TenantID, BatchID: b.BatchID, From: from, To: to, Reason: reason, Patch: patch,
		Log:    LogEntry{Kind: kind, Actor: a.PrincipalID, Reason: reason, CorrelationID: a.CorrelationID},
		Events: events,
	})
}

func batchEvent(typ string, b *Batch, extra map[string]any) Event {
	p := map[string]any{
		"batch_id": b.BatchID, "legal_entity_id": b.LegalEntityID, "profile_key": b.ProfileKey, "profile_version": b.ProfileVersion,
		"status": string(b.Status), "content_sha256": b.ContentSHA256,
	}
	for k, v := range extra {
		p[k] = v
	}
	return Event{Type: typ, Payload: p}
}

// ── upload ──────────────────────────────────────────────────────────────────

// UploadChunk stores one chunk of the file. It is idempotent per index: the same bytes again are a
// no-op, different bytes at a used index are refused, so an interrupted upload resumes by sending
// what is missing.
func (s *Service) UploadChunk(ctx context.Context, a Actor, batchID string, index int, data []byte, declaredSHA string) (stored bool, err error) {
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return false, err
	}
	if b.Status != StatusCreated && b.Status != StatusUploading {
		return false, fmt.Errorf("%w: content cannot be added to a batch that is %s", ErrInvalidState, b.Status)
	}
	if index < 0 || index >= 16384 {
		return false, fmt.Errorf("%w: chunk index out of range", ErrValidation)
	}
	if len(data) == 0 || len(data) > s.cfg.MaxChunkBytes {
		return false, fmt.Errorf("%w: a chunk must be between 1 and %d bytes", ErrValidation, s.cfg.MaxChunkBytes)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if strings.ToLower(strings.TrimSpace(declaredSHA)) != got {
		return false, fmt.Errorf("%w: the chunk does not match its declared hash", ErrHashMismatch)
	}
	chunks, err := s.repo.Chunks(ctx, a.TenantID, batchID)
	if err != nil {
		return false, err
	}
	var total int64
	replacing := false
	for _, c := range chunks {
		total += int64(c.Size)
		if c.Index == index {
			replacing = true
		}
	}
	if !replacing && total+int64(len(data)) > s.cfg.MaxBatchBytes {
		return false, ErrTooLarge
	}
	stored, err = s.repo.PutChunk(ctx, a.TenantID, batchID, index, data, got)
	if err != nil {
		return false, err
	}
	if b.Status == StatusCreated {
		if _, terr := s.transition(ctx, a, b, []Status{StatusCreated}, StatusUploading, "UPLOAD_STARTED", "", Patch{}); terr != nil && !errors.Is(terr, ErrInvalidState) {
			return stored, terr
		}
	}
	return stored, nil
}

// UploadState reports what has been received, so an interrupted upload can resume.
func (s *Service) UploadState(ctx context.Context, tenantID, batchID string) ([]ChunkInfo, error) {
	if _, err := s.repo.GetBatch(ctx, tenantID, batchID); err != nil {
		return nil, err
	}
	return s.repo.Chunks(ctx, tenantID, batchID)
}

// chainHash folds the chunk hashes in order: change, drop, add or reorder a chunk and it changes.
func chainHash(chunks []ChunkInfo) string {
	prev := ""
	for _, c := range chunks {
		sum := sha256.Sum256([]byte(prev + c.SHA256))
		prev = hex.EncodeToString(sum[:])
	}
	return prev
}

func (s *Service) contentHash(ctx context.Context, tenantID, batchID string) (string, int64, error) {
	rc, err := s.repo.OpenContent(ctx, tenantID, batchID)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Seal closes the upload. The chunks must be exactly 0..n-1, and the whole content must hash to
// what the sender declared; if it does not, the batch is quarantined with the content kept as
// evidence, and nothing is parsed.
func (s *Service) Seal(ctx context.Context, a Actor, batchID string, expectedChunks int) (*Batch, error) {
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusUploading {
		if b.Status == StatusCreated {
			return nil, ErrIncompleteUpload
		}
		return nil, fmt.Errorf("%w: the upload is not open (%s)", ErrInvalidState, b.Status)
	}
	chunks, err := s.repo.Chunks(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 || len(chunks) != expectedChunks {
		return nil, fmt.Errorf("%w: %d chunks stored, %d expected", ErrIncompleteUpload, len(chunks), expectedChunks)
	}
	var size int64
	for i, c := range chunks {
		if c.Index != i {
			return nil, fmt.Errorf("%w: chunk %d is missing", ErrIncompleteUpload, i)
		}
		size += int64(c.Size)
	}
	if size > s.cfg.MaxBatchBytes {
		return nil, ErrTooLarge
	}
	sum, n, err := s.contentHash(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if n != size {
		return nil, fmt.Errorf("%w: stored content is %d bytes, chunks say %d", ErrIncompleteUpload, n, size)
	}
	count := len(chunks)
	chain := chainHash(chunks)
	patch := Patch{ChunkCount: &count, SizeBytes: &size, ContentSHA256: &sum, ChainHash: &chain}
	if sum != b.DeclaredSHA256 {
		q, terr := s.transition(ctx, a, b, []Status{StatusUploading}, StatusQuarantined, "UPLOAD_HASH_MISMATCH", ReasonHashMismatch, patch,
			batchEvent(EventBatchQuarantined, b, map[string]any{"reason": ReasonHashMismatch}))
		if terr != nil {
			return nil, terr
		}
		return q, ErrHashMismatch
	}
	return s.transition(ctx, a, b, []Status{StatusUploading}, StatusUploaded, "UPLOAD_SEALED", "", patch)
}

// RequestValidation asks for the scan, parse and validation pipeline to run on a sealed batch.
func (s *Service) RequestValidation(ctx context.Context, a Actor, batchID string) (*Batch, error) {
	if s.scanner == nil {
		return nil, ErrNoScanner
	}
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusUploaded {
		return nil, fmt.Errorf("%w: only a sealed upload can be validated (%s)", ErrInvalidState, b.Status)
	}
	if b.ValidateRequestedAt != nil {
		return b, nil
	}
	yes := true
	return s.transition(ctx, a, b, []Status{StatusUploaded}, "", "VALIDATION_REQUESTED", "", Patch{ValidateRequested: &yes})
}

// ── the pipeline: scan, parse, validate ─────────────────────────────────────

// RunPipeline carries a sealed batch through scan, parse and validation, as far as it can go.
// Each stage is its own durable transition, so a crash resumes where it stopped.
func (s *Service) RunPipeline(ctx context.Context, tenantID, batchID string) (*Batch, error) {
	if s.scanner == nil {
		return nil, ErrNoScanner
	}
	b, err := s.repo.GetBatch(ctx, tenantID, batchID)
	if err != nil {
		return nil, err
	}
	a := Actor{TenantID: tenantID, PrincipalID: "system:import-pipeline", CorrelationID: b.CorrelationID}
	if b.Status == StatusUploaded {
		if b, err = s.scanStage(ctx, a, b); err != nil || b.Status != StatusScanned {
			return b, err
		}
	}
	if b.Status == StatusScanned {
		if b, err = s.parseStage(ctx, a, b); err != nil || b.Status != StatusParsed {
			return b, err
		}
	}
	if b.Status == StatusParsed {
		return s.validateStage(ctx, a, b)
	}
	return b, nil
}

func (s *Service) fail(ctx context.Context, a Actor, b *Batch, from Status, reason string, patch Patch) (*Batch, error) {
	return s.transition(ctx, a, b, []Status{from}, StatusFailed, "FAILED", reason, patch,
		batchEvent(EventBatchFailed, b, map[string]any{"reason": reason}))
}

func (s *Service) quarantine(ctx context.Context, a Actor, b *Batch, from Status, reason string, patch Patch) (*Batch, error) {
	return s.transition(ctx, a, b, []Status{from}, StatusQuarantined, "QUARANTINED", reason, patch,
		batchEvent(EventBatchQuarantined, b, map[string]any{"reason": reason}))
}

func (s *Service) scanStage(ctx context.Context, a Actor, b *Batch) (*Batch, error) {
	// the bytes must still be the bytes that were sealed before anything looks at them
	sum, _, err := s.contentHash(ctx, b.TenantID, b.BatchID)
	if err != nil {
		return b, err
	}
	if sum != b.ContentSHA256 {
		return s.quarantine(ctx, a, b, StatusUploaded, ReasonHashMismatch, Patch{})
	}
	open := func() (io.ReadCloser, error) { return s.repo.OpenContent(ctx, b.TenantID, b.BatchID) }
	v, err := s.scanner.ScanOpen(ctx, open, b.DeclaredContentType)
	if err != nil {
		if errors.Is(err, scan.ErrNoScanner) {
			return b, ErrNoScanner
		}
		// no verdict is never "clean": the batch stays sealed and is retried
		return b, fmt.Errorf("%w: %v", ErrScannerUnavailable, err)
	}
	now := s.now()
	verdict := "CLEAN"
	if !v.Clean {
		verdict = "INFECTED"
	}
	patch := Patch{ScanEngine: &v.Engine, ScanVerdict: &verdict, ScannedAt: &now}
	if !v.Clean {
		sig := v.Signature
		patch.ScanSignature = &sig
		reason := ReasonMalware
		if !strings.HasPrefix(sig, "Eicar") && strings.HasPrefix(sig, "Content.") {
			reason = ReasonScanFailed // a content check, not a malware signature
		}
		return s.quarantine(ctx, a, b, StatusUploaded, reason, patch)
	}
	return s.transition(ctx, a, b, []Status{StatusUploaded}, StatusScanned, "SCANNED", "", patch)
}

func (s *Service) profileOf(ctx context.Context, b *Batch) (*Profile, error) {
	p, err := s.repo.GetProfile(ctx, b.ProfileKey, b.ProfileVersion)
	if err != nil {
		return nil, err
	}
	if p.Status == ProfileWithdrawn {
		return nil, ErrProfileNotCertified
	}
	return p, nil
}

func (s *Service) parseStage(ctx context.Context, a Actor, b *Batch) (*Batch, error) {
	p, err := s.profileOf(ctx, b)
	if err != nil {
		if errors.Is(err, ErrProfileNotCertified) || errors.Is(err, ErrProfileNotFound) {
			return s.fail(ctx, a, b, StatusScanned, ReasonProfileUnavailable, Patch{})
		}
		return b, err
	}
	spec := p.Spec.Normalize()
	// a crashed parse is rerun from the immutable source: whatever it wrote is rebuilt
	if err := s.repo.ResetItems(ctx, b.TenantID, b.BatchID); err != nil {
		return b, err
	}
	rc, err := s.repo.OpenContent(ctx, b.TenantID, b.BatchID)
	if err != nil {
		return b, err
	}
	defer rc.Close()
	rr, err := NewRowReader(spec, rc)
	if err != nil {
		if errors.Is(err, ErrSchemaMismatch) {
			return s.fail(ctx, a, b, StatusScanned, ReasonSchemaMismatch, Patch{})
		}
		return b, err
	}
	var (
		rows, valid, invalid int
		control              = new(big.Rat)
		seen                 = map[string]bool{}
		buf                  []Item
	)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		err := s.repo.InsertItems(ctx, b.TenantID, b.BatchID, buf)
		buf = buf[:0]
		return err
	}
	for {
		row, err := rr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrSchemaMismatch) {
				return s.fail(ctx, a, b, StatusScanned, ReasonSchemaMismatch, Patch{})
			}
			return b, err
		}
		rows++
		if rows > spec.MaxRows {
			return s.fail(ctx, a, b, StatusScanned, ReasonTooManyRows, Patch{})
		}
		var values map[string]any
		var errs []RowError
		if row.Raw == nil {
			values, errs = map[string]any{"_line": row.No}, row.Errs
		} else {
			values, errs = spec.ValidateRow(row.Raw, b.LegalEntityID)
			if k := spec.KeyOf(values); k != "" {
				if seen[k] {
					errs = append(errs, RowError{Code: CodeDuplicateKey})
				}
				seen[k] = true
			}
		}
		// the control total covers every row whose control value is a number, valid or not
		if spec.Control != nil {
			if cv, ok := values[spec.Control.Column]; ok {
				if r, perr := ParseDecimal(fmt.Sprint(cv)); perr == nil {
					control.Add(control, r)
				}
			}
		}
		st := ItemValid
		if len(errs) > 0 {
			st = ItemInvalid
			invalid++
		} else {
			valid++
		}
		hash := RowHash(values)
		buf = append(buf, Item{
			BatchID: b.BatchID, RowNo: row.No, TenantID: b.TenantID, RowHash: hash, Values: values, Status: st, ErrorCodes: errs,
			IdempotencyKey: ItemKey(b.BatchID, row.No, hash),
		})
		if len(buf) >= s.cfg.ItemInsertBatch {
			if err := flush(); err != nil {
				return b, err
			}
		}
	}
	if err := flush(); err != nil {
		return b, err
	}
	patch := Patch{Rows: &rows, Valid: &valid, Invalid: &invalid}
	mh := spec.MappingHash()
	patch.MappingHash = &mh
	if spec.Control != nil {
		c := CanonicalDecimal(control)
		patch.ComputedControl = &c
	}
	return s.transition(ctx, a, b, []Status{StatusScanned}, StatusParsed, "PARSED", "", patch)
}

func (s *Service) validateStage(ctx context.Context, a Actor, b *Batch) (*Batch, error) {
	p, err := s.profileOf(ctx, b)
	if err != nil {
		return b, err
	}
	spec := p.Spec.Normalize()
	if spec.Control != nil && spec.Control.Required && b.DeclaredControl == "" {
		return s.fail(ctx, a, b, StatusParsed, ReasonControlMissing, Patch{})
	}
	if b.DeclaredControl != "" && spec.Control != nil {
		d, derr := ParseDecimal(b.DeclaredControl)
		c, cerr := ParseDecimal(b.ComputedControl)
		if derr != nil || cerr != nil || d.Cmp(c) != 0 {
			return s.fail(ctx, a, b, StatusParsed, ReasonControlMismatch, Patch{})
		}
	}
	if b.DeclaredRows != nil && *b.DeclaredRows != b.RowCount {
		return s.fail(ctx, a, b, StatusParsed, ReasonRowCountMismatch, Patch{})
	}
	if b.RowCount == 0 || b.ValidRows == 0 || (spec.PartialPolicy == PartialRejectBatch && b.InvalidRows > 0) {
		return s.fail(ctx, a, b, StatusParsed, ReasonValidationErrors, Patch{})
	}
	return s.transition(ctx, a, b, []Status{StatusParsed}, StatusValidated, "VALIDATED", "", Patch{},
		batchEvent(EventValidated, b, map[string]any{"rows": b.RowCount, "valid": b.ValidRows, "invalid": b.InvalidRows}))
}

// ── preview, approval, execution ────────────────────────────────────────────

// PreviewFingerprint is what an approval is for: this content, this profile version and mapping,
// these counts and this control total, under this partial-success policy. Preview, approval and
// execution each compute it independently and it must come out the same.
func PreviewFingerprint(contentSHA256, profileKey string, profileVersion int, mappingHash string, rows, valid, invalid int, control, policy string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s@%d|%s|%d|%d|%d|%s|%s", contentSHA256, profileKey, profileVersion, mappingHash, rows, valid, invalid, control, policy)))
	return hex.EncodeToString(sum[:])
}

// Preview builds what the approver sees and moves the batch to PREVIEWED.
func (s *Service) Preview(ctx context.Context, a Actor, batchID string) (*Batch, error) {
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusValidated {
		return nil, fmt.Errorf("%w: only a validated batch can be previewed (%s)", ErrInvalidState, b.Status)
	}
	p, err := s.profileOf(ctx, b)
	if err != nil {
		return nil, err
	}
	spec := p.Spec.Normalize()
	errCounts, err := s.repo.ErrorCounts(ctx, b.TenantID, b.BatchID)
	if err != nil {
		return nil, err
	}
	validRows, err := s.repo.ListItems(ctx, b.TenantID, b.BatchID, ItemValid, 0, s.cfg.SampleSize)
	if err != nil {
		return nil, err
	}
	invalidRows, err := s.repo.ListItems(ctx, b.TenantID, b.BatchID, ItemInvalid, 0, s.cfg.SampleSize)
	if err != nil {
		return nil, err
	}
	pv := &Preview{
		RowCount: b.RowCount, ValidRows: b.ValidRows, InvalidRows: b.InvalidRows, DeclaredRows: b.DeclaredRows,
		DeclaredControl: b.DeclaredControl, ErrorCounts: errCounts, ProfileKey: b.ProfileKey, ProfileVersion: b.ProfileVersion,
		MappingHash: b.MappingHash, ContentSHA256: b.ContentSHA256, PartialPolicy: spec.PartialPolicy, Material: b.Material,
		WillBeSent: b.ValidRows, WillBeSkipped: b.InvalidRows, ControlTotal: b.ComputedControl,
		SampleValid: []SampleRow{}, SampleInvalid: []SampleRow{},
	}
	if spec.Control != nil {
		pv.ControlColumn = spec.Control.Column
	}
	for _, it := range validRows {
		pv.SampleValid = append(pv.SampleValid, SampleRow{RowNo: it.RowNo, Values: it.Values})
	}
	for _, it := range invalidRows {
		pv.SampleInvalid = append(pv.SampleInvalid, SampleRow{RowNo: it.RowNo, Errors: it.ErrorCodes})
	}
	fp := PreviewFingerprint(b.ContentSHA256, b.ProfileKey, b.ProfileVersion, b.MappingHash, b.RowCount, b.ValidRows, b.InvalidRows, b.ComputedControl, spec.PartialPolicy)
	now := s.now()
	return s.transition(ctx, a, b, []Status{StatusValidated}, StatusPreviewed, "PREVIEWED", "", Patch{Preview: pv, PreviewFingerprint: &fp, PreviewedAt: &now})
}

// Approve approves exactly the previewed content. The approver names the fingerprint they were
// shown; a material import is approved by someone other than the person who created it.
func (s *Service) Approve(ctx context.Context, a Actor, batchID, fingerprint, reason string) (*Batch, error) {
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusPreviewed {
		return nil, fmt.Errorf("%w: only a previewed batch can be approved (%s)", ErrInvalidState, b.Status)
	}
	if fingerprint == "" || fingerprint != b.PreviewFingerprint {
		return nil, ErrPreviewMismatch
	}
	if b.Material && strings.EqualFold(a.PrincipalID, b.CreatedBy) {
		return nil, ErrSelfApproval
	}
	now := s.now()
	return s.transition(ctx, a, b, []Status{StatusPreviewed}, StatusApproved, "APPROVED", reason,
		Patch{ApprovedFingerprint: &fingerprint, ApprovedBy: &a.PrincipalID, ApprovedAt: &now},
		batchEvent(EventPreviewApproved, b, map[string]any{"approved_by": a.PrincipalID, "fingerprint": fingerprint}))
}

// Execute starts delivery of an approved batch to the owning domain. Before anything is sent it
// re-derives, from the stored bytes, the stored items and the profile as it is now, the very
// fingerprint that was approved. Any difference stops the batch: what is executed is what was
// previewed.
func (s *Service) Execute(ctx context.Context, a Actor, batchID string) (*Batch, error) {
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusApproved {
		return nil, fmt.Errorf("%w: only an approved batch can be executed (%s)", ErrInvalidState, b.Status)
	}
	p, err := s.profileOf(ctx, b)
	if err != nil {
		return nil, err
	}
	spec := p.Spec.Normalize()
	if !s.targets.Has(spec.Target.Key) {
		return nil, ErrTargetNotConfigured
	}
	differs := func(why string) (*Batch, error) {
		f, ferr := s.fail(ctx, a, b, StatusApproved, ReasonPreviewDiffers, Patch{})
		if ferr != nil {
			return nil, ferr
		}
		return f, fmt.Errorf("%w: %s", ErrPreviewMismatch, why)
	}
	sum, _, err := s.contentHash(ctx, b.TenantID, b.BatchID)
	if err != nil {
		return nil, err
	}
	if sum != b.ContentSHA256 {
		return differs("the stored content no longer matches its hash")
	}
	counts, err := s.repo.ItemCounts(ctx, b.TenantID, b.BatchID)
	if err != nil {
		return nil, err
	}
	pending := counts[ItemPending]
	valid := counts[ItemValid] + pending
	invalid := counts[ItemInvalid] + counts[ItemSkipped]
	if valid+invalid+counts[ItemImported]+counts[ItemQuarantined]+counts[ItemFailed] != b.RowCount {
		return differs("the stored items do not add up to the previewed row count")
	}
	fp := PreviewFingerprint(sum, b.ProfileKey, b.ProfileVersion, spec.MappingHash(), b.RowCount, b.ValidRows, b.InvalidRows, b.ComputedControl, spec.PartialPolicy)
	if fp != b.ApprovedFingerprint || b.ValidRows != valid || b.InvalidRows != invalid {
		return differs("the recomputed fingerprint is not the approved one")
	}
	if _, _, err := s.repo.ActivateItems(ctx, b.TenantID, b.BatchID); err != nil {
		return nil, err
	}
	now := s.now()
	return s.transition(ctx, a, b, []Status{StatusApproved}, StatusImporting, "EXECUTION_STARTED", "",
		Patch{ExecutedBy: &a.PrincipalID, ExecutedAt: &now},
		batchEvent(EventStarted, b, map[string]any{"executed_by": a.PrincipalID, "items": b.ValidRows}))
}

// Cancel stops a batch that has not started executing.
func (s *Service) Cancel(ctx context.Context, a Actor, batchID, reason string) (*Batch, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrValidation)
	}
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if !b.Status.PreExecution() {
		return nil, fmt.Errorf("%w: a batch that is %s cannot be cancelled", ErrInvalidState, b.Status)
	}
	return s.transition(ctx, a, b, []Status{b.Status}, StatusCancelled, "CANCELLED", reason, Patch{})
}

// ── items ───────────────────────────────────────────────────────────────────

// Items lists a batch's items.
func (s *Service) Items(ctx context.Context, tenantID, batchID string, status ItemStatus, afterRow, limit int) ([]Item, error) {
	if _, err := s.repo.GetBatch(ctx, tenantID, batchID); err != nil {
		return nil, err
	}
	return s.repo.ListItems(ctx, tenantID, batchID, status, afterRow, limit)
}

// QuarantineItem holds back a pending item with a reason and an owner, before it is sent.
func (s *Service) QuarantineItem(ctx context.Context, a Actor, batchID string, rowNo int, reason, owner string) error {
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(owner) == "" {
		return fmt.Errorf("%w: a quarantined item needs a reason and an owner", ErrValidation)
	}
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return err
	}
	if b.Status != StatusImporting {
		return fmt.Errorf("%w: items are quarantined while the batch is importing (%s)", ErrInvalidState, b.Status)
	}
	return s.repo.QuarantineItem(ctx, a.TenantID, batchID, rowNo, reason, owner, a.PrincipalID, []Event{{
		Type: EventItemQuarantined, Payload: map[string]any{"batch_id": batchID, "row_no": rowNo, "reason": reason, "owner": owner, "by": a.PrincipalID},
	}})
}

// RetryItem puts a quarantined or failed item back in the queue (a person decided it should be
// tried again, and said why). A batch that had finished with exceptions resumes importing.
func (s *Service) RetryItem(ctx context.Context, a Actor, batchID string, rowNo int, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: a reason is required", ErrValidation)
	}
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return err
	}
	if b.Status != StatusImporting && b.Status != StatusCompletedWithExceptions {
		return fmt.Errorf("%w: items are retried while the batch is importing or has exceptions (%s)", ErrInvalidState, b.Status)
	}
	if err := s.repo.RetryItem(ctx, a.TenantID, batchID, rowNo, s.now(), s.cfg.ItemMaxAttempts*3); err != nil {
		return err
	}
	if b.Status == StatusCompletedWithExceptions {
		if _, err := s.transition(ctx, a, b, []Status{StatusCompletedWithExceptions}, StatusImporting, "ITEM_RETRY", reason, Patch{}); err != nil && !errors.Is(err, ErrInvalidState) {
			return err
		}
	}
	return nil
}

// ── reconciliation ──────────────────────────────────────────────────────────

// Reconcile compares what was sent with what the owning domain accepted. A batch is RECONCILED only
// when every item was imported with its domain result reference, the counts agree with the file and
// the imported control total equals the file's. Otherwise the findings are recorded, the
// reconciliation-failed event is raised and the batch does not advance.
func (s *Service) Reconcile(ctx context.Context, a Actor, batchID string) (*Batch, error) {
	b, err := s.repo.GetBatch(ctx, a.TenantID, batchID)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusCompleted && b.Status != StatusCompletedWithExceptions {
		return nil, fmt.Errorf("%w: only a completed batch can be reconciled (%s)", ErrInvalidState, b.Status)
	}
	p, err := s.profileOf(ctx, b)
	if err != nil {
		return nil, err
	}
	spec := p.Spec.Normalize()
	counts, err := s.repo.ItemCounts(ctx, b.TenantID, b.BatchID)
	if err != nil {
		return nil, err
	}
	rec := &Reconciliation{
		TotalItems: b.RowCount, Imported: counts[ItemImported], Quarantined: counts[ItemQuarantined], Failed: counts[ItemFailed],
		Skipped: counts[ItemSkipped], Problems: []string{},
	}
	sum := rec.Imported + rec.Quarantined + rec.Failed + rec.Skipped
	if sum != b.RowCount {
		rec.Problems = append(rec.Problems, fmt.Sprintf("item statuses account for %d of %d rows", sum, b.RowCount))
	}
	if rec.Quarantined+rec.Failed+rec.Skipped > 0 {
		rec.Problems = append(rec.Problems, fmt.Sprintf("%d items are not imported (%d quarantined, %d failed, %d skipped)",
			rec.Quarantined+rec.Failed+rec.Skipped, rec.Quarantined, rec.Failed, rec.Skipped))
	}
	if rec.Imported != b.RowCount {
		rec.Problems = append(rec.Problems, fmt.Sprintf("%d of %d rows are imported", rec.Imported, b.RowCount))
	}
	if b.DeclaredRows != nil && *b.DeclaredRows != rec.Imported {
		rec.Problems = append(rec.Problems, "the declared row count differs from the number imported")
	}
	if spec.Control != nil {
		rec.ExpectedControl = b.ComputedControl
		got, err := s.repo.SumColumn(ctx, b.TenantID, b.BatchID, spec.Control.Column, ItemImported)
		if err != nil {
			return nil, err
		}
		rec.ImportedControl = got
		e, eerr := ParseDecimal(rec.ExpectedControl)
		g, gerr := ParseDecimal(rec.ImportedControl)
		if eerr != nil || gerr != nil || e.Cmp(g) != 0 {
			rec.Problems = append(rec.Problems, "the imported control total differs from the file's")
		}
	}
	if spec.RequireResultRef {
		missing, err := s.repo.CountMissingRefs(ctx, b.TenantID, b.BatchID)
		if err != nil {
			return nil, err
		}
		rec.MissingResultRef = missing
		if missing > 0 {
			rec.Problems = append(rec.Problems, fmt.Sprintf("%d imported items have no domain result reference", missing))
		}
	}
	rec.Matched = len(rec.Problems) == 0 && b.Status == StatusCompleted
	now := s.now()
	if rec.Matched {
		out, err := s.transition(ctx, a, b, []Status{StatusCompleted}, StatusReconciled, "RECONCILED", "", Patch{Reconciliation: rec, ReconciledAt: &now},
			batchEvent(EventReconciled, b, map[string]any{"imported": rec.Imported}))
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	out, err := s.transition(ctx, a, b, []Status{b.Status}, "", "RECONCILIATION_FAILED", strings.Join(rec.Problems, "; "), Patch{Reconciliation: rec},
		batchEvent(EventReconciliationFail, b, map[string]any{"problems": rec.Problems}))
	if err != nil {
		return nil, err
	}
	return out, ErrReconciliationFailed
}

// ControlTotals reports the declared, computed and imported control totals side by side.
type ControlTotals struct {
	Column   string `json:"column,omitempty"`
	Declared string `json:"declared,omitempty"`
	Computed string `json:"computed,omitempty"`
	Imported string `json:"imported,omitempty"`
	Rows     int    `json:"rows"`
	Declrows *int   `json:"declared_rows,omitempty"`
}

// ControlTotals reads a batch's control totals.
func (s *Service) ControlTotals(ctx context.Context, tenantID, batchID string) (*ControlTotals, error) {
	b, err := s.repo.GetBatch(ctx, tenantID, batchID)
	if err != nil {
		return nil, err
	}
	p, err := s.profileOf(ctx, b)
	if err != nil {
		return nil, err
	}
	ct := &ControlTotals{Declared: b.DeclaredControl, Computed: b.ComputedControl, Rows: b.RowCount, Declrows: b.DeclaredRows}
	if c := p.Spec.Control; c != nil {
		ct.Column = c.Column
		if b.Status == StatusImporting || b.Status == StatusCompleted || b.Status == StatusCompletedWithExceptions || b.Status == StatusReconciled {
			if ct.Imported, err = s.repo.SumColumn(ctx, tenantID, batchID, c.Column, ItemImported); err != nil {
				return nil, err
			}
		}
	}
	return ct, nil
}
