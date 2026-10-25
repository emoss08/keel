package schema

import (
	"fmt"
	"slices"
	"strings"
)

// FieldTag is a parsed `keel` tag of a struct field.
type FieldTag struct {
	Name     string
	Flags    ColFlag
	IDPrefix string
	TSVector bool
	TSConfig string
	Label    string
	Report   ReportTag

	Rel        RelKind
	RelKeys    []RelKey
	JoinTable  string
	JoinLocal  string
	JoinRemote string
	NoReport   bool
	PickPath   []string
	PickOrder  []PickRef
	PickLast   bool
	PickWhere  []PickCond
}

// RelKey is one column pair of a belongs, one or many relation as written: FK names the
// foreign-key column (this table's for belongs, the target's for one and many), Key the column it
// references on the other side, empty when the tag leaves it to keel gen (the primary key for the
// first pair, the column named like FK for the others).
type RelKey struct {
	FK  string
	Key string
}

// IsRelation reports whether the tag declares a relation.
func (t FieldTag) IsRelation() bool { return t.Rel != 0 }

// PickRef is one key of a pick tag's order as written: a segment of the path and a column of the
// table that segment reaches.
type PickRef struct {
	Segment string
	Column  string
}

// PickCond is one condition of a pick tag's where= option as written: the row a segment of the
// path reaches must hold one of Values in Column.
type PickCond struct {
	Segment string
	Column  string
	Values  []string
}

// ReportTag holds a column's report options as written in its tag. Zero values mean the option is
// absent and keel gen derives the fact.
type ReportTag struct {
	Exclude bool
	Include bool
	Kind    ReportKind
	Type    ValueType
	Aggs    Agg
	Format  string
}

// Set reports whether any report option is present.
func (r ReportTag) Set() bool { return r != ReportTag{} }

// TableTag is the parsed `keel` tag of the keel.Table marker.
type TableTag struct {
	Name     string
	Label    string
	Plural   string
	Report   bool
	Category string
}

// option is one comma-separated option of a tag: a key and, after = (or : for autoid), a value.
type option struct {
	key      string
	value    string
	hasValue bool
}

// splitOptions parses the options after a tag's head, refusing empty and repeated options.
func splitOptions(parts []string) ([]option, error) {
	opts := make([]option, 0, len(parts))
	seen := map[string]bool{}
	for _, raw := range parts {
		s := strings.TrimSpace(raw)
		if s == "" {
			return nil, fmt.Errorf("empty option")
		}
		o := option{key: s}
		if k, v, ok := strings.Cut(s, ":"); ok && k == "autoid" {
			o = option{key: k, value: strings.TrimSpace(v), hasValue: true}
		} else if k, v, ok := strings.Cut(s, "="); ok {
			o = option{key: strings.TrimSpace(k), value: strings.TrimSpace(v), hasValue: true}
			if o.key == "autoid" {
				return nil, fmt.Errorf("write autoid:<prefix>")
			}
		}
		if seen[o.key] {
			return nil, fmt.Errorf("option %s is repeated", o.key)
		}
		seen[o.key] = true
		opts = append(opts, o)
	}
	return opts, nil
}

// noValue refuses a value on an option that is a plain word.
func (o option) noValue() error {
	if o.hasValue {
		return fmt.Errorf("option %s takes no value", o.key)
	}
	return nil
}

// needValue returns the option's value, refusing an option written without one.
func (o option) needValue() (string, error) {
	if !o.hasValue || o.value == "" {
		sep := "="
		if o.key == "autoid" {
			sep = ":"
		}
		return "", fmt.Errorf("option %s needs a value (%s%s…)", o.key, o.key, sep)
	}
	return o.value, nil
}

// ParseTableTag parses the tag on a keel.Table marker field.
func ParseTableTag(tag string) (TableTag, error) {
	parts := strings.Split(tag, ",")
	tt := TableTag{Name: strings.TrimSpace(parts[0])}
	if err := checkIdent(tt.Name); err != nil {
		return TableTag{}, fmt.Errorf("keel tag %q: table name: %w", tag, err)
	}
	opts, err := splitOptions(parts[1:])
	if err != nil {
		return TableTag{}, fmt.Errorf("keel tag %q: %w", tag, err)
	}
	for _, o := range opts {
		if err := tt.apply(o); err != nil {
			return TableTag{}, fmt.Errorf("keel tag %q: %w", tag, err)
		}
	}
	return tt, nil
}

// apply sets one table option.
func (tt *TableTag) apply(o option) error {
	var err error
	switch o.key {
	case "report":
		tt.Report = true
		if o.hasValue {
			tt.Category, err = o.needValue()
		}
	case "label":
		tt.Label, err = o.needValue()
	case "plural":
		tt.Plural, err = o.needValue()
	default:
		err = fmt.Errorf("unknown table option %q", o.key)
	}
	return err
}

// ParseFieldTag parses the `keel` tag of a struct field. A tag of "-" or "" means the field is not
// stored; ok is false for it.
func ParseFieldTag(tag string) (ft FieldTag, ok bool, err error) {
	if tag == "" || tag == "-" {
		return FieldTag{}, false, nil
	}
	parts := strings.Split(tag, ",")
	head := strings.TrimSpace(parts[0])
	opts, err := splitOptions(parts[1:])
	if err == nil {
		if rel, key, isRel := parseRelation(head); isRel {
			err = ft.relation(rel, key, opts)
		} else {
			err = ft.column(head, opts)
		}
	}
	if err != nil {
		return FieldTag{}, false, fmt.Errorf("keel tag %q: %w", tag, err)
	}
	return ft, true, nil
}

// column fills a column tag from its name and options.
func (ft *FieldTag) column(name string, opts []option) error {
	if err := checkIdent(name); err != nil {
		return err
	}
	ft.Name = name
	for _, o := range opts {
		if err := ft.columnOption(o); err != nil {
			return err
		}
	}
	return ft.checkColumn()
}

// columnFlags are the column options that set a flag.
var columnFlags = map[string]ColFlag{
	"pk": FlagPK, "tenant": FlagTenant, "version": FlagVersion, "autocreate": FlagAutoCreate,
	"autoupdate": FlagAutoUpdate, "readonly": FlagReadOnly, "noselect": FlagNoSelect, "jsonb": FlagJSONB,
	"nullzero": FlagNullZero, "computed": FlagComputed, "jsonnumber": FlagJSONNumber,
	"default": FlagDefault,
}

// columnOption sets one column option.
func (ft *FieldTag) columnOption(o option) error {
	if f, ok := columnFlags[o.key]; ok {
		ft.Flags |= f
		return o.noValue()
	}
	switch o.key {
	case "tsvector":
		ft.TSVector = true
		if !o.hasValue {
			return nil
		}
		v, err := o.needValue()
		if err != nil {
			return err
		}
		ft.TSConfig = v
		return checkConfig(v)
	case "autoid":
		ft.Flags |= FlagAutoID
		v, err := o.needValue()
		if err != nil {
			return err
		}
		ft.IDPrefix = v
		return checkWord("autoid prefix", v)
	case "label":
		v, err := o.needValue()
		ft.Label = v
		return err
	}
	return ft.Report.apply(o)
}

// apply sets one report option of a column.
func (r *ReportTag) apply(o option) error {
	switch o.key {
	case "noreport":
		r.Exclude = true
		return o.noValue()
	case "report":
		r.Include = true
		return o.noValue()
	case "dimension", "measure":
		if r.Kind != 0 {
			return fmt.Errorf("dimension and measure exclude each other")
		}
		r.Kind = map[string]ReportKind{"dimension": Dimension, "measure": Measure}[o.key]
		return o.noValue()
	case "format":
		v, err := o.needValue()
		if err != nil {
			return err
		}
		r.Format = v
		return checkWord("format", v)
	case "type":
		return r.parseType(o)
	case "agg":
		return r.parseAggs(o)
	}
	return fmt.Errorf("unknown option %q", o.key)
}

// valueTypes are the value types a type= option may declare.
var valueTypes = map[string]ValueType{"epoch": ValueEpoch, "date": ValueDate, "decimal": ValueDecimal}

// parseType reads type=epoch|date|decimal.
func (r *ReportTag) parseType(o option) error {
	v, err := o.needValue()
	if err != nil {
		return err
	}
	t, ok := valueTypes[v]
	if !ok {
		return fmt.Errorf("type=%s: want epoch, date or decimal", v)
	}
	r.Type = t
	return nil
}

// parseAggs reads agg=a|b|….
func (r *ReportTag) parseAggs(o option) error {
	v, err := o.needValue()
	if err != nil {
		return err
	}
	for _, name := range strings.Split(v, "|") {
		a, ok := ParseAgg(strings.TrimSpace(name))
		if !ok {
			return fmt.Errorf("agg=%s: unknown aggregation %q (want count, count_distinct, sum, avg, min or max)",
				v, name)
		}
		if r.Aggs.Has(a) {
			return fmt.Errorf("agg=%s: %s is repeated", v, name)
		}
		r.Aggs |= a
	}
	return nil
}

// checkColumn refuses options that exclude each other.
func (ft *FieldTag) checkColumn() error {
	switch {
	case ft.Flags.Has(FlagReadOnly) && ft.Flags&(FlagPK|FlagTenant|FlagVersion|FlagAutoCreate|
		FlagAutoUpdate|FlagAutoID) != 0:
		return fmt.Errorf("readonly excludes write options")
	case ft.Flags.Has(FlagComputed) && ft.Flags&(FlagPK|FlagTenant|FlagVersion|FlagAutoCreate|FlagAutoUpdate|
		FlagAutoID|FlagReadOnly|FlagNoSelect) != 0:
		return fmt.Errorf("computed excludes pk, tenant, version, autoid, autocreate, autoupdate, readonly and noselect")
	case ft.Flags.Has(FlagNullZero) && ft.Flags&(FlagPK|FlagTenant|FlagVersion|FlagAutoCreate|FlagAutoUpdate|
		FlagAutoID) != 0:
		return fmt.Errorf("nullzero excludes pk, tenant, version, autoid, autocreate and autoupdate")
	case ft.Flags.Has(FlagDefault) && ft.Flags&(FlagPK|FlagTenant|FlagVersion|FlagAutoCreate|FlagAutoUpdate|
		FlagAutoID|FlagReadOnly|FlagComputed) != 0:
		return fmt.Errorf("default excludes pk, tenant, version, autoid, autocreate, autoupdate, readonly and computed")
	case ft.Flags.Has(FlagJSONB) && ft.TSVector:
		return fmt.Errorf("jsonb and tsvector exclude each other")
	case ft.Flags.Has(FlagJSONNumber) && !ft.Flags.Has(FlagJSONB):
		return fmt.Errorf("jsonnumber needs the jsonb option")
	case ft.Report.Include && ft.Report.Exclude:
		return fmt.Errorf("report and noreport exclude each other")
	}
	return nil
}

// relation fills a relation tag from its kind, key and options.
func (ft *FieldTag) relation(rel RelKind, key string, opts []option) error {
	ft.Rel = rel
	var err error
	switch rel {
	case ManyToMany:
		err = ft.joinTable(key)
	case Pick:
		err = ft.pickPath(key)
	default:
		err = ft.relKeys(key)
	}
	if err != nil {
		return err
	}
	for _, o := range opts {
		if err := ft.relationOption(o); err != nil {
			return err
		}
	}
	if rel == Pick && ft.PickOrder == nil {
		return fmt.Errorf("pick relations need first= or last=")
	}
	return nil
}

// relKeys reads the <fk>[=<key>]|… key of a belongs, one or many tag.
func (ft *FieldTag) relKeys(key string) error {
	for _, item := range strings.Split(key, "|") {
		fk, ref, explicit := strings.Cut(strings.TrimSpace(item), "=")
		if err := checkIdent(fk); err != nil {
			return err
		}
		if explicit {
			if err := checkIdent(ref); err != nil {
				return err
			}
		}
		for _, k := range ft.RelKeys {
			if k.FK == fk {
				return fmt.Errorf("relation key %s is repeated", fk)
			}
		}
		ft.RelKeys = append(ft.RelKeys, RelKey{FK: fk, Key: ref})
	}
	return nil
}

// joinTable reads the <join_table>.<local_fk>.<remote_fk> key of a many-to-many tag.
func (ft *FieldTag) joinTable(key string) error {
	segs := strings.Split(key, ".")
	if len(segs) != 3 {
		return fmt.Errorf("m2m needs m2m:<join_table>.<local_fk>.<remote_fk>")
	}
	for _, s := range segs {
		if err := checkIdent(s); err != nil {
			return err
		}
	}
	ft.JoinTable, ft.JoinLocal, ft.JoinRemote = segs[0], segs[1], segs[2]
	return nil
}

// pickPath reads the <relation>.<relation>… path of a pick tag.
func (ft *FieldTag) pickPath(key string) error {
	for _, s := range strings.Split(key, ".") {
		if err := checkIdent(s); err != nil {
			return fmt.Errorf("pick path: %w", err)
		}
		ft.PickPath = append(ft.PickPath, s)
	}
	return nil
}

// relationOption sets one relation option.
func (ft *FieldTag) relationOption(o option) error {
	switch o.key {
	case "label":
		v, err := o.needValue()
		ft.Label = v
		return err
	case "noreport":
		ft.NoReport = true
		return o.noValue()
	case "first", "last":
		if ft.Rel != Pick {
			return fmt.Errorf("option %s is only for pick relations", o.key)
		}
		if ft.PickOrder != nil {
			return fmt.Errorf("first= and last= exclude each other")
		}
		ft.PickLast = o.key == "last"
		return ft.pickOrder(o)
	case "where":
		if ft.Rel != Pick {
			return fmt.Errorf("option where is only for pick relations")
		}
		if ft.PickWhere != nil {
			return fmt.Errorf("option where is repeated")
		}
		return ft.pickWhere(o)
	}
	return fmt.Errorf("relation option %q: relations take label=, noreport and, on pick, first= or last= "+
		"and where=", o.key)
}

// pickOrder reads the <segment>.<column>|… order of a pick tag.
func (ft *FieldTag) pickOrder(o option) error {
	v, err := o.needValue()
	if err != nil {
		return err
	}
	for _, item := range strings.Split(v, "|") {
		seg, col, found := strings.Cut(strings.TrimSpace(item), ".")
		if !found {
			return fmt.Errorf("%s=%s: %q must be <segment>.<column>", o.key, v, item)
		}
		if err := checkIdent(seg); err != nil {
			return fmt.Errorf("%s=%s: %w", o.key, v, err)
		}
		if err := checkIdent(col); err != nil {
			return fmt.Errorf("%s=%s: %w", o.key, v, err)
		}
		if !slices.Contains(ft.PickPath, seg) {
			return fmt.Errorf("%s=%s: %s is not a segment of the path %s", o.key, v, seg,
				strings.Join(ft.PickPath, "."))
		}
		ft.PickOrder = append(ft.PickOrder, PickRef{Segment: seg, Column: col})
	}
	return nil
}

// pickWhere reads the <segment>.<column>:<value>;<value>|… conditions of a pick tag.
func (ft *FieldTag) pickWhere(o option) error {
	v, err := o.needValue()
	if err != nil {
		return err
	}
	for _, item := range strings.Split(v, "|") {
		ref, values, found := strings.Cut(strings.TrimSpace(item), ":")
		if !found || values == "" {
			return fmt.Errorf("where=%s: %q must be <segment>.<column>:<value>[;<value>…]", v, item)
		}
		seg, col, found := strings.Cut(ref, ".")
		if !found {
			return fmt.Errorf("where=%s: %q must be <segment>.<column>", v, ref)
		}
		if err := checkIdent(seg); err != nil {
			return fmt.Errorf("where=%s: %w", v, err)
		}
		if err := checkIdent(col); err != nil {
			return fmt.Errorf("where=%s: %w", v, err)
		}
		if !slices.Contains(ft.PickPath, seg) {
			return fmt.Errorf("where=%s: %s is not a segment of the path %s", v, seg,
				strings.Join(ft.PickPath, "."))
		}
		cond := PickCond{Segment: seg, Column: col}
		for _, val := range strings.Split(values, ";") {
			if val == "" {
				return fmt.Errorf("where=%s: %s.%s has an empty value", v, seg, col)
			}
			cond.Values = append(cond.Values, val)
		}
		ft.PickWhere = append(ft.PickWhere, cond)
	}
	return nil
}

// parseRelation splits a relation head (belongs:customer_id) into its kind and key.
func parseRelation(head string) (RelKind, string, bool) {
	name, key, found := strings.Cut(head, ":")
	if !found {
		return 0, "", false
	}
	switch name {
	case "belongs":
		return BelongsTo, key, true
	case "one":
		return HasOne, key, true
	case "many":
		return HasMany, key, true
	case "m2m":
		return ManyToMany, key, true
	case "pick":
		return Pick, key, true
	}
	return 0, "", false
}

// checkIdent refuses a name that is not a lower-case SQL identifier.
func checkIdent(s string) error {
	if s == "" {
		return fmt.Errorf("empty identifier")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return fmt.Errorf("identifier %q must match [a-z0-9_]+", s)
		}
	}
	if s[0] >= '0' && s[0] <= '9' {
		return fmt.Errorf("identifier %q must not start with a digit", s)
	}
	return nil
}

// checkConfig refuses a text search configuration name that is not an identifier, optionally
// qualified by one schema.
func checkConfig(name string) error {
	schemaName, cfg, qualified := strings.Cut(name, ".")
	if qualified {
		if err := checkIdent(schemaName); err != nil {
			return fmt.Errorf("tsvector=%s: %w", name, err)
		}
		name = cfg
	}
	if err := checkIdent(name); err != nil {
		return fmt.Errorf("tsvector=%s: %w", name, err)
	}
	return nil
}

// checkWord refuses a value with characters other than letters, digits, '_', '-', '.' and ':'.
func checkWord(what, s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("_-.:", rune(c)) {
			return fmt.Errorf("%s %q must match [A-Za-z0-9_.:-]+", what, s)
		}
	}
	return nil
}
