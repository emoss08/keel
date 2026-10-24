package gen

import (
	"fmt"
	"go/constant"
	"go/types"
	"sort"
	"strings"

	"github.com/emoss08/keel/schema"
)

// decimalTypes are the scanner types known to hold decimal numbers, by package path.
var decimalTypes = map[string][]string{
	"github.com/shopspring/decimal":  {"Decimal", "NullDecimal"},
	"github.com/jackc/pgx/v5/pgtype": {"Numeric"},
}

// valueTypeNames name value types in error messages.
var valueTypeNames = map[schema.ValueType]string{
	schema.ValueOther: "other", schema.ValueString: "string", schema.ValueEnum: "enum",
	schema.ValueInt: "integer", schema.ValueFloat: "float", schema.ValueDecimal: "decimal",
	schema.ValueBool: "bool", schema.ValueTime: "time", schema.ValueDate: "date", schema.ValueEpoch: "epoch",
	schema.ValueJSON: "jsonb", schema.ValueArray: "array", schema.ValueBytes: "bytea",
	schema.ValueTSVector: "tsvector",
}

// readTypeFacts records what the column's named type declares: its constants as enum values and
// the format its KeelFormat method returns.
func (rd *reader) readTypeFacts(c *column) error {
	n := c.named
	if n == nil {
		return nil
	}
	if c.kind == schema.KindString || c.kind == schema.KindInt64 || c.kind == schema.KindInt32 ||
		c.kind == schema.KindInt16 {
		values, err := rd.enumValues(n)
		if err != nil {
			return err
		}
		if len(values) > 0 {
			c.enum, c.values = n, values
		}
	}
	format, err := rd.formatOf(n)
	c.format = format
	return err
}

// enumValues returns the constants of type n declared in its package, in declaration order, with
// labels derived from their names without the type's name.
func (rd *reader) enumValues(n *types.Named) ([]schema.EnumValue, error) {
	if v, ok := rd.enums[n]; ok {
		return v, nil
	}
	scope := n.Obj().Pkg().Scope()
	var consts []*types.Const
	for _, name := range scope.Names() {
		if c, ok := scope.Lookup(name).(*types.Const); ok && types.Identical(c.Type(), n) {
			consts = append(consts, c)
		}
	}
	sort.SliceStable(consts, func(i, j int) bool { return rd.src.before(consts[i].Pos(), consts[j].Pos()) })
	values := make([]schema.EnumValue, 0, len(consts))
	for _, c := range consts {
		doc, err := rd.src.doc(c.Pos(), c.Name())
		if err != nil {
			return nil, err
		}
		values = append(values, schema.EnumValue{Value: constValue(c.Val()), Label: rd.enumLabel(n, c), Doc: doc})
	}
	rd.enums[n] = values
	return values, nil
}

// enumLabel derives a constant's label from its name without the type's name as a prefix.
func (rd *reader) enumLabel(n *types.Named, c *types.Const) string {
	name := c.Name()
	if rest := strings.TrimPrefix(name, n.Obj().Name()); rest != "" && rest != name {
		name = rest
	}
	return rd.labels.label(name)
}

// constValue writes a constant's value as an enum value: a string as it is, an integer in decimal.
func constValue(v constant.Value) string {
	if v.Kind() == constant.String {
		return constant.StringVal(v)
	}
	return v.ExactString()
}

// formatOf returns the literal the KeelFormat method of n (value or pointer receiver) returns, or
// "" when n has none.
func (rd *reader) formatOf(n *types.Named) (string, error) {
	if v, ok := rd.formats[n]; ok {
		return v, nil
	}
	sel := types.NewMethodSet(types.NewPointer(n)).Lookup(n.Obj().Pkg(), "KeelFormat")
	if sel == nil {
		rd.formats[n] = ""
		return "", nil
	}
	fn := sel.Obj().(*types.Func)
	sig := fn.Type().(*types.Signature)
	if sig.Params().Len() != 0 || sig.Results().Len() != 1 ||
		!types.Identical(sig.Results().At(0).Type(), types.Typ[types.String]) {
		return "", rd.src.errorf(fn.Pos(), "%s.KeelFormat must be func() string", n.Obj().Name())
	}
	v, err := rd.src.formatOf(fn)
	if err != nil {
		return "", err
	}
	rd.formats[n] = v
	return v, nil
}

// resolveReports marks key columns, then derives the report facts of every column of every
// reportable model and refuses report options elsewhere.
func resolveReports(src *source, models []*model) error {
	markKeys(models)
	for _, m := range models {
		if err := resolveReport(src, m); err != nil {
			return err
		}
	}
	return nil
}

// markKeys marks primary keys and the foreign keys of relations as keys, and the foreign keys a
// reported belongs-to relation exposes.
func markKeys(models []*model) {
	for _, m := range models {
		for _, c := range m.columns {
			c.key = c.key || c.tag.Flags.Has(schema.FlagPK)
		}
		for _, r := range m.rels {
			switch r.kind {
			case schema.BelongsTo:
				for _, l := range r.local {
					c := m.columns[l]
					c.key = true
					c.exposed = c.exposed || !r.tag.NoReport
				}
			case schema.HasOne, schema.HasMany:
				for _, rc := range r.remote {
					r.target.columns[rc].key = true
				}
			}
		}
	}
}

// resolveReport derives the report facts of m's columns.
func resolveReport(src *source, m *model) error {
	for _, c := range m.columns {
		if m.report == nil {
			if c.tag.Report.Set() {
				return src.errorf(c.pos, "%s.%s: report options need report on %s's table tag", m.name, c.field, m.name)
			}
			continue
		}
		ri, err := columnReport(c)
		if err != nil {
			return src.errorf(c.pos, "%s.%s: %v", m.name, c.field, err)
		}
		c.report = ri
	}
	if m.report == nil {
		for _, r := range m.rels {
			if r.tag.NoReport {
				return src.errorf(r.pos, "%s.%s: noreport needs report on %s's table tag", m.name, r.field, m.name)
			}
		}
	}
	return nil
}

// columnReport derives a column's report facts from its type, name, keys and tag options.
func columnReport(c *column) (*schema.ReportInfo, error) {
	t, err := valueType(c)
	if err != nil {
		return nil, err
	}
	kind, err := reportKind(c, t)
	if err != nil {
		return nil, err
	}
	aggs, err := reportAggs(c, t)
	if err != nil {
		return nil, err
	}
	format := c.format
	if c.tag.Report.Format != "" {
		format = c.tag.Report.Format
	}
	return &schema.ReportInfo{Included: included(c), Kind: kind, Type: t, Aggs: aggs, Format: format}, nil
}

// valueType derives the value type of a column, or checks the one its type= option declares.
func valueType(c *column) (schema.ValueType, error) {
	switch c.tag.Report.Type {
	case schema.ValueEpoch:
		if c.kind != schema.KindInt64 {
			return 0, fmt.Errorf("type=epoch needs an int64 column")
		}
		return schema.ValueEpoch, nil
	case schema.ValueDate:
		if c.kind != schema.KindTime {
			return 0, fmt.Errorf("type=date needs a time.Time column")
		}
		return schema.ValueDate, nil
	case schema.ValueDecimal:
		if !c.scanner && c.kind != schema.KindString {
			return 0, fmt.Errorf("type=decimal needs a sql.Scanner or string column")
		}
		return schema.ValueDecimal, nil
	}
	return derivedType(c), nil
}

// derivedType is the value type of a column's Go kind: enums by their constants, int64 columns
// named *_at, *_date or *_time as epochs, known decimal scanners as decimals.
func derivedType(c *column) schema.ValueType {
	switch c.kind {
	case schema.KindString:
		if c.enum != nil {
			return schema.ValueEnum
		}
		return schema.ValueString
	case schema.KindInt64, schema.KindInt32, schema.KindInt16:
		switch {
		case c.enum != nil:
			return schema.ValueEnum
		case c.kind == schema.KindInt64 && epochName(c.tag.Name):
			return schema.ValueEpoch
		}
		return schema.ValueInt
	case schema.KindFloat64, schema.KindFloat32:
		return schema.ValueFloat
	case schema.KindOther:
		if isDecimal(c.named) {
			return schema.ValueDecimal
		}
		return schema.ValueOther
	}
	return map[schema.Kind]schema.ValueType{
		schema.KindBool: schema.ValueBool, schema.KindTime: schema.ValueTime, schema.KindJSON: schema.ValueJSON,
		schema.KindStringArray: schema.ValueArray, schema.KindInt64Array: schema.ValueArray,
		schema.KindBytes: schema.ValueBytes, schema.KindTSVector: schema.ValueTSVector,
	}[c.kind]
}

// epochName reports whether a column name says it holds a point in time.
func epochName(name string) bool {
	return strings.HasSuffix(name, "_at") || strings.HasSuffix(name, "_date") || strings.HasSuffix(name, "_time")
}

// isDecimal reports whether n is a known decimal type.
func isDecimal(n *types.Named) bool {
	if n == nil || n.Obj().Pkg() == nil {
		return false
	}
	for _, name := range decimalTypes[n.Obj().Pkg().Path()] {
		if name == n.Obj().Name() {
			return true
		}
	}
	return false
}

// included reports whether reports offer the column: every column except tenant, version,
// noselect, bytea and tsvector columns and foreign keys a reported relation exposes, unless report
// or noreport says otherwise.
func included(c *column) bool {
	switch {
	case c.tag.Report.Include && !c.tag.Flags.Has(schema.FlagComputed):
		return true
	case c.tag.Report.Exclude:
		return false
	}
	f := c.tag.Flags
	if f.Has(schema.FlagComputed) {
		return false
	}
	return !f.Has(schema.FlagTenant) && !f.Has(schema.FlagVersion) && !f.Has(schema.FlagNoSelect) &&
		c.kind != schema.KindBytes && c.kind != schema.KindTSVector && !c.exposed
}

// reportKind makes numeric columns that are neither keys nor enums measures and everything else
// a dimension, unless dimension or measure says otherwise.
func reportKind(c *column, t schema.ValueType) (schema.ReportKind, error) {
	switch c.tag.Report.Kind {
	case schema.Measure:
		if !t.Numeric() {
			return 0, fmt.Errorf("measure needs a numeric column, not %s", valueTypeNames[t])
		}
		return schema.Measure, nil
	case schema.Dimension:
		return schema.Dimension, nil
	}
	if t.Numeric() && !c.key {
		return schema.Measure, nil
	}
	return schema.Dimension, nil
}

// reportAggs derives the aggregations of a value type, or checks the ones agg= lists against it.
func reportAggs(c *column, t schema.ValueType) (schema.Agg, error) {
	def := schema.AggsCount
	switch {
	case t.Numeric():
		def = schema.AggsNumeric
	case t.Ordered():
		def = schema.AggsOrdered
	}
	want := c.tag.Report.Aggs
	if want == 0 {
		return def, nil
	}
	if extra := want &^ def; extra != 0 {
		return 0, fmt.Errorf("agg=%s does not apply to %s values", strings.Join(extra.Names(), "|"),
			valueTypeNames[t])
	}
	return want, nil
}
