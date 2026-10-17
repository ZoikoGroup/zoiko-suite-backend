package imports

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrSchemaMismatch means the file's structure does not match its profile: a missing,
// unknown or repeated column. It stops the batch before preview.
var ErrSchemaMismatch = errors.New("the file does not match the profile's schema")

// Row is one parsed record, before validation. A line that could not be parsed at all comes
// back with Errs set and no Raw, so it is counted and reported rather than dropped.
type Row struct {
	No   int
	Raw  map[string]any
	Errs []RowError
}

// RowReader streams rows. Next returns io.EOF after the last one.
type RowReader interface {
	Next() (Row, error)
}

// maxJSONLLine bounds one JSON-lines record.
const maxJSONLLine = 1 << 20

// NewRowReader opens the content for the profile's format. For CSV the header is read and
// checked against the profile first, so a structural mismatch is reported before any row.
func NewRowReader(spec Spec, r io.Reader) (RowReader, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	if b, err := br.Peek(3); err == nil && bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3) // a UTF-8 byte order mark is not part of the first column name
	}
	switch spec.Format {
	case "jsonl":
		sc := bufio.NewScanner(br)
		sc.Buffer(make([]byte, 0, 64*1024), maxJSONLLine)
		return &jsonlReader{sc: sc}, nil
	case "csv":
		cr := csv.NewReader(br)
		cr.Comma = rune(spec.Delimiter[0])
		cr.FieldsPerRecord = -1
		cr.LazyQuotes = false
		header, err := cr.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: the file has no header", ErrSchemaMismatch)
			}
			return nil, fmt.Errorf("%w: the header cannot be read", ErrSchemaMismatch)
		}
		if err := checkHeader(spec, header); err != nil {
			return nil, err
		}
		return &csvReader{cr: cr, header: header}, nil
	}
	return nil, fmt.Errorf("%w: unsupported format", ErrValidation)
}

func checkHeader(spec Spec, header []string) error {
	seen := map[string]bool{}
	for _, h := range header {
		h = strings.TrimSpace(h)
		if _, ok := spec.column(h); !ok {
			return fmt.Errorf("%w: unexpected column %q", ErrSchemaMismatch, safeName(h))
		}
		if seen[h] {
			return fmt.Errorf("%w: column %q is repeated", ErrSchemaMismatch, safeName(h))
		}
		seen[h] = true
	}
	for _, c := range spec.Columns {
		if c.Required && !seen[c.Name] {
			return fmt.Errorf("%w: required column %q is missing", ErrSchemaMismatch, c.Name)
		}
	}
	return nil
}

// safeName bounds a header cell echoed in an error (it is file content).
func safeName(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

type csvReader struct {
	cr     *csv.Reader
	header []string
	n      int
}

func (c *csvReader) Next() (Row, error) {
	rec, err := c.cr.Read()
	if errors.Is(err, io.EOF) {
		return Row{}, io.EOF
	}
	c.n++
	if err != nil {
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			return Row{No: c.n, Errs: []RowError{{Code: CodeMalformedLine}}}, nil
		}
		return Row{}, err
	}
	if len(rec) != len(c.header) {
		return Row{No: c.n, Errs: []RowError{{Code: CodeFieldCount}}}, nil
	}
	raw := make(map[string]any, len(rec))
	for i, h := range c.header {
		raw[strings.TrimSpace(h)] = rec[i]
	}
	return Row{No: c.n, Raw: raw}, nil
}

type jsonlReader struct {
	sc *bufio.Scanner
	n  int
}

func (j *jsonlReader) Next() (Row, error) {
	for j.sc.Scan() {
		line := bytes.TrimSpace(j.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		j.n++
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil || obj == nil || dec.More() {
			return Row{No: j.n, Errs: []RowError{{Code: CodeMalformedLine}}}, nil
		}
		return Row{No: j.n, Raw: obj}, nil
	}
	if err := j.sc.Err(); err != nil {
		return Row{}, fmt.Errorf("%w: a line is too long", ErrSchemaMismatch)
	}
	return Row{}, io.EOF
}
