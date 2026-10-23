# keel

keel is a typed, Postgres-only data layer for Go, built for large multi-tenant applications.

- **Typed end to end**: columns, relations, values, assignments and results are checked by the compiler.
- **One round trip where Postgres allows it**: transactions start with their first statement and a lone statement runs as one pipelined `BEGIN; …; COMMIT`.
- **No reflection on the hot path**: `keel gen` writes field accessors for every model.
- **Tenancy by construction**: the tenant comes from the context and is applied to every statement.

```go
s := shipmentdb.Shipments
m := s.Moves()

page, err := s.
    Where(
        s.Status.In(shipment.StatusNew, shipment.StatusAssigned),
        keel.When(search != "", s.Customer().Name.Contains(search)),
        m.Any(m.Status.Eq(shipment.StatusAssigned)),
    ).
    Load(s.Customer(), m.Order(m.Sequence.Asc()), m.Stops().Location()).
    Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000))
```

See [docs/design.md](docs/design.md) for the design and [docs/guide.md](docs/guide.md) for usage.

## Development

```sh
docker run -d --name keelpg -e POSTGRES_USER=keel -e POSTGRES_PASSWORD=keel -e POSTGRES_DB=keel -p 55432:5432 postgres:18-alpine
go test ./...                      # integration tests skip when Postgres is unreachable
KEEL_TEST_REQUIRE=1 go test ./...  # fail instead of skipping
go run ./cmd/keel gen -kinds internal/testmodels/keelkinds ./internal/testmodels/...
```
