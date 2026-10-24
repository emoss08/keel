.PHONY: check gen test bench replica replica-down test-replica

REPLICA_DSN ?= postgres://keel:keel@localhost:55433/keel?sslmode=disable

check:
	@test -z "$$(git ls-files -z '*.go' | xargs -0 gofmt -l | tee /dev/stderr)" || (echo "run gofmt -w"; exit 1)
	go vet ./...
	cd bench && go vet ./...
	go run ./cmd/keel gen -check -kinds internal/testmodels/keelkinds ./internal/testmodels/...

gen:
	go run ./cmd/keel gen -kinds internal/testmodels/keelkinds ./internal/testmodels/...

test:
	go test -race -count=1 ./...

bench:
	cd bench && go test -run xxx -bench . -benchmem -count 6

replica:
	testdata/replica/replica.sh up

replica-down:
	testdata/replica/replica.sh down

test-replica: replica
	KEEL_TEST_REPLICA_DSN='$(REPLICA_DSN)' KEEL_TEST_REQUIRE=1 go test -race -count=1 -run 'Replica' .
