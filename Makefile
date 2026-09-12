# RiskGate developer entry points. CI runs the same commands (.github/workflows/ci.yml).

GO       ?= go
# The project venv if it exists, so `make train` works without activating it.
PYTHON   ?= $(if $(wildcard .venv/bin/python),.venv/bin/python,python3)
FUZZTIME ?= 20s
BIN      ?= bin

# Arguments for the pipeline steps, e.g. `make export EXPORT_ARGS="-data data/ieee"`.
SYNTH_ARGS  ?=
EXPORT_ARGS ?=
TRAIN_ARGS  ?=
TABLE_ARGS  ?=

.PHONY: build test race vet lint fuzz cover synth export table train bench

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

cover: ## tests with a coverage report, without -race (atomic counters under -race made this ~7 min)
	$(GO) test -covermode=set -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

synth: ## synthetic fixture data for development without the Kaggle download
	$(GO) run ./cmd/synth $(SYNTH_ARGS)

export: ## offline feature export (the same feature code the service runs)
	$(GO) run ./cmd/export $(EXPORT_ARGS)

table: ## backtest feature table for the service and the backtester, e.g. TABLE_ARGS="-synthetic -model models/synthetic"
	$(GO) run ./cmd/riskgate table $(TABLE_ARGS)

train: ## train the model on the Go export (offline, Python)
	$(PYTHON) python/train.py $(TRAIN_ARGS)

bench: ## Go benchmarks
	$(GO) test -run='^$$' -bench=. -benchmem ./...
