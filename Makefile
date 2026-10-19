GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILDTIME?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILDTIME)

GOLANGCI_VERSION := v2.15.0
GOLANGCI_NUM     := $(patsubst v%,%,$(GOLANGCI_VERSION))
GOLANGCI_TARBALL := golangci-lint-$(GOLANGCI_NUM)-linux-amd64.tar.gz
GOLANGCI_BASE    := https://github.com/golangci/golangci-lint/releases/download/$(GOLANGCI_VERSION)
GOVULN_VERSION   := v1.9.0
GOSEC_VERSION    := v2.29.0
GOBIN            ?= $(HOME)/.local/bin

.PHONY: all tools fmt vet lint tidy test fuzz vuln sec build repro gates check clean

all: check

## tools installs the pinned helpers into $(GOBIN).
## golangci-lint comes from its pinned release tarball, verified against the
## release checksums file. Never through install.sh: that script carries its own
## checksum table and breaks when the pin and the script disagree.
tools:
	GOBIN=$(GOBIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULN_VERSION)
	GOBIN=$(GOBIN) $(GO) install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)
	@set -e; mkdir -p $(GOBIN); tmp=$$(mktemp -d); \
	echo "fetching golangci-lint $(GOLANGCI_VERSION)"; \
	curl -sSfL -o $$tmp/$(GOLANGCI_TARBALL) $(GOLANGCI_BASE)/$(GOLANGCI_TARBALL); \
	curl -sSfL -o $$tmp/checksums.txt $(GOLANGCI_BASE)/golangci-lint-$(GOLANGCI_NUM)-checksums.txt; \
	( cd $$tmp && awk -v f="$(GOLANGCI_TARBALL)" '$$2==f' checksums.txt > one.sums && test -s one.sums && sha256sum -c one.sums ); \
	tar -xzf $$tmp/$(GOLANGCI_TARBALL) -C $$tmp; \
	install -m755 $$tmp/golangci-lint-$(GOLANGCI_NUM)-linux-amd64/golangci-lint $(GOBIN)/golangci-lint; \
	rm -rf $$tmp; \
	$(GOBIN)/golangci-lint --version

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

tidy:
	@cp go.mod go.mod.bak; @cp go.sum go.sum.bak 2>/dev/null || true
	$(GO) mod tidy
	@diff -q go.mod go.mod.bak >/dev/null || { echo "go.mod changed by tidy"; rm -f go.mod.bak go.sum.bak; exit 1; }
	@if [ -f go.sum ] && [ -f go.sum.bak ]; then diff -q go.sum go.sum.bak >/dev/null || { echo "go.sum changed by tidy"; rm -f go.mod.bak go.sum.bak; exit 1; }; fi
	@rm -f go.mod.bak go.sum.bak

test:
	$(GO) test -race ./...

## fuzz runs each target for a short bounded window; seed-only runs do not count.
fuzz:
	@found=0; \
	for pkg in $$($(GO) list ./...); do \
	  for target in $$(grep -rhoE '^func (Fuzz[A-Za-z0-9_]+)' $$($(GO) list -f '{{.Dir}}' $$pkg) 2>/dev/null | awk '{print $$2}'); do \
	    found=1; \
	    echo "fuzzing $$target in $$pkg"; \
	    $(GO) test -run '^$$' -fuzz "^$$target$$" -fuzztime=30s $$pkg || exit 1; \
	  done; \
	done; \
	if [ $$found -eq 0 ]; then echo "no fuzz targets present"; fi

vuln:
	govulncheck ./...

sec:
	gosec -quiet ./...

build:
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=true -ldflags "$(LDFLAGS)" -o dist/mgw ./cmd/mgw

## repro builds twice and requires identical bytes.
repro:
	@mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=true -ldflags "$(LDFLAGS)" -o dist/mgw.a ./cmd/mgw
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=true -ldflags "$(LDFLAGS)" -o dist/mgw.b ./cmd/mgw
	@a=$$(sha256sum dist/mgw.a | cut -d' ' -f1); b=$$(sha256sum dist/mgw.b | cut -d' ' -f1); \
	if [ "$$a" != "$$b" ]; then echo "NOT REPRODUCIBLE: $$a != $$b"; exit 1; fi; \
	echo "reproducible: $$a"; rm -f dist/mgw.a dist/mgw.b

## gates requires every gate marked implemented in docs/gates.md to have a test.
gates:
	@miss=0; \
	for n in $$(awk -F'|' '/^\| *G[0-9]+/ && $$4 ~ /implemented/ {gsub(/ /,"",$$2); gsub(/G/,"",$$2); print $$2}' docs/gates.md); do \
	  if ! grep -rqE "func TestGate0*$$n" --include='*_test.go' .; then echo "MISSING test for gate $$n"; miss=1; fi; \
	done; \
	if [ $$miss -ne 0 ]; then exit 1; fi; \
	echo "gate mapping ok"

check: fmt vet lint tidy test fuzz vuln sec build repro gates
	@echo "make check: green"

clean:
	rm -rf dist
