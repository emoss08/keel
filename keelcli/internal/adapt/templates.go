package adapt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/seed"
)

// owns reports whether a template belongs to the application: its key is the application's name
// alone, or the name, a colon and the fingerprint of the reference seeds the template carries.
// Templates and PruneTemplates see only the application's own templates, so pruning never drops
// another application's, nor those a test harness built with keys of its own.
func (x *Migrations) owns(t migrate.TemplateInfo) bool {
	return t.Key == x.c.Name || strings.HasPrefix(t.Key, x.c.Name+":")
}

// references lists the reference seeds a template carries.
func (x *Migrations) references() []string {
	if x.c.Seeds == nil {
		return nil
	}
	var out []string
	for _, s := range x.c.Seeds.Seeds() {
		if s.Kind == seed.Reference {
			out = append(out, s.Name)
		}
	}
	return out
}

// key is the current template's key. The reference seeds' fingerprints come from a plan read on
// the admin database, so the key is known before the application's database exists.
func (x *Migrations) key(ctx context.Context, admin string) (string, error) {
	refs := x.references()
	if len(refs) == 0 {
		return x.c.Name, nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.seedKey != "" {
		return x.c.Name + ":" + x.seedKey, nil
	}
	fp, err := x.referenceFingerprint(ctx, admin, refs)
	if err != nil {
		return "", err
	}
	x.seedKey = fp
	return x.c.Name + ":" + fp, nil
}

// referenceFingerprint hashes the fingerprints of the reference seeds refs.
func (x *Migrations) referenceFingerprint(ctx context.Context, admin string, refs []string) (string, error) {
	db, err := keel.Open(ctx, keel.Config{DSN: admin, SkipEnums: true})
	if err != nil {
		return "", fmt.Errorf("keelcli: template key: %w", err)
	}
	defer db.Close()
	p, err := x.c.Seeds.Plan(ctx, db, seed.Options{Only: refs})
	if err != nil {
		return "", fmt.Errorf("keelcli: template key: %w", err)
	}
	h := sha256.New()
	for _, it := range p.Seeds {
		if it.Reason != seed.Excluded {
			fmt.Fprintf(h, "%s\x00%s\x00", it.Seed.Name, it.Fingerprint)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// templateState is the admin DSN, the current template's key and name, and the application's
// templates.
type templateState struct {
	admin   string
	key     string
	current string
	owned   []migrate.TemplateInfo
}

// templateState reads the template state.
func (x *Migrations) templateState(ctx context.Context) (templateState, error) {
	var st templateState
	set, err := x.set()
	if err != nil {
		return st, err
	}
	if st.admin, err = x.c.DB.Admin(); err != nil {
		return st, err
	}
	if st.key, err = x.key(ctx, st.admin); err != nil {
		return st, err
	}
	if st.current, err = migrate.TemplateName(set, st.key); err != nil {
		return st, err
	}
	all, err := migrate.Templates(ctx, st.admin)
	if err != nil {
		return st, err
	}
	for _, t := range all {
		if x.owns(t) {
			st.owned = append(st.owned, t)
		}
	}
	return st, nil
}

// port converts a template, marking the current one.
func (st templateState) port(t migrate.TemplateInfo) migrateport.Template {
	return migrateport.Template{Name: t.Name, Fingerprint: t.Fingerprint, Current: t.Name == st.current, Size: t.Size, CreatedAt: t.Created}
}

// find returns the application's template named name.
func (st templateState) find(name string) (migrate.TemplateInfo, bool) {
	i := slices.IndexFunc(st.owned, func(t migrate.TemplateInfo) bool { return t.Name == name })
	if i < 0 {
		return migrate.TemplateInfo{}, false
	}
	return st.owned[i], true
}

// Templates lists the application's template databases, the current one first.
func (x *Migrations) Templates(ctx context.Context) ([]migrateport.Template, error) {
	st, err := x.templateState(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]migrateport.Template, 0, len(st.owned))
	for _, t := range st.owned {
		out = append(out, st.port(t))
	}
	slices.SortStableFunc(out, func(a, b migrateport.Template) int {
		switch {
		case a.Current == b.Current:
			return b.CreatedAt.Compare(a.CreatedAt)
		case a.Current:
			return -1
		default:
			return 1
		}
	})
	return out, nil
}

// BuildTemplate builds the current template unless it exists: the migrations, then the
// reference seeds.
func (x *Migrations) BuildTemplate(ctx context.Context) (migrateport.Template, error) {
	st, err := x.build(ctx)
	if err != nil {
		return migrateport.Template{}, err
	}
	t, _ := st.find(st.current)
	return st.port(t), nil
}

// build builds the current template unless it exists, dropping the copy migrate.Template makes,
// and returns the state after.
func (x *Migrations) build(ctx context.Context) (templateState, error) {
	st, err := x.templateState(ctx)
	if err != nil {
		return st, err
	}
	if _, ok := st.find(st.current); ok {
		return st, nil
	}
	set, err := x.set()
	if err != nil {
		return st, err
	}
	o := migrate.TemplateOptions{Admin: st.admin, Sources: set, Options: x.options(nil), Key: st.key, Prefix: copyPrefix(x.c.Name)}
	if refs := x.references(); len(refs) > 0 {
		o.Prepare = x.prepare(refs)
	}
	_, drop, err := migrate.Template(ctx, o)
	if err != nil {
		return st, err
	}
	if err := drop(ctx); err != nil {
		return st, err
	}
	return x.templateState(ctx)
}

// prepare applies the reference seeds refs to a template being built.
func (x *Migrations) prepare(refs []string) func(ctx context.Context, dsn string) error {
	return func(ctx context.Context, dsn string) error {
		db, err := x.c.DB.OpenAt(ctx, dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		o := x.c.SeedOptions
		o.Only, o.Force, o.DryRun, o.Observer, o.Environment = refs, false, false, nil, ""
		_, err = x.c.Seeds.Apply(ctx, db, o)
		return err
	}
}

// copyPrefix starts the name of a database copied from the application's template: keel_, the
// name in lower case letters, digits and underscores, cut so the copy's name stays within
// Postgres's 63-byte identifiers, and an underscore.
func copyPrefix(name string) string {
	clean := strings.Map(func(r rune) rune {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return unicode.ToLower(r)
		}
		return '_'
	}, name)
	clean = strings.Trim(clean, "_")
	if len(clean) > maxPrefixName {
		clean = clean[:maxPrefixName]
	}
	return "keel_" + clean + "_"
}

// maxPrefixName bounds the name in a copy's prefix, leaving room for keel_ and migrate's random
// suffix.
const maxPrefixName = 40

// PruneTemplates drops the application's templates that are not current.
func (x *Migrations) PruneTemplates(ctx context.Context) ([]migrateport.Template, error) {
	st, err := x.templateState(ctx)
	if err != nil {
		return nil, err
	}
	var dropped []migrateport.Template
	for _, t := range st.owned {
		if t.Name == st.current {
			continue
		}
		if err := migrate.DropTemplate(ctx, st.admin, t.Name); err != nil {
			return dropped, err
		}
		dropped = append(dropped, st.port(t))
	}
	return dropped, nil
}

// Reset recreates the application's database from the current template, building the template
// first when needed, and keeps the database's environment mark.
func (x *Migrations) Reset(ctx context.Context) (migrateport.Reset, error) {
	start := x.c.Now()
	name, err := x.c.DB.Name()
	if err != nil {
		return migrateport.Reset{}, err
	}
	if name == "" || name == AdminDatabase {
		return migrateport.Reset{}, fmt.Errorf("keelcli: reset: refusing to recreate database %q", name)
	}
	before, err := x.templateState(ctx)
	if err != nil {
		return migrateport.Reset{}, err
	}
	_, existed := before.find(before.current)
	st, err := x.build(ctx)
	if err != nil {
		return migrateport.Reset{}, err
	}
	env, err := x.environment(ctx)
	if err != nil {
		return migrateport.Reset{}, err
	}
	x.c.DB.Refresh()
	if err := migrate.ResetFromTemplate(ctx, st.admin, name, st.current); err != nil {
		return migrateport.Reset{}, err
	}
	if err := x.remark(ctx, env); err != nil {
		return migrateport.Reset{}, err
	}
	return migrateport.Reset{Database: name, Template: st.current, Built: !existed, Duration: x.c.Now().Sub(start)}, nil
}

// environment reads the database's environment mark.
func (x *Migrations) environment(ctx context.Context) (seed.Env, error) {
	db, err := x.c.DB.DB(ctx)
	if err != nil {
		return "", err
	}
	env, err := seed.Environment(ctx, db)
	if err != nil {
		return "", fmt.Errorf("keelcli: read environment: %w", err)
	}
	return env, nil
}

// remark records env on the recreated database.
func (x *Migrations) remark(ctx context.Context, env seed.Env) error {
	if env == "" {
		return nil
	}
	db, err := x.c.DB.DB(ctx)
	if err != nil {
		return err
	}
	if err := seed.MarkEnvironment(ctx, db, env, false); err != nil {
		return errors.Join(errors.New("keelcli: reset: mark the environment again"), err)
	}
	return nil
}
