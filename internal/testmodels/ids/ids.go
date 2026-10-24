// Package ids holds the identifier type of the test models.
package ids

// ID is a prefixed identifier, stored as text.
type ID string

// KeelFormat tells keel gen that reports display identifiers as IDs.
func (*ID) KeelFormat() string { return "id" }

// IsNil reports whether the ID is empty.
func (id ID) IsNil() bool { return id == "" }
