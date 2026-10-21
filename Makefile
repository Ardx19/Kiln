# kiln developer tasks. Run `make help` for the list.

GO ?= go
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.7.0
GO_DIRS := cmd internal spikes tests tools
CI_IMAGE ?= kiln-ci
CI_TARGET ?= lint test compat
PROJECTS ?= ninja fmt fmt-multi cxx-modules

# Runs a make target inside the CI image as the current user, so files it
# writes into the checkout aren't owned by root.
DOCKER_RUN = docker run --rm --user $$(id -u):$$(id -g) \
	-e HOME=/tmp -e GOCACHE=/tmp/go-cache -e GOMODCACHE=/tmp/go-mod \
	-v "$(CURDIR):/src" -w /src $(CI_IMAGE)

.PHONY: help build test lint tools compat compat-short corpus ci-image ci-run

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

ci-run: ci-image
	$(DOCKER_RUN) make $(CI_TARGET)
