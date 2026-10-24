package formula

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type testEnv struct {
	columns map[string]ColumnInfo
	fields  map[string]Type
	params  map[string]ParamInfo
}

func (e testEnv) Column(id string) (ColumnInfo, bool) {
	c, ok := e.columns[id]
	return c, ok
}

func (e testEnv) ColumnIDs() []string { return keys(e.columns) }

func (e testEnv) Field(path string) (Type, []string, bool) {
	t, ok := e.fields[path]
	return t, keys(e.fields), ok
}

func (e testEnv) Param(name string) (ParamInfo, bool) {
	p, ok := e.params[name]
	return p, ok
}

func (e testEnv) ParamNames() []string { return keys(e.params) }

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func env() testEnv {
	return testEnv{
		columns: map[string]ColumnInfo{
			"revenue":  {Type: Decimal, Level: Aggregate},
			"miles":    {Type: Int, Level: Aggregate},
			"loads":    {Type: Int, Level: Aggregate},
			"customer": {Type: String, Level: Row},
			"month":    {Type: Timestamp, Level: Row},
			"status":   {Type: Enum, Level: Row},
			"share":    {Type: Decimal, Level: Window},
			"ratio":    {Type: Float, Level: Aggregate},
		},
		fields: map[string]Type{
			"customer.name": String,
			"shippedAt":     Epoch,
			"deliveredOn":   Date,
			"pickedUpAt":    Timestamp,
			"weight":        Int,
			"totalCharge":   Decimal,
			"status":        Enum,
			"hazmat":        Bool,
			"meta":          JSON,
			"customerId":    Ref,
		},
		params: map[string]ParamInfo{
			"window":   {Type: Int},
			"statuses": {Type: Enum, Multi: true},
			"label":    {Type: String},
		},
	}
}

func mustParse(t *testing.T, src string) *Node {
	t.Helper()
	n, err := Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return n
}

func TestParsePrintsCanonically(t *testing.T) {
	cases := map[string]string{
		"round([revenue]/nullif([miles],0),2)": "round([revenue] / nullif([miles], 0), 2)",
		"1 + 2 * 3":                            "1 + 2 * 3",
		"(1 + 2) * 3":                          "(1 + 2) * 3",
		"1 - (2 - 3)":                          "1 - (2 - 3)",
		"(1 - 2) - 3":                          "1 - 2 - 3",
		"- - 1":                                "-(-1)",
		"-(1 + 2)":                             "-(1 + 2)",
		"+5":                                   "5",
		"NOT a_b(1) = 2":                       "",
		"not [customer] = 'x' AND [miles] > 0": "not [customer] = 'x' and [miles] > 0",
		"[a] = (not true)":                     "[a] = (not true)",
		"(not true) = [a]":                     "(not true) = [a]",
		"[a] IS NOT NULL or [b] is null":       "[a] is not null or [b] is null",
		"([a] = 1) is null":                    "([a] = 1) is null",
		"'it''s' || {customer.name}":           "'it''s' || {customer.name}",
		"CASE WHEN [miles] > 0 THEN 1 ELSE 0 END":           "case when [miles] > 0 then 1 else 0 end",
		"case [status] when 'A' then 1 when 'B' then 2 end": "case [status] when 'A' then 1 when 'B' then 2 end",
		"date '2026-01-31' < {deliveredOn}":                 "date '2026-01-31' < {deliveredOn}",
		"timestamp '2026-01-31T08:00:00+02:00' < now()":     "timestamp '2026-01-31T08:00:00+02:00' < now()",
		"007.50 + .5":           "7.50 + 0.5",
		"Coalesce(@label, 'x')": "coalesce(@label, 'x')",
		"[a] <> 1":              "[a] != 1",
		"1 * (2 / 3) % 4":       "1 * (2 / 3) % 4",
		"a_b":                   "",
	}
	for src, want := range cases {
		n, err := Parse(src)
		if want == "" {
			if err == nil {
				t.Errorf("%q parsed as %q, want an error", src, Format(n))
			}
			continue
		}
		if err != nil {
			t.Errorf("parse %q: %v", src, err)
			continue
		}
		if got := Format(n); got != want {
			t.Errorf("format(parse(%q)) = %q, want %q", src, got, want)
		}
		again := mustParse(t, Format(n))
		if !Equal(n, again) {
			t.Errorf("reparse of %q changed the tree", src)
		}
	}
}

func TestParseErrorsPointAtTheProblem(t *testing.T) {
	cases := []struct {
		src, msg  string
		line, col int
		excerpt   string
	}{
		{"round([revenue], 2", "expected , or ), found the end of the formula", 1, 19, ""},
		{"rnd([revenue])", "unknown function rnd; did you mean round?", 1, 1, "rnd([revenue])\n^^^"},
		{"1 +\n  * 2", "expected a value, found \"*\"", 2, 3, "  * 2\n  ^"},
		{"'open", "this string is never closed", 1, 1, ""},
		{"[a] == 1", "use = to compare, not ==", 1, 5, ""},
		{"[a] && [b]", "use and, not &&", 1, 5, ""},
		{"\"x\"", "strings use single quotes", 1, 1, ""},
		{"1 < 2 < 3", "comparisons cannot be chained", 1, 7, ""},
		{"1e5", "scientific notation", 1, 1, ""},
		{"round(1, 2, 3)", "round takes 1 to 2 arguments, not 3", 1, 1, ""},
		{"[bad name]", "\" \" cannot appear in a column reference", 1, 5, ""},
		{"{a..b}", "field reference is empty or malformed", 1, 1, ""},
		{"date '2026-13-01'", "a date is written as YYYY-MM-DD", 1, 6, ""},
		{"case end", "expected when", 1, 6, ""},
		{"", "the formula is empty", 1, 1, ""},
		{"revenue", "unknown name revenue; columns are written [id]", 1, 1, ""},
		{"@", "@ must be followed by a parameter name", 1, 1, ""},
		{"1 is 2", "expected null after is", 1, 6, ""},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		var es Errors
		if !errors.As(err, &es) || len(es) != 1 {
			t.Errorf("%q: want one error, got %v", c.src, err)
			continue
		}
		e := es[0]
		if !strings.Contains(e.Message, c.msg) || e.Pos.Line != c.line || e.Pos.Column != c.col {
			t.Errorf("%q: got %d:%d %q, want %d:%d containing %q", c.src, e.Pos.Line, e.Pos.Column, e.Message, c.line, c.col, c.msg)
		}
		if c.excerpt != "" && e.Excerpt(c.src) != c.excerpt {
			t.Errorf("%q: excerpt\n%s\nwant\n%s", c.src, e.Excerpt(c.src), c.excerpt)
		}
	}
}

func TestLimits(t *testing.T) {
	if _, err := ParseWith(strings.Repeat("(", 70)+"1"+strings.Repeat(")", 70), Limits{}); err == nil || !strings.Contains(err.Error(), "nest") {
		t.Errorf("deep nesting: %v", err)
	}
	if _, err := ParseWith(strings.Repeat("1 + ", 40)+"1", Limits{MaxNodes: 50}); err == nil || !strings.Contains(err.Error(), "terms") {
		t.Errorf("too many terms: %v", err)
	}
	if _, err := ParseWith(strings.Repeat("1 + ", 40)+"1", Limits{MaxDepth: 10}); err == nil {
		t.Error("a long chain is deeper than 10")
	}
	if _, err := ParseWith(strings.Repeat("1", 50), Limits{MaxLength: 10}); err == nil || !strings.Contains(err.Error(), "characters") {
		t.Errorf("too long: %v", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	srcs := []string{
		"round([revenue] / nullif([miles], 0), 2)",
		"case [status] when 'A' then 1 else null end",
		"case when true then date '2026-01-01' end",
		"-@window + count()",
		"not {hazmat} and {customer.name} is not null",
		"timestamp '2026-01-31T08:00:00Z' > now()",
	}
	for _, src := range srcs {
		n := mustParse(t, src)
		data, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		var back Node
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if err := Validate(&back, Limits{}); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if !Equal(n, &back) {
			t.Errorf("%q: JSON round trip changed the tree: %s", src, data)
		}
	}
}

func TestJSONShape(t *testing.T) {
	n := mustParse(t, "[revenue] / [miles]")
	data, _ := json.Marshal(n)
	want := `{"op":"/","args":[{"column":"revenue"},{"column":"miles"}]}`
	if string(data) != want {
		t.Errorf("got %s, want %s", data, want)
	}
}

func TestJSONRejectsMalformedNodes(t *testing.T) {
	cases := map[string]string{
		`{"number":"-1"}`:                                 "canonical form",
		`{"number":"01"}`:                                 "canonical form",
		`{"column":"a b"}`:                                "not a column id",
		`{"call":"rnd","args":[]}`:                        "did you mean round",
		`{"call":"round","args":[]}`:                      "takes 1 to 2 arguments",
		`{"op":"^","args":[{"null":true},{"null":true}]}`: "not an operator",
		`{"op":"not","args":[]}`:                          "not an operator",
		`{"date":"2026-1-1"}`:                             "canonical date",
		`{"case":{"when":[]}}`:                            "at least one when",
		`{"op":"+","args":[{"number":"1"},{"wat":1}]}`:    "unknown field",
		`{"column":"a","field":"b"}`:                      "exactly one of",
		`{"string":"x","args":[]}`:                        "only op and call",
	}
	for src, want := range cases {
		var n Node
		err := json.Unmarshal([]byte(src), &n)
		if err == nil {
			err = Validate(&n, Limits{})
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", src, err, want)
		}
	}
	var n Node
	_ = json.Unmarshal([]byte(`{"op":"+","args":[{"number":"1"},{"column":"a b"}]}`), &n)
	var es Errors
	if err := Validate(&n, Limits{}); !errors.As(err, &es) || es[0].Pointer != "/args/1" {
		t.Errorf("pointer: %v", err)
	}
}

func TestExprJSON(t *testing.T) {
	var e Expr
	if err := json.Unmarshal([]byte(`"[a] + 1"`), &e); err != nil || e.Text != "[a] + 1" {
		t.Fatalf("text: %v %+v", err, e)
	}
	out, _ := json.Marshal(e)
	if string(out) != `"[a] + 1"` {
		t.Errorf("text re-encodes as %s", out)
	}
	if err := json.Unmarshal([]byte(`{"op":"+","args":[{"column":"a"},{"number":"1"}]}`), &e); err != nil || e.Node == nil {
		t.Fatalf("ast: %v", err)
	}
	if e.String() != "[a] + 1" {
		t.Errorf("String = %q", e.String())
	}
	out, _ = json.Marshal(e)
	if string(out) != `{"op":"+","args":[{"column":"a"},{"number":"1"}]}` {
		t.Errorf("ast re-encodes as %s", out)
	}
	if err := json.Unmarshal([]byte(`12`), &e); err == nil {
		t.Error("a number is not a formula")
	}
	if _, err := Of("1 +").Tree(Limits{}); err == nil {
		t.Error("bad text parses")
	}
	if Of("1+1").String() != "1 + 1" || Of("1 +").String() != "1 +" {
		t.Error("String of text")
	}
}

func check(t *testing.T, src string) (*Typed, error) {
	t.Helper()
	return Check(mustParse(t, src), env())
}

func TestCheckTypes(t *testing.T) {
	cases := []struct {
		src   string
		typ   Type
		level Level
	}{
		{"[revenue] / [miles]", Decimal, Aggregate},
		{"[miles] / [loads]", Decimal, Aggregate},
		{"[miles] + [loads]", Int, Aggregate},
		{"[miles] % 7", Int, Aggregate},
		{"[ratio] * [revenue]", Float, Aggregate},
		{"round([revenue] / nullif([miles], 0), 2)", Decimal, Aggregate},
		{"round(1)", Decimal, Constant},
		{"floor({weight} / 1000)", Decimal, Row},
		{"floor([ratio])", Float, Aggregate},
		{"abs({weight})", Int, Row},
		{"{customer.name} || ' ' || 7", String, Row},
		{"concat({customer.name}, {weight})", String, Row},
		{"coalesce({weight}, 0.5)", Decimal, Row},
		{"coalesce(null, 'x')", String, Constant},
		{"if({hazmat}, 'yes', 'no')", String, Row},
		{"case when {weight} > 10 then 1 when {weight} > 5 then 2.5 end", Decimal, Row},
		{"case {status} when 'A' then 1 else 0 end", Int, Row},
		{"date_trunc('month', {shippedAt})", Timestamp, Row},
		{"date_diff('day', {pickedUpAt}, {deliveredOn})", Int, Row},
		{"extract('dow', {shippedAt})", Int, Row},
		{"today() > {deliveredOn}", Bool, Row},
		{"{shippedAt} < now()", Bool, Row},
		{"{status} = 'Open'", Bool, Row},
		{"{customerId} = 'cus_1'", Bool, Row},
		{"lower({status})", String, Row},
		{"length({customer.name})", Int, Row},
		{"sum({weight})", Int, Aggregate},
		{"sum({totalCharge}) / count()", Decimal, Aggregate},
		{"avg({weight})", Decimal, Aggregate},
		{"count_distinct({customer.name})", Int, Aggregate},
		{"max({shippedAt})", Epoch, Aggregate},
		{"[customer] || ': ' || [revenue]", String, Aggregate},
		{"if([status] = 'A', [revenue], 0)", Decimal, Aggregate},
		{"[share] * 100", Decimal, Window},
		{"@window * 2", Int, Constant},
		{"greatest({weight}, 10, 2.5)", Decimal, Row},
		{"{meta} is null", Bool, Row},
		{"-{weight}", Int, Row},
		{"99999999999999999999 + 1", Decimal, Constant},
	}
	for _, c := range cases {
		typed, err := check(t, c.src)
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if typed.Type != c.typ || typed.Level != c.level {
			t.Errorf("%q: got %s/%s, want %s/%s", c.src, typed.Type, typed.Level, c.typ, c.level)
		}
	}
}

func TestCheckErrors(t *testing.T) {
	cases := map[string]string{
		"[revnue] + 1":                    "unknown column [revnue]; did you mean [revenue]?",
		"{customer.nam}":                  "unknown field {customer.nam}; did you mean {customer.name}?",
		"@windw":                          "unknown parameter @windw; did you mean @window?",
		"@statuses = 'A'":                 "takes several values",
		"{customer.name} + 1":             "+ needs numbers, but this is string",
		"{shippedAt} - {pickedUpAt}":      "use date_diff",
		"[ratio] % 2":                     "% needs int or decimal",
		"{weight} = null":                 "use is null",
		"{hazmat} < true":                 "< compares numbers, text and dates, not bool and bool",
		"{weight} = 'x'":                  "= cannot compare int with string",
		"{meta} = {meta}":                 "cannot compare json",
		"not 1":                           "not needs bool",
		"-'x'":                            "- negates numbers",
		"1 and true":                      "and needs bool",
		"round({customer.name})":          "number of round must be a number",
		"round(1, [miles])":               "digits of round must be a whole number",
		"round(1, 11)":                    "from 0 to 10",
		"date_trunc('mnth', {shippedAt})": "did you mean 'month'?",
		"date_trunc({customer.name}, {shippedAt})": "unit of date_trunc must be one of",
		"date_diff('day', 1, {shippedAt})":         "start of date_diff must be a date or time",
		"lower({weight})":                          "text of lower must be text",
		"if(1, 2, 3)":                              "condition of if needs bool",
		"if(true, 1, 'x')":                         "mixes int with string",
		"case when 1 then 2 end":                   "a when condition needs bool",
		"case {weight} when 'x' then 1 end":        "cannot compare int with string",
		"min({hazmat})":                            "value of min must be a number, text or date",
		"[revenue] / {weight}":                     "{weight} is a value per row, but this calculation combines it with values per group",
		"sum([revenue])":                           "sum already works on groups",
		"greatest({meta}, {meta})":                 "must be a number, text or date",
		"sum({customer.name})":                     "number of sum must be a number",
	}
	for src, want := range cases {
		_, err := check(t, src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", src, err, want)
		}
	}
}

func TestCheckReportsEveryError(t *testing.T) {
	_, err := check(t, "[nope] + {nada} + @zip")
	var es Errors
	if !errors.As(err, &es) || len(es) != 3 {
		t.Fatalf("want 3 errors, got %v", err)
	}
	if es[1].Code != CodeUnknownField || es[1].Pos.Column != 10 || es[1].End.Column != 16 {
		t.Errorf("field error span: %+v", es[1])
	}
	desc := es.Describe("[nope] + {nada} + @zip")
	if !strings.Contains(desc, "         ^^^^^^") {
		t.Errorf("describe:\n%s", desc)
	}
}

func TestCoercionsAreRecorded(t *testing.T) {
	typed, err := check(t, "[miles] / [loads]")
	if err != nil {
		t.Fatal(err)
	}
	if typed.Args[0].Coerce != Decimal || typed.Args[1].Coerce != Decimal {
		t.Errorf("int / int converts both to decimal: %s %s", typed.Args[0].Coerce, typed.Args[1].Coerce)
	}
	typed, _ = check(t, "date_trunc('week', {shippedAt})")
	if typed.Args[1].Coerce != Timestamp || typed.Func.Template(typed.Node.Args) != "date_trunc({0}, {1}, {tz})" {
		t.Errorf("epoch converts to timestamp: %+v", typed.Args[1])
	}
	if tmpl, _ := Coercion(Epoch, Timestamp); tmpl != "to_timestamp({0})" {
		t.Errorf("coercion template %q", tmpl)
	}
	typed, _ = check(t, "date_diff('month', {shippedAt}, {deliveredOn})")
	if !strings.Contains(typed.Func.Template(typed.Node.Args), "* 12") {
		t.Errorf("month diff template %q", typed.Func.Template(typed.Node.Args))
	}
	typed, _ = check(t, "count()")
	if typed.Func.Template(typed.Node.Args) != "count(*)" {
		t.Errorf("count() renders %q", typed.Func.Template(typed.Node.Args))
	}
	if OperatorSQL(mustParse(t, "1 / 2")) != "({0} / nullif({1}, 0))" || OperatorSQL(mustParse(t, "-1")) != "(-{0})" || OperatorSQL(mustParse(t, "1 is not null")) != "({0} IS NOT NULL)" {
		t.Error("operator templates")
	}
}

func TestCallType(t *testing.T) {
	cases := []struct {
		fn   string
		args []Type
		want Type
	}{
		{"sum", []Type{Int}, Int},
		{"sum", []Type{Decimal}, Decimal},
		{"avg", []Type{Int}, Decimal},
		{"avg", []Type{Float}, Float},
		{"count", []Type{String}, Int},
		{"count_distinct", []Type{Ref}, Int},
		{"min", []Type{Timestamp}, Timestamp},
		{"max", []Type{Epoch}, Epoch},
	}
	for _, c := range cases {
		got, err := CallType(c.fn, c.args...)
		if err != nil || got != c.want {
			t.Errorf("%s(%v) = %s, %v; want %s", c.fn, c.args, got, err, c.want)
		}
	}
	if _, err := CallType("sum", String); err == nil {
		t.Error("sum of text")
	}
	if _, err := CallType("nope"); err == nil {
		t.Error("unknown function")
	}
}

func TestDistanceAndSuggest(t *testing.T) {
	cases := []struct {
		a, b string
		d    int
	}{
		{"", "", 0}, {"abc", "abc", 0}, {"abc", "acb", 1}, {"ca", "abc", 2}, {"kitten", "sitting", 3}, {"revnue", "revenue", 1}, {"", "abc", 3},
	}
	for _, c := range cases {
		if got := Distance(c.a, c.b); got != c.d {
			t.Errorf("Distance(%q, %q) = %d, want %d", c.a, c.b, got, c.d)
		}
	}
	got := Suggest("custmer", []string{"customer", "customers", "carrier", "customer"})
	if len(got) != 1 || got[0] != "customer" {
		t.Errorf("Suggest = %v", got)
	}
	if got := Suggest("rate", []string{"date", "late", "rates", "x"}); len(got) != 3 || got[0] != "date" || got[2] != "rates" {
		t.Errorf("ties: %v", got)
	}
	if got := Suggest("Revenue", []string{"revenue"}); len(got) != 1 {
		t.Errorf("case-only difference: %v", got)
	}
	if got := Suggest("x", []string{"completely_different"}); len(got) != 0 {
		t.Errorf("far names: %v", got)
	}
}

func TestReferenceListsEveryFunction(t *testing.T) {
	ref := Reference()
	for _, f := range Functions() {
		if !strings.Contains(ref, "`"+escapeCell(f.Signature())+"`") {
			t.Errorf("reference misses %s", f.Name)
		}
	}
	if !strings.Contains(ref, "`a \\|\\| b`") || !strings.Contains(ref, "`-a`") {
		t.Errorf("operator forms:\n%s", ref)
	}
}

func TestRegistryExamplesCheck(t *testing.T) {
	e := testEnv{columns: map[string]ColumnInfo{}, fields: map[string]Type{}, params: map[string]ParamInfo{}}
	for _, f := range Functions() {
		n, err := Parse(f.Example)
		if err != nil {
			t.Errorf("%s example: %v", f.Name, err)
			continue
		}
		n.Walk(func(m *Node) bool {
			switch m.Kind {
			case KindColumn:
				e.columns[m.Value] = ColumnInfo{Level: Aggregate}
			case KindField:
				e.fields[m.Value] = Unknown
			}
			return true
		})
		if f.Aggregate {
			for k := range e.columns {
				e.columns[k] = ColumnInfo{Level: Row}
			}
		}
		if _, err := Check(n, e); err != nil {
			t.Errorf("%s example %q: %v", f.Name, f.Example, err)
		}
		e.columns = map[string]ColumnInfo{}
	}
}

func TestReferencesAndWalk(t *testing.T) {
	n := mustParse(t, "[a] + [b] + [a] + {x.y} + @p + case [c] when 1 then 2 else [d] end")
	if got := strings.Join(n.References(KindColumn), ","); got != "a,b,c,d" {
		t.Errorf("columns %s", got)
	}
	if got := strings.Join(mustParse(t, "{x.y}").Path(), "/"); got != "x/y" {
		t.Errorf("path %s", got)
	}
	count := 0
	n.Walk(func(*Node) bool {
		count++
		return count < 3
	})
	if count != 3 {
		t.Errorf("walk stops early: %d", count)
	}
}

func TestTypesHelpers(t *testing.T) {
	if c, ok := Common(Date, Epoch); !ok || c != Timestamp {
		t.Error("date and epoch combine to timestamp")
	}
	if c, ok := Common(Enum, Ref); !ok || c != String {
		t.Error("enum and ref combine to string")
	}
	if _, ok := Common(Bool, Int); ok {
		t.Error("bool and int do not combine")
	}
	if !Int.Valid() || Null.Valid() || Unknown.Valid() {
		t.Error("Valid")
	}
	if Window.String() != "window" || Constant.String() != "constant" || Row.String() != "row" || Aggregate.String() != "aggregate" {
		t.Error("level names")
	}
}

func TestConstructors(t *testing.T) {
	n := Call("coalesce", FieldRef("customer.name"), Text("it's"), ParamRef("label"), Unary("-", Number("1")))
	if got := Format(n); got != "coalesce({customer.name}, 'it''s', @label, -1)" {
		t.Fatalf("Format = %q", got)
	}
	if err := Validate(n, Limits{}); err != nil {
		t.Fatal(err)
	}
}
