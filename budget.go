package keel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ErrBudgetExceeded is matched by a *BudgetError: work under a Budget went past one of its limits.
var ErrBudgetExceeded = errors.New("keel: budget exceeded")

// BudgetError is the error of the terminal whose round trip took a Budget past a limit, returned
// after its statements ran, and of every later round trip under that budget, refused before it is
// sent. A write outside a transaction has committed when it returns a BudgetError.
type BudgetError struct {
	// Limit names the limit crossed: "round trips", "statements" or "repeats".
	Limit string
	// Max is the limit's value.
	Max int
	// Report is the budget's account when the error was made.
	Report BudgetReport
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("keel: budget exceeded: more than %d %s (%d round trips, %d statements)",
		e.Max, e.Limit, e.Report.RoundTrips, e.Report.Statements)
}

// Is makes errors.Is(err, ErrBudgetExceeded) true.
func (e *BudgetError) Is(target error) bool { return target == ErrBudgetExceeded }

// BudgetReport is the account of the work under a Budget.
type BudgetReport struct {
	// RoundTrips counts the round trips that carried keel's statements, with the BEGIN keel sends
	// on its own before a COPY; the COMMIT or ROLLBACK that ends a transaction is not counted.
	RoundTrips int
	// Statements counts the statements sent: queries, relation loads, writes, COPY, savepoints,
	// advisory locks and raw SQL, not BEGIN, the Config.OnBegin statements or COMMIT.
	Statements int
	// Repeats lists the statement texts sent in more than one round trip, the most repeated first.
	Repeats []Repeat
}

// Repeat is a statement text sent in more than one round trip under a Budget: the shape of an N+1
// read when the count follows the size of a result.
type Repeat struct {
	SQL   string
	Count int
	// CallSites are the distinct places (file:line) outside keel and the generated packages that
	// sent the text again after its first round trip, at most maxCallSites of them. The first run
	// is not captured: keel records a stack only once a text repeats, so a budget costs nothing
	// for statements that do not.
	CallSites []string
}

// BudgetOption configures a Budget.
type BudgetOption func(b *budget)

// MaxRoundTrips limits the budget to n round trips.
func MaxRoundTrips(n int) BudgetOption { return func(b *budget) { b.maxTrips = n } }

// MaxStatements limits the budget to n statements.
func MaxStatements(n int) BudgetOption { return func(b *budget) { b.maxStmts = n } }

// MaxRepeats limits how many round trips one statement text may be sent in to n: a text sent in
// more than n crosses it. MaxRepeats(1) allows no repeated text at all.
func MaxRepeats(n int) BudgetOption { return func(b *budget) { b.maxRepeats = n } }

// OnExceeded makes crossing a limit call fn, once per budget, with the context of the terminal
// that crossed it and the account at that moment, instead of failing the terminal: for logging
// N+1 reads in production while the work goes on.
func OnExceeded(fn func(ctx context.Context, r BudgetReport)) BudgetOption {
	return func(b *budget) { b.onExceeded = fn }
}

// Budget returns a context that counts the round trips, statements and repeated statement texts
// of the work keel does under it, every terminal, transaction and goroutine included, and limits
// them as opts say. A budget under another one counts against both. Without limits it only counts,
// for BudgetReportFrom. A statement text counts once per round trip it is sent in, so the chunks
// of one CreateMany, which share a text and a round trip, are not repeats. It is safe for
// concurrent use.
func Budget(ctx context.Context, opts ...BudgetOption) context.Context {
	b := &budget{maxTrips: -1, maxStmts: -1, maxRepeats: -1}
	if parent, ok := ctx.Value(budgetKey{}).(*budget); ok {
		b.parent = parent
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return context.WithValue(ctx, budgetKey{}, b)
}

// BudgetReportFrom returns the account of the innermost budget on ctx, and false when ctx carries
// none.
func BudgetReportFrom(ctx context.Context) (BudgetReport, bool) {
	b, ok := ctx.Value(budgetKey{}).(*budget)
	if !ok || b == nil {
		return BudgetReport{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reportLocked(), true
}

type budgetKey struct{}

// maxCallSites bounds the call sites kept per repeated statement text.
const maxCallSites = 8

// budget is the state of one Budget. Its limits are -1 when unset.
type budget struct {
	parent     *budget
	maxTrips   int
	maxStmts   int
	maxRepeats int
	onExceeded func(ctx context.Context, r BudgetReport)

	mu    sync.Mutex
	trips int
	stmts int
	// texts counts the round trips each statement text was sent in; sites holds the call sites
	// of the texts that repeated.
	texts map[string]int
	sites map[string][]string
	// top is the highest count in texts.
	top int
	// crossed names the first limit crossed, max its value.
	crossed string
	max     int
}

// callSite captures, at most once per round trip and only when a text repeats, where the round
// trip came from.
type callSite struct {
	done bool
	site string
}

func (c *callSite) get() string {
	if !c.done {
		c.done = true
		c.site = captureCallSite()
	}
	return c.site
}

// refuseBudgets returns the error of a budget on ctx that is over a limit already and has no
// OnExceeded, so a round trip under it is not sent.
func refuseBudgets(ctx context.Context) error {
	b, _ := ctx.Value(budgetKey{}).(*budget)
	for ; b != nil; b = b.parent {
		if err := b.refusal(); err != nil {
			return err
		}
	}
	return nil
}

func (b *budget) refusal() error {
	if b.onExceeded != nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.crossed == "" {
		return nil
	}
	return &BudgetError{Limit: b.crossed, Max: b.max, Report: b.reportLocked()}
}

// chargeBudgets counts trips round trips that carried stmts against every budget on ctx. It
// returns the error of the first budget without OnExceeded that they took over a limit.
func chargeBudgets(ctx context.Context, stmts []*stmt, trips int) error {
	b, _ := ctx.Value(budgetKey{}).(*budget)
	if b == nil {
		return nil
	}
	var site callSite
	var err error
	for ; b != nil; b = b.parent {
		if berr := b.charge(ctx, stmts, trips, &site); berr != nil && err == nil {
			err = berr
		}
	}
	return err
}

// charge counts a round trip against b and reports a limit it crossed.
func (b *budget) charge(ctx context.Context, stmts []*stmt, trips int, site *callSite) error {
	b.mu.Lock()
	b.trips += trips
	b.stmts += len(stmts)
	for i, s := range stmts {
		if sentBefore(stmts[:i], s.sql) {
			continue
		}
		if b.texts == nil {
			b.texts = make(map[string]int)
		}
		n := b.texts[s.sql] + 1
		b.texts[s.sql] = n
		b.top = max(b.top, n)
		if n < 2 {
			continue
		}
		if b.sites == nil {
			b.sites = make(map[string][]string)
		}
		sites := b.sites[s.sql]
		if len(sites) < maxCallSites {
			if at := site.get(); !slices.Contains(sites, at) {
				b.sites[s.sql] = append(sites, at)
			}
		}
	}
	if b.crossed != "" {
		b.mu.Unlock()
		return nil
	}
	switch {
	case b.maxTrips >= 0 && b.trips > b.maxTrips:
		b.crossed, b.max = "round trips", b.maxTrips
	case b.maxStmts >= 0 && b.stmts > b.maxStmts:
		b.crossed, b.max = "statements", b.maxStmts
	case b.maxRepeats >= 0 && b.top > b.maxRepeats:
		b.crossed, b.max = "repeats", b.maxRepeats
	default:
		b.mu.Unlock()
		return nil
	}
	report := b.reportLocked()
	b.mu.Unlock()
	if b.onExceeded != nil {
		b.onExceeded(ctx, report)
		return nil
	}
	return &BudgetError{Limit: b.crossed, Max: b.max, Report: report}
}

// sentBefore reports whether one of stmts has the text sql.
func sentBefore(stmts []*stmt, sql string) bool {
	for _, s := range stmts {
		if s.sql == sql {
			return true
		}
	}
	return false
}

// reportLocked builds b's report; b.mu is held.
func (b *budget) reportLocked() BudgetReport {
	r := BudgetReport{RoundTrips: b.trips, Statements: b.stmts}
	for sql, n := range b.texts {
		if n > 1 {
			r.Repeats = append(r.Repeats, Repeat{SQL: sql, Count: n, CallSites: slices.Clone(b.sites[sql])})
		}
	}
	slices.SortFunc(r.Repeats, func(a, b Repeat) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.SQL, b.SQL)
	})
	return r
}

// keelPackage is the import path of package keel.
var keelPackage = reflect.TypeFor[budget]().PkgPath()

// captureCallSite returns file:line of the first frame of the calling goroutine outside package
// keel and outside generated packages (a package whose name ends in "db"), or "unknown".
func captureCallSite() string {
	var pcs [64]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function != "" && !internalFrame(f.Function) {
			return f.File + ":" + strconv.Itoa(f.Line)
		}
		if !more {
			return "unknown"
		}
	}
}

// internalFrame reports whether the function fn, as runtime.Frame names it, belongs to package
// keel or a generated package.
func internalFrame(fn string) bool {
	pkg := funcPackage(fn)
	if pkg == keelPackage {
		return true
	}
	name := pkg[strings.LastIndexByte(pkg, '/')+1:]
	return strings.HasSuffix(name, "db")
}

// funcPackage returns the import path of the function fn, as runtime.Frame names it
// ("example.com/app/store.(*Repo).List.func1" → "example.com/app/store").
func funcPackage(fn string) string {
	slash := strings.LastIndexByte(fn, '/')
	dot := strings.IndexByte(fn[slash+1:], '.')
	if dot < 0 {
		return fn
	}
	return fn[:slash+1+dot]
}
