.PHONY: build test vet check clean release
build:
	CGO_ENABLED=0 go build -trimpath -o bin/vault-mcp ./cmd/vault-mcp
test:
	go test -race ./...
vet:
	go vet ./...
check: test vet
clean:
	rm -rf bin dist
# Cross-compile static binaries for every release target into dist/.
release:
	rm -rf dist && mkdir dist
	for os in darwin linux; do for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w" -o dist/vault-mcp-$$os-$$arch ./cmd/vault-mcp || exit 1; \
	done; done
	cd dist && (sha256sum vault-mcp-* 2>/dev/null || shasum -a 256 vault-mcp-*) > SHA256SUMS
