module github.com/emoss08/keel/bench

go 1.26.0

toolchain go1.26.10

replace (
	github.com/emoss08/keel => ../
	github.com/uptrace/bun => github.com/emoss08/bun v0.0.0-20261016005222-2580fd217ad8
	github.com/uptrace/bun/dialect/pgdialect => github.com/emoss08/bun/dialect/pgdialect v0.0.0-20261016005222-2580fd217ad8
	github.com/uptrace/bun/driver/pgdriver => github.com/emoss08/bun/driver/pgdriver v0.0.0-20261016005222-2580fd217ad8
)

require (
	github.com/emoss08/keel v0.0.0-00010101000000-000000000000
	github.com/jackc/pgx/v5 v5.11.0
	github.com/uptrace/bun v1.3.0
	github.com/uptrace/bun/dialect/pgdialect v0.0.0-00010101000000-000000000000
	github.com/uptrace/bun/driver/pgdriver v0.0.0-00010101000000-000000000000
)

require (
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/maypok86/otter/v2 v2.4.0 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	mellium.im/sasl v0.3.2 // indirect
)
