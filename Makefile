GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOVET=$(GOCMD) vet
BIN_TERRAPLANE=./bin/terraplane
BIN_TERRAPLANE_LINUX=$(BIN_TERRAPLANE)-linux-amd64
BIN_TERRAPLANE_DARWIN=$(BIN_TERRAPLANE)-darwin-arm64
GOLANGCI_LINT := golangci-lint

.PHONY: build unit-test tests clean generate run-orchestrator run-agent build-linux build-darwin lint db-migrate-diff db-migrate-validate govulncheck coverage

default: build


build:
		CGO_ENABLED=0 $(GOBUILD) -o $(BIN_TERRAPLANE) -v .

# Packages expected to stay at 100% unit coverage (pure / minimal deps).
# scm/github and agent/orchestrator are intentionally omitted: unreachable without live external services
# marshal-error and transport-construction branches are not worth package-level test hooks.
# workspace/terraform intentionally omit Ensure/download and some OS edge paths.
COVERAGE_FULL_PKGS=./pkg/log ./internal/auth ./pkg/terraplaneconfig ./pkg/orchestratorconfig ./pkg/authz ./pkg/feedback ./pkg/command ./pkg/orchestrator/services ./pkg/agent/handlers ./pkg/webserver
# Exclude generated mocks and protobuf stubs from the aggregate report.
COVER_PKGS=$$(go list ./... | grep -vE '/mock_|/pkg/terraplane/v1$$')

unit-test:
		$(GOVET) ./...
		TEST=true $(GOTEST) -v -covermode=atomic -coverprofile=coverage.out $(COVER_PKGS)
		$(GOCMD) tool cover -func=coverage.out
		@$(GOCMD) test -covermode=atomic -coverprofile=coverage-full.out $(COVERAGE_FULL_PKGS)
		@$(GOCMD) tool cover -func=coverage-full.out | awk '/total:/ { \
			gsub(/%/, "", $$3); \
			if ($$3+0 < 100) { printf "expected 100%% coverage for pure packages, got %s%%\n", $$3; exit 1 } \
			printf "pure packages coverage: %s%%\n", $$3; \
		}'

coverage:
		$(GOCMD) tool cover -html=coverage.out -o coverage.html

tests:
		$(GOVET) ./...
		$(GOTEST) -v --tags=integration -covermode=atomic -coverprofile=coverage.out $(COVER_PKGS)

clean:
		$(GOCLEAN)
		rm -f $(BIN_TERRAPLANE) $(BIN_TERRAPLANE_LINUX) coverage.out coverage-full.out coverage.html
generate:
		$(GOCMD) generate -v ./...

run-orchestrator: build
		$(BIN_TERRAPLANE) orchestrator

run-agent: build
		$(BIN_TERRAPLANE) agent

# Cross compilation
build-linux:
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BIN_TERRAPLANE_LINUX) -v .

build-linux-arm64:
		CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BIN_TERRAPLANE)-linux-arm64 -v .

build-darwin:
		CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GOBUILD) -o $(BIN_TERRAPLANE_DARWIN) -v .

build-darwin-amd64:
		CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GOBUILD) -o $(BIN_TERRAPLANE)-darwin-amd64 -v .

lint:
	$(GOLANGCI_LINT) run

# Generate a migration from GORM model changes. Usage: make db-migrate-diff name=add_job_status
db-migrate-diff:
	@test -n "$(name)" || (echo 'usage: make db-migrate-diff name=<migration_name>' && exit 1)
	atlas migrate diff $(name) --env gorm

db-migrate-validate:
	atlas migrate validate --dir file://pkg/storage/migrations

# tools/atlas is build-tagged out of ./... so govulncheck does not traverse the
# Atlas provider dependency graph (which panics symbol scan on Go 1.25).
govulncheck:
	govulncheck -C . -format text ./...
