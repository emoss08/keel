package seed

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// csvPlan decodes records of one header into one struct type.
type csvPlan struct {
	cols []csvColumn
}

// csvColumn is one column of the header and the field it fills.
type csvColumn struct {
	name  string
	index []int
	set   setter
}

// setter stores a non-empty cell in a field.
type setter func(v reflect.Value, s string) error

var (
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	timeType        = reflect.TypeFor[time.Time]()
	durationType    = reflect.TypeFor[time.Duration]()
)

// planCSV maps a header to the fields of t.
func planCSV(t reflect.Type, header []string) (*csvPlan, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("CSV decodes into a struct, not %v", t)
	}
	fields := map[string][]int{}
	if err := csvFields(t, nil, fields); err != nil {
		return nil, err
	}
	p := &csvPlan{}
	seen := map[string]bool{}
	var unknown []string
	for i, h := range header {
		name := strings.TrimSpace(h)
		if i == 0 {
			name = strings.TrimPrefix(name, "\ufeff")
		}
		if seen[name] {
			return nil, fmt.Errorf("column %q appears twice in the header", name)
		}
		seen[name] = true
		index, ok := fields[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		set, err := setterFor(t.FieldByIndex(index).Type)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", name, err)
		}
		p.cols = append(p.cols, csvColumn{name: name, index: index, set: set})
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("no field of %v for column %s", t, quoteAll(unknown))
	}
	return p, nil
}

func quoteAll(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	return strings.Join(q, ", ")
}

// csvFields collects the column name of every exported field of t, descending into embedded
// structs.
func csvFields(t reflect.Type, prefix []int, out map[string][]int) error {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("csv")
		if tag == "-" {
			continue
		}
		index := append(append([]int(nil), prefix...), i)
		if f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct {
			if err := csvFields(f.Type, index, out); err != nil {
				return err
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		name := tag
		if name == "" {
			name = snake(f.Name)
		}
		if _, dup := out[name]; dup {
			return fmt.Errorf("two fields of %v read column %q", t, name)
		}
		out[name] = index
	}
	return nil
}

// snake converts a Go field name to snake case: ProNumber is pro_number, UserID is user_id.
func snake(name string) string {
	rs := []rune(name)
	var b strings.Builder
	for i, r := range rs {
		if unicode.IsUpper(r) && i > 0 {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// decode fills v from one record.
func (p *csvPlan) decode(v reflect.Value, rec []string) error {
	for i, c := range p.cols {
		cell := rec[i]
		if cell == "" {
			continue
		}
		if err := c.set(v.FieldByIndex(c.index), cell); err != nil {
			return fmt.Errorf("column %q: %w", c.name, err)
		}
	}
	return nil
}

// setterFor returns the setter of a field type, or an error for a type CSV cannot fill.
func setterFor(t reflect.Type) (setter, error) {
	switch {
	case t == timeType:
		return setTime, nil
	case t == durationType:
		return setDuration, nil
	case reflect.PointerTo(t).Implements(textUnmarshaler):
		return setText, nil
	case t.Kind() == reflect.Pointer:
		elem, err := setterFor(t.Elem())
		if err != nil {
			return nil, err
		}
		return func(v reflect.Value, s string) error {
			p := reflect.New(t.Elem())
			if err := elem(p.Elem(), s); err != nil {
				return err
			}
			v.Set(p)
			return nil
		}, nil
	}
	return kindSetter(t)
}

func kindSetter(t reflect.Type) (setter, error) {
	switch t.Kind() {
	case reflect.String:
		return func(v reflect.Value, s string) error { v.SetString(s); return nil }, nil
	case reflect.Bool:
		return setBool, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return setInt, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return setUint, nil
	case reflect.Float32, reflect.Float64:
		return setFloat, nil
	}
	return nil, fmt.Errorf("CSV cannot fill a field of type %v", t)
}

func setBool(v reflect.Value, s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	v.SetBool(b)
	return nil
}

func setInt(v reflect.Value, s string) error {
	n, err := strconv.ParseInt(s, 10, v.Type().Bits())
	if err != nil {
		return err
	}
	v.SetInt(n)
	return nil
}

func setUint(v reflect.Value, s string) error {
	n, err := strconv.ParseUint(s, 10, v.Type().Bits())
	if err != nil {
		return err
	}
	v.SetUint(n)
	return nil
}

func setFloat(v reflect.Value, s string) error {
	f, err := strconv.ParseFloat(s, v.Type().Bits())
	if err != nil {
		return err
	}
	v.SetFloat(f)
	return nil
}

func setTime(v reflect.Value, s string) error {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		var dateErr error
		if t, dateErr = time.Parse(time.DateOnly, s); dateErr != nil {
			return err
		}
	}
	v.Set(reflect.ValueOf(t))
	return nil
}

func setDuration(v reflect.Value, s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	v.SetInt(int64(d))
	return nil
}

func setText(v reflect.Value, s string) error {
	return v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s))
}
