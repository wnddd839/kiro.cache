VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
EXT := $(if $(filter Windows_NT,$(OS)),.exe,)

# 只格式化 git 跟踪的 Go 文件：vendor/ 里放的是参考用的 .mjs，将来若放 Go 也不该被 check 拦下。
# 配方不用 shell 内建（test / ! / grep）：make 在 Windows 上默认用 cmd.exe，它们都不存在。
GOFILES := $(shell git ls-files '*.go' 2>/dev/null)
ifeq ($(strip $(GOFILES)),)
GOFILES := .
endif

.PHONY: build test check fmt lint release clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/kiro-proxy$(EXT) ./cmd/kiro-proxy

test:
	go test -race -count=1 ./...

# 配方里不放 shell 语法（test / [ / ! 在 Windows 的 cmd.exe 里都不存在）；
# 判断放在 make 这一层，配方只 echo 和 exit。
GOFMT_NEEDED := $(strip $(shell gofmt -l $(GOFILES) 2>/dev/null))

# gofmt 的判定与 CI 同一处、同一写法：gofmt -l 无输出即通过，有输出则列出文件。
check: fmt lint
	go test -race -count=1 ./...

fmt:
ifeq ($(GOFMT_NEEDED),)
	@echo "gofmt: clean"
else
	@echo "gofmt needed in:"
	@echo "$(GOFMT_NEEDED)"
	@exit 1
endif

lint:
	go vet ./...

# 交叉编译到 dist/
release:
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kiro-proxy-windows-amd64.exe ./cmd/kiro-proxy
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kiro-proxy-linux-amd64 ./cmd/kiro-proxy
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kiro-proxy-darwin-arm64 ./cmd/kiro-proxy

clean:
	rm -rf bin dist
