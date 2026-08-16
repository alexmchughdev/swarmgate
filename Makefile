GO      ?= go
DIST    := dist
LDFLAGS := -s -w

# VERSION is embedded into both binaries via -X main.version; falls back to
# "dev" outside a git checkout (e.g. a source tarball) rather than failing.
VERSION          := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RELEASE_LDFLAGS  := -s -w -X main.version=$(VERSION)
RELEASE_PLATFORMS := linux/amd64 linux/arm64

.PHONY: build test check fmt-check vet clean check-analysis test-integration release

build:
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o $(DIST)/swarmgate ./cmd/swarmgate
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o $(DIST)/harness ./cmd/harness

test:
	$(GO) test ./...

check: fmt-check vet test

fmt-check:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then \
		echo "gofmt required for:"; echo "$$files"; exit 1; \
	fi

vet:
	$(GO) vet ./...

clean:
	rm -rf $(DIST)

# check-analysis verifies analysis/stats.py against its checked-in golden
# output. It is separate from `check` so Go's check stays Go-only and fast;
# it assumes pandas is already installed (see analysis/README.md), the same
# way `check` assumes Go is already on PATH.
check-analysis:
	python3 analysis/check_golden.py

# test-integration runs the internal/gate cosign checks against a real
# registry (see pipeline/README.md for the fixture images it expects).
# Separate from `check`: it needs network and a signed image set that
# `pipeline/build.sh --registry $(REGISTRY)` must have already produced.
test-integration:
	SWARMGATE_TEST_REGISTRY=$(REGISTRY) $(GO) test -tags integration ./internal/gate/... -v

# release builds static (CGO_ENABLED=0) swarmgate+harness binaries for each
# platform in RELEASE_PLATFORMS and packages each pair into its own
# dist/swarmgate-$(VERSION)-$(os)-$(arch).tar.gz. Static linking is what
# makes the Alpine CI job below meaningful: a binary with no libc
# dependency runs on any base image, glibc or musl alike.
release:
	@mkdir -p $(DIST)
	@for platform in $(RELEASE_PLATFORMS); do \
		os=$$(echo $$platform | cut -d/ -f1); \
		arch=$$(echo $$platform | cut -d/ -f2); \
		pkg=swarmgate-$(VERSION)-$$os-$$arch; \
		outdir=$(DIST)/$$pkg; \
		echo "release: building $$platform"; \
		mkdir -p $$outdir; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -buildvcs=false -ldflags '$(RELEASE_LDFLAGS)' -o $$outdir/swarmgate ./cmd/swarmgate; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -buildvcs=false -ldflags '$(RELEASE_LDFLAGS)' -o $$outdir/harness ./cmd/harness; \
		tar -C $(DIST) -czf $(DIST)/$$pkg.tar.gz $$pkg; \
		rm -rf $$outdir; \
	done
