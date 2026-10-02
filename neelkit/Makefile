# NeelKIT

SHELL := /bin/bash
BIN   := ./bin
MODELS := ./models
CORPUS ?= ./testdata/speech

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/neeldas0032/NeelKIT-AI-voice-assistant
LDFLAGS := -s -w \
  -X $(PKG)/internal/version.Version=$(VERSION) \
  -X $(PKG)/internal/version.Commit=$(COMMIT) \
  -X $(PKG)/internal/version.BuildDate=$(DATE)

# FTS5 is a build tag, not a default, in go-sqlite3. Forgetting it makes
# transcript search fail at runtime with "no such module", so every Go target
# here carries it rather than relying on anyone remembering.
TAGS := sqlite_fts5

# ONNX Runtime is a C library. Point CGO at it wherever it landed.
ORT ?= $(firstword $(wildcard /opt/onnxruntime /usr/local/onnxruntime))
ifneq ($(ORT),)
export CGO_CFLAGS  := -I$(ORT)/include
export CGO_LDFLAGS := -L$(ORT)/lib
export LD_LIBRARY_PATH := $(ORT)/lib
export ONNXRUNTIME_LIB := $(ORT)/lib/libonnxruntime.so
export NEELKIT_ORT_LIBRARY := $(ORT)/lib/libonnxruntime.so
endif

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ----------------------------------------------------------------- build

.PHONY: build
build: ## Build neelkitd, nk and nkbench
	@mkdir -p $(BIN)
	go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/neelkitd ./cmd/neelkitd
	go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/nk ./cmd/nk
	go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/nkbench ./cmd/nkbench
	@echo "built $(BIN)/neelkitd, $(BIN)/nk and $(BIN)/nkbench ($(VERSION))"

.PHONY: run
run: build ## Run the server
	$(BIN)/neelkitd

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) web/.next web/out

# ------------------------------------------------------------------ test

.PHONY: test
test: ## Run every Go test
	go test -tags $(TAGS) -count=1 ./...

.PHONY: test-race
test-race: ## Run every Go test under the race detector
	go test -tags $(TAGS) -race -count=1 ./...

.PHONY: test-speech
test-speech: ## Run the tests that need a real speech recording
	@test -n "$(FIXTURE)" || { echo "usage: make test-speech FIXTURE=/path/to/speech.wav"; exit 1; }
	NEELKIT_SPEECH_FIXTURE=$(FIXTURE) go test -tags $(TAGS) -count=1 -v ./internal/vad/

.PHONY: cover
cover: ## Report coverage for the pure packages
	go test -tags $(TAGS) -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: bench
bench: ## Run the Go microbenchmarks
	go test -tags $(TAGS) -run XXX -bench . -benchmem ./internal/audio/ ./internal/normalize/ ./internal/intent/...

.PHONY: measure
measure: build ## Measure this machine (see docs/performance.md)
	@test -d $(CORPUS) || { \
	  echo "No corpus at $(CORPUS)."; \
	  echo "Record 16 kHz mono WAVs of the commands you actually say, or run:"; \
	  echo "  make measure CORPUS=/path/to/wavs"; \
	  echo; \
	  echo "Running the two benchmarks that need no audio:"; \
	  $(BIN)/nkbench route; $(BIN)/nkbench normalize; exit 0; }
	$(BIN)/nkbench all -models $(MODELS) -corpus $(CORPUS) -json bench.json

.PHONY: e2e
e2e: ## Run the Playwright suite against a running stack
	@test -n "$(NEELKIT_API_KEY)" || { echo "set NEELKIT_API_KEY to the key neelkitd printed"; exit 1; }
	@test -n "$(NEELKIT_WORKSPACE_ROOT)" || { echo "set NEELKIT_WORKSPACE_ROOT to the sandbox root"; exit 1; }
	cd web && npx playwright test

# ------------------------------------------------------------------ lint

.PHONY: fmt
fmt: ## Format Go and web sources
	gofmt -w $(shell git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './web/*')

.PHONY: vet
vet: ## Run go vet
	go vet -tags $(TAGS) ./...

.PHONY: check
check: fmt vet test ## Format, vet and test

# ---------------------------------------------------------------- models

.PHONY: models
models: ## Download the speech models (about 700 MB, once)
	@bash scripts/fetch-models.sh $(MODELS)

.PHONY: testmodel
testmodel: ## Generate the synthetic model used by the engine tests
	python3 scripts/gen_testmodel.py testdata/synthetic-model

# ------------------------------------------------------------------- web

.PHONY: web-install
web-install: ## Install web dependencies
	cd web && npm install

.PHONY: web-dev
web-dev: ## Run the web interface in development mode
	cd web && npm run dev

.PHONY: web-build
web-build: ## Build the web interface
	cd web && npm run build

.PHONY: web-check
web-check: ## Type-check the web interface
	cd web && npx tsc --noEmit

# ---------------------------------------------------------------- docker

.PHONY: up
up: ## Start the whole stack
	docker compose up --build

.PHONY: up-llm
up-llm: ## Start the stack including the local language model
	docker compose --profile llm up --build

.PHONY: down
down: ## Stop the stack
	docker compose down

.PHONY: logs
logs: ## Follow the server logs
	docker compose logs -f neelkitd
