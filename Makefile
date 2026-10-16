.PHONY: build test vet check clean
build:
	CGO_ENABLED=0 go build -trimpath -o bin/vault-mcp ./cmd/vault-mcp
test:
	go test -race ./...
vet:
	go vet ./...
check: test vet
clean:
	rm -rf bin
