# kiln developer tasks. Run `make help` for the list.

GO ?= go
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.7.0
GO_DIRS := cmd internal spikes tests tools
CI_IMAGE ?= kiln-ci
CI_TARGET ?= lint test compat
PROJECTS ?= ninja fmt fmt-multi cxx-modules

# Runs a make target inside the CI image as the current user, so files it
# writes into the checkout aren't owned by root. Build trees made in the
# image hold /src/... paths, so they go to .work/ci, apart from the host's.
DOCKER_RUN = docker run --rm --user $$(id -u):$$(id -g) \
	-e HOME=/tmp -e GOCACHE=/tmp/go-cache -e GOMODCACHE=/tmp/go-mod \
	-e KILN_WORK=/src/.work/ci -e KILN_CORPUS=/src/.work/ci/corpus \
	-v "$(CURDIR):/src" -w /src $(CI_IMAGE)

.PHONY: help build test lint tools compat compat-short corpus ci-image ci-run mem-container root-install-test dist

help:
	@echo "build          build bin/kiln"
	@echo "test           unit tests with the race detector"
	@echo "lint           gofmt, go vet and staticcheck"
	@echo "tools          install the pinned Ninja/CMake/Meson matrix into .tools"
	@echo "compat         differential suite against every pinned Ninja"
	@echo "compat-short   same, newest Ninja only"
	@echo "corpus         fetch, configure and build PROJECTS (default: $(PROJECTS))"
	@echo "ci-image       build the CI container image"
	@echo "ci-run         run CI_TARGET (default: $(CI_TARGET)) inside the CI image"

build:
	$(GO) build -o bin/kiln ./cmd/kiln

test:
	$(GO) test -race ./...

lint:
	@unformatted=$$(gofmt -l $(GO_DIRS)); \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags compat ./tests/compat
	$(GO) run $(STATICCHECK) ./...
	$(GO) run $(STATICCHECK) -tags compat ./tests/compat

# The CI image already carries the tools and sets KILN_TOOLS.
tools:
	@if [ -z "$$KILN_TOOLS" ]; then scripts/tools.sh; fi

compat: tools
	$(GO) test -tags compat -count=1 -timeout 60m -parallel 4 ./tests/compat

compat-short: tools
	$(GO) test -tags compat -short -count=1 -timeout 30m ./tests/compat

corpus: tools
	corpus/corpus.sh all $(PROJECTS)

ci-image:
	docker build -f ci/Dockerfile -t $(CI_IMAGE) .

# Static Linux binaries for amd64 and arm64 in dist/, with checksums.
# usage: make dist VERSION=v0.1.0
VERSION ?= dev
dist:
	rm -rf dist && mkdir -p dist
	for arch in amd64 arm64; do \
		d=dist/kiln-$(VERSION)-linux-$$arch; mkdir -p $$d; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $$d/kiln ./cmd/kiln; \
		cp LICENSE NOTICE README.md $$d/; \
		tar -C dist -czf dist/kiln-$(VERSION)-linux-$$arch.tar.gz kiln-$(VERSION)-linux-$$arch; \
		rm -rf $$d; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS

# Eight 400 MiB jobs in a 2 GiB container: kiln finishes, ninja -j8 is OOM-killed.
mem-container: ci-image
	docker run --rm --user $$(id -u):$$(id -g) --memory=2g --memory-swap=2g \
		-e HOME=/tmp -e GOCACHE=/tmp/go-cache -e GOMODCACHE=/tmp/go-mod \
		-e KILN_WORK=/src/.work/ci -e KILN_MEM_CONTAINER=1 \
		-v "$(CURDIR):/src" -w /src $(CI_IMAGE) \
		go test -tags compat -count=1 -run TestMemContainer -v ./tests/compat

# Stage an install as a normal user and commit it as root, in a container.
root-install-test: ci-image
	docker run --rm --user 0 -e HOME=/tmp -e GOCACHE=/tmp/go-cache -e GOMODCACHE=/tmp/go-mod \
		-e KILN_WORK=/src/.work/ci-root -e KILN_ROOT_TEST=1 \
		-v "$(CURDIR):/src" -w /src $(CI_IMAGE) \
		go test -tags compat -count=1 -run TestInstallRootCommit -v ./tests/compat

ci-run: ci-image
	$(DOCKER_RUN) make $(CI_TARGET)
