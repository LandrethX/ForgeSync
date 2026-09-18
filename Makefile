VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
	-X scenegit.org/forgesync/internal/buildinfo.Version=$(VERSION) \
	-X scenegit.org/forgesync/internal/buildinfo.Commit=$(COMMIT)

.PHONY: build test test-db vet fmt check run clean

build: ## Build forgesyncd and forgesync into bin/
	go build -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test: ## Unit tests (database tests skip without FORGESYNC_TEST_DATABASE_URL)
	go test ./...

# Uses the test environment's forgesync-db; the tests wipe its public schema.
test-db: ## Unit tests including the database tests
	FORGESYNC_TEST_DATABASE_URL='postgres://forgesync:forgesync-test-pw@localhost:5432/forgesync?sslmode=disable' \
		go test -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: vet test ## vet, gofmt check and tests
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

run: ## Run the controller against the local test environment
	go run ./cmd/forgesyncd -config deploy/test/forgesync.yaml

clean:
	rm -rf bin/
