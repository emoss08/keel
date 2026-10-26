// Package benchjson decodes the benchmark's jsonb column for every library the same way: into a
// map, with sonic, the codec keel and pgx are configured with in the benchmark.
package benchjson

import (
	"database/sql/driver"
	"fmt"

	"github.com/bytedance/sonic"
)

// Map is a jsonb object, decoded by Scan and encoded by Value.
type Map map[string]any

// Scan decodes a jsonb value.
func (m *Map) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*m = nil
		return nil
	case []byte:
		return sonic.Unmarshal(v, m)
	case string:
		return sonic.UnmarshalString(v, m)
	}
	return fmt.Errorf("benchjson: cannot scan %T", src)
}

// Value encodes the map as JSON text.
func (m Map) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return sonic.MarshalString(m)
}
