package batch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/integration"
	"zoiko.io/eventing/scan"
)

// Connections is what the batch engine needs of the connection layer (XIC-02).
type Connections interface {
	Connection(ctx context.Context, id string) (*xicconn.Connection, error)
	PinnedContract(ctx context.Context, c *xicconn.Connection) (*registry.ContractVersion, error)
	Operable(ctx context.Context, tenant, connectionID, operationID string) (*xicconn.Decision, error)
}

// Scanner is the content gate every parsed byte passes. *scan.Chain satisfies it; a deployment with no scanner is not
// given a "clean" default.
type Scanner interface {
	ScanOpen(ctx context.Context, open func() (io.ReadCloser, error), declaredType string) (scan.Verdict, error)
}

// Config holds the engine's policy knobs.
type Config struct {
	MaxRequestBytes      int64         // the read cap for one HTTPS delivery (manifest and files together)
	MaxFileBytes         int64         // the most one stored file may hold, whatever a contract asks (XIC-013 beyond it)
	MaxDecompressedBytes int64         // the platform ceiling for what a file may expand to
	ContentRetention     time.Duration // how long file bodies are kept for processing and replay. Zero keeps them only
	// for processing.
	ProcessingGrace    time.Duration // how long a body is kept when its data classes forbid retention
	ContentDenyClasses []string      // data classes whose file bodies are kept only for ProcessingGrace
	ConnRatePerMin     int
	IPRatePerMin       int
	ProcessLease       time.Duration // a claimed batch is left alone this long before another worker may resume it
	RowChunk           int           // rows admitted between progress saves
	RefreshPending     time.Duration // how often a batch with unresolved rows is re-evaluated
	RefreshPartial     time.Duration // how often a PARTIAL batch is re-evaluated
	ProcessBackoff     time.Duration // delay before a batch whose processing could not complete is tried again

	// Replay governance.
	ReplayMaxItems     int           // the most exchanges one replay may cover
	ReplayApprovalRows int           // a replay covering at least this many exchanges needs approval (XIC-015)
	ReplayWindowMax    time.Duration // the widest time window a WINDOW replay may cover
	RegulatedClasses   []string      // data classes whose replay always needs approval
	ReplayLease        time.Duration
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{MaxRequestBytes: 64 << 20, MaxFileBytes: 25 << 20, MaxDecompressedBytes: 256 << 20, ContentRetention: 7 * 24 * time.Hour,
		ProcessingGrace: time.Hour, ContentDenyClasses: []string{"PERSONAL", "RESTRICTED"}, ConnRatePerMin: 60, IPRatePerMin: 120,
		ProcessLease: 2 * time.Minute, RowChunk: 100, RefreshPending: 30 * time.Second, RefreshPartial: 5 * time.Minute, ProcessBackoff: 30 * time.Second,
		ReplayMaxItems: 50000, ReplayApprovalRows: 100, ReplayWindowMax: 7 * 24 * time.Hour, RegulatedClasses: []string{"REGULATED", "PAYROLL", "TAX"},
		ReplayLease: 5 * time.Minute}
}

// Service is the batch engine.
type Service struct {
	Repo       Repo
	Conns      Connections
	In         *inbound.Service
	Secrets    inbound.Secrets
	EdgeSecret []byte
	Scanner    Scanner
	Cfg        Config
	Clock      func() time.Time
	// Observe receives a short reason for every refusal and outcome, for metrics. It must not block.
	Observe func(kind, reason string)

	limMu sync.Mutex
	lim   map[string]*bucket
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) cfg() Config {
	if s.Cfg.MaxFileBytes == 0 {
		return DefaultConfig()
	}
	return s.Cfg
}

func (s *Service) observe(kind, reason string) {
	if s.Observe != nil {
		s.Observe(kind, reason)
	}
}

// Delivery is one file delivery as the transport hands it over. The bodies are already bounded by the read cap.
type Delivery struct {
	ConnectorKey  string
	ConnectionID  string
	OperationID   string
	Channel       string
	Header        http.Header
	Manifest      []byte
	Files         []DeliveredFile
	RemoteAddr    string
	CorrelationID string
}

// Outcome is what the intake answers. A refusal carries a stable XIC error; nothing here ever contains file content.
type Outcome struct {
	Status     int
	Batch      *Batch
	Duplicate  bool
	XIC        *integration.XICError
	RetryAfter int
	Detail     string
	Reason     string
}

var (
	connectorKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{2,62}$`)
	operationIDRE  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,62}$`)
)

func (s *Service) authFailure(reason string) *Outcome {
	s.observe("batch_refused", reason)
	return &Outcome{Status: http.StatusUnauthorized, XIC: integration.NewXIC(integration.XICAuthenticationFailed, "authentication failed"), Reason: reason}
}

func (s *Service) unavailable(reason string) *Outcome {
	s.observe("batch_refused", reason)
	return &Outcome{Status: http.StatusServiceUnavailable, RetryAfter: 5, Reason: reason, Detail: "temporarily unavailable"}
}

// Receive handles one delivery over the HTTPS object channel. The manifest is signed with the contract's
// authentication profile and carries the digest of every file, so the signature vouches for the whole delivery: a
// file that does not match its manifest entry fails integrity before any row is read.
func (s *Service) Receive(ctx context.Context, d Delivery) *Outcome {
	now := s.now()
	cfg := s.cfg()
	ipKey := hostOf(d.RemoteAddr)
	if !s.allow("conn:"+d.ConnectionID, cfg.ConnRatePerMin, now) || !s.allow("ip:"+ipKey, cfg.IPRatePerMin, now) {
		s.observe("batch_refused", "rate_limited")
		return &Outcome{Status: http.StatusTooManyRequests, RetryAfter: 30, Reason: "rate_limited", XIC: integration.NewXIC(integration.XICRateLimited, "too many requests")}
	}
	if !connectorKeyRE.MatchString(d.ConnectorKey) || !operationIDRE.MatchString(d.OperationID) {
		return s.authFailure("bad_route")
	}
	if _, err := uuid.Parse(d.ConnectionID); err != nil {
		return s.authFailure("bad_route")
	}
	conn, err := s.Conns.Connection(ctx, d.ConnectionID)
	if err != nil {
		if errors.Is(err, xicconn.ErrNotFound) {
			return s.authFailure("unknown_connection")
		}
		return s.unavailable("dependency")
	}
	if conn.ConnectorID != d.ConnectorKey {
		return s.authFailure("connector_mismatch")
	}
	v, err := s.Conns.PinnedContract(ctx, conn)
	if err != nil {
		if errors.Is(err, xicconn.ErrContractUnavailable) {
			return s.authFailure("contract_unavailable")
		}
		return s.unavailable("dependency")
	}
	op := v.Operation(d.OperationID)
	if op == nil || op.Direction != registry.Inbound || op.Channel != "OBJECT" || v.BatchFor(op) == nil {
		return s.authFailure("unknown_operation")
	}
	info, ok := v.UsedAuthProfiles()[op.AuthProfile]
	if !ok {
		return s.authFailure("no_auth_profile")
	}
	res, aerr := inbound.Authenticate(ctx, s.Secrets, s.EdgeSecret, conn, info, d.Header, d.Manifest, now)
	switch {
	case errors.Is(aerr, inbound.ErrSecretUnavailable):
		return s.unavailable("secret_unavailable")
	case errors.Is(aerr, inbound.ErrAuthUnsupported):
		return s.authFailure("mechanism_unsupported")
	case errors.Is(aerr, inbound.ErrAuthFailed):
		return s.authFailure("bad_credential")
	}
	in := ingestInput{conn: conn, v: v, op: op, d: d, auth: res.Mechanism, netHash: sha("net", ipKey)[:32]}
	if errors.Is(aerr, inbound.ErrReplayWindow) {
		return s.reject(ctx, in, nil, nil, "REPLAY_WINDOW", integration.XICReplayWindowFailed, http.StatusUnauthorized, "the delivery is outside the replay window")
	}
	return s.ingest(ctx, in)
}

// IngestPulled handles a delivery the platform collected itself from a provider's SFTP server. The channel
// authenticated the provider (a pinned host key and the connection's credential), so what is left is the same
// verification every delivery gets.
func (s *Service) IngestPulled(ctx context.Context, conn *xicconn.Connection, v *registry.ContractVersion, op *registry.Operation, manifest []byte, files []DeliveredFile) *Outcome {
	d := Delivery{ConnectorKey: conn.ConnectorID, ConnectionID: conn.ConnectionID, OperationID: op.ID, Channel: ChannelSFTP, Manifest: manifest, Files: files, CorrelationID: "sftp-pull"}
	return s.ingest(ctx, ingestInput{conn: conn, v: v, op: op, d: d, auth: "SFTP_PULL", netHash: sha("net", "sftp", conn.ConnectionID)[:32]})
}

type ingestInput struct {
	conn    *xicconn.Connection
	v       *registry.ContractVersion
	op      *registry.Operation
	d       Delivery
	auth    string
	netHash string
}

func (s *Service) base(in ingestInput, now time.Time) *Batch {
	return &Batch{BatchID: uuid.NewString(), TenantID: in.conn.TenantID, ConnectionID: in.conn.ConnectionID, ConnectorID: in.conn.ConnectorID, OperationID: in.op.ID,
		ContractVersionID: in.v.ContractVersionID, SourceSchema: in.op.RequestSchema, Channel: dflt(in.d.Channel, ChannelHTTPS), State: StateReceived, ManifestHash: hashBytes(in.d.Manifest),
		AuthMechanism: in.auth, SourceNetHash: in.netHash, CorrelationID: in.d.CorrelationID, Version: 1, ReceivedAt: now, UpdatedAt: now}
}

// reject records a delivery that was refused before processing, as evidence, and answers it. The sender is
// authenticated by now, so a refusal is durable; no file content is kept for it.
func (s *Service) reject(ctx context.Context, in ingestInput, m *Manifest, files []File, reason string, xic integration.XICCode, status int, detail string) *Outcome {
	now := s.now()
	b := s.base(in, now)
	b.State, b.ReasonCode, b.XICCode = StateRejected, reason, string(xic)
	b.ExternalBatchID, b.SourceSystem = "(unidentified)", ""
	if m != nil {
		b.ExternalBatchID, b.SourceSystem, b.FileCount = m.BatchID, m.SourceSchemaSystem(), len(m.Files)
		b.DeclaredRows = m.RecordCount
		b.ContentHash = ContentHash(m)
	} else {
		b.ContentHash = hashBytes(in.d.Manifest)
	}
	for i := range files {
		files[i].Content, files[i].ContentRetained = nil, false
	}
	ev := Event{Kind: "REJECTED", To: StateRejected, Actor: "system:xic-batch", At: now, Detail: mustJSON(map[string]any{"reason": reason})}
	out := []OutEvent{{Type: "integration.batch.rejected", AggregateType: "batch", AggregateID: b.BatchID, ActorID: "system:xic-batch", CorrelationID: b.CorrelationID,
		Data: map[string]any{"batch_id": b.BatchID, "tenant_id": b.TenantID, "connection_id": b.ConnectionID, "reason_code": reason, "xic_code": string(xic)}}}
	if err := s.Repo.InsertRejected(withTenant(ctx, in.conn.TenantID), b, files, ev, out); err != nil {
		return s.unavailable("storage_unavailable")
	}
	s.observe("batch_rejected", reason)
	o := &Outcome{Status: status, Batch: b, Reason: reason, Detail: detail}
	if xic != "" {
		o.XIC = integration.NewXIC(xic, detail)
	}
	return o
}

// SourceSchemaSystem is the manifest's source system (a method so a nil check stays in one place).
func (m *Manifest) SourceSchemaSystem() string { return m.SourceSystem }

func (s *Service) ingest(ctx context.Context, in ingestInput) *Outcome {
	now := s.now()
	cfg := s.cfg()
	conn, v, op := in.conn, in.v, in.op
	ictx := withTenant(ctx, conn.TenantID)
	bi := v.BatchFor(op)
	if bi == nil {
		return s.authFailure("no_batch_profile")
	}

	// the connection must be usable for this operation right now
	if _, err := s.Conns.Operable(ctx, conn.TenantID, conn.ConnectionID, op.ID); err != nil {
		var ref *xicconn.Refusal
		if errors.As(err, &ref) {
			return s.reject(ictx, in, nil, nil, "CONNECTION_"+string(ref.Code), integration.XICCode(ref.Code), statusFor(integration.XICCode(ref.Code)), ref.Detail)
		}
		return s.unavailable("dependency")
	}

	// the manifest, then the schema it names, then every file against its entry - before any row is read
	m, reason := ParseManifest(in.d.Manifest)
	if m == nil {
		return s.reject(ictx, in, nil, nil, reason, integration.XICBatchIntegrityFailed, http.StatusUnprocessableEntity, "the manifest is not valid")
	}
	if m.Schema != op.RequestSchema {
		return s.reject(ictx, in, m, nil, ReasonSchemaVersion, integration.XICSchemaVersionUnsupported, http.StatusUnprocessableEntity, "the schema version is not supported")
	}
	verdict := Verify(m, in.d.Files, bi, cfg.MaxFileBytes)
	if !verdict.OK {
		return s.reject(ictx, in, m, verdict.Files, verdict.Reason, integration.XICBatchIntegrityFailed, statusForReason(verdict.Reason), verdict.Detail)
	}

	b := s.base(in, now)
	b.ExternalBatchID, b.SourceSystem, b.FileCount, b.DeclaredRows = m.BatchID, m.SourceSystem, len(m.Files), m.RecordCount
	b.Manifest = in.d.Manifest
	b.ContentHash = ContentHash(m)
	b.DedupeKey = sha("batch", conn.ConnectionID, op.ID, m.BatchID)
	t := now
	b.NextProcessAt = &t
	// file bodies are kept for processing and for replay for a bounded time, and only for processing when the data
	// classes forbid keeping them
	keep := cfg.ContentRetention
	if keep <= 0 || denied(op.DataClasses, cfg.ContentDenyClasses) {
		keep = cfg.ProcessingGrace
	}
	exp := now.Add(keep)
	byName := map[string][]byte{}
	for _, f := range in.d.Files {
		byName[f.Name] = f.Data
	}
	files := verdict.Files
	for i := range files {
		files[i].FileID, files[i].BatchID, files[i].TenantID = uuid.NewString(), b.BatchID, b.TenantID
		files[i].Content, files[i].ContentRetained, files[i].ContentExpiresAt = byName[files[i].Name], true, &exp
	}
	ev := Event{Kind: "RECEIVED", To: StateReceived, Actor: "system:xic-batch", At: now,
		Detail: mustJSON(map[string]any{"channel": b.Channel, "files": len(files), "declared_rows": m.RecordCount, "auth": in.auth})}
	out := []OutEvent{{Type: "integration.batch.received", AggregateType: "batch", AggregateID: b.BatchID, ActorID: "system:xic-batch", CorrelationID: b.CorrelationID,
		Data: map[string]any{"batch_id": b.BatchID, "tenant_id": b.TenantID, "connection_id": b.ConnectionID, "operation_id": op.ID, "external_batch_id": b.ExternalBatchID,
			"source_schema": b.SourceSchema, "file_count": b.FileCount, "manifest_hash": b.ManifestHash}}}
	existing, err := s.Repo.Insert(ictx, b, files, ev, out)
	switch {
	case err == nil:
		s.observe("batch_received", op.ID)
		return &Outcome{Status: http.StatusAccepted, Batch: b, Reason: "received"}
	case errors.Is(err, ErrDuplicate):
		return s.duplicateOrConflict(ictx, in, m, existing, b)
	}
	return s.unavailable("storage_unavailable")
}

// duplicateOrConflict answers a batch identity that is already held. The same content is the same batch: the original
// reference is returned and nothing is reprocessed (NP-37). The same identity with different content is never "the
// same batch": it is kept apart as a conflict to investigate and the original is untouched (NP-38).
func (s *Service) duplicateOrConflict(ctx context.Context, in ingestInput, m *Manifest, existing *Batch, fresh *Batch) *Outcome {
	now := s.now()
	if existing == nil {
		return s.unavailable("storage_unavailable")
	}
	if existing.ContentHash == fresh.ContentHash {
		_ = s.Repo.AddEvent(ctx, existing.BatchID, Event{Kind: "DUPLICATE_DELIVERY", Actor: "system:xic-batch", At: now,
			Detail: mustJSON(map[string]any{"manifest_hash": fresh.ManifestHash, "channel": fresh.Channel})})
		_ = s.Repo.RecordSignal(ctx, existing.TenantID, OutEvent{Type: "integration.batch.duplicate", AggregateType: "batch", AggregateID: existing.BatchID, ActorID: "system:xic-batch",
			CorrelationID: fresh.CorrelationID, Data: map[string]any{"batch_id": existing.BatchID, "tenant_id": existing.TenantID, "connection_id": existing.ConnectionID,
				"external_batch_id": existing.ExternalBatchID}})
		s.observe("batch_duplicate", "same_content")
		return &Outcome{Status: http.StatusOK, Batch: existing, Duplicate: true, Reason: "DUPLICATE"}
	}
	c := s.base(in, now)
	c.ExternalBatchID, c.SourceSystem, c.FileCount, c.DeclaredRows = m.BatchID, m.SourceSystem, len(m.Files), m.RecordCount
	c.ContentHash = fresh.ContentHash
	c.State, c.ReasonCode, c.XICCode, c.OriginalBatchID = StateQuarantined, ReasonConflict, string(integration.XICBatchIntegrityFailed), existing.BatchID
	ev := Event{Kind: "CONFLICT", To: StateQuarantined, Actor: "system:xic-batch", At: now, Detail: mustJSON(map[string]any{"original": existing.BatchID, "reason": ReasonConflict})}
	out := []OutEvent{{Type: "integration.batch.conflict", AggregateType: "batch", AggregateID: c.BatchID, ActorID: "system:xic-batch", CorrelationID: c.CorrelationID,
		Data: map[string]any{"batch_id": c.BatchID, "original_batch_id": existing.BatchID, "tenant_id": c.TenantID, "connection_id": c.ConnectionID, "reason_code": ReasonConflict}}}
	if err := s.Repo.InsertRejected(ctx, c, nil, ev, out); err != nil {
		return s.unavailable("storage_unavailable")
	}
	s.observe("batch_conflict", ReasonConflict)
	return &Outcome{Status: http.StatusConflict, Batch: c, Reason: ReasonConflict, Detail: "the batch identity is held with different content",
		XIC: integration.NewXIC(integration.XICBatchIntegrityFailed, "the batch identity is held with different content")}
}

func denied(classes, deny []string) bool {
	for _, c := range classes {
		for _, d := range deny {
			if strings.EqualFold(c, d) {
				return true
			}
		}
	}
	return false
}

func statusFor(c integration.XICCode) int {
	switch c {
	case integration.XICConnectionNotActive, integration.XICCertificationStale:
		return http.StatusConflict
	case integration.XICContractVersionMismatch:
		return http.StatusUnprocessableEntity
	case integration.XICResidencyRouteBlocked, integration.XICTenantScopeMismatch:
		return http.StatusForbidden
	}
	return c.Info().Status
}

func statusForReason(r string) int {
	switch r {
	case ReasonFileTooLarge:
		return http.StatusRequestEntityTooLarge
	case ReasonIntegrity:
		return http.StatusUnprocessableEntity
	}
	return http.StatusUnprocessableEntity
}

func hostOf(remote string) string {
	if i := strings.LastIndex(remote, ":"); i > 0 && !strings.Contains(remote[i:], "]") {
		return remote[:i]
	}
	return remote
}

// ── rate limiting (a small token bucket per key) ───────────────────────────

type bucket struct {
	tokens float64
	at     time.Time
}

const maxBuckets = 20000

func (s *Service) allow(key string, perMin int, now time.Time) bool {
	if perMin <= 0 {
		return true
	}
	s.limMu.Lock()
	defer s.limMu.Unlock()
	if s.lim == nil {
		s.lim = map[string]*bucket{}
	}
	b, ok := s.lim[key]
	if !ok {
		if len(s.lim) >= maxBuckets {
			for k, v := range s.lim {
				if now.Sub(v.at) > time.Minute {
					delete(s.lim, k)
				}
			}
			if len(s.lim) >= maxBuckets {
				return false
			}
		}
		b = &bucket{tokens: float64(perMin), at: now}
		s.lim[key] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * float64(perMin) / 60
	if b.tokens > float64(perMin) {
		b.tokens = float64(perMin)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
