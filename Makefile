# SlopGuard Makefile
# Cross-compile: make build-all  (pure Go, zero CGO)

BINARY  := slopguard
PKG     := github.com/HunterLzap/slopguard
LDFLAGS := -s -w

.PHONY: all build build-all test lint clean

all: build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/slopguard

build-all:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64   ./cmd/slopguard
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64   ./cmd/slopguard
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-amd64  ./cmd/slopguard
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-arm64  ./cmd/slopguard
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-windows-amd64.exe ./cmd/slopguard

test:
	CGO_ENABLED=0 go test ./...

lint:
	go vet ./...

clean:
	rm -rf bin/
