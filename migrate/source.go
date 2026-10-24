package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// versionLayout is the layout of the versions Create writes: a UTC timestamp. Any 14-digit version
// is accepted, so applications that number migrations within a day keep their versions.
const versionLayout = "20060102150405"

// Kind is what a migration is.
type Kind uint8

const (
	// KindVersioned is a <version>_<name>.sql file, applied once.
	KindVersioned Kind = iota
	// KindRepeatable is an R_<name>.sql file, applied again whenever its checksum changes.
	KindRepeatable
	// KindGo is a Func registered with a source.
	KindGo
	// KindBaseline is a file marked --keel:baseline, usually written by Squash.
	KindBaseline
)

// kindNames are the names of the kinds, as stored in keel_migrations.
var kindNames = [...]string{"versioned", "repeatable", "go", "baseline"}

// String returns the kind's name: versioned, repeatable, go or baseline.
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", k)
}

// MarshalText encodes the kind as its name.
func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// parseKind returns the kind named s.
func parseKind(s string) Kind {
	for i, n := range kindNames {
		if n == s {
			return Kind(i)
		}
	}
	return KindVersioned
}

// Directives are the --keel: lines at the top of a migration file.
type Directives struct {
	// NoTx runs the migration outside a transaction, statement by statement (--keel:notx).
	NoTx bool
	// LockTimeout overrides Options.LockTimeout (--keel:lock_timeout); zero keeps the default and
	// a negative value, written "--keel:lock_timeout 0", disables it.
	LockTimeout time.Duration
	// StatementTimeout bounds each statement (--keel:statement_timeout); zero means none.
	StatementTimeout time.Duration
	// NoRetry fails on the first lock timeout instead of retrying (--keel:no_retry).
	NoRetry bool
	// LintIgnore names lint rules that do not apply to this migration (--keel:lint_ignore).
	LintIgnore []string
}

// Migration is one migration of a Set.
type Migration struct {
	// Version is the migration's 14-digit version, ordered as a number; empty for a repeatable
	// migration. Create writes a UTC timestamp.
	Version string
	// Name is the file name's label, or the Func's name.
	Name string
	// Source is the name of the Source it came from.
	Source string
	// Kind is versioned, repeatable, go or baseline.
	Kind Kind
	// Directives are the file's --keel: directives.
	Directives Directives
	// Checksum is the sha256 of the file, or of a Go migration's version and name.
	Checksum string
	// SQL is the file's content; empty for a Go migration.
	SQL string
	// File is the file name within its source; empty for a Go migration.
	File string

	up     func(context.Context, pgx.Tx) error
	upConn func(context.Context, *pgx.Conn) error
}

// ID is the migration's version and name, or R_<name> for a repeatable migration.
func (m Migration) ID() string {
	if m.Kind == KindRepeatable {
		return "R_" + m.Name
	}
	return m.Version + "_" + m.Name
}

// Statements returns the statements Up runs one by one: the parts of a notx file split at
// --keel:split lines, or the whole file for a transactional one. It is nil for a Go migration.
func (m Migration) Statements() []string {
	if m.Kind == KindGo {
		return nil
	}
	if m.Directives.NoTx {
		return split(m.SQL)
	}
	if strings.TrimSpace(m.SQL) == "" {
		return nil
	}
	return []string{m.SQL}
}

// Func is a migration written in Go, for data changes SQL cannot express. Set exactly one of Up,
// which runs in the migration's transaction, and UpNoTx, which runs on the connection outside a
// transaction. LockTimeout, StatementTimeout and NoRetry act as the file directives do. Its
// checksum covers its version and name, so renaming an applied Func is ErrChanged.
type Func struct {
	Version          string
	Name             string
	Up               func(ctx context.Context, tx pgx.Tx) error
	UpNoTx           func(ctx context.Context, conn *pgx.Conn) error
	LockTimeout      time.Duration
	StatementTimeout time.Duration
	NoRetry          bool
}

// Source is one file system of migrations, with the Go migrations that belong to it. FS holds
// the .sql files at its root; other files and directories are ignored. FS may be nil for a source
// of Go migrations only.
type Source struct {
	Name  string
	FS    fs.FS
	Funcs []Func
}

// Set is the migrations of one or more sources merged into one version line, as made by
// Sources. A Set that failed to read carries its error, returned by every call that uses it.
type Set struct {
	migrations  []Migration
	fingerprint string
	err         error
}

// Sources reads and merges the sources. A version or repeatable name that appears twice, in one
// source or two, is an error, as is a migration older than a baseline.
func Sources(sources ...Source) Set {
	var all []Migration
	names := map[string]bool{}
	for _, src := range sources {
		if src.Name == "" {
			src.Name = "default"
		}
		if names[src.Name] {
			return Set{err: fmt.Errorf("migrate: two sources are named %q", src.Name)}
		}
		names[src.Name] = true
		ms, err := readSource(src)
		if err != nil {
			return Set{err: err}
		}
		all = append(all, ms...)
	}
	return newSet(all)
}

// newSet orders and validates migrations.
func newSet(all []Migration) Set {
	sortMigrations(all)
	if err := validate(all); err != nil {
		return Set{err: err}
	}
	return Set{migrations: all, fingerprint: fingerprint(all)}
}

// Migrations returns the set's migrations: versioned, Go and baseline migrations by version,
// then repeatable migrations by name.
func (s Set) Migrations() ([]Migration, error) { return s.migrations, s.err }

// Err returns the error the set was read with.
func (s Set) Err() error { return s.err }

// Version returns the newest version in the set, or "" when it has none.
func (s Set) Version() string {
	v := ""
	for _, m := range s.migrations {
		if m.Kind != KindRepeatable && m.Version > v {
			v = m.Version
		}
	}
	return v
}

// Fingerprint returns the sha256 over every migration's source, file name and content, and every
// Go migration's identity, in a stable order. It changes whenever any migration does.
func (s Set) Fingerprint() string { return s.fingerprint }

// readSource reads one source's files and Go migrations.
func readSource(src Source) ([]Migration, error) {
	var out []Migration
	if src.FS != nil {
		entries, err := fs.ReadDir(src.FS, ".")
		if err != nil {
			return nil, fmt.Errorf("migrate: read source %s: %w", src.Name, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			body, err := fs.ReadFile(src.FS, e.Name())
			if err != nil {
				return nil, fmt.Errorf("migrate: read %s: %w", e.Name(), err)
			}
			m, err := parseFile(src.Name, e.Name(), string(body))
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
	}
	for _, f := range src.Funcs {
		m, err := goMigration(src.Name, f)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// parseFile parses one migration file.
func parseFile(source, file, body string) (Migration, error) {
	m := Migration{Source: source, File: file, SQL: body, Checksum: checksum(body)}
	base := strings.TrimSuffix(file, ".sql")
	switch {
	case strings.HasSuffix(base, ".down"):
		return m, fmt.Errorf("migrate: %s: down migrations are not supported; fix forward with a new migration", file)
	case strings.HasSuffix(base, ".up"), strings.HasSuffix(base, ".notx"):
		return m, fmt.Errorf("migrate: %s: name it <version>_<name>.sql and use --keel: directives", file)
	case strings.HasPrefix(base, "R_"):
		m.Kind, m.Name = KindRepeatable, strings.TrimPrefix(base, "R_")
		if m.Name == "" {
			return m, fmt.Errorf("migrate: %s: a repeatable migration needs a name", file)
		}
	default:
		version, name, _ := strings.Cut(base, "_")
		if err := checkVersion(version); err != nil || name == "" {
			return m, fmt.Errorf("migrate: %s must be named <version>_<name>.sql with a 14-digit version, or R_<name>.sql", file)
		}
		m.Version, m.Name = version, name
	}
	d, baseline, err := parseDirectives(body)
	if err != nil {
		return m, fmt.Errorf("migrate: %s: %w", file, err)
	}
	m.Directives = d
	if baseline {
		if m.Kind == KindRepeatable {
			return m, fmt.Errorf("migrate: %s: a repeatable migration cannot be a baseline", file)
		}
		m.Kind = KindBaseline
	}
	return m, nil
}

// checkVersion reports whether v is a 14-digit version.
func checkVersion(v string) error {
	if len(v) != len(versionLayout) || strings.Trim(v, "0123456789") != "" {
		return fmt.Errorf("migrate: version %q is not 14 digits", v)
	}
	return nil
}

// parseDirectives reads the --keel: directives at the top of body and reports a misplaced or
// unknown one.
func parseDirectives(body string) (Directives, bool, error) {
	var d Directives
	baseline, header, splits := false, true, false
	seen := map[string]bool{}
	for _, ln := range lex(body).lines {
		text := strings.TrimSpace(ln.text)
		if !ln.code {
			header = false
			continue
		}
		if text == "--keel:split" {
			splits = true
			continue
		}
		if text != "" && !strings.HasPrefix(text, "--") {
			header = false
			continue
		}
		if !strings.HasPrefix(text, "--keel:") {
			continue
		}
		fields := append(strings.Fields(strings.TrimPrefix(text, "--keel:")), "")
		name, arg := fields[0], strings.Join(fields[1:], " ")
		if !header {
			return d, false, fmt.Errorf("--keel:%s must be at the top of the file, before any statement", name)
		}
		if seen[name] {
			return d, false, fmt.Errorf("--keel:%s appears twice", name)
		}
		seen[name] = true
		arg = strings.TrimSpace(arg)
		if err := applyDirective(&d, &baseline, name, arg); err != nil {
			return d, false, err
		}
	}
	if splits && !d.NoTx {
		return d, false, errors.New("--keel:split needs --keel:notx")
	}
	return d, baseline, nil
}

// applyDirective sets the directive name with its argument.
func applyDirective(d *Directives, baseline *bool, name, arg string) error {
	switch name {
	case "notx", "no_retry", "baseline":
		if arg != "" {
			return fmt.Errorf("--keel:%s takes no argument", name)
		}
		switch name {
		case "notx":
			d.NoTx = true
		case "no_retry":
			d.NoRetry = true
		default:
			*baseline = true
		}
		return nil
	case "lock_timeout", "statement_timeout":
		v, err := parseTimeout(arg)
		if err != nil {
			return fmt.Errorf("--keel:%s: %w", name, err)
		}
		switch {
		case name == "statement_timeout":
			d.StatementTimeout = v
		case v == 0:
			d.LockTimeout = -1
		default:
			d.LockTimeout = v
		}
		return nil
	case "lint_ignore":
		d.LintIgnore = strings.FieldsFunc(arg, func(r rune) bool { return r == ',' || r == ' ' })
		if len(d.LintIgnore) == 0 {
			return errors.New("--keel:lint_ignore needs rule names")
		}
		return nil
	}
	return fmt.Errorf("unknown directive --keel:%s", name)
}

// parseTimeout parses a directive duration: a Go duration such as 2s or 500ms, or 0.
func parseTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("needs a duration such as 2s")
	}
	if s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration such as 2s", s)
	}
	return d, nil
}

// goMigration converts a Func.
func goMigration(source string, f Func) (Migration, error) {
	id := f.Version + "_" + f.Name
	if err := checkVersion(f.Version); err != nil {
		return Migration{}, fmt.Errorf("migrate: Go migration %s: %w", id, err)
	}
	if f.Name == "" {
		return Migration{}, fmt.Errorf("migrate: Go migration %s needs a name", f.Version)
	}
	if (f.Up == nil) == (f.UpNoTx == nil) {
		return Migration{}, fmt.Errorf("migrate: Go migration %s needs exactly one of Up and UpNoTx", id)
	}
	return Migration{
		Version: f.Version, Name: f.Name, Source: source, Kind: KindGo,
		Checksum: checksum("go\x00" + f.Version + "\x00" + f.Name),
		Directives: Directives{
			NoTx: f.UpNoTx != nil, LockTimeout: f.LockTimeout, StatementTimeout: f.StatementTimeout,
			NoRetry: f.NoRetry,
		},
		up: f.Up, upConn: f.UpNoTx,
	}, nil
}

// checksum returns the hex sha256 of s.
func checksum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sortMigrations orders versioned, Go and baseline migrations by version, then repeatable
// migrations by name.
func sortMigrations(ms []Migration) {
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if (a.Kind == KindRepeatable) != (b.Kind == KindRepeatable) {
			return b.Kind == KindRepeatable
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Source < b.Source
	})
}

// validate reports duplicate versions and names, and migrations older than a baseline.
func validate(ms []Migration) error {
	baseline := ""
	for i, m := range ms {
		if m.Kind == KindBaseline {
			baseline = m.Version
		}
		if i == 0 {
			continue
		}
		prev := ms[i-1]
		switch {
		case m.Kind == KindRepeatable && prev.Kind == KindRepeatable && m.Name == prev.Name:
			return fmt.Errorf("migrate: repeatable migration %s is in sources %s and %s", m.ID(), prev.Source, m.Source)
		case m.Kind != KindRepeatable && m.Version == prev.Version:
			return fmt.Errorf("migrate: %s (%s) and %s (%s) share a version", prev.ID(), prev.Source, m.ID(), m.Source)
		}
	}
	for _, m := range ms {
		if m.Kind != KindRepeatable && m.Version < baseline {
			return fmt.Errorf("migrate: %s (%s) is older than baseline %s; squash or remove it", m.ID(), m.Source, baseline)
		}
	}
	return nil
}

// fingerprint hashes every migration's identity and content.
func fingerprint(ms []Migration) string {
	sorted := append([]Migration(nil), ms...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Source != sorted[j].Source {
			return sorted[i].Source < sorted[j].Source
		}
		return sorted[i].ID() < sorted[j].ID()
	})
	h := sha256.New()
	for _, m := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00", m.Source, m.Kind, m.ID(), m.File, len(m.SQL), m.SQL)
	}
	return hex.EncodeToString(h.Sum(nil))
}
