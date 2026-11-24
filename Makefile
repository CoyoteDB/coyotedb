# CoyoteDB dev commands. Run "make help" for the list.
SHELL := /bin/bash
export PATH := $(HOME)/.local/bin:$(HOME)/.local/go/bin:$(PATH)

.PHONY: build test itest chaos check fmt help

# On macOS, keep the machine from idle-sleeping during the long test runs:
# a sleep longer than the lease makes the main node fence itself (as it
# should), and the test sees that as a failure.
AWAKE := $(if $(filter Darwin,$(shell uname -s 2>/dev/null)),caffeinate -i,)

help:
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/'

build: ## build ./bin/coyote
	go build -o bin/coyote ./cmd/coyote

test: ## unit tests
	go test ./...

itest: ## integration tests (real Postgres)
	$(AWAKE) go test -tags=integration -count=1 -timeout 40m ./test/integration/...

chaos: ## chaos tests with the write checker
	$(AWAKE) go test -tags=chaos -count=1 -timeout 150m ./test/chaos/...

fmt: ## format Go code
	@[ -f go.mod ] && gofmt -w cmd internal test 2>/dev/null || true

check: ## gofmt + vet + unit tests; run before every commit
	@out="$$(gofmt -l cmd internal test 2>/dev/null)"; [ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }
	go vet ./...
	go test ./...
