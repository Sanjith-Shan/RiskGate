# RiskGate developer entry points. CI runs the same commands (.github/workflows/ci.yml).

GO       ?= go
PYTHON   ?= python3
FUZZTIME ?= 20s
BIN      ?= bin

# Arguments for the pipeline steps, e.g. `make export EXPORT_ARGS="-data data/ieee"`.
SYNTH_ARGS  ?=
EXPORT_ARGS ?=
TRAIN_ARGS  ?=

.PHONY: build test race vet lint fuzz cover synth export train bench

build: ## compile every package and binary into ./bin
	$(GO) build ./...
	$(GO) build -o $(BIN)/ ./cmd/...

test: ## unit tests
	$(GO) test ./...

race: ## unit tests under the race detector (what CI runs)
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint: vet ## go vet + staticcheck (a tool, not a go.mod dependency)
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

fuzz: ## every Fuzz target for FUZZTIME each
	scripts/fuzz_all.sh $(FUZZTIME)

cover: ## race tests with a coverage report
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

synth: ## synthetic fixture data for development without the Kaggle download
	$(GO) run ./cmd/synth $(SYNTH_ARGS)

export: ## offline feature export (the same feature code the service runs)
	$(GO) run ./cmd/export $(EXPORT_ARGS)

train: ## train the model on the Go export (offline, Python)
	$(PYTHON) python/train.py $(TRAIN_ARGS)

bench: ## Go benchmarks
	$(GO) test -run='^$$' -bench=. -benchmem ./...
