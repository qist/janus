BINARY  := janus
GO      ?= go
DIST    := dist

# 版本号来源优先级：显式 VERSION= > git tag > VERSION 文件 > dev
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || cat VERSION 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.Date=$(DATE)

# Linux 发布架构（对应 GitHub Release 里的 janus_<version>_linux_<arch>.tar.gz）
LINUX_ARCHES := amd64 arm64 arm 386

.PHONY: all build test vet fmt run dist-linux compat smoke clean

all: vet test build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w *.go

# 前台运行（读取仓库根的 janus.env）
run: build
	./$(BINARY)

# 兼容性矩阵（需先起服务；python 环境需装 openai）。
# 指定一个可用的模型（免费额度模型经 API 会 403）：
#   make compat BRIDGE_MODEL=opencode-go/deepseek-v4.1-flash:max
compat:
	BRIDGE_MODEL="$(BRIDGE_MODEL)" python3 tests/compat_matrix.py

# Claude Code 冒烟（Anthropic 官方 SDK）。需先起服务 + 装 anthropic：
#   make smoke BRIDGE_MODEL=opencode-go/deepseek-v4.1-flash:max
smoke:
	ANTHROPIC_BASE_URL="http://127.0.0.1:2810" ANTHROPIC_API_KEY="$(BRIDGE_API_KEY)" \
		BRIDGE_MODEL="$(BRIDGE_MODEL)" python3 tests/claude_smoke.py

# Linux 全架构静态二进制 + tar.gz（CGO 关闭，纯静态）
dist-linux:
	@mkdir -p $(DIST)
	@for arch in $(LINUX_ARCHES); do \
		echo ">> linux/$$arch  ($(VERSION))"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$(BINARY)_linux_$$arch . ; \
		tar -C $(DIST) -czf $(DIST)/$(BINARY)_$(VERSION)_linux_$$arch.tar.gz $(BINARY)_linux_$$arch ; \
		rm -f $(DIST)/$(BINARY)_linux_$$arch ; \
	done
	@echo "==> $(DIST)/"; ls -lh $(DIST)

clean:
	rm -rf $(BINARY) $(DIST)
