# Reading

A query starts from a handle and ends with a terminal that takes `(ctx, db)`. Handles are immutable
and safe to share. Queries are builders that change themselves: build one per call, and `Clone()`
to branch.

```go
base := s.Where(s.CustomerID.Eq(customerID))
open := base.Clone().Where(s.Status.Ne(fleet.StatusDelivered))
```

A handle starts a query with `Query`, `Where`, `Pick`, `Load`, `Order`, `Limit`, `Group`, `Join`,
`LeftJoin`, `JoinLateral`, `LeftJoinLateral`, `Lock`, `DistinctOn`, `With`, `AllTenants`.
`Having`, `Offset`, `CustomPlan`, `Timeout` exist on queries only: `s.Query().Timeout(d)`.

## Terminals

| Method | Result |
|---|---|
| `Get(ctx, db, key)` | row by PK (generated); `ErrNotFound` |
| `One` | exactly one: `ErrNotFound`, `ErrNotSingular` |
| `First` | first in order; `ErrNotFound` |
| `List` | rows; `ErrUnbounded` without `Limit` |
| `All` | every row (small sets only) |
| `Count`, `CountUpTo(ctx, db, n)`, `CountEstimate(ctx, db, exactUpTo)` | counts (`keel.RowCount{N, Exact}` for the estimate) |
| `Exists` | any row? |
| `Page(ctx, db, req)`, `Each(ctx, db, size, fn)` | keyset page / batches |
| `Update(ctx, db, sets...)`, `UpdateReturning`, `Delete(ctx, db)` | bulk writes |
| `keel.Into[R](ctx, db, q)`, `keel.IntoOne[R]` | projections |
| `ToSQL(ctx, db)`, `Explain(ctx, db, opts)` | the rendered statement / its plan |

Rows come back as `*T`. `Pick` with only the table's columns narrows what's read (other fields stay
zero, PK always read). `Pick` with anything else makes a projection that needs `keel.Into`.
`Omit(cols...)` reads the default columns except heavy/secret ones; loads take it too:
`s.Load(s.Customer().Omit(s.Customer().Email))`.

## Operators

Every clause takes expressions, never strings. Values are bind parameters.

| Column type | Operators |
|---|---|
| every column | `IsNull`, `IsNotNull`, `EqCol`, `NeCol`, `InQuery`, `NotInQuery`, `Asc`, `Desc`, `Count`, `CountDistinct`, `Min`, `Max`, `Coalesce(v)`, `ToNull`, `ToDefault`, `ToExpr(e)` |
| `StringCol` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `In`, `NotIn`, `To`; `Contains`, `ContainsFold`, `HasPrefix`, `HasPrefixFold`, `HasSuffix` (escaped: `%`/`_` literal); `Like`, `ILike` (patterns as given); `Match`, `MatchFold` (regex); `Cat`, `Collate`, `InArray`; `Lower`, `Upper`, `Trim`, `Length`, `Left`, `Right`, `Substr`, `Replace`, `SplitPart`, `NullIf`, … |
| `Int64Col`, `Int32Col` | comparisons, `In`, `NotIn`, `To`, `Increment`, `Sum`, `Avg`, `Add`, `Sub`, `Mul`, `Div`, `Mod`, `Neg`, `Abs`, `NullIf`, `Greatest`, `Least`, `InArray` |
| `Int16Col` | as Int32 without `Avg`/arithmetic (use `keel.CastInt64(col)` first) |
| `Float64Col` | comparisons, `To`, `Increment`, `Sum`, `Avg`, arithmetic, `Round`, `Floor`, `Ceil`, `Ln`, `Exp` |
| `BoolCol` | `Eq`, `IsTrue`, `IsFalse`, `To` |
| `TimeCol` | comparisons, `To`, `Add(interval)`, `Sub(interval)`, `Since(t)`, `ToChar`, `AtTimeZone` |
| `AnyCol` / wrappers (`MoneyCol`) | comparisons, `In`, `NotIn`, `To`, `Increment`, `Sum`, `Avg`, numeric arithmetic |
| `JSONBCol` | `Contains(v)` (`@>`), `HasKey`, `HasAnyKey`, `HasAllKeys`, `Text(path...)` (`#>>`), `To`, `TypeOf`, `Set`, `Concat` |
| `StringArrayCol`, `Int64ArrayCol` | `Has`, `Overlaps`, `ContainsAll`, `ContainedBy`, `Len`, `Unnest`, `To`, `Append`, `Remove`, `At(i)`, `Without(v)` |
| `TSVectorCol` | `Match(q, opts...)`, `Rank(q, opts...)`, `RankCD(q, opts...)` |

Generated wrappers (`StatusCol`, `IdsIDCol`) take the named type. A named string type's wrapper has
`Col`'s methods (`Asc`, `IsNull`, `Count`, ...) and the typed comparisons; text functions go through
`Text()`: `s.Status.Text().Lower()`, `s.ID.Text().Collate("C")`, `s.ID.Text().InArray(a)`. Read a
CTE with `keel.RowsOf(cte.TableRef())`; `CTETable` has no `Query` method. `In` binds one array (`= ANY($1)`),
so 3 and 3,000 values are one prepared statement. Combinators: `keel.And`, `keel.Or` (skip nil
predicates), `keel.Not`, `keel.True()`. `Where` ANDs its arguments. `keel.When(cond, p)` returns
`p` or nil.

```go
n, err := s.Where(
    s.Status.In(fleet.StatusNew, fleet.StatusAssigned),
    s.Weight.Between(10_000, 40_000),
    s.BillToID.IsNull(),
    s.ProNumber.HasPrefix("PRO-"),
    s.Labels.Has("expedite"),
    s.Attributes.Text("trailer", "type").Eq("reefer"),
    s.SearchVector.Match("dallas reefer"),
    keel.Or(s.CanceledAt.IsNull(), s.CanceledAt.Gt(since.Unix())),
    keel.Not(s.Customer().Name.ContainsFold("test")),
).Count(ctx, db)
```

## Typed expressions

Expression types per kind: `keel.StringExpr`, `Int64Expr`, `Int32Expr`, `Float64Expr`,
`NumericExpr`, `BoolExpr`, `TimeExpr`, `JSONBExpr`, `IntervalExpr`. They compare with Go values
(`Eq`, …) and with each other (`EqExpr`, `LtExpr`, …). Kinds don't mix implicitly: convert with
`keel.CastInt64`, `CastNumeric`, `CastFloat64`, `CastText`, `CastBool`, `CastTime`, `CastJSONB`
(SQL `CAST`), or type an untyped expression without changing SQL with `keel.StringOf`, `Int64Of`,
`NumericOf`, ….

- Bind a Go value as a typed expression: `keel.Text(s)`, `Int64(n)`, `Int32`, `Float64`, `Numeric`,
  `Bool`, `Time`, `JSONB`, `Interval("1 day")`, `Duration(d)`; nullable: `keel.TextPtr(p)`, `Int64Ptr`, …;
  typed NULL: `keel.NullText()`, `NullInt64()`, ….
- Constants written into SQL (only to match an expression index, partial index predicate or
  `ON CONFLICT` target; never user values): `keel.ConstText`, `ConstInt64`, `ConstInt32`,
  `ConstBool`, `ConstFloat64`, `ConstNumeric`.
- Arithmetic: `a.Add(b)`, `Sub`, `Mul`, `Div`, `Mod`, `Neg`; integer division truncates, guard a
  zero divisor with `NullIf(0)`. Assignment `col = col + v` is `col.Increment(v)`.
- Text: `Cat` (`||`, NULL-propagating) vs `keel.Concat`/`keel.ConcatWS` (skip NULLs).
- Functions: `keel.Now()`, `NowEpoch()`, `Epoch(t)`, `ToTimestamp(sec)`, `DatePart(field, t)`,
  `DateTrunc(field, t)`, `BoolOr`, `BoolAnd`, `JSONBAgg`, `JSONBuildObject(keel.Pair(k, v)…)`,
  `ArrayPosition(list, v)` (keep a caller's order), `WidthBucket`, `GenerateSeries`,
  `GenRandomUUID()`.
- CASE: `keel.CaseString().When(cond, keel.Text("a")).Else(keel.Text("b"))`; also `CaseInt64`,
  `CaseNumeric`, `CaseBool`, `CaseTime`, …
- Rows: `keel.Tuple(a, b).Gt(keel.Tuple(x, y))` (keyset condition); `keel.Tuple(a, b).InUnnest(keel.TextArray(xs), keel.Int64Array(ys))`.
- Arrays: `keel.TextArray(ids)` (one param), `keel.ArrayOfString(e1, e2)` (`ARRAY[…]`),
  `keel.ArrayOf(q)` (`ARRAY(subquery)`), `e.InArray(a)`, `a.ContainsAllExpr(o)`.
- Declared DB functions: `var slugify = keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("public.slugify")`,
  then `slugify.Call(c.Name)`; check with `keel.VerifyFuncs`.

An expression used in two clauses (`Pick` and `Group`) binds once only if it is the same value:
assign it to a variable and reuse it.

```go
day := keel.DateTrunc("day", keel.ToTimestamp(s.CreatedAt))
rows, err := keel.Into[daily](ctx, db, s.Pick(day, keel.Count()).Group(day).Order(keel.Asc(day)))
```

Inside typed queries prefer the typed forms over `keel.DynOf`/`DynFunc`/`DynLit`/`DynCase`/`DynConst`
(same SQL, compile-time checks).

## Raw SQL

`keel.Raw(sql, args...)` is SQL text keel does not understand; each `?` is the next argument (an
expression is written in place with its alias, anything else is bound); `??` is a literal `?`.
It's both an expression and a predicate. Build the text from constants only. `keel.Val(v)` binds a
value where an expression is expected. If a `Raw` fragment reads a table and the query is cached,
name that table: `.Cache(keel.AlsoReads("table"))`.

```go
s.Where(keel.Raw("? ?? 'trailer'", s.Attributes)) // jsonb ? operator
```

Whole statements outside the query builder: `keel.Exec(ctx, db, sql, args...)`,
`keel.QueryAll[T](ctx, db, pgx.RowToStructByName[T], sql, args...)`, `keel.QueryValue[T]`,
`keel.QueryRows`. They join the context's transaction but are **not scoped**: write the tenant
predicate yourself and call `keel.MarkChanged(ctx, db, tables...)` after writes if data versions or
the cache are on.

## Relation paths, Any and None

To-one paths (`belongs`, `one`, `pick`) LEFT JOIN automatically with the tenant condition. To-many
paths are only valid inside `Any` (EXISTS) and `None` (NOT EXISTS). Each path gets its own alias.
Use the path handle (`s.Customer().Code`), never the base handle (`fleetdb.Customers.Code`), to filter
on a relation.

```go
m := s.Moves()
st := m.Stops()
rows, err := s.Where(
    s.Customer().Code.Eq("ACME"),
    m.Any(m.Status.Eq(fleet.StatusInTransit), m.Loaded.IsTrue()),
    m.Any(st.Any(st.Kind.Eq("delivery"), st.Location().City.Eq("Dallas"))),
    s.Tags().None(s.Tags().Name.Eq("on-hold")),
).Limit(100).List(ctx, db)
```

## Column paths from names

For APIs that filter/sort by client field names: `keel.ResolvePath(handle, segments...)` returns a
`DynExpr` (last segment a column by SQL or Go name; earlier ones to-one relations). It shares joins
with typed paths. A bad name makes an expression whose `Err()` the terminal returns. Map JSON field
names with the generated schema (`Column.JSON`, `Relation.JSON`).

```go
rows, err := s.Where(keel.ResolvePath(s, "customer", "name").ContainsFold(q)).
    Order(keel.ResolvePath(s, sortField...).Desc()).Page(ctx, db, keel.After(cursor, 50))
```

## Joins, subqueries, lateral

```go
w := fleetdb.Workers
mgr := w.As("mgr") // constant alias names
reports, err := w.Join(mgr, mgr.ID.EqCol(w.ManagerID)).Where(mgr.LastName.Eq("Hopper")).All(ctx, db)

idle, err := c.Where(keel.NotExists(s.Where(s.CustomerID.EqCol(c.ID), s.CreatedAt.Gte(cutoff)))).All(ctx, db)
n, err := c.Where(c.ID.InQuery(s.Pick(s.CustomerID).Where(s.Hazmat.IsTrue()))).Count(ctx, db)
loads := keel.Scalar(s.Pick(keel.Count()).Where(s.CustomerID.EqCol(c.ID))) // (SELECT …) as an expression

last := fleetdb.Stops.As("last_stop")
rows, err := keel.Into[moveEnd](ctx, db, m.
    LeftJoinLateral(last.Pick(last.LocationID, last.Sequence).Where(last.MoveID.EqCol(m.ID)).
        Order(last.Sequence.Desc()).Limit(1)).
    Pick(m.ID, last.LocationID, last.Sequence))
```

Joined rows still decode into the base model; to read joined columns, `Pick` them and use
`keel.Into`. Subqueries are scoped like any statement. Prefer relation paths and `Load` over manual
joins when a relation exists.

## CTEs and set operations

```go
busy := keel.CTE("busy", []string{"customer_id", "loads"},
    s.Group(s.CustomerID).Pick(s.CustomerID, keel.Count()).Where(s.Status.Eq(fleet.StatusInTransit)))
customers, err := c.With(busy).Join(busy, busy.Col("customer_id").EqCol(c.ID)).
    Where(busy.Col("loads").Gte(10)).All(ctx, db)
top, err := keel.Into[load](ctx, db, keel.RowsOf(busy.TableRef()).With(busy).
    Pick(busy.Col("customer_id"), busy.Col("loads")).Order(busy.Col("loads").Desc()).Limit(5))

chain := keel.RecursiveCTE("chain", []string{"id", "depth"}, base) // .Recurse(step), .Distinct()
codes, err := keel.Into[string](ctx, db, keel.UnionAll(s.Pick(s.ProNumber), c.Pick(c.Code)))
```

`keel.Union`, `UnionAll`, `Intersect`, `Except` return a `keel.Compound` (read with `Into`, or use as
a subquery/CTE body). `keel.Rows(name, cols, exprs...)` is a lateral source from set-returning
functions; `keel.Unnest(name, cols, slices...)` a CTE over Go slices.

## Aggregates, grouping, windows

```go
type statusTotal struct {
    Status  fleet.Status
    Loads   int64
    Hazmat  int64
    Revenue fleet.Money
}
rows, err := keel.Into[statusTotal](ctx, db, s.
    Group(s.Status).
    Pick(s.Status, keel.Count(), keel.Count().Filter(s.Hazmat.IsTrue()), s.Total.Sum()).
    Having(keel.Count().Gt(10)).
    Order(keel.Count().Desc()))
heaviest, err := keel.IntoOne[*int64](ctx, db, s.Pick(s.Weight.Max())) // pointer: may be NULL
```

- `keel.Into[R]` fills `R`'s exported fields in pick order; counts must match. `R` may be a scalar.
- Aggregates are `keel.Agg`: `keel.Count()`, `CountOf(e)`, `Sum`, `Avg`, `Min`, `Max`, column
  methods; `.Filter(pred)`, `.Over(partition, order...)`, `.OverWindow(w)`.
- `Rollup(a, b)`, `Cube(...)`, `GroupingSets(sets...)` (nil = grand total), `keel.Grouping(es...)`
  to tell detail from subtotal rows.
- Windows: `keel.RowNumber()`, `Rank`, `DenseRank`, `Ntile`, `Lag`, `Lead`, `FirstValue`,
  `LastValue`, `NthValue`; `keel.PartitionBy(...).OrderBy(...).Rows(keel.UnboundedPreceding, keel.CurrentRow)`;
  `Query.Window(name, w)` + `keel.NamedWindow(name)`.
- `DistinctOn(es...)` with an `Order` that starts with the same expressions.

## Ordering, locks, plans

- `col.Asc()`, `col.Desc()`, `keel.Asc(expr)`, `.NullsFirst()`, `.NullsLast()`.
- `Lock(keel.ForUpdate|ForNoKeyUpdate|ForShare|ForKeyShare, keel.Wait|NoWait|SkipLocked)` locks the
  base table's rows; only useful inside `keel.Write`. `NoWait` fails with `keel.Code(err) == "55P03"`.
  Work queue: `Where(...).Order(...).Limit(20).Lock(keel.ForUpdate, keel.SkipLocked).List`.
- `CustomPlan()`: plan this query for its values every time (tenant-skewed selectivity).
- `Timeout(d)`: deadline for the terminal and its loads (`context.DeadlineExceeded`).

## Full-text search

```go
rows, err := n.Compute(n.Rank, n.SearchVector.Rank(q)).
    Where(n.SearchVector.Match(q)).Order(n.Rank.Desc()).Limit(20).List(ctx, db)
s.SearchVector.Match(q, keel.SearchConfig("simple", "english"))
s.SearchVector.Match(q, keel.SearchModes(keel.WebSearch, keel.Prefix)) // search as you type
```

The search string is always bound; pass user input as typed. Never use `keel.TSQuery` mode with
user input. Trigram: `ContainsFold`/`ILike` (a trigram GIN index serves them), or
`keel.Raw("similarity(?, ?)", col, text)`.

## Debugging

```go
sql, args, err := q.ToSQL(ctx, db)                                  // main statement only
plan, err := q.Explain(ctx, db, keel.ExplainOptions{})              // TotalCost, Rows, JSON
plan, err = q.Explain(ctx, db, keel.ExplainOptions{Analyze: true})  // runs inside a rolled-back tx
plan, err = q.Explain(ctx, db, keel.ExplainOptions{GenericPlan: true}) // plan.Indexes(), plan.SeqScans()
```
