package migrate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TemplatePrefix starts the name of every template database Template builds.
const TemplatePrefix = "keel_tpl_"

// buildPrefix starts the name of a template database while it is being built.
const buildPrefix = "keel_build_"

// TemplateOptions configure Template.
type TemplateOptions struct {
	// Admin is a DSN for any database on the server, used to create and drop databases; its
	// role needs CREATEDB.
	Admin string
	// Sources are the migrations the template applies.
	Sources Set
	// Options configure the migrations, such as After.
	Options Options
	// Key joins the template's identity, for what Prepare does: a seed fingerprint, say.
	Key string
	// Prepare runs on the template after the migrations, with its DSN; it must close its
	// connections before returning.
	Prepare func(ctx context.Context, dsn string) error
	// Prefix starts the name of each database Template returns; default "keel_".
	Prefix string
}

// TemplateInfo describes a template database.
type TemplateInfo struct {
	Name        string
	Size        int64
	Created     time.Time
	Fingerprint string
	Key         string
}

// templateMeta is stored as the template database's comment.
type templateMeta struct {
	Created     time.Time `json:"created"`
	Fingerprint string    `json:"fingerprint"`
	Key         string    `json:"key,omitempty"`
}

// TemplateName returns the name of the template Template builds for set and key:
// keel_tpl_ followed by the first 16 hex digits of sha256(fingerprint + key).
func TemplateName(set Set, key string) (string, error) {
	if set.err != nil {
		return "", set.err
	}
	sum := sha256.Sum256([]byte(set.fingerprint + key))
	return TemplatePrefix + hex.EncodeToString(sum[:])[:16], nil
}

// Template returns the DSN of a new database holding the set's schema, and a function that drops
// it. The first call for a set and key builds a template database (migrations, then Prepare) under
// an advisory lock, so concurrent callers in any process build it once; every call then copies it
// with CREATE DATABASE … TEMPLATE … STRATEGY FILE_COPY, which takes milliseconds, and much less on
// Postgres 18 with file_copy_method = clone on a copy-on-write file system.
func Template(ctx context.Context, o TemplateOptions) (string, func(context.Context) error, error) {
	name, err := TemplateName(o.Sources, o.Key)
	if err != nil {
		return "", nil, err
	}
	admin, err := pgx.Connect(ctx, o.Admin)
	if err != nil {
		return "", nil, fmt.Errorf("migrate: template: connect: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()
	if err := ensureTemplate(ctx, admin, name, o); err != nil {
		return "", nil, err
	}
	prefix := o.Prefix
	if prefix == "" {
		prefix = "keel_"
	}
	db := prefix + randomHex(6)
	if err := createFrom(ctx, admin, db, name); err != nil {
		return "", nil, err
	}
	dsn, err := withDatabase(o.Admin, db)
	if err != nil {
		return "", nil, err
	}
	drop := func(ctx context.Context) error {
		c, err := pgx.Connect(ctx, o.Admin)
		if err != nil {
			return fmt.Errorf("migrate: drop %s: %w", db, err)
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		return dropDatabase(ctx, c, db)
	}
	return dsn, drop, nil
}

// ensureTemplate builds the template name unless it exists, holding an advisory lock keyed by
// the name. It builds under a temporary name and renames the finished template, so a failed
// build never leaves a half-built template behind.
func ensureTemplate(ctx context.Context, admin *pgx.Conn, name string, o TemplateOptions) error {
	sum := sha256.Sum256([]byte("keel template " + name))
	key := int64(binary.BigEndian.Uint64(sum[:8]))
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return fmt.Errorf("migrate: template: lock: %w", err)
	}
	defer func() { _, _ = admin.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key) }()
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("migrate: template: %w", err)
	}
	if exists {
		return nil
	}
	build := buildPrefix + strings.TrimPrefix(name, TemplatePrefix)
	if err := dropDatabase(ctx, admin, build); err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{build}.Sanitize()); err != nil {
		return fmt.Errorf("migrate: template: %w", err)
	}
	if err := fillTemplate(ctx, admin, build, o); err != nil {
		return errors.Join(err, dropDatabase(context.WithoutCancel(ctx), admin, build))
	}
	return finishTemplate(ctx, admin, build, name, o)
}

// fillTemplate migrates and prepares the build database.
func fillTemplate(ctx context.Context, admin *pgx.Conn, build string, o TemplateOptions) error {
	dsn, err := withDatabase(o.Admin, build)
	if err != nil {
		return err
	}
	m, err := Open(ctx, dsn, o.Sources, o.Options)
	if err != nil {
		return err
	}
	_, err = m.Up(ctx, UpOptions{})
	if cerr := m.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("migrate: template: %w", err)
	}
	if o.Prepare != nil {
		if err := o.Prepare(ctx, dsn); err != nil {
			return fmt.Errorf("migrate: template: prepare: %w", err)
		}
	}
	return nil
}

// finishTemplate records the template's metadata, marks it a template that accepts no
// connections, and renames it into place.
func finishTemplate(ctx context.Context, admin *pgx.Conn, build, name string, o TemplateOptions) error {
	meta, err := json.Marshal(templateMeta{Created: time.Now().UTC(), Fingerprint: o.Sources.fingerprint, Key: o.Key})
	if err != nil {
		return err
	}
	b := pgx.Identifier{build}.Sanitize()
	err = execAll(ctx, admin,
		"COMMENT ON DATABASE "+b+" IS "+quoteLiteral(string(meta)),
		"ALTER DATABASE "+b+" WITH ALLOW_CONNECTIONS false")
	if err == nil {
		err = terminate(ctx, admin, build)
	}
	if err == nil {
		err = execAll(ctx, admin,
			"ALTER DATABASE "+b+" WITH IS_TEMPLATE true",
			"ALTER DATABASE "+b+" RENAME TO "+pgx.Identifier{name}.Sanitize())
	}
	if err != nil {
		return errors.Join(fmt.Errorf("migrate: template: %w", err), dropDatabase(context.WithoutCancel(ctx), admin, build))
	}
	return nil
}

// execAll runs statements in order, stopping at the first error.
func execAll(ctx context.Context, c *pgx.Conn, stmts ...string) error {
	for _, s := range stmts {
		if _, err := c.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// terminate ends every connection to db and waits until they are gone.
func terminate(ctx context.Context, admin *pgx.Conn, db string) error {
	for {
		var n int
		err := admin.QueryRow(ctx, `SELECT count(*) FROM (SELECT pg_terminate_backend(pid)
FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()) t`, db).Scan(&n)
		if err != nil || n == 0 {
			return err
		}
		if err := sleep(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}

// createFrom creates db as a copy of template.
func createFrom(ctx context.Context, admin *pgx.Conn, db, template string) error {
	_, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()+" TEMPLATE "+
		pgx.Identifier{template}.Sanitize()+" STRATEGY FILE_COPY")
	if err != nil {
		return fmt.Errorf("migrate: create %s from %s: %w", db, template, err)
	}
	return nil
}

// dropDatabase drops name if it exists, first clearing its template flag and terminating its
// connections.
func dropDatabase(ctx context.Context, admin *pgx.Conn, name string) error {
	var template bool
	err := admin.QueryRow(ctx, "SELECT datistemplate FROM pg_database WHERE datname = $1", name).Scan(&template)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("migrate: drop %s: %w", name, err)
	}
	id := pgx.Identifier{name}.Sanitize()
	if template {
		if _, err := admin.Exec(ctx, "ALTER DATABASE "+id+" WITH IS_TEMPLATE false"); err != nil {
			return fmt.Errorf("migrate: drop %s: %w", name, err)
		}
	}
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+id+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("migrate: drop %s: %w", name, err)
	}
	return nil
}

// Templates lists the keel template databases on the server admin connects to.
func Templates(ctx context.Context, admin string) ([]TemplateInfo, error) {
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		return nil, fmt.Errorf("migrate: templates: connect: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return listTemplates(ctx, c)
}

// listTemplates lists the keel template databases.
func listTemplates(ctx context.Context, c *pgx.Conn) ([]TemplateInfo, error) {
	rows, err := c.Query(ctx, `SELECT datname, pg_database_size(oid), coalesce(shobj_description(oid, 'pg_database'), '')
FROM pg_database WHERE datistemplate AND starts_with(datname, $1) ORDER BY datname`, TemplatePrefix)
	if err != nil {
		return nil, fmt.Errorf("migrate: templates: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TemplateInfo, error) {
		var t TemplateInfo
		var comment string
		if err := row.Scan(&t.Name, &t.Size, &comment); err != nil {
			return t, err
		}
		var meta templateMeta
		if json.Unmarshal([]byte(comment), &meta) == nil {
			t.Created, t.Fingerprint, t.Key = meta.Created, meta.Fingerprint, meta.Key
		}
		return t, nil
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: templates: %w", err)
	}
	return out, nil
}

// PruneTemplates drops every keel template database not named in keep and returns the names it
// dropped.
func PruneTemplates(ctx context.Context, admin string, keep []string) ([]string, error) {
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		return nil, fmt.Errorf("migrate: prune templates: connect: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	ts, err := listTemplates(ctx, c)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	var dropped []string
	for _, t := range ts {
		if kept[t.Name] {
			continue
		}
		if err := dropDatabase(ctx, c, t.Name); err != nil {
			return dropped, err
		}
		dropped = append(dropped, t.Name)
	}
	return dropped, nil
}

// ResetFromTemplate drops dbname, terminating its connections, and recreates it as a copy of
// template.
func ResetFromTemplate(ctx context.Context, admin, dbname, template string) error {
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		return fmt.Errorf("migrate: reset: connect: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var ok bool
	if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1 AND datistemplate)",
		template).Scan(&ok); err != nil {
		return fmt.Errorf("migrate: reset: %w", err)
	}
	if !ok {
		return fmt.Errorf("migrate: reset: %s is not a template database", template)
	}
	if err := dropDatabase(ctx, c, dbname); err != nil {
		return err
	}
	return createFrom(ctx, c, dbname, template)
}

// dbnamePattern finds a dbname keyword in a keyword/value DSN.
var dbnamePattern = regexp.MustCompile(`(^|\s)dbname=('(\\.|[^'])*'|\S*)`)

// withDatabase returns dsn with its database replaced by db.
func withDatabase(dsn, db string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("migrate: parse DSN: %w", err)
		}
		u.Path, u.RawPath = "/"+db, ""
		return u.String(), nil
	}
	if dbnamePattern.MatchString(dsn) {
		return dbnamePattern.ReplaceAllString(dsn, "${1}dbname="+db), nil
	}
	return strings.TrimSpace(dsn + " dbname=" + db), nil
}

// quoteLiteral quotes s as a SQL string literal.
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// randomHex returns n random bytes as hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
