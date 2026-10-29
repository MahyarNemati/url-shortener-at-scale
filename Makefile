GO ?= go
export CGO_ENABLED ?= 0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test test-race lint fmt vet cover bench run up down migrate load image clean

all: lint test

build:
	$(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/shortener ./cmd/shortener

test:
	$(GO) test -count=1 ./...

# The race detector needs cgo, which this host's macOS SDK cannot link.
test-race:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

cover:
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/...

lint: fmt vet

fmt:
	@unformatted=$$(gofmt -l . | grep -v '^$$' || true); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

vet:
	$(GO) vet ./...

## run: start the service with an in-memory store
run:
	SHORTENER_TOKEN=$${SHORTENER_TOKEN:-dev-token-at-least-16ch} \
	SHORTENER_SLUG_SECRET=$${SHORTENER_SLUG_SECRET:-dev-slug-secret} \
	$(GO) run ./cmd/shortener

## up: service + ClickHouse
up:
	docker compose up -d --build --wait

down:
	docker compose down -v

## migrate: apply the ClickHouse schema
migrate: build
	./bin/shortener -clickhouse http://localhost:8123 -migrate

## load: run the k6 read-path load test (needs k6 and a running service)
load:
	k6 run -e TOKEN=$${SHORTENER_TOKEN:-dev-token-at-least-16ch} k6/redirect.js

image:
	docker build --build-arg VERSION=$(VERSION) -t url-shortener:$(VERSION) .

clean:
	rm -rf bin coverage.out
