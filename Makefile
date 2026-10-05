VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
EXT := $(if $(filter Windows_NT,$(OS)),.exe,)

.PHONY: build test check release clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/kiro-go$(EXT) ./cmd/kiro-go

test:
	go test -race -count=1 ./...

check:
	gofmt -l . | (! grep .)
	go vet ./...
	go test -race -count=1 ./...

# 交叉编译到 dist/
release:
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kiro-go-windows-amd64.exe ./cmd/kiro-go
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kiro-go-linux-amd64 ./cmd/kiro-go
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kiro-go-darwin-arm64 ./cmd/kiro-go

clean:
	rm -rf bin dist
