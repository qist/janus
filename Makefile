BINARY  := janus
BUILD   := build
GO      ?= go
UI      := web
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: all build test vet fmt run clean ui ui-dev ui-check

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

# 重新构建控制台产物到 internal/ui/dist（产物随仓库提交，go build 不依赖 Node）
ui:
	cd $(UI) && npm ci && npm run build

ui-dev:
	cd $(UI) && npm run dev

ui-check:
	cd $(UI) && npm ci && npm run type-check && npm run build

clean:
	rm -rf $(BUILD) dist
