SHELL := /bin/sh

VERSION ?= $(shell sed -n 's/^const DefaultVersion = "\(.*\)"/\1/p' internal/core/version/version.go)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -buildid= \
	-X github.com/ayeaaaa/mix2api-v2/internal/core/version.Version=$(VERSION) \
	-X github.com/ayeaaaa/mix2api-v2/internal/core/version.Commit=$(COMMIT) \
	-X github.com/ayeaaaa/mix2api-v2/internal/core/version.Date=$(BUILD_DATE)

.PHONY: all build version test race vet fmt fmt-check check dist dist-plan clean docker-build

all: check build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=exe \
		-ldflags "$(LDFLAGS)" -o bin/mix2api ./cmd/mix2api

version: build
	./bin/mix2api version

test:
	CGO_ENABLED=0 go test ./...

race:
	CGO_ENABLED=0 go test -race ./...

vet:
	CGO_ENABLED=0 go vet ./...

fmt:
	gofmt -w cmd internal tests tools

fmt-check:
	@files="$$(gofmt -l cmd internal tests tools)"; \
	if [ -n "$$files" ]; then echo "gofmt needed:"; echo "$$files"; exit 1; fi

check: fmt-check vet test

# Full release matrix: Linux amd64/arm64/armv7, Windows amd64, macOS
# amd64/arm64, OpenWrt, and portable assets. Deterministic metadata comes from
# VERSION/COMMIT/BUILD_DATE; pass SOURCE_DATE_EPOCH for archive timestamps.
dist:
	VERSION="$(VERSION)" COMMIT="$(COMMIT)" BUILD_DATE="$(BUILD_DATE)" \
		./scripts/build-release.sh $(ARGS)

dist-plan:
	VERSION="$(VERSION)" COMMIT="$(COMMIT)" BUILD_DATE="$(BUILD_DATE)" \
		./scripts/build-release.sh --dry-run

docker-build:
	docker build --build-arg VERSION="$(VERSION)" --build-arg COMMIT="$(COMMIT)" \
		--build-arg BUILD_DATE="$(BUILD_DATE)" -t mix2api:$(VERSION) .

clean:
	rm -rf bin dist
