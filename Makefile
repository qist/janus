BINARY  := janus
BUILD   := build
GO      ?= go
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all build test vet fmt run clean

all: vet test build

build:
	@mkdir -p $(BUILD)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD)/$(BINARY) ./cmd/janus

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

run:
	$(GO) run ./cmd/janus

clean:
	rm -rf $(BUILD) dist
