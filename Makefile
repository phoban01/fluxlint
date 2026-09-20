.PHONY: build test e2e lint snapshot

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build: ## Build bin/fluxlint
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/fluxlint ./cmd/fluxlint

test: ## Hermetic unit and fixture tests (no network)
	go test -race -count=1 ./...

e2e: ## End-to-end tests against real upstream charts and repositories (network on a cold cache)
	FLUXLINT_E2E_CACHE=$${FLUXLINT_E2E_CACHE:-$(CURDIR)/.fluxlint-cache/e2e} go test -tags e2e -count=1 -v ./e2e/...

lint: ## gofmt and go vet; CI also runs golangci-lint and shellcheck
	test -z "$$(gofmt -l .)"
	go vet ./...
	go vet -tags e2e ./e2e/...

snapshot: ## Build every release target into dist/ without publishing (needs goreleaser)
	goreleaser release --snapshot --clean --skip=publish
