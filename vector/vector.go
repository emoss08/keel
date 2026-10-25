// Package vector builds pgvector expressions for keel queries: the distance operators, the casts
// an expression index is declared over, and the HNSW search settings. Operands are keel
// expressions (a column, a DynExpr) written in place, or values bound as parameters, such as
// pgvector-go's Vector and HalfVector.
//
//	q := embeddings.Embedding
//	distance := vector.CosineDistance(vector.HalfVec(q, 1536), vector.HalfVec(query, 1536))
//	e.Pick(e.ID, distance).Order(distance.Asc()).Limit(20)
package vector

import (
	"strconv"

	"github.com/emoss08/keel"
)

func expr(sql string, args ...any) keel.DynExpr { return keel.DynOf(keel.Raw(sql, args...)) }

func dims(n int) string {
	if n <= 0 {
		return ""
	}
	return "(" + strconv.Itoa(n) + ")"
}

// CosineDistance is a <=> b, the cosine distance an HNSW index built with vector_cosine_ops or
// halfvec_cosine_ops serves when the query orders by it.
func CosineDistance(a, b any) keel.DynExpr { return expr("(? <=> ?)", a, b) }

// L2Distance is a <-> b, the Euclidean distance (the *_l2_ops index classes).
func L2Distance(a, b any) keel.DynExpr { return expr("(? <-> ?)", a, b) }

// NegativeInnerProduct is a <#> b, the inner product negated so that ascending order ranks the
// largest product first (the *_ip_ops index classes).
func NegativeInnerProduct(a, b any) keel.DynExpr { return expr("(? <#> ?)", a, b) }

// L1Distance is a <+> b, the taxicab distance (the *_l1_ops index classes).
func L1Distance(a, b any) keel.DynExpr { return expr("(? <+> ?)", a, b) }

// HalfVec is e cast to halfvec(dimensions), as an expression index over a vector column of mixed
// dimensions declares it; the dimensions are written into the statement, so the expression
// matches the index's. A dimensions of 0 casts to halfvec without a size.
func HalfVec(e any, dimensions int) keel.DynExpr {
	return expr("(?)::halfvec"+dims(dimensions), e)
}

// Vec is e cast to vector(dimensions), as HalfVec casts to halfvec.
func Vec(e any, dimensions int) keel.DynExpr {
	return expr("(?)::vector"+dims(dimensions), e)
}

// EFSearch sets hnsw.ef_search for the transaction: how many candidates an HNSW scan keeps, so a
// query that wants more than the default 40 results, or better recall, gets them.
func EFSearch(n int) keel.TxOption { return keel.Setting("hnsw.ef_search", strconv.Itoa(n)) }

// IterativeScan sets hnsw.iterative_scan for the transaction: "strict_order" or "relaxed_order"
// lets an HNSW scan continue past ef_search candidates when a filter rejects them, "off" stops it.
func IterativeScan(mode string) keel.TxOption { return keel.Setting("hnsw.iterative_scan", mode) }
