.PHONY: build test check install run fmt verify deploy update

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/rst-metrology-mcp ./cmd/rst-metrology-mcp

install:
	go install -ldflags "-X main.version=$(VERSION)" ./cmd/rst-metrology-mcp

test:
	go test ./...

fmt:
	gofmt -w .

check: fmt
	go vet ./...
	go test -race ./...

# Проверить доступ к ФГИС «Аршин», не запуская сервер.
verify:
	go run ./cmd/rst-metrology-mcp --check

# Развернуть на VPS: make deploy DOMAIN=rst-mcp.example.com
deploy:
	./deploy.sh $(DOMAIN)

# Обновить развёрнутый сервер до свежего коммита.
update:
	./deploy.sh update
