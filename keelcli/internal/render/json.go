package render

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

// JSON writes run events as JSON lines and results as one JSON document per line. It is safe for
// concurrent use.
type JSON struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSON writes JSON to w.
func NewJSON(w io.Writer) *JSON {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &JSON{enc: enc}
}

// Write encodes one document.
func (j *JSON) Write(v any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.enc.Encode(v)
}

// Migration writes one migration event.
func (j *JSON) Migration(e migrateport.Event) {
	out := JSONEvent{
		Source:     "migrate",
		Kind:       e.Kind.String(),
		Index:      e.Index,
		Total:      e.Total,
		Statement:  e.Statement,
		Statements: e.Statements,
		Attempt:    e.Attempt,
		DurationMS: ms(e.Duration),
		Error:      errText(e.Err),
	}
	if e.Migration != nil {
		m := MigrationJSON(*e.Migration, false)
		out.Migration = &m
	}
	for _, b := range e.Blockers {
		out.Blockers = append(out.Blockers, JSONBlocker{
			PID: b.PID, Application: b.Application, State: b.State, Mode: b.Mode, AgeMS: ms(b.Age), Query: b.Query,
		})
	}
	_ = j.Write(out)
}

// Seed writes one seed event.
func (j *JSON) Seed(e seedport.Event) {
	out := JSONEvent{
		Source:      "seed",
		Kind:        e.Kind.String(),
		Index:       e.Index,
		Total:       e.Total,
		Tenant:      e.Tenant,
		Tenants:     e.Tenants,
		TenantsDone: e.TenantsDone,
		Rows:        e.Rows,
		DurationMS:  ms(e.Duration),
		Error:       errText(e.Err),
	}
	if e.Seed != nil {
		s := SeedJSON(*e.Seed)
		out.Seed = &s
	}
	if e.Kind == seedport.Planned || e.Kind == seedport.SeedStart {
		r := ReasonJSON(e.Reason)
		out.Reason = &r
	}
	_ = j.Write(out)
}

// JSONEvent is one run event.
type JSONEvent struct {
	Source      string         `json:"source"`
	Kind        string         `json:"kind"`
	Migration   *JSONMigration `json:"migration,omitempty"`
	Seed        *JSONSeed      `json:"seed,omitempty"`
	Reason      *JSONReason    `json:"reason,omitempty"`
	Index       int            `json:"index,omitempty"`
	Total       int            `json:"total,omitempty"`
	Statement   int            `json:"statement,omitempty"`
	Statements  int            `json:"statements,omitempty"`
	Attempt     int            `json:"attempt,omitempty"`
	Blockers    []JSONBlocker  `json:"blockers,omitempty"`
	Tenant      string         `json:"tenant,omitempty"`
	Tenants     int            `json:"tenants,omitempty"`
	TenantsDone int            `json:"tenants_done,omitempty"`
	Rows        int64          `json:"rows,omitempty"`
	DurationMS  int64          `json:"duration_ms,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// JSONBlocker is a backend a migration waits for.
type JSONBlocker struct {
	PID         int32  `json:"pid"`
	Application string `json:"application"`
	State       string `json:"state"`
	Mode        string `json:"mode"`
	AgeMS       int64  `json:"age_ms"`
	Query       string `json:"query"`
}

// JSONDirectives are a migration's directives; NoLockTimeout marks a disabled lock timeout.
type JSONDirectives struct {
	NoTx               bool  `json:"notx,omitempty"`
	LockTimeoutMS      int64 `json:"lock_timeout_ms,omitempty"`
	NoLockTimeout      bool  `json:"no_lock_timeout,omitempty"`
	StatementTimeoutMS int64 `json:"statement_timeout_ms,omitempty"`
	NoRetry            bool  `json:"no_retry,omitempty"`
}

// JSONMigration is one migration.
type JSONMigration struct {
	Version    string          `json:"version,omitempty"`
	Name       string          `json:"name"`
	Source     string          `json:"source,omitempty"`
	Kind       string          `json:"kind"`
	Directives *JSONDirectives `json:"directives,omitempty"`
	Checksum   string          `json:"checksum,omitempty"`
	SQL        string          `json:"sql,omitempty"`
}

// MigrationJSON converts a migration, with its SQL when withSQL is set.
func MigrationJSON(m migrateport.Migration, withSQL bool) JSONMigration {
	out := JSONMigration{
		Version: m.Version, Name: m.Name, Source: m.Source, Kind: m.Kind.String(), Checksum: m.Checksum,
	}
	if d := m.Directives; d != (migrateport.Directives{}) {
		out.Directives = &JSONDirectives{
			NoTx: d.NoTx, LockTimeoutMS: ms(max(d.LockTimeout, 0)), NoLockTimeout: d.LockTimeout < 0,
			StatementTimeoutMS: ms(d.StatementTimeout), NoRetry: d.NoRetry,
		}
	}
	if withSQL {
		out.SQL = m.SQL
	}
	return out
}

// JSONStatus is a migration with its state.
type JSONStatus struct {
	JSONMigration
	State           string     `json:"state"`
	AppliedAt       *time.Time `json:"applied_at,omitempty"`
	DurationMS      int64      `json:"duration_ms,omitempty"`
	AppliedChecksum string     `json:"applied_checksum,omitempty"`
}

// MigrationStatusJSON is the document of migrate status.
func MigrationStatusJSON(rows []migrateport.Status) any {
	out := make([]JSONStatus, 0, len(rows))
	for _, s := range rows {
		out = append(out, JSONStatus{
			JSONMigration:   MigrationJSON(s.Migration, false),
			State:           s.State.String(),
			AppliedAt:       timePtr(s.AppliedAt),
			DurationMS:      ms(s.Duration),
			AppliedChecksum: s.AppliedChecksum,
		})
	}
	return map[string]any{"migrations": out}
}

// HistoryJSON is the document of migrate history.
func HistoryJSON(rows []migrateport.Record) any {
	type record struct {
		Version    string     `json:"version"`
		Name       string     `json:"name"`
		Source     string     `json:"source"`
		Checksum   string     `json:"checksum"`
		AppliedAt  *time.Time `json:"applied_at,omitempty"`
		DurationMS int64      `json:"duration_ms"`
	}
	out := make([]record, 0, len(rows))
	for _, r := range rows {
		out = append(out, record{r.Version, r.Name, r.Source, r.Checksum, timePtr(r.AppliedAt), ms(r.Duration)})
	}
	return map[string]any{"history": out}
}

// PlanJSON is the document of migrate plan, with SQL for a dry run.
func PlanJSON(p migrateport.Plan, withSQL bool) any {
	run := make([]JSONMigration, 0, len(p.Migrations))
	for _, m := range p.Migrations {
		run = append(run, MigrationJSON(m, withSQL))
	}
	base := make([]JSONMigration, 0, len(p.Baseline))
	for _, m := range p.Baseline {
		base = append(base, MigrationJSON(m, false))
	}
	after := p.After
	if after == nil {
		after = []string{}
	}
	return map[string]any{"migrations": run, "baseline": base, "after": after}
}

// FindingsJSON is the document of migrate lint.
func FindingsJSON(fs []migrateport.Finding) any {
	type finding struct {
		Rule      string        `json:"rule"`
		Severity  string        `json:"severity"`
		Migration JSONMigration `json:"migration"`
		Line      int           `json:"line"`
		Message   string        `json:"message"`
	}
	out := make([]finding, 0, len(fs))
	for _, f := range fs {
		out = append(out, finding{f.Rule, f.Severity.String(), MigrationJSON(f.Migration, false), f.Line, f.Message})
	}
	return map[string]any{"findings": out}
}

// ProblemsJSON is the document of migrate check.
func ProblemsJSON(ps []migrateport.Problem) any {
	type problem struct {
		Kind      string        `json:"kind"`
		Migration JSONMigration `json:"migration"`
		Message   string        `json:"message"`
	}
	out := make([]problem, 0, len(ps))
	for _, p := range ps {
		out = append(out, problem{p.Kind.String(), MigrationJSON(p.Migration, false), p.Message})
	}
	return map[string]any{"problems": out}
}

// TemplatesJSON is the document of template.
func TemplatesJSON(ts []migrateport.Template) any {
	type template struct {
		Name        string     `json:"name"`
		Fingerprint string     `json:"fingerprint"`
		Current     bool       `json:"current"`
		Size        int64      `json:"size"`
		CreatedAt   *time.Time `json:"created_at,omitempty"`
	}
	out := make([]template, 0, len(ts))
	for _, t := range ts {
		out = append(out, template{t.Name, t.Fingerprint, t.Current, t.Size, timePtr(t.CreatedAt)})
	}
	return map[string]any{"templates": out}
}

// JSONSeed is one seed.
type JSONSeed struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description,omitempty"`
	After       []string `json:"after,omitempty"`
	PerTenant   bool     `json:"per_tenant,omitempty"`
}

// SeedJSON converts a seed.
func SeedJSON(s seedport.Seed) JSONSeed {
	return JSONSeed{s.Name, s.Kind.String(), s.Description, s.After, s.PerTenant}
}

// JSONReason is why a seed runs or does not.
type JSONReason struct {
	Kind   string   `json:"kind"`
	Runs   bool     `json:"runs"`
	Inputs []string `json:"inputs,omitempty"`
}

// ReasonJSON converts a reason.
func ReasonJSON(r seedport.Reason) JSONReason {
	return JSONReason{r.Kind.String(), r.Runs(), r.Inputs}
}

// SeedStatusJSON is the document of seed status and seed prune.
func SeedStatusJSON(rows []seedport.Status) any {
	type status struct {
		JSONSeed
		State      string     `json:"state"`
		Reason     JSONReason `json:"reason"`
		AppliedAt  *time.Time `json:"applied_at,omitempty"`
		DurationMS int64      `json:"duration_ms,omitempty"`
		Rows       int64      `json:"rows,omitempty"`
		Tenants    int        `json:"tenants,omitempty"`
	}
	out := make([]status, 0, len(rows))
	for _, s := range rows {
		out = append(out, status{SeedJSON(s.Seed), s.State.String(), ReasonJSON(s.Reason), timePtr(s.AppliedAt),
			ms(s.Duration), s.Rows, s.Tenants})
	}
	return map[string]any{"seeds": out}
}

// SeedPlanJSON is the document of seed plan.
func SeedPlanJSON(items []seedport.PlanItem) any {
	type item struct {
		JSONSeed
		Reason JSONReason `json:"reason"`
	}
	out := make([]item, 0, len(items))
	for _, p := range items {
		out = append(out, item{SeedJSON(p.Seed), ReasonJSON(p.Reason)})
	}
	return map[string]any{"seeds": out}
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
