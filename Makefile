GOCMD ?= go
BIN_DIR ?= bin
DIST_DIR ?= dist
WEB_DIR := cmd/worms-server/web
GOGIO_VERSION ?= v0.10.0
VERSION ?= $$(git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
SOURCE_DATE_EPOCH ?= $$(git show -s --format=%ct HEAD 2>/dev/null || date +%s)
BUILD_TIME ?= $$(date -u -d "@$(SOURCE_DATE_EPOCH)" +%Y-%m-%dT%H:%M:%SZ)
GOOS ?= linux
GOARCH ?= amd64
BENCHTIME ?= 1s
BENCH_BUDGET ?= 60s
ENGINE_BUDGET_NS ?= 2000000
ENGINE_BUDGET_BYTES ?= 65536
ENGINE_MAX_BUDGET_NS ?= 20000000
ENGINE_MAX_BUDGET_BYTES ?= 524288
MATCH_BUDGET_NS ?= 20000000
STORE_BUDGET_NS ?= 50000000
STORE_BUDGET_BYTES ?= 0
STORE_REPLAY_BUDGET_NS ?= 250000000
DB ?= worms.db
GO_BUILD_FLAGS ?= -trimpath -buildvcs=false
CLIENT_BUILD_TAGS ?= novulkan
GO_LDFLAGS ?=

.PHONY: build build-native build-server build-client build-cli build-agent build-wasm wasm-test \
	test fmt clean run-server smoke browser-smoke bench bench-budget performance \
	metadata package-native checksums release release-acceptance reproducible db-check license-audit \
	check-import-boundaries

GO_BUILD := $(GOCMD) build $(GO_BUILD_FLAGS) $(if $(GO_LDFLAGS),-ldflags "$(GO_LDFLAGS)")
build: build-wasm build-server build-cli build-agent


build-native: build-server build-client build-cli build-agent

build-server: build-wasm
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/worms-server ./cmd/worms-server

build-client:
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) $(if $(CLIENT_BUILD_TAGS),-tags "$(CLIENT_BUILD_TAGS)") -o $(BIN_DIR)/worms-client ./cmd/worms-client

build-cli:
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/wormsctl ./cmd/wormsctl

build-agent:
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/worms-agent ./cmd/worms-agent

build-wasm:
	@mkdir -p $(WEB_DIR)
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(GOCMD) run gioui.org/cmd/gogio@$(GOGIO_VERSION) -target js -o "$$tmp" ./cmd/worms-client && \
	wasm_hash=$$(sha256sum "$$tmp/main.wasm" | cut -c1-12) && \
	js_hash=$$(sha256sum "$$tmp/wasm.js" | cut -c1-12) && \
	wasm="main-$$wasm_hash.wasm" && js="wasm-$$js_hash.js" && \
	sed -e "s#main\\.wasm#$$wasm#g" -e "s#wasm\\.js#$$js#g" -e "s#</head>#<link rel=\"preload\" href=\"$$wasm\" as=\"fetch\" type=\"application/wasm\" crossorigin></head>#g" "$$tmp/index.html" > "$$tmp/index-versioned.html" && \
	sed -e "s#main\\.wasm#$$wasm#g" "$$tmp/wasm.js" > "$$tmp/wasm-versioned.js" && \
	rm -f $(WEB_DIR)/main.wasm $(WEB_DIR)/wasm.js $(WEB_DIR)/main-*.wasm $(WEB_DIR)/wasm-*.js && \
	cp "$$tmp/index-versioned.html" $(WEB_DIR)/index.html && \
	cp "$$tmp/main.wasm" "$(WEB_DIR)/$$wasm" && \
	cp "$$tmp/wasm-versioned.js" "$(WEB_DIR)/$$js"
# Compile pure Go tests for the js/wasm target. The generated test modules stay
# in a temporary directory and are intentionally not executed on the host.
wasm-test:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	for pkg in ./internal/engine ./internal/protocol ./internal/ui; do \
		name="$${pkg##*/}"; \
		GOOS=js GOARCH=wasm $(GOCMD) test -c -o "$$tmp/$$name.test.wasm" "$$pkg"; \
	done

test:
	$(GOCMD) test ./internal/engine ./internal/server

fmt:
	gofmt -w internal cmd

lint:
	golangci-lint run $(if $(CLIENT_BUILD_TAGS),--build-tags "$(CLIENT_BUILD_TAGS)")

run-server: build-server
	$(BIN_DIR)/worms-server
smoke:
	SMOKE_ARTIFACT_DIR="$(SMOKE_ARTIFACT_DIR)" SMOKE_KEEP_TEMP="$(SMOKE_KEEP_TEMP)" ./scripts/smoke.sh

browser-smoke:
	BROWSER_ARTIFACT_DIR="$(BROWSER_ARTIFACT_DIR)" BROWSER_VIEWPORT="$(BROWSER_VIEWPORT)" ./scripts/browser-smoke.sh


bench:
	BENCHTIME=$(BENCHTIME) BENCH_BUDGET=$(BENCH_BUDGET) ENGINE_BUDGET_NS=$(ENGINE_BUDGET_NS) ENGINE_BUDGET_BYTES=$(ENGINE_BUDGET_BYTES) ENGINE_MAX_BUDGET_NS=$(ENGINE_MAX_BUDGET_NS) ENGINE_MAX_BUDGET_BYTES=$(ENGINE_MAX_BUDGET_BYTES) MATCH_BUDGET_NS=$(MATCH_BUDGET_NS) STORE_BUDGET_NS=$(STORE_BUDGET_NS) STORE_BUDGET_BYTES=$(STORE_BUDGET_BYTES) STORE_REPLAY_BUDGET_NS=$(STORE_REPLAY_BUDGET_NS) PERF_ARTIFACT_DIR="$(PERF_ARTIFACT_DIR)" ./scripts/benchmark.sh

bench-budget: bench

performance:
	WASM_ARTIFACT_DIR="$(WASM_ARTIFACT_DIR)" BENCHTIME="$(BENCHTIME)" BENCH_BUDGET="$(BENCH_BUDGET)" ENGINE_BUDGET_NS="$(ENGINE_BUDGET_NS)" ENGINE_BUDGET_BYTES="$(ENGINE_BUDGET_BYTES)" ENGINE_MAX_BUDGET_NS="$(ENGINE_MAX_BUDGET_NS)" ENGINE_MAX_BUDGET_BYTES="$(ENGINE_MAX_BUDGET_BYTES)" MATCH_BUDGET_NS="$(MATCH_BUDGET_NS)" STORE_BUDGET_NS="$(STORE_BUDGET_NS)" STORE_BUDGET_BYTES="$(STORE_BUDGET_BYTES)" STORE_REPLAY_BUDGET_NS="$(STORE_REPLAY_BUDGET_NS)" ./scripts/performance.sh
license-audit:
	LICENSE_ARTIFACT_DIR="$(LICENSE_ARTIFACT_DIR)" ./scripts/license-audit.sh

check-import-boundaries:
	./scripts/check-import-boundaries.sh

metadata: build-wasm
	@mkdir -p $(DIST_DIR)
	@module_hash=$$($(GOCMD) list -m all | sha256sum | cut -d' ' -f1); \
	asset_hash=$$(sha256sum $(WEB_DIR)/index.html $(WEB_DIR)/main-*.wasm $(WEB_DIR)/wasm-*.js | sha256sum | cut -d' ' -f1); \
	asset_files=$$(printf '%s\n' $(WEB_DIR)/index.html $(WEB_DIR)/main-*.wasm $(WEB_DIR)/wasm-*.js | sed 's#.*/##' | python3 -c 'import json,sys; print(json.dumps([x.strip() for x in sys.stdin if x.strip()]))'); \
	printf '{\n  "version": "%s",\n  "commit": "%s",\n  "source_date_epoch": "%s",\n  "built_at": "%s",\n  "go": "%s",\n  "gio": "%s",\n  "api_version": "v1",\n  "schema_version": "v1",\n  "protocol_version": "v1",\n  "service_version": "0.1.0",\n  "target": "%s/%s",\n  "module_hash": "%s",\n  "asset_hash": "%s",\n  "asset_files": %s\n}\n' \
		"$(VERSION)" "$(COMMIT)" "$(SOURCE_DATE_EPOCH)" "$(BUILD_TIME)" "$$($(GOCMD) version)" "$(GOGIO_VERSION)" "$(GOOS)" "$(GOARCH)" "$$module_hash" "$$asset_hash" "$$asset_files" \
		> $(DIST_DIR)/build-metadata.json

package-native: build-wasm
	@mkdir -p $(DIST_DIR)
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	ext=; if [ "$(GOOS)" = windows ]; then ext=.exe; fi; \
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO_BUILD) -o "$$tmp/worms-server$$ext" ./cmd/worms-server && \
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO_BUILD) -o "$$tmp/wormsctl$$ext" ./cmd/wormsctl && \
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO_BUILD) -o "$$tmp/worms-agent$$ext" ./cmd/worms-agent && \
	for binary in worms-server wormsctl worms-agent; do test -s "$$tmp/$$binary$$ext"; done && \
	tar --sort=name --mtime="@$(SOURCE_DATE_EPOCH)" --owner=0 --group=0 --numeric-owner -C "$$tmp" -cf - "worms-server$$ext" "wormsctl$$ext" "worms-agent$$ext" | gzip -n > "$(DIST_DIR)/worms-ng-$(VERSION)-$(GOOS)-$(GOARCH).tar.gz"

checksums: metadata package-native
	@mkdir -p $(DIST_DIR)
	@cd $(DIST_DIR) && { for f in *.tar.gz build-metadata.json; do [ -f "$$f" ] && sha256sum -- "$$f"; done; } > SHA256SUMS

release: metadata package-native checksums
release-acceptance:
	ARTIFACT_DIR="$(ARTIFACT_DIR)" VERSION="$(VERSION)" ./scripts/release-acceptance.sh

reproducible:
	@a="$$(mktemp -d)"; b="$$(mktemp -d)"; trap 'rm -rf "$$a" "$$b"' EXIT; \
	$(MAKE) --no-print-directory package-native metadata DIST_DIR="$$a" VERSION="$(VERSION)" COMMIT="$(COMMIT)" SOURCE_DATE_EPOCH="$(SOURCE_DATE_EPOCH)" GOOS="$(GOOS)" GOARCH="$(GOARCH)" && \
	$(MAKE) --no-print-directory package-native metadata DIST_DIR="$$b" VERSION="$(VERSION)" COMMIT="$(COMMIT)" SOURCE_DATE_EPOCH="$(SOURCE_DATE_EPOCH)" GOOS="$(GOOS)" GOARCH="$(GOARCH)" && \
	cmp "$$a/build-metadata.json" "$$b/build-metadata.json" && \
	cmp "$$a/worms-ng-$(VERSION)-$(GOOS)-$(GOARCH).tar.gz" "$$b/worms-ng-$(VERSION)-$(GOOS)-$(GOARCH).tar.gz" && \
	echo "reproducible: metadata and package match"

db-check:
	@test -f "$(DB)" || { echo "database not found: $(DB)" >&2; exit 2; }
	@if command -v sqlite3 >/dev/null 2>&1; then \
		test "$$(sqlite3 "$(DB)" 'PRAGMA integrity_check;')" = ok && \
		test -z "$$(sqlite3 "$(DB)" 'PRAGMA foreign_key_check;')"; \
	elif command -v python3 >/dev/null 2>&1; then \
		python3 -c 'import sqlite3,sys; db=sqlite3.connect(sys.argv[1]); integrity=db.execute("PRAGMA integrity_check").fetchone()[0]; foreign=db.execute("PRAGMA foreign_key_check").fetchall(); sys.exit(0 if integrity=="ok" and not foreign else 1)' "$(DB)"; \
	else echo "sqlite3 or python3 is required for db-check" >&2; exit 2; fi
	@echo "database checks passed: $(DB)"

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR) $(WEB_DIR)/index.html $(WEB_DIR)/main.wasm $(WEB_DIR)/wasm.js $(WEB_DIR)/main-*.wasm $(WEB_DIR)/wasm-*.js $(WEB_DIR)/worms.wasm $(WEB_DIR)/wasm_exec.js worms.db
