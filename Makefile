.PHONY: build test e2e lint

build: ## Build bin/fluxlint
	go build -o bin/fluxlint ./cmd/fluxlint

test: ## Hermetic unit and fixture tests (no network)
	go test -race -count=1 ./...

e2e: ## End-to-end tests against real upstream charts and repositories (network on a cold cache)
	FLUXLINT_E2E_CACHE=$${FLUXLINT_E2E_CACHE:-$(CURDIR)/.fluxlint-cache/e2e} go test -tags e2e -count=1 -v ./e2e/...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go vet -tags e2e ./e2e/...
