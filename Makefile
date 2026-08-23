.PHONY: build test race check clean

BINARY := bin/agent-fs

build:
	go build -trimpath -o $(BINARY) ./cmd/agent-fs

test:
	go test ./...

race:
	go test -race ./...

check:
	test -z "$$(gofmt -l .)"
	bash -n scripts/install.sh scripts/smoke-mcp.sh scripts/test-install.sh
	go vet ./...
	go build ./...
	go test -race ./...

clean:
	go clean -testcache
