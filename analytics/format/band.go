package format

import "math"

// MaxBandEdges is the most boundaries an explicit band may list.
const MaxBandEdges = 24

// Band groups a numeric dimension into ranges. The query groups each row on its range's lower
// bound, which keeps grouping, sorting and drill-through on exact numbers, and the display
// shows that bound as the range it stands for ("$0.00 – $1,000.00", "4:00+").
//
// A band is either fixed-width (every range Width wide, unbounded both ways) or an ascending
// list of Edges whose last range is open-ended; a value below the first edge belongs to the
// first range. Bounds are computed as exact decimals.
type Band struct {
	// Width is the width of every range of a fixed-width band.
	Width float64 `json:"width,omitempty"`
	// Edges are the ascending boundaries of an explicit band.
	Edges []float64 `json:"edges,omitempty"`
}

// IsEmpty reports whether no band was authored. A malformed band is not empty, so it fails
// validation instead of silently grouping nothing.
func (b *Band) IsEmpty() bool { return b == nil || (b.Width == 0 && len(b.Edges) == 0) }

// IsWhole reports whether every boundary is an integer, which a band on a whole-number column
// needs: its lower bounds are cast back to the column's type.
func (b *Band) IsWhole() bool {
	if b == nil {
		return true
	}
	if b.Width != math.Trunc(b.Width) {
		return false
	}
	for _, edge := range b.Edges {
		if edge != math.Trunc(edge) {
			return false
		}
	}
	return true
}

// Lowest reports whether a grouped value fell in the first range of an explicit band. Values
// below the first edge are grouped there too, so a drill-through into that range must not
// apply its lower bound.
func (b *Band) Lowest(value float64) bool {
	return !b.IsEmpty() && b.Width == 0 && len(b.Edges) > 0 && value <= b.Edges[0]
}

// Validate reports the first problem with the band, or nil. An empty band is valid.
func (b *Band) Validate() error {
	if field, code := b.problem(); code != "" {
		return &Error{Field: "band" + field, Code: code}
	}
	return nil
}

// problem returns the field and code of the band's first problem, or empty strings.
func (b *Band) problem() (string, string) {
	switch {
	case b.IsEmpty():
		return "", ""
	case b.Width != 0 && len(b.Edges) > 0:
		return "", CodeBandWidthAndEdges
	case b.Width != 0 && (b.Width < 0 || !finite(b.Width)):
		return ".width", CodeBandWidth
	case b.Width != 0:
		return "", ""
	case len(b.Edges) < 2:
		return ".edges", CodeBandTooFewEdges
	case len(b.Edges) > MaxBandEdges:
		return ".edges", CodeBandTooManyEdges
	}
	for i, edge := range b.Edges {
		if !finite(edge) {
			return ".edges", CodeNotFinite
		}
		if i > 0 && edge <= b.Edges[i-1] {
			return ".edges", CodeBandNotAscending
		}
	}
	return "", ""
}

// usable reports whether the band is set and valid, so values can be shown as ranges.
func (b *Band) usable() bool {
	_, code := b.problem()
	return !b.IsEmpty() && code == ""
}
