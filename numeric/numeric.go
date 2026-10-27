// Package numeric reads and writes github.com/shopspring/decimal values in PostgreSQL's binary
// numeric format.
//
// Without it, pgx hands a decimal.Decimal the text of every numeric value it scans, because the
// type implements sql.Scanner: each value is decoded from binary, formatted as a string and parsed
// back. Register gives the numeric type a codec that pgx consults before that fallback, so a
// decimal.Decimal or decimal.NullDecimal is built from the decoded number directly and is sent as
// binary when it is bound.
package numeric

import (
	"context"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
)

// Register teaches conn to scan numeric values into decimal.Decimal and decimal.NullDecimal, and
// to bind them, in binary, for numeric and numeric[] alike. Every other Go type reads and writes
// numeric as pgx does. Use it as Config.AfterConnect, or call it from one.
func Register(_ context.Context, conn *pgx.Conn) error {
	m := conn.TypeMap()
	elem := &pgtype.Type{Name: "numeric", OID: pgtype.NumericOID, Codec: codec{}}
	m.RegisterType(elem)
	m.RegisterType(&pgtype.Type{Name: "_numeric", OID: pgtype.NumericArrayOID,
		Codec: &pgtype.ArrayCodec{ElementType: elem}})
	for _, v := range []any{
		decimal.Decimal{}, decimal.NullDecimal{}, &decimal.Decimal{}, &decimal.NullDecimal{},
	} {
		m.RegisterDefaultPgType(v, "numeric")
	}
	for _, v := range []any{[]decimal.Decimal{}, []decimal.NullDecimal{}, []*decimal.Decimal{}} {
		m.RegisterDefaultPgType(v, "_numeric")
	}
	return nil
}

// codec is pgx's numeric codec with binary plans for the decimal types.
type codec struct{ pgtype.NumericCodec }

func (c codec) PlanScan(m *pgtype.Map, oid uint32, format int16, target any) pgtype.ScanPlan {
	if format == pgtype.BinaryFormatCode {
		switch target.(type) {
		case *decimal.Decimal, *decimal.NullDecimal:
			return scanPlan{next: c.NumericCodec.PlanScan(m, oid, format, (*decimalTarget)(nil))}
		}
	}
	return c.NumericCodec.PlanScan(m, oid, format, target)
}

func (c codec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format == pgtype.BinaryFormatCode {
		switch value.(type) {
		case decimal.Decimal, decimal.NullDecimal:
			return encodePlan{next: c.NumericCodec.PlanEncode(m, oid, format, pgtype.Numeric{})}
		}
	}
	return c.NumericCodec.PlanEncode(m, oid, format, value)
}

// decimalTarget and nullDecimalTarget are the decimal types as pgtype.NumericScanners. NULL,
// NaN and infinities go to the decimal's own Scan, so they fail or set NullDecimal invalid as
// they do from text.
type (
	decimalTarget     decimal.Decimal
	nullDecimalTarget decimal.NullDecimal
)

func (t *decimalTarget) ScanNumeric(v pgtype.Numeric) error {
	if text, special := specialText(v); special {
		return (*decimal.Decimal)(t).Scan(text)
	}
	*t = decimalTarget(fromNumeric(v))
	return nil
}

func (t *nullDecimalTarget) ScanNumeric(v pgtype.Numeric) error {
	if text, special := specialText(v); special {
		return (*decimal.NullDecimal)(t).Scan(text)
	}
	*t = nullDecimalTarget{Decimal: fromNumeric(v), Valid: true}
	return nil
}

// specialText returns what a decimal's Scan is given for v when v is NULL, NaN or an infinity.
func specialText(v pgtype.Numeric) (any, bool) {
	switch {
	case !v.Valid:
		return nil, true
	case v.NaN:
		return "NaN", true
	case v.InfinityModifier != pgtype.Finite:
		return v.InfinityModifier.String(), true
	}
	return nil, false
}

// fromNumeric returns the decimal of a finite v as text would give it: pgx gives an integer with
// trailing zeros a positive exponent (100 is 1e2), where text has exponent 0.
func fromNumeric(v pgtype.Numeric) decimal.Decimal {
	if v.Int == nil {
		return decimal.New(0, min(v.Exp, 0))
	}
	if v.Exp <= 0 {
		return decimal.NewFromBigInt(v.Int, v.Exp)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(v.Exp)), nil)
	return decimal.NewFromBigInt(scale.Mul(scale, v.Int), 0)
}

// scanPlan decodes a binary numeric through next, pgx's plan for a pgtype.NumericScanner.
type scanPlan struct{ next pgtype.ScanPlan }

func (p scanPlan) Scan(src []byte, dst any) error {
	switch d := dst.(type) {
	case *decimal.Decimal:
		return p.next.Scan(src, (*decimalTarget)(d))
	case *decimal.NullDecimal:
		return p.next.Scan(src, (*nullDecimalTarget)(d))
	}
	return fmt.Errorf("numeric: cannot scan into %T", dst)
}

// encodePlan encodes a decimal as the pgtype.Numeric of its coefficient and exponent through next.
type encodePlan struct{ next pgtype.EncodePlan }

func (p encodePlan) Encode(value any, buf []byte) ([]byte, error) {
	var n pgtype.Numeric
	switch v := value.(type) {
	case decimal.Decimal:
		n = pgtype.Numeric{Int: v.Coefficient(), Exp: v.Exponent(), Valid: true}
	case decimal.NullDecimal:
		if v.Valid {
			n = pgtype.Numeric{Int: v.Decimal.Coefficient(), Exp: v.Decimal.Exponent(), Valid: true}
		}
	}
	return p.next.Encode(n, buf)
}
