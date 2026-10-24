package ir

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/emoss08/keel/analytics/formula"
)

const maxLabel = 200

func (v *validator) parameters() {
	if len(v.def.Parameters) > v.lim.MaxParameters {
		v.add(CodeLimit, "/parameters", "a report can have at most %d parameters", v.lim.MaxParameters)
	}
	for i := range v.def.Parameters {
		v.parameter(&v.def.Parameters[i], ptr("/parameters", i))
	}
}

func (v *validator) parameter(p *Parameter, at string) {
	switch {
	case p.Name == "":
		v.add(CodeRequired, at+"/name", "name the parameter")
		return
	case !formula.ValidName(p.Name) || len(p.Name) > 64:
		v.add(CodeInvalid, at+"/name", "%q is not a parameter name; use a letter followed by letters, digits and underscores", p.Name)
		return
	case v.params[p.Name] != nil:
		v.add(CodeDuplicate, at+"/name", "parameter %q is declared twice", p.Name)
		return
	}
	v.params[p.Name] = p
	if len(p.Label) > maxLabel {
		v.add(CodeLimit, at+"/label", "a label can be at most %d characters", maxLabel)
	}
	if !p.Type.Valid() || p.Type == formula.JSON {
		v.unknown(CodeInvalid, at+"/type", "parameter type", string(p.Type), typeNames())
		return
	}
	v.paramEntity(p, at)
	v.paramAllowed(p, at)
	v.paramDefault(p, at)
}

func typeNames() []string {
	var out []string
	for _, t := range formula.Types() {
		if t != formula.JSON {
			out = append(out, string(t))
		}
	}
	return out
}

func (v *validator) paramEntity(p *Parameter, at string) {
	if p.Type != formula.Ref {
		if p.Entity != "" {
			v.add(CodeConflict, at+"/entity", "only a ref parameter names an entity")
		}
		return
	}
	switch {
	case p.Entity == "":
		v.add(CodeRequired, at+"/entity", "a ref parameter names the entity its values point at")
	case len(p.Allowed) > 0:
		v.add(CodeConflict, at+"/allowed", "a ref parameter takes its values from %s, so it cannot list allowed values", p.Entity)
	case v.cat != nil && !v.cat.HasEntity(p.Entity):
		v.unknown(CodeUnknownEntity, at+"/entity", "entity", p.Entity, v.cat.EntityKeys())
	}
}

func (v *validator) paramAllowed(p *Parameter, at string) {
	seen := map[string]bool{}
	for i, a := range p.Allowed {
		if msg := valueFits(p.Type, a); msg != "" {
			v.add(CodeType, ptr(at+"/allowed", i), "%s", msg)
			continue
		}
		key := fmt.Sprint(a)
		if seen[key] {
			v.add(CodeDuplicate, ptr(at+"/allowed", i), "%v is allowed twice", a)
		}
		seen[key] = true
	}
}

func (v *validator) paramDefault(p *Parameter, at string) {
	if p.Default == nil {
		return
	}
	values := []any{p.Default}
	if p.Multi {
		list, ok := asList(p.Default)
		if !ok {
			v.add(CodeType, at+"/default", "a multi-value parameter's default is a list")
			return
		}
		values = list
	}
	for _, d := range values {
		if msg := valueFits(p.Type, d); msg != "" {
			v.add(CodeType, at+"/default", "%s", msg)
			return
		}
		if len(p.Allowed) > 0 && !containsValue(p.Allowed, d) {
			v.add(CodeInvalid, at+"/default", "the default %v is not one of the allowed values", d)
			return
		}
	}
}

func containsValue(list []any, x any) bool {
	return slices.ContainsFunc(list, func(a any) bool { return fmt.Sprint(a) == fmt.Sprint(x) })
}

func asList(v any) ([]any, bool) {
	if l, ok := v.([]any); ok {
		return l, true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) || rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

func valueFits(t formula.Type, x any) string {
	if _, isList := asList(x); isList {
		return fmt.Sprintf("expected one %s value, got a list", t)
	}
	if x == nil {
		return fmt.Sprintf("expected a %s value, got null", t)
	}
	ok := true
	switch {
	case t == formula.Unknown:
	case t == formula.Int:
		_, ok = asInt(x)
	case t.Numeric():
		_, ok = asFloat(x)
	case t.Textual(), t == formula.Ref:
		_, ok = x.(string)
	case t == formula.Bool:
		_, ok = x.(bool)
	case t.Temporal():
		ok = temporalValue(x)
	default:
		ok = false
	}
	if !ok {
		return fmt.Sprintf("%v is not a %s value", x, t)
	}
	return ""
}

func asInt(x any) (int64, bool) {
	switch n := x.(type) {
	case json.Number:
		i, err := strconv.ParseInt(n.String(), 10, 64)
		return i, err == nil
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), n == math.Trunc(n) && math.Abs(n) < 1<<53
	default:
		return 0, false
	}
}

func asFloat(x any) (float64, bool) {
	switch n := x.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	default:
		i, ok := asInt(x)
		return float64(i), ok
	}
}

func temporalValue(x any) bool {
	switch t := x.(type) {
	case time.Time:
		return true
	case string:
		if _, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return true
		}
		_, err := time.Parse(time.DateOnly, t)
		return err == nil
	default:
		_, ok := asInt(x)
		return ok
	}
}

func compatible(a, b formula.Type) bool {
	_, ok := formula.Common(a, b)
	return ok
}
