package batch

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/scan"
)

var (
	errNotDue = errors.New("not due")
	// errUnsafe and errLimit stop a batch for a security reason; errFile for a file-level reason.
	errUnsafe = errors.New("content not plain text")
	errLimit  = errors.New("decompression limit")
)

// prepared is everything processing a batch needs, read once.
type prepared struct {
	b      *Batch
	conn   *xicconn.Connection
	v      *registry.ContractVersion
	op     *registry.Operation
	bi     *registry.BatchInfo
	schema *registry.Schema
	csv    *registry.CSVInfo
	files  []File
}

// parsedFile is one file's decompressed, scanned and counted content.
type parsedFile struct {
	file File
	data []byte
	rows int
}

type rawRow struct {
	no      int
	hash    string
	body    []byte // the row as a JSON object, or nil when the row is structurally bad
	badness string // why the row is bad
}

// Process drives one batch as far as it can go: verification of the files' content, the file-level decision, the
// admission of every row and the first report on the outcome. It is safe to call again: it resumes where an
// interrupted run stopped and never admits a row twice.
func (s *Service) Process(ctx context.Context, tenant, id string) (*Batch, error) {
	ictx := withTenant(ctx, tenant)
	now := s.now()
	cur, err := s.Repo.Get(ictx, id)
	if err != nil {
		return nil, err
	}
	switch cur.State {
	case StateReconciled, StatePartial:
		return s.Refresh(ictx, tenant, id)
	case StateProcessing:
		if cur.ProcessingDone {
			return s.Refresh(ictx, tenant, id)
		}
	case StateReceived, StateValidating, StateAccepted:
	default:
		return cur, nil
	}
	// claim: one worker at a time per batch
	lease := now.Add(s.cfg().ProcessLease)
	b, err := s.Repo.Mutate(ictx, id, func(tx *Tx) error {
		st := tx.B.State
		if st != StateReceived && tx.B.LeaseUntil != nil && tx.B.LeaseUntil.After(now) {
			return errNotDue
		}
		if st != StateReceived && st != StateValidating && st != StateAccepted && st != StateProcessing {
			return errNotDue
		}
		if st == StateReceived {
			tx.B.State = StateValidating
			tx.Event = &Event{Kind: "VALIDATING", From: StateReceived, To: StateValidating, Actor: "system:xic-batch", At: now}
		}
		tx.B.LeaseUntil, tx.B.NextProcessAt = &lease, nil
		tx.B.ProcessAttempts++
		return nil
	})
	if errors.Is(err, errNotDue) {
		return cur, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := s.prepare(ictx, b)
	if err != nil {
		s.retryLater(ictx, b, "PREPARE")
		return b, err
	}

	if !b.ProcessingDone && b.State == StateValidating {
		parsed, ferr := s.verifyFiles(ictx, p)
		switch {
		case ferr != nil:
			var fe *fileError
			if errors.As(ferr, &fe) {
				return s.stop(ictx, p, fe), nil
			}
			s.retryLater(ictx, b, "VERIFY")
			return b, ferr
		}
		if b, err = s.accept(ictx, p, parsed); err != nil {
			return nil, err
		}
		p.b = b
		return s.admit(ictx, p, parsed)
	}
	// resume: the files were verified in an earlier run; verify again (cheap) to rebuild the content and continue
	parsed, ferr := s.verifyFiles(ictx, p)
	if ferr != nil {
		var fe *fileError
		if errors.As(ferr, &fe) {
			return s.stop(ictx, p, fe), nil
		}
		s.retryLater(ictx, b, "VERIFY")
		return b, ferr
	}
	return s.admit(ictx, p, parsed)
}

func (s *Service) prepare(ctx context.Context, b *Batch) (*prepared, error) {
	conn, err := s.Conns.Connection(ctx, b.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	if conn.TenantID != b.TenantID {
		return nil, ErrNotFound
	}
	// a batch is processed under the contract version it ARRIVED under, never a newer one
	var v *registry.ContractVersion
	if s.In != nil && s.In.Versions != nil {
		if v, err = s.In.Versions.GetVersion(ctx, b.ContractVersionID); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
	} else if v, err = s.Conns.PinnedContract(ctx, conn); err != nil || v.ContractVersionID != b.ContractVersionID {
		return nil, fmt.Errorf("%w: the contract version the batch arrived under is not available", ErrDependency)
	}
	op := v.Operation(b.OperationID)
	if op == nil {
		return nil, fmt.Errorf("%w: the operation is not in the batch's contract version", ErrDependency)
	}
	bi := v.BatchFor(op)
	sch := v.SchemaByRef(op.RequestSchema)
	if bi == nil || sch == nil {
		return nil, fmt.Errorf("%w: the batch profile or schema is not available", ErrDependency)
	}
	files, err := s.Repo.Files(ctx, b.BatchID, true)
	if err != nil {
		return nil, err
	}
	p := &prepared{b: b, conn: conn, v: v, op: op, bi: bi, schema: sch, files: files}
	if info, ok := sch.CSVLayout(); ok {
		p.csv = info
	}
	return p, nil
}

type fileError struct {
	Reason string
	State  string // the batch state it leads to: REJECTED or QUARANTINED
	File   string
	Detail string
	Signal string // a security event type to record
}

func (e *fileError) Error() string { return e.Reason }

// verifyFiles checks every file's content before any row is admitted: expansion within limits, plain text that the
// content gate finds clean, the layout the schema declares, and a row count within the contract's limit.
func (s *Service) verifyFiles(ctx context.Context, p *prepared) ([]parsedFile, error) {
	var out []parsedFile
	total := 0
	for _, f := range p.files {
		if len(f.Content) == 0 && f.ActualSize > 0 {
			return nil, &fileError{Reason: ReasonFileMissing, State: StateRejected, File: f.Name, Detail: "the file content is no longer retained"}
		}
		data, err := s.expand(f, p.bi)
		if err != nil {
			return nil, &fileError{Reason: ReasonDecompression, State: StateQuarantined, File: f.Name, Detail: "the file expands beyond the allowed limits", Signal: "integration.security.batch-decompression-limit"}
		}
		if err := s.gate(ctx, data, p.schema.MediaType); err != nil {
			var fe *fileError
			if errors.As(err, &fe) {
				fe.File = f.Name
				return nil, fe
			}
			return nil, err
		}
		n, ferr := countRows(data, p)
		if ferr != nil {
			ferr.File = f.Name
			return nil, ferr
		}
		total += n
		if total > p.bi.MaxRows {
			return nil, &fileError{Reason: ReasonTooManyRows, State: StateRejected, File: f.Name, Detail: "the files hold more rows than the contract allows"}
		}
		f.ParsedRows = n
		out = append(out, parsedFile{file: f, data: data, rows: n})
	}
	return out, nil
}

// expand returns a file's content as the parser reads it: as delivered, or gunzipped within the contract's limits. A
// file that expands past the contract's ceiling, past the platform's, or past its ratio is not read further.
func (s *Service) expand(f File, bi *registry.BatchInfo) ([]byte, error) {
	if f.Compression != "gzip" {
		return f.Content, nil
	}
	cfg := s.cfg()
	limit := bi.MaxDecompressedBytes
	if limit <= 0 || limit > cfg.MaxDecompressedBytes {
		limit = cfg.MaxDecompressedBytes
	}
	if ratio := int64(bi.MaxExpansionRatio) * int64(len(f.Content)); ratio > 0 && ratio < limit {
		limit = ratio
	}
	zr, err := gzip.NewReader(bytes.NewReader(f.Content))
	if err != nil {
		return nil, errLimit
	}
	defer zr.Close()
	zr.Multistream(false)
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(zr, limit+1))
	if err != nil || n > limit {
		return nil, errLimit
	}
	return buf.Bytes(), nil
}

// gate runs the content gate over the bytes the parser will read: nothing that is not plain text of the declared
// kind is parsed, and a deployment with no scanner is not given a "clean" default.
func (s *Service) gate(ctx context.Context, data []byte, declared string) error {
	if s.Scanner == nil {
		return fmt.Errorf("%w: %v", ErrDependency, scan.ErrNoScanner)
	}
	v, err := s.Scanner.ScanOpen(ctx, func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, declared)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDependency, err)
	}
	if !v.Clean {
		return &fileError{Reason: ReasonContentUnsafe, State: StateQuarantined, Detail: "the content is not clean plain text", Signal: "integration.security.batch-content-unsafe"}
	}
	return nil
}

// ── parsing ─────────────────────────────────────────────────────────────────

func bomTrim(b []byte) []byte { return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) }

// eachRow parses a file's content and calls fn for every data row, in order, with its number. A structurally bad row
// is reported (not dropped): the row is still a row, with its own outcome.
func eachRow(data []byte, p *prepared, fn func(r rawRow) error) *fileError {
	data = bomTrim(data)
	if p.csv != nil {
		rd := csv.NewReader(bytes.NewReader(data))
		rd.Comma = p.csv.Delimiter
		rd.FieldsPerRecord = -1
		rd.ReuseRecord = false
		first := true
		no := 0
		bad := 0
		for {
			rec, err := rd.Read()
			if err == io.EOF {
				return nil
			}
			if first && p.csv.Header {
				first = false
				if err != nil || !sameStrings(rec, p.csv.Columns) {
					return &fileError{Reason: ReasonHeaderMismatch, State: StateRejected, Detail: "the header does not match the schema's columns"}
				}
				continue
			}
			first = false
			no++
			if err != nil {
				if bad++; bad > 1000 {
					return &fileError{Reason: ReasonRowStructure, State: StateRejected, Detail: "the file is not readable as csv"}
				}
				if ferr := fn(rawRow{no: no, hash: hashBytes([]byte(fmt.Sprint(no, err.Error()))), badness: ReasonRowStructure}); ferr != nil {
					return asFileError(ferr)
				}
				continue
			}
			if len(rec) == 1 && rec[0] == "" { // a blank line is not a row
				no--
				continue
			}
			if len(rec) != len(p.csv.Columns) {
				if ferr := fn(rawRow{no: no, hash: hashBytes([]byte(strings.Join(rec, "\x1f"))), badness: ReasonRowStructure}); ferr != nil {
					return asFileError(ferr)
				}
				continue
			}
			obj := make(map[string]string, len(rec))
			for i, c := range p.csv.Columns {
				obj[c] = rec[i]
			}
			body, _ := json.Marshal(obj)
			if ferr := fn(rawRow{no: no, hash: hashBytes([]byte(strings.Join(rec, "\x1f"))), body: body}); ferr != nil {
				return asFileError(ferr)
			}
		}
	}
	// application/x-ndjson: one JSON object per line
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	no := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		no++
		r := rawRow{no: no, hash: hashBytes(line)}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil || obj == nil {
			r.badness = ReasonRowNotJSON
		} else {
			r.body = append([]byte(nil), line...)
		}
		if ferr := fn(r); ferr != nil {
			return asFileError(ferr)
		}
	}
	if err := sc.Err(); err != nil {
		return &fileError{Reason: ReasonRowStructure, State: StateRejected, Detail: "a line is longer than a row may be"}
	}
	return nil
}

func asFileError(err error) *fileError {
	var fe *fileError
	if errors.As(err, &fe) {
		return fe
	}
	return &fileError{Reason: ReasonRowStructure, State: StateRejected, Detail: err.Error()}
}

func countRows(data []byte, p *prepared) (int, *fileError) {
	n := 0
	err := eachRow(data, p, func(rawRow) error { n++; return nil })
	return n, err
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimPrefix(a[i], "\xef\xbb\xbf") != b[i] {
			return false
		}
	}
	return true
}

// ── decisions ───────────────────────────────────────────────────────────────

// stop ends a batch at the file level: REJECTED, or QUARANTINED when the content is a security matter. Its file
// bodies are dropped at once, and nothing of it was admitted.
func (s *Service) stop(ctx context.Context, p *prepared, fe *fileError) *Batch {
	now := s.now()
	for i := range p.files {
		f := p.files[i]
		f.State = FileRejected
		if f.Name == fe.File {
			f.ReasonCode = fe.Reason
		}
		f.Content, f.ContentRetained = nil, false
		_ = s.Repo.UpdateFile(ctx, f)
	}
	b, err := s.Repo.Mutate(ctx, p.b.BatchID, func(tx *Tx) error {
		from := tx.B.State
		tx.B.State, tx.B.ReasonCode, tx.B.LeaseUntil, tx.B.NextProcessAt = fe.State, fe.Reason, nil, nil
		tx.B.XICCode = "XIC-013"
		tx.Event = &Event{Kind: strings.ToUpper(fe.State), From: from, To: fe.State, Actor: "system:xic-batch", At: now, Detail: mustJSON(map[string]any{"reason": fe.Reason, "file": fe.File})}
		typ := "integration.batch.rejected"
		tx.Out = append(tx.Out, OutEvent{Type: typ, AggregateType: "batch", AggregateID: tx.B.BatchID, ActorID: "system:xic-batch", CorrelationID: tx.B.CorrelationID,
			Data: map[string]any{"batch_id": tx.B.BatchID, "tenant_id": tx.B.TenantID, "connection_id": tx.B.ConnectionID, "reason_code": fe.Reason, "xic_code": "XIC-013"}})
		if fe.Signal != "" {
			tx.Out = append(tx.Out, OutEvent{Type: fe.Signal, AggregateType: "batch", AggregateID: tx.B.BatchID, ActorID: "system:xic-batch", CorrelationID: tx.B.CorrelationID,
				Data: map[string]any{"batch_id": tx.B.BatchID, "tenant_id": tx.B.TenantID, "connection_id": tx.B.ConnectionID, "reason_code": fe.Reason}})
		}
		return nil
	})
	if err != nil {
		return p.b
	}
	s.observe("batch_"+strings.ToLower(fe.State), fe.Reason)
	return b
}

// accept records the file-level decision: the envelope is valid. It says nothing about any row (§10.2).
func (s *Service) accept(ctx context.Context, p *prepared, parsed []parsedFile) (*Batch, error) {
	now := s.now()
	total := 0
	for _, pf := range parsed {
		total += pf.rows
		f := pf.file
		f.State = FileVerified
		_ = s.Repo.UpdateFile(ctx, f)
	}
	return s.Repo.Mutate(ctx, p.b.BatchID, func(tx *Tx) error {
		from := tx.B.State
		tx.B.State, tx.B.ParsedRows = StateAccepted, total
		tx.B.MappingVersionID = mappingVersionOf(p)
		if tx.B.DeclaredRows != nil {
			tx.B.VarianceRows = *tx.B.DeclaredRows - total
		}
		detail := map[string]any{"parsed_rows": total, "files": len(parsed), "variance_rows": tx.B.VarianceRows}
		tx.Event = &Event{Kind: "ACCEPTED", From: from, To: StateAccepted, Actor: "system:xic-batch", At: now, Detail: mustJSON(detail)}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.batch.accepted", AggregateType: "batch", AggregateID: tx.B.BatchID, ActorID: "system:xic-batch", CorrelationID: tx.B.CorrelationID,
			Data: map[string]any{"batch_id": tx.B.BatchID, "tenant_id": tx.B.TenantID, "connection_id": tx.B.ConnectionID, "parsed_rows": total, "declared_rows": tx.B.DeclaredRows,
				"variance_rows": tx.B.VarianceRows, "mapping_version_id": tx.B.MappingVersionID}})
		return nil
	})
}

func mappingVersionOf(p *prepared) string {
	if mv := p.v.MappingFor(p.op.RequestSchema); mv != nil {
		return mv.MappingVersionID
	}
	return ""
}

// admit runs every row through the pipeline, saving progress as it goes, then reports on the batch.
func (s *Service) admit(ctx context.Context, p *prepared, parsed []parsedFile) (*Batch, error) {
	cfg := s.cfg()
	admitted := 0
	for _, pf := range parsed {
		done, err := s.Repo.RowCount(ctx, p.b.BatchID, pf.file.FileID)
		if err != nil {
			s.retryLater(ctx, p.b, "ROWS")
			return p.b, err
		}
		admitted += done
		var chunk []Row
		flush := func() error {
			if len(chunk) == 0 {
				return nil
			}
			if err := s.Repo.AddRows(ctx, chunk); err != nil {
				return err
			}
			admitted += len(chunk)
			chunk = chunk[:0]
			lease := s.now().Add(cfg.ProcessLease)
			_, err := s.Repo.Mutate(ctx, p.b.BatchID, func(tx *Tx) error {
				tx.B.AdmittedRows, tx.B.LeaseUntil = admitted, &lease
				return nil
			})
			return err
		}
		var rerr error
		ferr := eachRow(pf.data, p, func(r rawRow) error {
			if r.no <= done {
				return nil // admitted by an earlier run
			}
			row := Row{RowID: uuid.NewString(), BatchID: p.b.BatchID, FileID: pf.file.FileID, TenantID: p.b.TenantID, RowNo: r.no, SourceRowHash: r.hash, UpdatedAt: s.now()}
			if r.body == nil {
				row.State, row.ReasonCode = RowRejected, r.badness
			} else {
				res, err := s.In.AdmitRow(ctx, inbound.RowInput{Conn: p.conn, V: p.v, Op: p.op, Body: r.body, SourceRef: p.op.RequestSchema, CorrelationID: p.b.CorrelationID, RowKey: fmt.Sprintf("%s:%d", pf.file.FileID, r.no)})
				if err != nil {
					rerr = err
					return err
				}
				row.ExchangeID, row.ReasonCode = res.ExchangeID, res.Reason
				switch res.State {
				case inbound.StateValidated:
					row.State, row.ReasonCode = RowPending, ""
				case inbound.StateQuarantined:
					row.State = RowQuarantined
				case inbound.StateDuplicate:
					// a row whose exchange committed before the row itself was saved meets its own exchange on resume: that
					// is not a duplicate in the file, so it is picked up as pending and the refresh settles it
					if res.Own {
						row.State, row.ReasonCode = RowPending, ""
					} else {
						row.State = RowDuplicate
					}
				default:
					row.State = RowRejected
				}
			}
			chunk = append(chunk, row)
			if len(chunk) >= cfg.RowChunk {
				return flush()
			}
			return nil
		})
		if rerr != nil || ferr != nil {
			_ = flush()
			s.retryLater(ctx, p.b, "ADMIT")
			if rerr != nil {
				return p.b, rerr
			}
			return p.b, ferr
		}
		if err := flush(); err != nil {
			s.retryLater(ctx, p.b, "ROWS")
			return p.b, err
		}
	}
	now := s.now()
	b, err := s.Repo.Mutate(ctx, p.b.BatchID, func(tx *Tx) error {
		from := tx.B.State
		tx.B.State, tx.B.ProcessingDone, tx.B.AdmittedRows, tx.B.LeaseUntil = StateProcessing, true, tx.B.ParsedRows, nil
		t := now
		tx.B.NextProcessAt = &t
		tx.Event = &Event{Kind: "ROWS_ADMITTED", From: from, To: StateProcessing, Actor: "system:xic-batch", At: now, Detail: mustJSON(map[string]any{"rows": tx.B.ParsedRows})}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// the content was needed only to be read: a batch whose data classes forbid keeping it loses it as soon as it is
	// processed
	if denied(p.op.DataClasses, cfg.ContentDenyClasses) {
		for _, pf := range parsed {
			f := pf.file
			f.Content, f.ContentRetained = nil, false
			_ = s.Repo.UpdateFile(ctx, f)
		}
	}
	return s.Refresh(ctx, p.b.TenantID, b.BatchID)
}

// retryLater schedules another run for a batch whose processing could not complete (a dependency was down). It is
// never failed to clear a queue.
func (s *Service) retryLater(ctx context.Context, b *Batch, why string) {
	next := s.now().Add(s.cfg().ProcessBackoff)
	_, _ = s.Repo.Mutate(ctx, b.BatchID, func(tx *Tx) error {
		tx.B.LeaseUntil, tx.B.NextProcessAt = nil, &next
		tx.Event = &Event{Kind: "PROCESSING_DEFERRED", From: tx.B.State, To: tx.B.State, Actor: "system:xic-batch", At: s.now(), Detail: mustJSON(map[string]any{"at": why})}
		return nil
	})
}

// ── reporting on rows ───────────────────────────────────────────────────────

var refreshStates = []string{RowPending, RowQuarantined, RowDomainRejected}

// Refresh recomputes what became of the rows of an admitted batch - from the exchanges they became - and reports the
// batch: PROCESSING while any row awaits its domain, PARTIAL when some rows were not accepted or the manifest's count
// does not match, RECONCILED when every row is accounted for and the counts match. "File accepted" never makes a
// batch RECONCILED, and a batch is never "successful" while a row was rejected (§10.2, NP-36).
func (s *Service) Refresh(ctx context.Context, tenant, id string) (*Batch, error) {
	ictx := withTenant(ctx, tenant)
	b, err := s.Repo.Get(ictx, id)
	if err != nil {
		return nil, err
	}
	if !b.ProcessingDone || (b.State != StateProcessing && b.State != StatePartial && b.State != StateReconciled) {
		return b, nil
	}
	after := 0
	for {
		rows, err := s.Repo.Rows(ictx, id, RowFilter{States: refreshStates, After: after, Limit: 500})
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			if r.ExchangeID != "" {
				ids = append(ids, r.ExchangeID)
			}
			after = r.RowNo
		}
		sums, err := s.In.Repo.Summaries(ictx, ids)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		var changed []Row
		for _, r := range rows {
			sm, ok := sums[r.ExchangeID]
			if !ok {
				continue
			}
			ns, reason := rowOutcome(sm)
			if ns != r.State || reason != r.ReasonCode {
				r.State, r.ReasonCode, r.UpdatedAt = ns, reason, s.now()
				changed = append(changed, r)
			}
		}
		if len(changed) > 0 {
			if err := s.Repo.UpdateRows(ictx, changed); err != nil {
				return nil, err
			}
		}
		if len(rows) < 500 {
			break
		}
	}
	counts, err := s.Repo.RowStateCounts(ictx, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	cfg := s.cfg()
	return s.Repo.Mutate(ictx, id, func(tx *Tx) error {
		from := tx.B.State
		tx.B.AcceptedRows, tx.B.PendingRows, tx.B.RejectedRows = counts[RowAccepted], counts[RowPending], counts[RowRejected]
		tx.B.DuplicateRows, tx.B.QuarantinedRows, tx.B.DomainRejected = counts[RowDuplicate], counts[RowQuarantined], counts[RowDomainRejected]
		bad := tx.B.RejectedRows + tx.B.QuarantinedRows + tx.B.DomainRejected
		next := StateReconciled
		switch {
		case tx.B.PendingRows > 0:
			next = StateProcessing
		case bad > 0 || tx.B.VarianceRows != 0:
			next = StatePartial
		}
		tx.B.State = next
		switch next {
		case StateProcessing:
			t := now.Add(cfg.RefreshPending)
			tx.B.NextProcessAt = &t
		case StatePartial:
			t := now.Add(cfg.RefreshPartial)
			tx.B.NextProcessAt = &t
		default:
			tx.B.NextProcessAt = nil
		}
		if next != from {
			tx.Event = &Event{Kind: next, From: from, To: next, Actor: "system:xic-batch", At: now, Detail: mustJSON(s.countsDetail(tx.B))}
			switch next {
			case StatePartial:
				tx.Out = append(tx.Out, OutEvent{Type: "integration.batch.partial", AggregateType: "batch", AggregateID: tx.B.BatchID, ActorID: "system:xic-batch", CorrelationID: tx.B.CorrelationID,
					Data: map[string]any{"batch_id": tx.B.BatchID, "tenant_id": tx.B.TenantID, "connection_id": tx.B.ConnectionID, "accepted_count": tx.B.AcceptedRows,
						"rejected_count": tx.B.RejectedRows + tx.B.DomainRejected, "duplicate_count": tx.B.DuplicateRows, "unresolved_count": tx.B.PendingRows + tx.B.QuarantinedRows,
						"variance_rows": tx.B.VarianceRows}})
			case StateReconciled:
				tx.Out = append(tx.Out, OutEvent{Type: "integration.batch.reconciled", AggregateType: "batch", AggregateID: tx.B.BatchID, ActorID: "system:xic-batch", CorrelationID: tx.B.CorrelationID,
					Data: map[string]any{"batch_id": tx.B.BatchID, "tenant_id": tx.B.TenantID, "connection_id": tx.B.ConnectionID, "accepted_count": tx.B.AcceptedRows, "duplicate_count": tx.B.DuplicateRows}})
			}
		}
		return nil
	})
}

func (s *Service) countsDetail(b *Batch) map[string]any {
	return map[string]any{"accepted": b.AcceptedRows, "pending": b.PendingRows, "rejected": b.RejectedRows, "duplicate": b.DuplicateRows,
		"quarantined": b.QuarantinedRows, "domain_rejected": b.DomainRejected, "variance_rows": b.VarianceRows, "parsed": b.ParsedRows}
}

// rowOutcome maps what became of a row's exchange onto the row's own outcome.
func rowOutcome(sm inbound.ExchangeSummary) (string, string) {
	switch sm.State {
	case inbound.StateHandedOff:
		if sm.DomainOutcome == inbound.DomainAccepted {
			return RowAccepted, ""
		}
		return RowPending, ""
	case inbound.StateValidated:
		return RowPending, ""
	case inbound.StateQuarantined:
		if strings.HasPrefix(sm.ReasonCode, "DOMAIN_REJECTED") {
			return RowDomainRejected, sm.ReasonCode
		}
		return RowQuarantined, sm.ReasonCode
	}
	return RowRejected, sm.ReasonCode
}

// ── the worker ──────────────────────────────────────────────────────────────

// DriveDue processes every batch the store says is due. It returns how many it worked on; one batch failing does not
// stop the others.
func (s *Service) DriveDue(ctx context.Context, limit int) (int, error) {
	due, err := s.Repo.DueForProcessing(ctx, s.now(), limit)
	if err != nil {
		return 0, err
	}
	var first error
	for _, d := range due {
		if _, err := s.Process(ctx, d.TenantID, d.ID); err != nil && first == nil && !errors.Is(err, ErrDependency) {
			first = err
		}
	}
	return len(due), first
}

// RunWorker drives due batches and replays and purges expired file bodies every interval, until ctx ends.
func (s *Service) RunWorker(ctx context.Context, interval time.Duration, batch int, report func(n int, err error)) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.DriveDue(ctx, batch)
			if m, rerr := s.DriveReplays(ctx, batch); rerr != nil && err == nil {
				err = rerr
			} else {
				n += m
			}
			if _, perr := s.Repo.PurgeContent(ctx, s.now(), batch); perr != nil && err == nil {
				err = perr
			}
			if report != nil {
				report(n, err)
			}
		}
	}
}

var _ = scan.ErrNoScanner
