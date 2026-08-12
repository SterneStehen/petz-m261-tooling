PYTHON ?= .venv/bin/python3

.PHONY: generate

# Task 3 acceptance: "make generate peregenerates everything from scratch,
# git diff is empty". build/test/run targets are Task 8's scope, not added
# here to avoid stepping on that task's own PR.
generate:
	$(PYTHON) -m catalog.build_catalog
	$(PYTHON) -m catalog.validate_catalog
	$(PYTHON) -m codegen.gen_go
	$(PYTHON) -m codegen.gen_python
	$(PYTHON) -m codegen.gen_seeds
