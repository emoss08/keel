package keellog

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Inline writes values into sql in place of its $n placeholders, as SQL literals, for reading. A
// string longer than maxValue characters and a slice longer than maxElems elements are cut short.
// The result is for display: a cut value or a byte slice does not round-trip.
func Inline(sql string, values []any, maxValue, maxElems int) string {
	if len(values) == 0 {
		return sql
	}
	code := codeMask(sql)
	var b strings.Builder
	b.Grow(len(sql) + 16*len(values))
	for i := 0; i < len(sql); i++ {
		if !code[i] || sql[i] != '$' || i+1 >= len(sql) || !isDigit(sql[i+1]) || (i > 0 && isIdent(sql[i-1])) {
			b.WriteByte(sql[i])
			continue
		}
		j := i + 1
		for j < len(sql) && isDigit(sql[j]) {
			j++
		}
		n, err := strconv.Atoi(sql[i+1 : j])
		if err != nil || n < 1 || n > len(values) {
			b.WriteString(sql[i:j])
		} else {
			b.WriteString(literal(values[n-1], maxValue, maxElems))
		}
		i = j - 1
	}
	return b.String()
}

var timeType = reflect.TypeFor[time.Time]()

// Literal writes v as the SQL literal Inline writes for it, cutting a string longer than maxValue
// characters and a slice longer than maxElems elements.
func Literal(v any, maxValue, maxElems int) string {
	return literal(v, maxValue, maxElems)
}

func literal(v any, maxValue, maxElems int) string {
	if v == nil {
		return "NULL"
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "NULL"
		}
		if vl, ok := rv.Interface().(driver.Valuer); ok {
			return valuer(vl, maxValue, maxElems)
		}
		rv = rv.Elem()
	}
	v = rv.Interface()
	switch x := v.(type) {
	case string:
		return quote(cut(x, maxValue))
	case []byte:
		if utf8.Valid(x) && printable(x) {
			return quote(cut(string(x), maxValue))
		}
		return "<" + strconv.Itoa(len(x)) + " bytes>"
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return quote(x.Format("2006-01-02 15:04:05.999999Z07:00"))
	case driver.Valuer:
		return valuer(x, maxValue, maxElems)
	case fmt.Stringer:
		return quote(cut(x.String(), maxValue))
	}
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64)
	case reflect.String:
		return quote(cut(rv.String(), maxValue))
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return "NULL"
		}
		return quote(array(rv, maxValue, maxElems))
	}
	return quote(cut(fmt.Sprint(v), maxValue))
}

func valuer(v driver.Valuer, maxValue, maxElems int) string {
	dv, err := v.Value()
	if err != nil {
		return "<" + err.Error() + ">"
	}
	if _, again := dv.(driver.Valuer); again {
		return quote(cut(fmt.Sprint(dv), maxValue))
	}
	return literal(dv, maxValue, maxElems)
}

func array(rv reflect.Value, maxValue, maxElems int) string {
	var b strings.Builder
	b.WriteByte('{')
	n := rv.Len()
	for i := range min(n, maxElems) {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(element(rv.Index(i), maxValue, maxElems))
	}
	if n > maxElems {
		b.WriteString(",… +")
		b.WriteString(strconv.Itoa(n - maxElems))
	}
	b.WriteByte('}')
	return b.String()
}

func element(rv reflect.Value, maxValue, maxElems int) string {
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "NULL"
		}
		rv = rv.Elem()
	}
	if rv.Type() == timeType {
		return `"` + rv.Interface().(time.Time).Format("2006-01-02 15:04:05.999999Z07:00") + `"`
	}
	switch rv.Kind() {
	case reflect.String:
		s := cut(rv.String(), maxValue)
		if s == "" || s == "NULL" || strings.ContainsAny(s, ` ,{}"\`) {
			return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
		}
		return s
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() != reflect.Uint8 {
			return array(rv, maxValue, maxElems)
		}
	}
	return strings.Trim(literal(rv.Interface(), maxValue, maxElems), "'")
}

func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for range n {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return s[:i] + "…"
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
	}
	return true
}

// Format breaks sql before its clauses (FROM, WHERE, JOIN, GROUP BY, ORDER BY, RETURNING, …), at
// the top level and inside a long parenthesised statement (a CTE body, a subquery), which it
// indents. It removes the double quotes around identifiers that need none.
func Format(sql string) string {
	sql = unquote(sql)
	code := codeMask(sql)
	match := parens(sql, code)
	type frame struct {
		breaks bool
		indent string
		last   string
	}
	stack := []frame{{breaks: true}}
	var b strings.Builder
	b.Grow(len(sql) + 32)
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		top := &stack[len(stack)-1]
		if !code[i] {
			b.WriteByte(c)
			continue
		}
		switch {
		case c == '(':
			b.WriteByte(c)
			f := frame{indent: top.indent}
			if end, ok := match[i]; ok && (end-i > 60 || (top.breaks && top.last == "AS")) && statementAt(sql, i+1) {
				f.breaks = true
				f.indent = top.indent + "  "
				newline(&b, f.indent)
				for i+1 < len(sql) && sql[i+1] == ' ' {
					i++
				}
			}
			stack = append(stack, f)
			continue
		case c == ')':
			if len(stack) > 1 {
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if f.breaks {
					newline(&b, stack[len(stack)-1].indent)
				}
			}
			b.WriteByte(c)
			continue
		case isLetter(c) && (i == 0 || !isIdent(sql[i-1])):
			j := i
			for j < len(sql) && isIdent(sql[j]) {
				j++
			}
			word := strings.ToUpper(sql[i:j])
			if top.breaks {
				if n := breaker(sql, i, word, top.last); n > 0 {
					if lineHasText(&b) {
						newline(&b, top.indent)
					}
					b.WriteString(sql[i : i+n])
					top.last = lastWord(sql[i : i+n])
					i += n - 1
					continue
				}
			}
			top.last = word
			b.WriteString(sql[i:j])
			i = j - 1
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

var clauses = []string{
	"LEFT OUTER JOIN", "RIGHT OUTER JOIN", "FULL OUTER JOIN",
	"LEFT JOIN", "RIGHT JOIN", "FULL JOIN", "INNER JOIN", "CROSS JOIN",
	"JOIN", "FROM", "WHERE", "GROUP BY", "HAVING", "WINDOW", "ORDER BY", "ON CONFLICT",
	"RETURNING", "UNION", "INTERSECT", "EXCEPT", "SELECT", "INSERT", "UPDATE", "DELETE", "VALUES",
}

func breaker(sql string, i int, word, last string) int {
	for _, k := range clauses {
		if !strings.HasPrefix(k, word) || len(sql)-i < len(k) || !strings.EqualFold(sql[i:i+len(k)], k) {
			continue
		}
		if i+len(k) < len(sql) && isIdent(sql[i+len(k)]) {
			continue
		}
		switch k {
		case "FROM":
			if last == "DISTINCT" || last == "DELETE" {
				return 0
			}
		case "UPDATE":
			if last == "DO" || last == "FOR" || last == "KEY" {
				return 0
			}
		case "SELECT":
			if last == "ALL" || last == "UNION" || last == "INTERSECT" || last == "EXCEPT" || last == "" {
				return 0
			}
		case "VALUES":
			if last == "DEFAULT" {
				return 0
			}
		}
		return len(k)
	}
	return 0
}

func statementAt(sql string, i int) bool {
	for i < len(sql) && sql[i] == ' ' {
		i++
	}
	for _, k := range []string{"SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "VALUES"} {
		if len(sql)-i >= len(k) && strings.EqualFold(sql[i:i+len(k)], k) &&
			(i+len(k) == len(sql) || !isIdent(sql[i+len(k)])) {
			return true
		}
	}
	return false
}

func lastWord(s string) string {
	if i := strings.LastIndexByte(s, ' '); i >= 0 {
		return strings.ToUpper(s[i+1:])
	}
	return strings.ToUpper(s)
}

func newline(b *strings.Builder, indent string) {
	s := b.String()
	trimmed := strings.TrimRight(s, " ")
	if len(trimmed) != len(s) {
		rest := trimmed
		b.Reset()
		b.WriteString(rest)
	}
	b.WriteByte('\n')
	b.WriteString(indent)
}

func lineHasText(b *strings.Builder) bool {
	s := b.String()
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s) != ""
}

// codeMask marks the bytes of sql outside string literals, quoted identifiers and comments.
func codeMask(sql string) []bool {
	code := make([]bool, len(sql))
	for i := 0; i < len(sql); i++ {
		switch c := sql[i]; {
		case c == '\'':
			backslash := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i == 1 || !isIdent(sql[i-2]))
			i++
			for i < len(sql) {
				if backslash && sql[i] == '\\' {
					i += 2
					continue
				}
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						i += 2
						continue
					}
					break
				}
				i++
			}
		case c == '"':
			i++
			for i < len(sql) && sql[i] != '"' {
				i++
			}
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return code
			}
			i += end + 3
		default:
			code[i] = true
		}
	}
	return code
}

func parens(sql string, code []bool) map[int]int {
	match := map[int]int{}
	var stack []int
	for i := range len(sql) {
		if !code[i] {
			continue
		}
		switch sql[i] {
		case '(':
			stack = append(stack, i)
		case ')':
			if n := len(stack); n > 0 {
				match[stack[n-1]] = i
				stack = stack[:n-1]
			}
		}
	}
	return match
}

// unquote drops the double quotes around an identifier that is lower case, starts with a letter
// or underscore and is not a reserved word.
func unquote(sql string) string {
	if !strings.Contains(sql, `"`) {
		return sql
	}
	code := codeMask(sql)
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); i++ {
		if sql[i] != '"' || !opensIdent(code, i) {
			b.WriteByte(sql[i])
			continue
		}
		end := strings.IndexByte(sql[i+1:], '"')
		if end < 0 {
			b.WriteString(sql[i:])
			break
		}
		if id := sql[i+1 : i+1+end]; plain(id) {
			b.WriteString(id)
		} else {
			b.WriteString(sql[i : i+end+2])
		}
		i += end + 1
	}
	return b.String()
}

// opensIdent reports whether the double quote at i opens a quoted identifier rather than sitting
// inside a string literal or comment.
func opensIdent(code []bool, i int) bool {
	return i == 0 || code[i-1]
}

func plain(id string) bool {
	if id == "" || !(id[0] == '_' || (id[0] >= 'a' && id[0] <= 'z')) {
		return false
	}
	for i := 1; i < len(id); i++ {
		c := id[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || isDigit(c)) {
			return false
		}
	}
	return !reserved[id]
}

var reserved = map[string]bool{}

func init() {
	for w := range strings.FieldsSeq(`all analyse analyze and any array as asc asymmetric
		authorization binary both case cast check collate collation column concurrently constraint
		create cross current_catalog current_date current_role current_schema current_time
		current_timestamp current_user default deferrable desc distinct do else end except false
		fetch for foreign freeze from full grant group having ilike in initially inner intersect
		into is isnull join lateral leading left like limit localtime localtimestamp natural not
		notnull null offset on only or order outer overlaps placing primary references returning
		right select session_user similar some symmetric system_user table tablesample then to
		trailing true union unique user using variadic verbose when where window with`) {
		reserved[w] = true
	}
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }
func isIdent(c byte) bool  { return isLetter(c) || isDigit(c) || c == '$' || c >= 0x80 }
