package rollup

import (
	"strings"

	"github.com/emoss08/keel/schema"
)

// camel turns a snake_case or Go name into lower camel case with acronyms as words: total_sum →
// totalSum, CustomerID → customerId.
func camel(name string) string {
	words := strings.Split(schema.Snake(name), "_")
	var b strings.Builder
	for i, w := range words {
		if w == "" {
			continue
		}
		if i == 0 {
			b.WriteString(w)
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]))
		b.WriteString(w[1:])
	}
	return b.String()
}

// pascal turns a snake_case name into upper camel case: total_sum → TotalSum.
func pascal(name string) string {
	c := camel(name)
	if c == "" {
		return c
	}
	return strings.ToUpper(c[:1]) + c[1:]
}

// label turns a snake_case name into words: total_sum → Total Sum.
func label(name string) string {
	words := strings.Split(name, "_")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
