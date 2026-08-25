PYTHON ?= .venv/bin/python3
GO ?= go

BIN_DIR := simulator/bin
BINARY := $(BIN_DIR)/m261sim

.PHONY: generate webui build test run validate lint clean

# Task 3 acceptance: "make generate regenerates everything from scratch,
# git diff is empty" (see `validate`, below, for the automated check).
# Needs the private manufacturer register maps (m261-registermap/, not
# included in this repository) — see README.md.
generate:
	$(PYTHON) -m catalog.build_catalog
	$(PYTHON) -m catalog.validate_catalog
	$(PYTHON) -m codegen.gen_go
	$(PYTHON) -m codegen.gen_python
	$(PYTHON) -m codegen.gen_seeds
	$(PYTHON) -m codegen.gen_docs

# Task 8 item 2.
webui:
	cd webui && npm ci && npm run build

build:
	$(GO) build -o $(BINARY) ./simulator/cmd/m261sim

# go vet + go test -race, then the Python suite. Most of the Python suite
# needs the private register maps and skips itself cleanly when they're
# absent (see tests/conftest.py); the Go half never does — gen/go/
# m261points is committed, so the simulator builds and its own tests run
# without ever touching catalog/ or the register maps.
test:
	$(GO) vet ./...
	$(GO) test ./... -race
	$(PYTHON) -m pytest tests/ -q

run: build
	./$(BINARY)

# Task 3's own acceptance criterion, automated: regenerate from scratch
# and fail if the result differs from what's actually committed. Scoped
# to gen/ — the only thing `generate` writes that git actually tracks.
# catalog/point_catalog.json, catalog/validation_report.md,
# docs/point-reference.md, and docs/open-questions.md are all private/
# gitignored (Task 8 review: the latter two are just as much a derived
# view of the private register map as the first two, so they stay local-
# only artifacts, not committed output) — there is nothing for git to
# compare there, by design; see README.md for why most CI environments
# can't run this target at all without the register maps.
validate: generate webui
	@git diff --exit-code -- gen/ webui/dist/ || \
		(echo "make generate or webui produced a diff against committed output" && exit 1)
	@if [ -n "$$(git status --porcelain -- gen/ webui/dist/)" ]; then \
		echo "make generate or webui produced untracked/deleted output"; \
		git status --porcelain -- gen/ webui/dist/; \
		exit 1; \
	fi
	@echo "generated catalog and web UI: no diff"

# gofmt is scoped to tracked *.go files (git ls-files), not a bare
# "gofmt -l ." over the whole working tree — a plain "." walk also
# reaches any local, un-added .go scratch file or (in principle) build
# output, whose formatting has no bearing on what's actually committed;
# scoping to git ls-files checks exactly what CI/reviewers actually see.
lint:
	$(GO) vet ./...
	@files="$$(git ls-files '*.go')"; \
	out="$$(gofmt -l $$files)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt: the following files need formatting:"; \
		echo "$$out"; \
		exit 1; \
	fi
	$(PYTHON) -m mypy catalog codegen

clean:
	rm -rf $(BIN_DIR)
