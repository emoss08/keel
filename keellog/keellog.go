// Package keellog prints the statements keel sends, for a developer reading a terminal: one header
// line per statement (operation, table, transaction, duration, rows or outcome, and the code that
// issued it), then the SQL broken at its clauses, with the bound values written in when
// keel.Config.ObserveArgs is set.
//
//	db, err := keel.Open(ctx, keel.Config{
//		DSN:         dsn,
//		ObserveArgs: true,
//		Observers:   []keel.Observer{keellog.New(keellog.Options{Mode: keellog.All})},
//	})
//
// It is a development tool: production logging wants structured fields, which an Observer of its
// own writes to the application's logger.
package keellog

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emoss08/keel"
)

// Mode chooses which statements are printed.
type Mode uint8

const (
	// Errors prints the statements that failed and the slow ones.
	Errors Mode = iota
	// All prints every statement.
	All
)

// ParseMode reads a Mode from its name: "errors" or "all".
func ParseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "errors":
		return Errors, true
	case "all":
		return All, true
	}
	return 0, false
}

// Options configures a Logger.
type Options struct {
	// Out receives the output. Defaults to os.Stderr.
	Out io.Writer
	// Mode chooses which statements are printed. Defaults to Errors.
	Mode Mode
	// Slow is the duration from which a statement is marked slow, and printed in Errors mode.
	// Defaults to 100ms.
	Slow time.Duration
	// Color writes ANSI colors. Defaults to on when Out is a terminal and NO_COLOR is unset.
	Color *bool
	// Skip lists function name prefixes (package paths) that never count as a statement's caller,
	// besides keel's own: a repository helper every query passes through, for one.
	Skip []string
	// MaxValue is how many characters of a bound string are printed, and MaxElems how many
	// elements of a bound slice. Default to 64 and 8.
	MaxValue int
	MaxElems int
	// MaxSQL is how many characters of a statement's SQL are printed. Defaults to 4000.
	MaxSQL int
}

// Logger is a keel.Observer that prints statements.
type Logger struct {
	out      io.Writer
	mode     Mode
	slow     time.Duration
	color    bool
	skip     []string
	maxValue int
	maxElems int
	maxSQL   int

	mu sync.Mutex
}

var _ keel.Observer = (*Logger)(nil)

// New returns a Logger.
func New(o Options) *Logger {
	l := &Logger{
		out:      o.Out,
		mode:     o.Mode,
		slow:     o.Slow,
		skip:     o.Skip,
		maxValue: o.MaxValue,
		maxElems: o.MaxElems,
		maxSQL:   o.MaxSQL,
	}
	if l.out == nil {
		l.out = os.Stderr
	}
	if l.slow <= 0 {
		l.slow = 100 * time.Millisecond
	}
	if l.maxValue <= 0 {
		l.maxValue = 64
	}
	if l.maxElems <= 0 {
		l.maxElems = 8
	}
	if l.maxSQL <= 0 {
		l.maxSQL = 4000
	}
	if o.Color != nil {
		l.color = *o.Color
	} else {
		l.color = isTerminal(l.out) && os.Getenv("NO_COLOR") == ""
	}
	return l
}

// BeforeQuery implements keel.Observer.
func (l *Logger) BeforeQuery(ctx context.Context, _ *keel.QueryEvent) context.Context {
	return ctx
}

// AfterQuery implements keel.Observer.
func (l *Logger) AfterQuery(_ context.Context, e *keel.QueryEvent) {
	notFound := errors.Is(e.Err, keel.ErrNotFound)
	failed := e.Err != nil && !notFound
	slow := e.Duration >= l.slow
	if l.mode == Errors && !failed && !slow {
		return
	}
	var b strings.Builder
	l.header(&b, e, failed, notFound, slow)
	if l.mode == All || failed || slow {
		l.body(&b, e)
	}
	if failed {
		b.WriteString("     ")
		b.WriteString(l.paint(red, describe(e.Err)))
		b.WriteByte('\n')
	}
	l.mu.Lock()
	_, _ = io.WriteString(l.out, b.String())
	l.mu.Unlock()
}

func (l *Logger) header(b *strings.Builder, e *keel.QueryEvent, failed, notFound, slow bool) {
	b.WriteString(l.paint(dim, "keel "))
	op := strings.ToUpper(e.Op)
	if op == "" {
		op = "EXEC"
	}
	b.WriteString(l.paint(opColor(op), pad(op, 6)))
	b.WriteByte(' ')
	b.WriteString(pad(e.Table, 22))
	tx := "  "
	if e.Tx {
		tx = "tx"
	}
	b.WriteString(l.paint(dim, tx))
	b.WriteByte(' ')
	d := padLeft(duration(e.Duration), 9)
	if slow {
		d = l.paint(yellow, d)
	}
	b.WriteString(d)
	b.WriteString("  ")
	var outcome string
	switch {
	case failed:
		outcome = l.paint(red, pad("ERROR", 10))
	case notFound:
		outcome = l.paint(dim, pad("not found", 10))
	default:
		outcome = pad(rows(e.Rows), 10)
	}
	b.WriteString(outcome)
	if c := l.caller(); c != "" {
		b.WriteString("  ")
		b.WriteString(l.paint(dim, c))
	}
	if e.Replica != "" {
		b.WriteString(l.paint(dim, "  replica "+e.Replica))
	}
	b.WriteByte('\n')
}

func (l *Logger) body(b *strings.Builder, e *keel.QueryEvent) {
	sql := e.SQL
	if e.Values != nil {
		sql = Inline(sql, e.Values, l.maxValue, l.maxElems)
	}
	sql = Format(sql)
	if len(sql) > l.maxSQL {
		cut := l.maxSQL
		for cut > 0 && !utf8Start(sql[cut]) {
			cut--
		}
		sql = sql[:cut] + "… (" + strconv.Itoa(len(sql)-cut) + " more bytes)"
	}
	for line := range strings.SplitSeq(sql, "\n") {
		b.WriteString("     ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// caller returns the file and line of the first frame outside keel and the skipped packages.
func (l *Logger) caller() string {
	var pcs [48]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function != "" && !l.skipped(f.Function) {
			return shortFile(f.File) + ":" + strconv.Itoa(f.Line)
		}
		if !more {
			return ""
		}
	}
}

func (l *Logger) skipped(fn string) bool {
	if internal(fn) {
		return true
	}
	for _, p := range l.skip {
		if strings.HasPrefix(fn, p) {
			return true
		}
	}
	return false
}

// internal reports whether fn belongs to keel or the runtime; keel's own tests are callers.
func internal(fn string) bool {
	if strings.HasPrefix(fn, "runtime.") {
		return true
	}
	if !strings.HasPrefix(fn, "github.com/emoss08/keel.") && !strings.HasPrefix(fn, "github.com/emoss08/keel/") {
		return false
	}
	pkg := fn
	if slash := strings.LastIndexByte(fn, '/'); slash >= 0 {
		if dot := strings.IndexByte(fn[slash:], '.'); dot >= 0 {
			pkg = fn[:slash+dot]
		}
	}
	return !strings.HasSuffix(pkg, "_test")
}

func shortFile(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return path
	}
	if j := strings.LastIndexByte(path[:i], '/'); j >= 0 {
		return path[j+1:]
	}
	return path
}

func describe(err error) string {
	if ce, ok := errors.AsType[*keel.ConstraintError](err); ok {
		var b strings.Builder
		b.WriteString(ce.Kind.String())
		b.WriteString(" violation")
		if ce.Constraint != "" {
			b.WriteByte(' ')
			b.WriteString(ce.Constraint)
		}
		if len(ce.Columns) > 0 {
			b.WriteString(" → ")
			b.WriteString(strings.Join(ce.Columns, ", "))
		}
		if pe := keel.Code(err); pe != "" {
			return pe + " " + b.String()
		}
		return b.String()
	}
	msg := err.Error()
	if code := keel.Code(err); code != "" && !strings.Contains(msg, code) {
		return code + " " + msg
	}
	return msg
}

func duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return strconv.FormatFloat(float64(d)/float64(time.Microsecond), 'f', 0, 64) + "µs"
	case d < time.Second:
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64) + "ms"
	default:
		return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + "s"
	}
}

func rows(n int64) string {
	if n == 1 {
		return "1 row"
	}
	return group(n) + " rows"
}

func group(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 || n < 0 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func pad(s string, n int) string {
	if w := len([]rune(s)); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func padLeft(s string, n int) string {
	if w := len([]rune(s)); w < n {
		return strings.Repeat(" ", n-w) + s
	}
	return s
}

func utf8Start(c byte) bool {
	return c&0xC0 != 0x80
}

const (
	dim    = "2"
	red    = "31"
	green  = "32"
	yellow = "33"
	blue   = "34"
	purple = "35"
	cyan   = "36"
)

func opColor(op string) string {
	switch op {
	case "SELECT", "COUNT", "QUERY":
		return green
	case "INSERT", "COPY":
		return blue
	case "UPDATE":
		return yellow
	case "DELETE":
		return red
	case "LINKS":
		return cyan
	}
	return purple
}

func (l *Logger) paint(code, s string) string {
	if !l.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
