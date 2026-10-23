// Package migratecli implements the migrate commands with plain or JSON output, for the keel
// command and for application CLIs that embed them. Each command writes to an io.Writer and
// returns an error; a command that finds problems (check, lint, stamp -check) returns an error
// wrapping ErrProblems after printing them.
//
// Plain output is one line per item, made for logs and CI. JSON output is one JSON value per
// command, except Up, which writes one JSON object per event (JSON lines).
package migratecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/emoss08/keel/migrate"
)

// ErrProblems is wrapped by the error of a command that found problems it reported.
var ErrProblems = errors.New("migratecli: problems found")

// Config is what the commands that open a database need.
type Config struct {
	// DSN is the database's connection string.
	DSN string
	// Sources are the migrations.
	Sources migrate.Set
	// Options configure the Migrator; an Observer set here is called after the printer.
	Options migrate.Options
	// JSON selects JSON output.
	JSON bool
}

// open opens a Migrator for c with observer in front of c's own.
func (c Config) open(ctx context.Context, observer func(migrate.Event)) (*migrate.Migrator, error) {
	if c.DSN == "" {
		return nil, errors.New("migratecli: no database; pass -dsn or set KEEL_DSN")
	}
	opts := c.Options
	if observer != nil {
		own := opts.Observer
		opts.Observer = func(e migrate.Event) {
			observer(e)
			if own != nil {
				own(e)
			}
		}
	}
	return migrate.Open(ctx, c.DSN, c.Sources, opts)
}

// writeJSON writes v as one JSON value.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table returns a tabwriter for aligned columns.
func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

// ms formats a duration rounded to milliseconds.
func ms(d time.Duration) string { return d.Round(time.Millisecond).String() }

// label names a migration with its source.
func label(m migrate.Migration) string { return m.ID() + " (" + m.Source + ")" }

// directiveNotes lists a migration's non-default directives.
func directiveNotes(d migrate.Directives) []string {
	var out []string
	if d.NoTx {
		out = append(out, "notx")
	}
	switch {
	case d.LockTimeout < 0:
		out = append(out, "no lock timeout")
	case d.LockTimeout > 0:
		out = append(out, "lock_timeout "+d.LockTimeout.String())
	}
	if d.StatementTimeout > 0 {
		out = append(out, "statement_timeout "+d.StatementTimeout.String())
	}
	if d.NoRetry {
		out = append(out, "no_retry")
	}
	return out
}

// migrationJSON is a migration in JSON output.
type migrationJSON struct {
	Version    string          `json:"version,omitempty"`
	Name       string          `json:"name"`
	Source     string          `json:"source"`
	Kind       migrate.Kind    `json:"kind"`
	File       string          `json:"file,omitempty"`
	Checksum   string          `json:"checksum,omitempty"`
	Directives *directivesJSON `json:"directives,omitempty"`
	SQL        string          `json:"sql,omitempty"`
}

// directivesJSON is a migration's directives in JSON output.
type directivesJSON struct {
	NoTx               bool     `json:"notx,omitempty"`
	LockTimeoutMS      int64    `json:"lock_timeout_ms,omitempty"`
	StatementTimeoutMS int64    `json:"statement_timeout_ms,omitempty"`
	NoRetry            bool     `json:"no_retry,omitempty"`
	LintIgnore         []string `json:"lint_ignore,omitempty"`
}

// toMigrationJSON converts a migration, with its SQL when withSQL is set.
func toMigrationJSON(m migrate.Migration, withSQL bool) *migrationJSON {
	out := &migrationJSON{Version: m.Version, Name: m.Name, Source: m.Source, Kind: m.Kind, File: m.File, Checksum: m.Checksum}
	d := m.Directives
	if d.NoTx || d.LockTimeout != 0 || d.StatementTimeout != 0 || d.NoRetry || len(d.LintIgnore) > 0 {
		out.Directives = &directivesJSON{NoTx: d.NoTx, LockTimeoutMS: d.LockTimeout.Milliseconds(),
			StatementTimeoutMS: d.StatementTimeout.Milliseconds(), NoRetry: d.NoRetry, LintIgnore: d.LintIgnore}
	}
	if withSQL {
		out.SQL = m.SQL
	}
	return out
}

// toMigrationsJSON converts migrations.
func toMigrationsJSON(ms []migrate.Migration, withSQL bool) []*migrationJSON {
	out := make([]*migrationJSON, len(ms))
	for i, m := range ms {
		out[i] = toMigrationJSON(m, withSQL)
	}
	return out
}

// plural formats n with a noun, adding s when n is not one.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// oneLine collapses whitespace in s.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
