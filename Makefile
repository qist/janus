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
# macOS 发布架构（Intel + Apple Silicon）
DARWIN_ARCHES := amd64 arm64
# Windows 发布架构（二进制带 .exe 后缀）
WINDOWS_ARCHES := amd64 arm64

.PHONY: all build test test-race vet fmt run dist dist-linux dist-darwin dist-windows compat smoke stress clean

all: vet test build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	$(GO) test ./...

# 竞态检测（需要 cgo/本机 C 工具链）
test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

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

# 并发压测（需先起服务 + 装 openai）。N 默认 100：
#   make stress BRIDGE_MODEL=opencode-go/deepseek-v4.1-flash N=100
stress:
	BRIDGE_MODEL="$(BRIDGE_MODEL)" N="$(N)" JANUS_PID="$$(pgrep -x $(BINARY) | head -1)" python3 tests/stress.py

# Claude Code 冒烟（Anthropic 官方 SDK）。需先起服务 + 装 anthropic：
#   make smoke BRIDGE_MODEL=opencode-go/deepseek-v4.1-flash:max
smoke:
	ANTHROPIC_BASE_URL="http://127.0.0.1:2810" ANTHROPIC_API_KEY="$(BRIDGE_API_KEY)" \
		BRIDGE_MODEL="$(BRIDGE_MODEL)" python3 tests/claude_smoke.py

# Linux 全架构静态二进制 + tar.gz（CGO 关闭，纯静态）
# 包里除二进制外，附带 janus.env.example（配置模板）与 README.md。
dist-linux:
	@mkdir -p $(DIST)
	@for arch in $(LINUX_ARCHES); do \
		echo ">> linux/$$arch  ($(VERSION))"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$(BINARY)_linux_$$arch . ; \
		tar -czf $(DIST)/$(BINARY)_$(VERSION)_linux_$$arch.tar.gz \
			-C $(DIST) $(BINARY)_linux_$$arch \
			-C $(CURDIR) janus.env.example README.md ; \
		rm -f $(DIST)/$(BINARY)_linux_$$arch ; \
	done
	@echo "==> $(DIST)/"; ls -lh $(DIST)

# macOS 全架构二进制 + tar.gz（CGO 关闭；Intel + Apple Silicon）
# 包里除二进制外，附带 janus.env.example（配置模板）与 README.md。
dist-darwin:
	@mkdir -p $(DIST)
	@for arch in $(DARWIN_ARCHES); do \
		echo ">> darwin/$$arch  ($(VERSION))"; \
		CGO_ENABLED=0 GOOS=darwin GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$(BINARY)_darwin_$$arch . ; \
		tar -czf $(DIST)/$(BINARY)_$(VERSION)_darwin_$$arch.tar.gz \
			-C $(DIST) $(BINARY)_darwin_$$arch \
			-C $(CURDIR) janus.env.example README.md ; \
		rm -f $(DIST)/$(BINARY)_darwin_$$arch ; \
	done
	@echo "==> $(DIST)/"; ls -lh $(DIST)

# Windows 全架构二进制 + zip（CGO 关闭）
# 包里除 janus.exe 外，附带 janus.env.example（配置模板）与 README.md。
dist-windows:
	@mkdir -p $(DIST)
	@for arch in $(WINDOWS_ARCHES); do \
		echo ">> windows/$$arch  ($(VERSION))"; \
		CGO_ENABLED=0 GOOS=windows GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$(BINARY)_windows_$$arch.exe . ; \
		zip -qj $(DIST)/$(BINARY)_$(VERSION)_windows_$$arch.zip \
			$(DIST)/$(BINARY)_windows_$$arch.exe $(CURDIR)/janus.env.example $(CURDIR)/README.md ; \
		rm -f $(DIST)/$(BINARY)_windows_$$arch.exe ; \
	done
	@echo "==> $(DIST)/"; ls -lh $(DIST)

# 全部平台（linux + darwin + windows）
dist: dist-linux dist-darwin dist-windows

clean:
	rm -rf $(BINARY) $(DIST)
