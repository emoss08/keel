// Package ids holds the identifier type of the test models.
package ids

// ID is a prefixed identifier, stored as text.
type ID string

// IsNil reports whether the ID is empty.
func (id ID) IsNil() bool { return id == "" }
