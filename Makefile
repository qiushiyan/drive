PREFIX ?= $(HOME)/.local
BIN := $(PREFIX)/bin/drive

.PHONY: build install check test
build:
	go build -o drive ./cmd/drive

install:
	@mkdir -p "$(PREFIX)/bin"
	@tmp=$$(mktemp "$(BIN).XXXXXX"); trap 'rm -f "$$tmp"' EXIT; \
	go build -o "$$tmp" ./cmd/drive && chmod 755 "$$tmp" && mv -f "$$tmp" "$(BIN)"

test:
	go test -race ./...

check: test
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)"
