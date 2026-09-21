VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
	-X scenegit.org/forgesync/internal/buildinfo.Version=$(VERSION) \
	-X scenegit.org/forgesync/internal/buildinfo.Commit=$(COMMIT)

DIST    ?= dist
# What a machine installing ForgeSync needs and nothing else: the two
# binaries with the admin UI already inside them, and the files that set
# the thing up. No toolchain, no source, no build.
PLATFORMS ?= linux/amd64 linux/arm64

.PHONY: build web web-dev web-test test test-race test-db vet fmt check run clean release

build: web ## Build the web UI, then forgesyncd (with the UI embedded) and forgesync into bin/
	go build -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

# node_modules is not in the repository, so every target that runs the UI
# toolchain has to be able to create it. Making it a file target rather
# than a step inside `web` means `make check` works on a machine that has
# never built the UI, and that a changed lock file reinstalls: a fresh CI
# runner is exactly that machine every time, and `make check` used to fail
# there with `tsc: not found` before anything had installed tsc.
web/node_modules: web/package-lock.json web/package.json
	cd web && npm ci --no-audit --no-fund
	@touch web/node_modules

web: web/node_modules ## Build the admin UI into internal/webui/dist (embedded by go build)
	cd web && npm run build

web-dev: web/node_modules ## Vite dev server on :5173, proxying /api to a controller on :8090 (make run)
	cd web && npm run dev

web-test: web/node_modules ## Type-check and unit-test the admin UI
	cd web && npm run typecheck && npm test

test: ## Unit tests (database tests skip without FORGESYNC_TEST_DATABASE_URL)
	go test ./...

# The controller is concurrent by design: a health monitor per node, the
# lease, the replication engine, the watcher. The race detector needs cgo,
# so this wants a C compiler (apt-get install gcc) and is slower; it is
# part of `make check` because a data race that only shows up in
# production is the kind of bug nobody finds by reading.
test-race: ## Unit tests with the race detector
	CGO_ENABLED=1 go test -race ./...

# Uses the test environment's forgesync-db; the tests wipe its public schema.
test-db: ## Unit tests including the database tests
	FORGESYNC_TEST_DATABASE_URL='postgres://forgesync:forgesync-test-pw@localhost:5432/forgesync?sslmode=disable' \
		go test -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: vet test test-race web-test ## vet, gofmt check, Go tests with and without the race detector, UI tests
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

run: ## Run the controller against the local test environment
	go run ./cmd/forgesyncd -config deploy/test/forgesync.yaml

# release builds what a GitHub release carries. The admin UI is built once
# and embedded into every binary, because it is the same bytes whatever the
# processor is. Run `make release VERSION=v1.2.3` to stamp a version; it
# otherwise takes whatever `git describe` says, as the other targets do.
#
# The point of this is the install: deploy/prod/install.sh --binary takes
# one of these tarballs and needs no Go, no Node and no build, which is
# about 400 MB of toolchain a machine would otherwise fetch and keep.
release: web
	rm -rf '$(DIST)'
	mkdir -p '$(DIST)'
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  building $$os/$$arch"; \
		stage='$(DIST)'/forgesync-$(VERSION)-$$os-$$arch; \
		mkdir -p "$$stage"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' -o "$$stage"/ ./cmd/... || exit 1; \
		cp -r deploy/prod "$$stage"/deploy-prod; \
		cp LICENSE NOTICE README.md SECURITY.md "$$stage"/; \
		mkdir -p "$$stage"/docs; \
		cp docs/LIMITATIONS.md "$$stage"/docs/; \
		tar -C '$(DIST)' -czf "$$stage".tar.gz "$$(basename "$$stage")" || exit 1; \
		rm -rf "$$stage"; \
	done
	@cd '$(DIST)' && sha256sum ./*.tar.gz > SHA256SUMS
	@echo; ls -l '$(DIST)'; echo; cat '$(DIST)'/SHA256SUMS

clean:
	rm -rf bin/ '$(DIST)'
