TOOL := go tool -modfile=tools/tools.mod
BUF := $(TOOL) buf
GOLANGCI_LINT := $(TOOL) golangci-lint

# Paths whose contents are produced by `make gen` and must never drift.
GENERATED := gen internal/apiserver/servicegen internal/store/sqlite/db

.PHONY: all gen check lint lint-proto lint-go breaking test e2e gen-check dev tools deps

all: check

deps: node_modules/.modules.yaml

node_modules/.modules.yaml: package.json pnpm-lock.yaml gen/ts/package.json
	pnpm install --frozen-lockfile

gen: deps
	@if [ -n "$$(find proto -name '*.proto' 2>/dev/null)" ]; then \
		$(BUF) generate && $(BUF) generate --template buf.gen.tower.yaml; \
	else \
		echo "gen: no .proto files yet"; \
	fi
	$(TOOL) sqlc generate

lint: lint-proto lint-go

lint-proto:
	@if [ -n "$$(find proto -name '*.proto' 2>/dev/null)" ]; then $(BUF) lint; fi

lint-go:
	$(GOLANGCI_LINT) run ./...

# Compares against main; skipped while main has no schema to compare with.
breaking:
	@if git cat-file -e main:proto 2>/dev/null; then \
		$(BUF) breaking --against '.git#branch=main'; \
	else \
		echo "breaking: main has no proto/ yet"; \
	fi

test:
	go test -race -timeout 20m ./...

# Hermetic end-to-end tests: tower, hostd and the CLI with fake drivers.
# Passes while test/e2e holds no package yet.
e2e:
	@if [ -n "$$(find test/e2e -name '*.go' 2>/dev/null)" ]; then \
		go test -race -timeout 20m -tags e2e ./test/e2e/...; \
	else \
		echo "e2e: no packages under test/e2e yet"; \
	fi

# Regenerates into the working tree and fails if anything changed, so it holds
# for uncommitted work as well as in CI.
gen-check: deps
	@snap=$$(mktemp -d) && cp -r --parents $(GENERATED) $$snap/ && \
	$(MAKE) --no-print-directory gen >/dev/null && \
	if ! for d in $(GENERATED); do diff -r --exclude=node_modules $$snap/$$d $$d || exit 1; done; then \
		echo "gen-check: generated code is stale; run 'make gen' and commit the result"; \
		rm -rf $$snap; exit 1; \
	fi; rm -rf $$snap

check: lint breaking test e2e gen-check

dev:
	go run ./cmd/tower-apiserver --dev

tools:
	@go tool protoc-gen-go --version
	@go list -m -f '{{.Path}} {{.Version}}' connectrpc.com/connect
	@$(BUF) --version | sed 's/^/buf /'
	@$(TOOL) sqlc version | sed 's/^/sqlc /'
	@$(GOLANGCI_LINT) version --short | sed 's/^/golangci-lint /'
	@node --version | sed 's/^/node /'
	@pnpm --version | sed 's/^/pnpm /'
