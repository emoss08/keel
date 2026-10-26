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
	entgo.io/ent v0.14.6
	github.com/aarondl/opt v0.0.0-20250607033636-982744e1bd65
	github.com/bytedance/sonic v1.15.4
	github.com/emoss08/keel v0.0.0-00010101000000-000000000000
	github.com/go-jet/jet/v2 v2.16.1
	github.com/jackc/pgx/v5 v5.11.0
	github.com/jmoiron/sqlx v1.4.0
	github.com/lib/pq v1.12.3
	github.com/shopspring/decimal v1.6.0
	github.com/stephenafamo/bob v0.50.0
	github.com/stephenafamo/scan v0.9.0
	github.com/uptrace/bun v1.3.0
	github.com/uptrace/bun/dialect/pgdialect v0.0.0-00010101000000-000000000000
	github.com/uptrace/bun/driver/pgdriver v0.0.0-00010101000000-000000000000
	gorm.io/driver/postgres v1.6.3
	gorm.io/gorm v1.31.2
)

require (
	ariga.io/atlas v0.36.2-0.20250730182955-2c6300d0a3e1 // indirect
	github.com/agext/levenshtein v1.2.3 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/bmatcuk/doublestar v1.3.4 // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic/loader v0.5.2 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/go-openapi/inflect v0.19.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/hcl/v2 v2.18.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/maypok86/otter/v2 v2.4.0 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/qdm12/reprint v0.0.0-20200326205758-722754a53494 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/zclconf/go-cty v1.14.4 // indirect
	github.com/zclconf/go-cty-yaml v1.1.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/arch v0.0.0-20210923205945-b76863e36670 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	mellium.im/sasl v0.3.2 // indirect
)
