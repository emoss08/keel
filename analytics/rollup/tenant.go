package rollup

import (
	"strings"

	"github.com/emoss08/keel"
)

// splitKey returns the values of a data-versions tenant key (see keel.TenantKeyOf): the parts
// between unescaped "/", unescaped. The empty key has no values.
func splitKey(key string) []string {
	if key == "" {
		return nil
	}
	var out []string
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		switch key[i] {
		case '\\':
			if i+1 < len(key) {
				i++
				b.WriteByte(key[i])
			}
		case '/':
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteByte(key[i])
		}
	}
	return append(out, b.String())
}

// joinKey is the tenant key of values; it inverts splitKey.
func joinKey(values []string) string {
	vs := make([]any, len(values))
	for i, v := range values {
		vs[i] = v
	}
	return keel.TenantKeyOf(vs...)
}
