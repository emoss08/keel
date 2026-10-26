package fleet

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestMoneyScanNumericRoundsToTheCent(t *testing.T) {
	cases := []struct {
		digits int64
		exp    int32
		want   int64
	}{
		{12349, -4, 123},
		{12350, -4, 124},
		{-12350, -4, -124},
		{5, 0, 500},
		{1234, -2, 1234},
		{0, 0, 0},
	}
	for _, c := range cases {
		var m Money
		err := m.ScanNumeric(pgtype.Numeric{Int: big.NewInt(c.digits), Exp: c.exp, Valid: true})
		if err != nil || m.Cents != c.want {
			t.Errorf("%de%d: got %d (%v), want %d", c.digits, c.exp, m.Cents, err, c.want)
		}
	}
}
