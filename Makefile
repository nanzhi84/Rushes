SHELL := /bin/bash
PNPM := npx -y pnpm@10.13.1

.PHONY: dev install-web generate contracts test coverage lint vet-integration web e2e check

dev:
	bash scripts/dev_all.sh

install-web:
	$(PNPM) --dir apps/web install --frozen-lockfile
	$(PNPM) --dir e2e install --frozen-lockfile

generate:
	cd go && go generate ./internal/api
	bash scripts/gen_web_types.sh

contracts:
	bash scripts/check_go_contracts.sh

test:
	cd go && go test -race ./...

coverage:
	bash scripts/check_go_coverage.sh

lint:
	cd go && go vet ./...
	cd go && golangci-lint run --timeout=5m

vet-integration:
	cd go && go vet -tags=integration ./...

web:
	$(PNPM) --dir apps/web typecheck
	$(PNPM) --dir apps/web exec vitest run
	$(PNPM) --dir apps/web build
	$(PNPM) --dir apps/web run check:bundle

e2e:
	cd go && go test -race -tags=e2e_scaffold ./internal/agent ./internal/api
	$(PNPM) --dir e2e exec playwright test

check: contracts test coverage lint web e2e
