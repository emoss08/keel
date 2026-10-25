package keel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
)

// Shape is one distinct statement text keel sent: the text (which never contains values), its
// operation and table as QueryEvent reports them, and the number of parameters it binds.
// RecordShapes writes shapes as JSON lines, {"sql": "...", "op": "...", "table": "...", "args": n},
// and keel plans reads them back to explain every one.
type Shape struct {
	SQL   string `json:"sql"`
	Op    string `json:"op"`
	Table string `json:"table"`
	Args  int    `json:"args"`
}

// RecordShapes returns an observer that writes each distinct statement text it sees to w once, as a
// JSON line of a Shape. Only statements EXPLAIN can plan are written (those that start with SELECT,
// WITH, INSERT, UPDATE, DELETE, MERGE, VALUES or TABLE, and not keel's advisory locks), so
// savepoints, COPY, DDL run through Exec and Explain's own statements are left out. It is safe
// for concurrent use: a statement already seen costs a read-locked map lookup, and each line goes
// to w in one Write under a lock, so lines never interleave. It keeps every distinct text in
// memory, which suits a test run or a staging soak, the runs a plan guard records; write errors
// are dropped, and RecordShapesFile reports them.
func RecordShapes(w io.Writer) Observer { return newShapeRecorder(w) }

// RecordShapesFile returns an observer that appends the shapes it sees to the file at path,
// creating it, as RecordShapes writes them, and a function that closes the file and returns the
// first write error. The file is opened for appending, so the test binaries of one go test ./...
// can share it: each line is one write, and keel plans reads each distinct text once.
func RecordShapesFile(path string) (Observer, func() error, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, errf("record shapes: %w", err)
	}
	r := newShapeRecorder(f)
	closeFn := func() error {
		r.mu.Lock()
		werr := r.err
		r.mu.Unlock()
		return errors.Join(werr, f.Close())
	}
	return r, closeFn, nil
}

// ReadShapes reads the JSON lines RecordShapes wrote and returns each distinct statement text
// once, in the order first seen, so a file several processes appended to reads as one set.
func ReadShapes(r io.Reader) ([]Shape, error) {
	dec := json.NewDecoder(r)
	seen := map[string]bool{}
	var out []Shape
	for n := 1; ; n++ {
		var s Shape
		err := dec.Decode(&s)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, errf("read shapes: shape %d: %w", n, err)
		}
		if strings.TrimSpace(s.SQL) == "" {
			return nil, errf("read shapes: shape %d has no sql", n)
		}
		if seen[s.SQL] {
			continue
		}
		seen[s.SQL] = true
		out = append(out, s)
	}
}

// shapeRecorder is the observer RecordShapes returns. seen holds every statement text it has
// looked at, plannable or not, so each text is classified and written once.
type shapeRecorder struct {
	mu   sync.RWMutex
	seen map[string]struct{}
	w    io.Writer
	buf  bytes.Buffer
	err  error
}

// newShapeRecorder returns a recorder writing to w.
func newShapeRecorder(w io.Writer) *shapeRecorder {
	return &shapeRecorder{seen: map[string]struct{}{}, w: w}
}

// BeforeQuery records the statement's shape the first time its text is seen.
func (r *shapeRecorder) BeforeQuery(ctx context.Context, e *QueryEvent) context.Context {
	r.record(e)
	return ctx
}

// AfterQuery does nothing: the shape is recorded before the statement runs.
func (r *shapeRecorder) AfterQuery(context.Context, *QueryEvent) {}

// record writes e's shape unless its text was seen before or EXPLAIN cannot plan it.
func (r *shapeRecorder) record(e *QueryEvent) {
	r.mu.RLock()
	_, ok := r.seen[e.SQL]
	r.mu.RUnlock()
	if ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[e.SQL]; ok {
		return
	}
	r.seen[e.SQL] = struct{}{}
	if e.Op == "lock" || !plannable(e.SQL) {
		return
	}
	r.buf.Reset()
	enc := json.NewEncoder(&r.buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Shape{SQL: e.SQL, Op: e.Op, Table: e.Table, Args: e.Args}); err != nil {
		r.fail(err)
		return
	}
	if _, err := r.w.Write(r.buf.Bytes()); err != nil {
		r.fail(err)
	}
}

// fail keeps the first write error; the caller holds mu.
func (r *shapeRecorder) fail(err error) {
	if r.err == nil {
		r.err = errf("record shapes: %w", err)
	}
}

// plannable reports whether EXPLAIN can plan sql: its first keyword, after whitespace, opening
// parentheses and comments, starts a query or a data-changing statement.
func plannable(sql string) bool {
	switch strings.ToUpper(firstWord(sql)) {
	case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "MERGE", "VALUES", "TABLE":
		return true
	}
	return false
}

// firstWord returns the first run of letters in sql after whitespace, opening parentheses, line
// comments and block comments, or "" when there is none.
func firstWord(sql string) string {
	s := sql
	for {
		s = strings.TrimLeft(s, " \t\r\n(")
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return ""
			}
			s = s[i+2:]
		default:
			end := strings.IndexFunc(s, func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') })
			if end < 0 {
				return s
			}
			return s[:end]
		}
	}
}
