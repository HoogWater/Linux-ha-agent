VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOARCH  ?= $(shell go env GOARCH)
BINARY  := dist/linux-ha-agent-linux-$(GOARCH)
# Package versions must be semver, see packaging/pkg-version.sh.
PKG_VERSION ?= $(shell packaging/pkg-version.sh "$(VERSION)")
# nfpm itself is built for this machine, whatever GOARCH is being packaged.
NFPM := env -u GOARCH -u GOOS go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0

.PHONY: help build packages spk test lint vuln nix clean

help:
	@echo "make build    static binary in dist/ (GOARCH=amd64|arm64, VERSION=...)"
	@echo "make packages .deb and .rpm in dist/ (builds the binary first)"
	@echo "make spk      Synology DSM 7 package in dist/ (see docs/synology.md)"
	@echo "make test     unit and integration tests with the race detector"
	@echo "make lint     all pre-commit hooks, including golangci-lint"
	@echo "make vuln     govulncheck"
	@echo "make nix      nix flake check"

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/linux-ha-agent

packages: build
	install -D -m 0755 $(BINARY) dist/stage/linux-ha-agent
	PKG_ARCH=$(GOARCH) PKG_VERSION=$(PKG_VERSION) $(NFPM) package --config nfpm.yaml --packager deb --target dist/
	PKG_ARCH=$(GOARCH) PKG_VERSION=$(PKG_VERSION) $(NFPM) package --config nfpm.yaml --packager rpm --target dist/
	rm -rf dist/stage

spk: build
	packaging/synology/build-spk.sh $(BINARY) "$(VERSION)" $(GOARCH) dist/linux-ha-agent_$(PKG_VERSION)_dsm7_$(GOARCH).spk

test:
	go test -race -count=1 ./...

lint:
	pre-commit run --all-files

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

nix:
	nix flake check --print-build-logs --no-write-lock-file

clean:
	rm -rf dist e2e.test
