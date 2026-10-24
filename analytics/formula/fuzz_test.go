package formula

import (
	"encoding/json"
	"testing"
)

func FuzzParse(f *testing.F) {
	seeds := []string{
		"round([revenue] / nullif([miles], 0), 2)",
		"case [status] when 'A' then 1 when 'B' then -2.5 else null end",
		"not {a.b} is null and (1 < 2 or @p = 'x''y')",
		"date '2026-01-31' < date_trunc('month', {shippedAt}) || 'x'",
		"timestamp '2026-01-31T08:00:00.5+02:00'",
		"--1 - -(2 * 3) % 4",
		"[a] = (not true)",
		"count() / count_distinct({x})",
		"((((1))))",
		"'\n'",
		"1 is not null is null",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		n, err := Parse(src)
		if err != nil {
			if _, ok := err.(Errors); !ok {
				t.Fatalf("Parse returned %T, want Errors", err)
			}
			return
		}
		printed := Format(n)
		again, err := Parse(printed)
		if err != nil {
			t.Fatalf("Format(Parse(%q)) = %q does not parse: %v", src, printed, err)
		}
		if !Equal(n, again) {
			t.Fatalf("Parse(Format(Parse(%q))) differs: %q", src, printed)
		}
		if Format(again) != printed {
			t.Fatalf("Format is not stable for %q", src)
		}
		data, err := json.Marshal(n)
		if err != nil {
			t.Fatalf("marshal %q: %v", src, err)
		}
		var back Node
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", data, err)
		}
		if err := Validate(&back, Limits{}); err != nil || !Equal(n, &back) {
			t.Fatalf("JSON round trip of %q: %v", src, err)
		}
		_, _ = Check(n, env())
	})
}
