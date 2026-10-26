package fleet

import (
	"database/sql/driver"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
)

// Money is an amount in cents, stored as numeric. It is a sql.Scanner, so keel treats it as a
// custom type, and a pgtype.NumericScanner, so pgx decodes the binary numeric straight into it
// instead of through its text form.
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

// ScanNumeric reads a binary numeric, rounding to the cent.
func (m *Money) ScanNumeric(v pgtype.Numeric) error {
	if !v.Valid {
		m.Cents = 0
		return nil
	}
	if v.NaN || v.InfinityModifier != pgtype.Finite || !v.Int.IsInt64() {
		return fmt.Errorf("money: cannot scan numeric %v", v)
	}
	cents := v.Int.Int64()
	for e := v.Exp + 2; e > 0; e-- {
		cents *= 10
	}
	if v.Exp+2 < 0 {
		div := int64(1)
		for e := v.Exp + 2; e < 0; e++ {
			div *= 10
		}
		half := div / 2
		if cents < 0 {
			half = -half
		}
		cents = (cents + half) / div
	}
	m.Cents = cents
	return nil
}

func (m *Money) parse(s string) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	m.Cents = int64(f*100 + 0.5)
	return nil
}

// KeelFormat tells keel gen that reports display amounts as money.
func (Money) KeelFormat() string { return "money" }

// Value writes a numeric value.
func (m Money) Value() (driver.Value, error) {
	return strconv.FormatFloat(float64(m.Cents)/100, 'f', 2, 64), nil
}
