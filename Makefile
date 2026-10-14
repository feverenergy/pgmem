# pgmem developer entry points. Containers run with podman by default; set
# CONTAINER=docker to use Docker. ARCH selects the toolchain image's CPU.
# RUN_FLAGS adds flags to the container runs of make fixtures and make wasm,
# and so of the targets that run them, such as check, assets, assets-check
# and repro. With Docker on a Linux host, RUN_FLAGS="--user $(id -u):$(id -g)
# -e HOME=/tmp" makes the files they write in build/out belong to you and not
# to root. make toolchain-check and make test-linux don't use it.
include build/versions.env

CONTAINER ?= podman
ARCH ?= $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
IMAGE = pgmem-toolchain:$(ARCH)
BUILD_ARGS = $(shell sed -e '/^\#/d' -e '/^$$/d' -e 's/^/--build-arg /' build/versions.env)
RUN = $(CONTAINER) run --rm --platform linux/$(ARCH) -v "$(CURDIR)":/repo -w /repo $(RUN_FLAGS) $(IMAGE)

# ENGINE_DIR is the pg17 directory of a pgmem-engine checkout. make assets
# writes the engine files there, and make assets-check compares with it.
ENGINE_DIR ?= ../pgmem-engine/pg17
ENGINE_MOD = github.com/feverenergy/pgmem-engine/pg17

# When ENGINE_DIR holds the engine module, every Go command here builds
# against it instead of the version go.mod requires. Make writes a go.work
# for that, outside the module's tree as Go sees it, and exports GOWORK:
# build/out/go.work for the host, and build/out/gowork-linux/go.work,
# mounted at /gowork, for the test container, which also mounts ENGINE_DIR
# read-only at /engine. Without ENGINE_DIR, or with GOWORK set in the
# environment (GOWORK=off for the published engine), Go resolves the
# engine from the module proxy as any user's build does. A sub-make sees
# the GOWORK this file exported, and keeps it.
HOST_GOWORK = $(CURDIR)/build/out/go.work
ifneq ($(filter undefined,$(origin GOWORK))$(filter $(HOST_GOWORK),$(GOWORK)),)
ifneq ($(wildcard $(ENGINE_DIR)/go.mod),)
ENGINE_ABS := $(abspath $(ENGINE_DIR))
ENGINE_VERSION := $(shell awk '{ for (i = 1; i < NF; i++) if ($$i == "$(ENGINE_MOD)") { print $$(i+1); exit } }' go.mod)
GOWORK_GO := $(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
# gowork prints a go.work that uses the module at $(1) and the engine at $(2).
gowork = printf 'go %s\n\nuse (\n\t%s\n\t%s\n)\n\nreplace %s %s => %s\n' \
	'$(GOWORK_GO)' '$(1)' '$(2)' '$(ENGINE_MOD)' '$(ENGINE_VERSION)' '$(2)'
# writework writes the go.work for $(2) and $(3) to $(1), when it changed.
writework = mkdir -p "$(dir $(1))" && $(call gowork,$(2),$(3)) > "$(1).tmp" && \
	{ cmp -s "$(1).tmp" "$(1)" && rm "$(1).tmp" || mv "$(1).tmp" "$(1)"; }
$(shell $(call writework,$(HOST_GOWORK),$(CURDIR),$(ENGINE_ABS)))
$(shell $(call writework,$(CURDIR)/build/out/gowork-linux/go.work,/repo,/engine))
export GOWORK := $(HOST_GOWORK)
LINUX_ENGINE = -v "$(ENGINE_ABS)":/engine:ro -v "$(CURDIR)/build/out/gowork-linux":/gowork \
	-e GOWORK=/gowork/go.work
endif
endif

.PHONY: toolchain toolchain-check

toolchain:
	$(CONTAINER) build --platform linux/$(ARCH) $(BUILD_ARGS) -t $(IMAGE) -f build/Containerfile build

toolchain-check:
	$(CONTAINER) run --rm --platform linux/$(ARCH) -v "$(CURDIR)":/repo -w /repo -e EXPECTED_ARCH=$(ARCH) $(IMAGE) build/check-toolchain.sh

.PHONY: fixtures test

fixtures:
	$(RUN) build/build-fixtures.sh

test:
	go test ./...

.PHONY: wasm

OUT ?= build/out

wasm:
	$(RUN) build/build-postgres.sh "/repo/$(OUT)"

.PHONY: repro repro-cross test-linux test-linux-amd64 check

# Two from-scratch builds must be byte-identical.
repro:
	$(MAKE) wasm OUT=build/out/repro-a
	$(MAKE) wasm OUT=build/out/repro-b
	diff build/out/repro-a/SHA256SUMS build/out/repro-b/SHA256SUMS
	@echo "reproducible"

# The amd64 toolchain must produce the same bytes as the native one.
# It needs `make toolchain ARCH=amd64` first. It runs emulated, so it is slow.
repro-cross:
	$(MAKE) wasm OUT=build/out/cross-native
	$(MAKE) wasm ARCH=amd64 OUT=build/out/cross-amd64
	diff build/out/cross-native/SHA256SUMS build/out/cross-amd64/SHA256SUMS
	@echo "identical across CPUs"

# TIME_SCALE multiplies the tests' timing bounds (PGMEM_TIME_SCALE). It is
# 1 on the native CPU and 10 under amd64 emulation.
TIME_SCALE ?= 1

# The tests run against the local build in $(OUT), which needs make wasm
# cluster first. PGMEM_BUILD_DIR, an absolute path, points every package at
# it. TestDefaultEngineMatchesBuild fails if the default engine's files
# differ from it (run make assets). TestStartFromDefaultEngine still runs
# the engine. The container uses ENGINE_DIR as the host does (see GOWORK
# above).
test-linux:
	$(CONTAINER) run --rm --platform linux/$(ARCH) -v "$(CURDIR)":/repo -w /repo \
		-v pgmem-gomod-$(ARCH):/root/go/pkg/mod $(LINUX_ENGINE) -e PGMEM_REQUIRE_BUILD=1 \
		-e PGMEM_BUILD_DIR="/repo/$(OUT)" -e PGMEM_TIME_SCALE=$(TIME_SCALE) \
		$(IMAGE) go test -timeout 45m ./...

test-linux-amd64:
	$(MAKE) test-linux ARCH=amd64 TIME_SCALE=10

check: fixtures
	$(MAKE) wasm
	$(MAKE) cluster
	git diff --exit-code -- '*.wasm'
	PGMEM_REQUIRE_BUILD=1 PGMEM_BUILD_DIR="$(abspath $(OUT))" go test ./...
	$(MAKE) test-linux

.PHONY: cluster

# A cluster image made by initdb inside pgmem. It needs make wasm first.
cluster:
	go run ./internal/cmd/mkcluster -wasm "$(OUT)" -o "$(OUT)/cluster.tar"

.PHONY: regress

# PostgreSQL's regression schedule against pgmem, compared with
# postgres/regress-allowlist. It needs make wasm cluster first, and builds
# native psql and pg_regress into $(OUT)/native-<goos>-<goarch>/ when they
# are missing.
regress:
	build/build-native-regress.sh "$(OUT)"
	build/regress.sh "$(OUT)"

.PHONY: assets assets-check assets-compare

# An engine holds postgres.wasm, share.tar and cluster.tar, compressed by
# internal/cmd/mkassets into ENGINE_DIR. mkassets also writes SHA256SUMS,
# INPUTS.SHA256SUMS and a PROVENANCE made from build/versions.env, the
# patches and a sha256 of the build sources. Commit those files in the engine repo. ENGINE_DIR must already
# hold the engine module's go.mod, so a mistyped path fails here instead of
# filling a new directory.
ASSETS_CHECK = build/out/assets-check

# need_engine fails, before anything is built, unless ENGINE_DIR holds the
# engine module, and with $(1) set, its committed engine files too.
need_engine = @test -f "$(ENGINE_DIR)/go.mod" || { echo "$@: $(ENGINE_DIR) has no go.mod; set ENGINE_DIR to the pg17 directory of a pgmem-engine checkout" >&2; exit 1; } \
	$(if $(1),; test -f "$(ENGINE_DIR)/SHA256SUMS" || { echo "$@: no engine files in $(ENGINE_DIR) (run make assets, or set ENGINE_DIR)" >&2; exit 1; })

assets:
	$(call need_engine)
	$(MAKE) wasm
	$(MAKE) cluster
	go run ./internal/cmd/mkassets -in "$(OUT)" -o "$(ENGINE_DIR)"

# Rebuilds the engine files from the working tree's sources into a fresh
# $(ASSETS_CHECK)/assets and compares them with $(ENGINE_DIR) as it is on
# disk, not with the engine repo's HEAD. If a file differs, it fails and
# names the files. It also says whether the contents differ (run make
# assets) or only their compression does. Only the compression differs
# when the zstd encoder changed, for example after a klauspost/compress
# update, and make assets re-encodes the files.
assets-check:
	$(call need_engine,files)
	rm -rf "$(ASSETS_CHECK)"
	$(MAKE) wasm OUT=$(ASSETS_CHECK)
	$(MAKE) cluster OUT=$(ASSETS_CHECK)
	go run ./internal/cmd/mkassets -in "$(ASSETS_CHECK)" -o "$(ASSETS_CHECK)/assets"
	$(MAKE) assets-compare

# The comparison step of assets-check, against the files an earlier
# assets-check built. It rebuilds nothing.
assets-compare:
	$(call need_engine,files)
	@test -f "$(ASSETS_CHECK)/assets/INPUTS.SHA256SUMS" || { echo "assets-compare: no rebuilt files in $(ASSETS_CHECK)/assets (run make assets-check)" >&2; exit 1; }
	@stale=; for f in cluster.tar postgres.wasm share.tar; do \
		line=$$(grep "  $$f$$" "$(ASSETS_CHECK)/assets/INPUTS.SHA256SUMS") && \
		grep -qxF "$$line" "$(ENGINE_DIR)/INPUTS.SHA256SUMS" 2>/dev/null || stale="$$stale $$f"; \
	done; \
	if [ -n "$$stale" ]; then echo "assets-check: contents differ from a rebuild:$$stale (run make assets)" >&2; exit 1; fi
	@stale=; for f in SHA256SUMS cluster.tar.zst postgres.wasm.zst share.tar.zst; do \
		cmp -s "$(ENGINE_DIR)/$$f" "$(ASSETS_CHECK)/assets/$$f" || stale="$$stale $$f"; \
	done; \
	if [ -n "$$stale" ]; then echo "assets-check: contents match a rebuild, but the compressed files differ:$$stale (the zstd encoder changed; run make assets to re-encode)" >&2; exit 1; fi
	@stale=; for f in INPUTS.SHA256SUMS PROVENANCE; do \
		cmp -s "$(ENGINE_DIR)/$$f" "$(ASSETS_CHECK)/assets/$$f" || stale="$$stale $$f"; \
	done; \
	if [ -n "$$stale" ]; then echo "assets-check: the archives match a rebuild, but these files differ from what make assets writes:$$stale (run make assets)" >&2; exit 1; fi
	@echo "assets match their sources in $(ENGINE_DIR)"
