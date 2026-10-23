package fleet

import (
	"database/sql/driver"
	"fmt"
	"strconv"
)

// Money is an amount in cents, stored as numeric. It is a sql.Scanner, so keel treats it as a
// custom type.
type Money struct {
	Cents int64
}

// Scan reads a numeric value.
func (m *Money) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		m.Cents = 0
		return nil
	case string:
		return m.parse(v)
	case []byte:
		return m.parse(string(v))
	case int64:
		m.Cents = v * 100
		return nil
	case float64:
		m.Cents = int64(v*100 + 0.5)
		return nil
	}
	return fmt.Errorf("money: cannot scan %T", src)
}

func (m *Money) parse(s string) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	m.Cents = int64(f*100 + 0.5)
	return nil
}

// Value writes a numeric value.
func (m Money) Value() (driver.Value, error) {
	return strconv.FormatFloat(float64(m.Cents)/100, 'f', 2, 64), nil
}
