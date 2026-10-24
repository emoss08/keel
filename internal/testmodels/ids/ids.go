// Package ids holds the identifier type of the test models.
package ids

import (
	"database/sql/driver"
	"fmt"
)

// ID is a prefixed identifier, stored as text. Like Trenova's pulid.ID it is a sql.Scanner and a
// driver.Valuer that store the empty ID as NULL; keel stores a named string type as a string and
// leaves those methods to code that binds the type itself, so its columns are strings and empty
// IDs become NULL only in nullzero columns.
type ID string

// KeelFormat tells keel gen that reports display identifiers as IDs.
func (*ID) KeelFormat() string { return "id" }

// IsNil reports whether the ID is empty.
func (id ID) IsNil() bool { return id == "" }

// Scan reads a text value; NULL is the empty ID.
func (id *ID) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*id = ""
	case string:
		*id = ID(v)
	case []byte:
		*id = ID(v)
	default:
		return fmt.Errorf("ids: cannot scan %T", src)
	}
	return nil
}

// Value writes the ID as text, and the empty ID as NULL.
func (id ID) Value() (driver.Value, error) {
	if id == "" {
		return nil, nil
	}
	return string(id), nil
}
