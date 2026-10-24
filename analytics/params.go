package analytics

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/value"
)

// bindParams returns the value of every declared parameter that has one, given or default,
// converted to its type: int64, float64, keel.Decimal, string, bool, time.Time (a timestamp) or a
// "2006-01-02" string (a date), and a slice of those for a multi-value parameter. A required
// parameter without a value, a value of the wrong type, a value outside Allowed and a name the
// definition does not declare are errors.
func bindParams(def *ir.Definition, given map[string]any, loc *time.Location) (map[string]any, error) {
	for name := range given {
		if _, ok := def.Parameter(name); !ok {
			return nil, fail(CodeParameter, "/parameters", "the report has no parameter %q", name)
		}
	}
	out := make(map[string]any, len(def.Parameters))
	for i := range def.Parameters {
		p := &def.Parameters[i]
		at := fmt.Sprintf("/parameters/%d", i)
		raw, ok := given[p.Name]
		if !ok || raw == nil {
			raw = p.Default
		}
		if raw == nil {
			if p.Required {
				return nil, fail(CodeParameter, at, "the parameter %q needs a value", p.Name)
			}
			continue
		}
		v, err := paramValue(p, raw, loc)
		if err != nil {
			return nil, &Error{Code: CodeParameter, Message: fmt.Sprintf("the parameter %q: %v", p.Name, err), Pointer: at}
		}
		out[p.Name] = v
	}
	return out, nil
}

// paramValue converts a parameter's raw value and checks it against Allowed.
func paramValue(p *ir.Parameter, raw any, loc *time.Location) (any, error) {
	if !p.Multi {
		v, err := convert(p.Type, raw, loc)
		if err != nil {
			return nil, err
		}
		return v, allowed(p, v, loc)
	}
	list, ok := listOf(raw)
	if !ok {
		return nil, fmt.Errorf("expected a list of %s values", p.Type)
	}
	out := make([]any, len(list))
	for i, x := range list {
		v, err := convert(p.Type, x, loc)
		if err != nil {
			return nil, err
		}
		if err := allowed(p, v, loc); err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// allowed checks a converted value against a parameter's allowed values.
func allowed(p *ir.Parameter, v any, loc *time.Location) error {
	if len(p.Allowed) == 0 {
		return nil
	}
	for _, a := range p.Allowed {
		if av, err := convert(p.Type, a, loc); err == nil && fmt.Sprint(av) == fmt.Sprint(v) {
			return nil
		}
	}
	return fmt.Errorf("%v is not one of the allowed values", v)
}

// listOf returns a slice value's elements.
func listOf(v any) ([]any, bool) {
	if l, ok := v.([]any); ok {
		return l, true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Slice || rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// convert reads a value as type t, as bindParams describes.
func convert(t formula.Type, x any, loc *time.Location) (any, error) {
	switch t {
	case formula.Int:
		return toInt(x)
	case formula.Float:
		return toFloat(x)
	case formula.Decimal:
		return toDecimal(x)
	case formula.Bool:
		return toBool(x)
	case formula.Timestamp:
		return toTime(x, loc)
	case formula.Epoch:
		return toEpoch(x, loc)
	case formula.Date:
		return toDate(x, loc)
	case formula.String, formula.Enum, formula.Ref, formula.Unknown:
		return toText(x)
	}
	return nil, fmt.Errorf("a %s value cannot be bound", t)
}

// toInt reads a whole number.
func toInt(x any) (int64, error) {
	switch n := x.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case json.Number:
		return strconv.ParseInt(n.String(), 10, 64)
	case float64:
		if n == math.Trunc(n) && math.Abs(n) < 1<<53 {
			return int64(n), nil
		}
	case string:
		return strconv.ParseInt(strings.TrimSpace(n), 10, 64)
	}
	return 0, fmt.Errorf("%v is not a whole number", x)
}

// toFloat reads a number as a float.
func toFloat(x any) (float64, error) {
	switch n := x.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(n), 64)
	case keel.Decimal:
		return n.Float64()
	}
	i, err := toInt(x)
	return float64(i), err
}

// toDecimal reads a number exactly.
func toDecimal(x any) (keel.Decimal, error) {
	switch n := x.(type) {
	case keel.Decimal:
		return value.ParseDecimal(string(n))
	case json.Number:
		return value.ParseDecimal(n.String())
	case string:
		return value.ParseDecimal(n)
	case float64:
		return value.DecimalFromFloat(n), nil
	case float32:
		return value.DecimalFromFloat(float64(n)), nil
	}
	i, err := toInt(x)
	if err != nil {
		return "", err
	}
	return value.DecimalFromInt(i), nil
}

// toBool reads a boolean.
func toBool(x any) (bool, error) {
	switch b := x.(type) {
	case bool:
		return b, nil
	case string:
		return strconv.ParseBool(b)
	}
	return false, fmt.Errorf("%v is not true or false", x)
}

// toText reads text; numbers are written as they are.
func toText(x any) (string, error) {
	switch s := x.(type) {
	case string:
		return s, nil
	case json.Number:
		return s.String(), nil
	case int64, int, int32, int16, bool:
		return fmt.Sprint(s), nil
	}
	return "", fmt.Errorf("%v is not text", x)
}

// toTime reads an instant: a time, RFC 3339 text, a date (midnight in loc) or epoch seconds.
func toTime(x any, loc *time.Location) (time.Time, error) {
	switch t := x.(type) {
	case time.Time:
		return t, nil
	case string:
		if v, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return v, nil
		}
		if v, err := time.ParseInLocation(time.DateOnly, t, loc); err == nil {
			return v, nil
		}
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return time.Unix(n, 0).UTC(), nil
		}
		return time.Time{}, fmt.Errorf("%q is not a date or time", t)
	}
	n, err := toInt(x)
	if err != nil {
		return time.Time{}, fmt.Errorf("%v is not a date or time", x)
	}
	return time.Unix(n, 0).UTC(), nil
}

// Instant reads a condition's value written for a field of type t as the instant it denotes in
// loc, as the compiler binds it: epoch seconds (or anything an epoch field accepts) for an epoch
// field, a calendar date's local midnight for a date field, and RFC 3339 text, a date (local
// midnight) or epoch seconds for a timestamp field.
func Instant(v any, t formula.Type, loc *time.Location) (time.Time, error) {
	switch t {
	case formula.Epoch:
		n, err := toEpoch(v, loc)
		return time.Unix(n, 0).UTC(), err
	case formula.Date:
		d, err := toDate(v, loc)
		if err != nil {
			return time.Time{}, err
		}
		return time.ParseInLocation(time.DateOnly, d, loc)
	}
	return toTime(v, loc)
}

// toEpoch reads an instant as epoch seconds.
func toEpoch(x any, loc *time.Location) (int64, error) {
	t, err := toTime(x, loc)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// toDate reads a calendar date as "2006-01-02"; an instant is taken in loc.
func toDate(x any, loc *time.Location) (string, error) {
	if s, ok := x.(string); ok {
		if _, err := time.Parse(time.DateOnly, s); err == nil {
			return s, nil
		}
	}
	t, err := toTime(x, loc)
	if err != nil {
		return "", err
	}
	return t.In(loc).Format(time.DateOnly), nil
}

// countOf returns the value of a count (a limit, a top N), from a number or an int parameter, and
// false when its parameter has no value.
func countOf(c ir.Count, params map[string]any) (int, bool) {
	if c.Param == "" {
		return c.Value, c.Value > 0
	}
	v, ok := params[c.Param].(int64)
	return int(v), ok && v > 0
}

// stringsOf returns a list parameter's values as text, as pivot values are compared.
func stringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, textOf(x))
	}
	return slices.Clip(out)
}

// textOf writes a bound value as text: a decimal as written, a time in RFC 3339.
func textOf(x any) string {
	switch v := x.(type) {
	case time.Time:
		return v.UTC().Format(time.RFC3339)
	case keel.Decimal:
		return string(v)
	}
	return fmt.Sprint(x)
}
