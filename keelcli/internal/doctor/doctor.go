// Package doctor runs keelcli's database checks from the facts and migration ports and renders
// them; the doctor command and the console's Doctor tab share it.
package doctor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

// DefaultLongQuery is how long a query runs before doctor lists it.
const DefaultLongQuery = time.Minute

// Level grades a check.
type Level uint8

// The check levels.
const (
	Pass Level = iota
	Info
	Warn
	Fail
)

// String names the level.
func (l Level) String() string {
	switch l {
	case Info:
		return "info"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "pass"
	}
}

// Glyph is the level's symbol.
func (l Level) Glyph() render.Glyph {
	switch l {
	case Info:
		return render.GlyphInfo
	case Warn:
		return render.GlyphWarn
	case Fail:
		return render.GlyphFail
	default:
		return render.GlyphOK
	}
}

// Check is one result.
type Check struct {
	Name   string
	Level  Level
	Detail string
}

// Report is every check with the activity behind the lock check.
type Report struct {
	Connection  factsport.Connection
	Environment seedport.Environment
	Schema      string
	Checks      []Check
	Activity    factsport.Activity
	At          time.Time
}

// Failed reports whether a check failed.
func (r Report) Failed() bool {
	return slices.ContainsFunc(r.Checks, func(c Check) bool { return c.Level == Fail })
}

// Migrations is what doctor reads from the migration engine.
type Migrations interface {
	migrateport.Reader
	Templates(ctx context.Context) ([]migrateport.Template, error)
}

// Sources are the ports doctor reads and what it expects of them.
type Sources struct {
	Facts      factsport.Facts
	Migrations Migrations
	Extensions []string
	LongQuery  time.Duration
	Now        func() time.Time
}

// Run runs every check. A failed connection is the only check when it fails.
func Run(ctx context.Context, src Sources) Report {
	now := time.Now
	if src.Now != nil {
		now = src.Now
	}
	r := Report{At: now()}
	conn, err := src.Facts.Connection(ctx)
	if err != nil {
		r.Checks = []Check{{Name: "connection", Level: Fail, Detail: err.Error()}}
		return r
	}
	r.Connection = conn
	r.Checks = append(r.Checks,
		Check{Name: "connection", Detail: fmt.Sprintf("%s:%d/%s as %s in %s", conn.Host, conn.Port, conn.Database, conn.User, render.Duration(conn.Latency))},
		Check{Name: "server", Detail: "PostgreSQL " + conn.ServerVersion},
	)
	r.Checks = append(r.Checks, settingsChecks(ctx, src.Facts, conn)...)
	r.Checks = append(r.Checks, extensionsCheck(ctx, src.Facts, src.Extensions))
	migrations, schema := migrationsCheck(ctx, src.Migrations)
	r.Schema = schema
	r.Checks = append(r.Checks, migrations, templatesCheck(ctx, src.Migrations, r.At))
	env, envCheck := environmentCheck(ctx, src.Facts)
	r.Environment = env
	r.Checks = append(r.Checks, envCheck)
	long := src.LongQuery
	if long <= 0 {
		long = DefaultLongQuery
	}
	activity, checks := activityChecks(ctx, src.Facts, long)
	r.Activity = activity
	r.Checks = append(r.Checks, checks...)
	return r
}

func settingsChecks(ctx context.Context, f factsport.Inspector, conn factsport.Connection) []Check {
	settings, err := f.Settings(ctx, "plan_cache_mode", "file_copy_method")
	if err != nil {
		return []Check{{Name: "settings", Level: Fail, Detail: err.Error()}}
	}
	values := map[string]string{}
	for _, s := range settings {
		values[s.Name] = s.Value
	}
	plan := Check{Name: "plan_cache_mode", Detail: values["plan_cache_mode"]}
	switch values["plan_cache_mode"] {
	case "auto":
		plan.Detail = "auto: generic plans when they are no worse (keel's default)"
	case "force_custom_plan":
		plan.Level, plan.Detail = Info, "force_custom_plan: every execution is planned"
	case "force_generic_plan":
		plan.Level, plan.Detail = Warn, "force_generic_plan: skewed tenants get one plan for all; prefer auto"
	default:
		plan.Level, plan.Detail = Info, "not reported"
	}
	copyCheck := Check{Name: "template copy", Level: Info,
		Detail: "file copy; Postgres 18 with file_copy_method = clone makes resets instant"}
	if conn.ServerNum >= 180000 && values["file_copy_method"] == "clone" {
		copyCheck = Check{Name: "template copy", Detail: "file_copy_method = clone: templates copy by reflink"}
	}
	return []Check{plan, copyCheck}
}

func extensionsCheck(ctx context.Context, f factsport.Inspector, required []string) Check {
	exts, err := f.Extensions(ctx)
	if err != nil {
		return Check{Name: "extensions", Level: Fail, Detail: err.Error()}
	}
	installed := make([]string, 0, len(exts))
	for _, e := range exts {
		installed = append(installed, e.Name)
	}
	var missing []string
	for _, name := range required {
		if !slices.Contains(installed, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Check{Name: "extensions", Level: Fail, Detail: "missing " + strings.Join(missing, ", ")}
	}
	return Check{Name: "extensions", Detail: strings.Join(installed, ", ")}
}

func migrationsCheck(ctx context.Context, m Migrations) (Check, string) {
	rows, err := m.Status(ctx)
	if err != nil {
		return Check{Name: "migrations", Level: Fail, Detail: err.Error()}, ""
	}
	counts := map[migrateport.State]int{}
	schema := ""
	for _, s := range rows {
		counts[s.State]++
		if s.Recorded() && s.Migration.Version > schema {
			schema = s.Migration.Version
		}
	}
	c := Check{Name: "migrations", Detail: "up to date at " + schema}
	if schema == "" {
		c.Detail = "nothing applied"
	}
	var parts []string
	if n := counts[migrateport.Changed] + counts[migrateport.Missing]; n > 0 {
		c.Level = Fail
		parts = append(parts, fmt.Sprintf("%d changed or missing (migrate check)", n))
	}
	if n := counts[migrateport.Pending] + counts[migrateport.RepeatableDue]; n > 0 {
		c.Level = max(c.Level, Warn)
		parts = append(parts, fmt.Sprintf("%d pending (up)", n))
	}
	if len(parts) > 0 {
		c.Detail = strings.Join(parts, " · ")
	}
	return c, schema
}

func templatesCheck(ctx context.Context, m Migrations, now time.Time) Check {
	ts, err := m.Templates(ctx)
	if err != nil {
		return Check{Name: "templates", Level: Fail, Detail: err.Error()}
	}
	stale := 0
	c := Check{Name: "templates", Level: Warn, Detail: "current template not built (template build)"}
	for _, t := range ts {
		if !t.Current {
			stale++
			continue
		}
		c = Check{Name: "templates", Detail: fmt.Sprintf("%s, %s, %s old", t.Name, render.Size(t.Size), render.Duration(now.Sub(t.CreatedAt)))}
	}
	if stale > 0 {
		c.Level = max(c.Level, Info)
		c.Detail += fmt.Sprintf(" · %d stale (template prune)", stale)
	}
	return c
}

func environmentCheck(ctx context.Context, f factsport.EnvironmentReader) (seedport.Environment, Check) {
	env, err := f.Environment(ctx)
	if err != nil {
		return "", Check{Name: "environment", Level: Fail, Detail: err.Error()}
	}
	if env == "" {
		return env, Check{Name: "environment", Level: Info, Detail: "unmarked (up -env <name> marks it)"}
	}
	if env == seedport.Production {
		return env, Check{Name: "environment", Level: Info, Detail: "production: fixtures and destructive commands refuse"}
	}
	return env, Check{Name: "environment", Detail: string(env)}
}

func activityChecks(ctx context.Context, f factsport.Monitor, long time.Duration) (factsport.Activity, []Check) {
	a, err := f.Activity(ctx, long)
	if err != nil {
		return a, []Check{{Name: "locks", Level: Fail, Detail: err.Error()}}
	}
	locks := Check{Name: "locks", Detail: "no backend holds a lock another waits for"}
	if n := len(a.LockHolders); n > 0 {
		locks = Check{Name: "locks", Level: Warn, Detail: render.Plural(n, "backend holds", "backends hold") + " locks others wait for; a migration would queue behind them"}
	}
	queries := Check{Name: "long queries", Detail: "none running longer than " + render.Duration(long)}
	if n := len(a.LongQueries); n > 0 {
		queries = Check{Name: "long queries", Level: Warn, Detail: render.Plural(n, "query", "queries") + " running longer than " + render.Duration(long)}
	}
	return a, []Check{locks, queries}
}
