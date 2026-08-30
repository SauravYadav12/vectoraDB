# SPDX-License-Identifier: AGPL-3.0-or-later
#
# VectoraDB runs inside the Linux dev VM (ZFS + Docker); day-to-day operation is
# via `lima /tmp/vdb <command>`. This Makefile just builds/checks the CLI.

.PHONY: build vet fmt vm-build test integration web-dev web-build release release-linux wsl-zfs wsl-distro

VERSION ?= 0.1.0
LDFLAGS := -s -w -X github.com/vectoradb/vectoradb/internal/version.Version=$(VERSION)

build:            ## Build the CLI into ./bin/vdb (host)
	go build -o bin/vdb ./cmd/vdb

# The distro image bakes in one binary. Building the other four release
# targets to get it cost several minutes of cross-compilation per run, and
# release.yml already builds and publishes the full set separately.
release-linux: web-build   ## Cross-compile just the Linux engine binary (for the distro image)
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -tags embedui -ldflags "$(LDFLAGS)" -o dist/vdb-linux-amd64 ./cmd/vdb

release: web-build   ## Cross-compile release binaries + the Windows image context into ./dist
	@mkdir -p dist
	@rm -f dist/vdb-* dist/vectoradb-docker-context.tar.gz
	@for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		tags=""; [ "$$os" = "linux" ] && tags="-tags embedui"; \
		echo "  building vdb-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath $$tags -ldflags "$(LDFLAGS)" -o dist/vdb-$$os-$$arch$$ext ./cmd/vdb; \
	done
	@echo "  building vectoradb-docker-context.tar.gz"
	@tar -C docker/postgres -czf dist/vectoradb-docker-context.tar.gz .
	@echo "release binaries in ./dist (version $(VERSION))"
	@echo "note: the Windows installer also needs the ZFS module bundle (see make wsl-zfs / docs/windows-setup.md)"

vet:              ## go vet
	go vet ./...

fmt:              ## list files needing gofmt
	gofmt -l cmd internal

vm-build: web-build   ## Build the Linux binary (UI embedded) inside the Lima VM to /tmp/vdb
	lima bash -c 'cd "$(CURDIR)" && go build -tags embedui -o /tmp/vdb ./cmd/vdb'

test:             ## Run unit tests (host, no VM needed)
	go test ./...

integration:      ## Run the full end-to-end integration test in the Lima VM
	lima bash -c 'cd "$(CURDIR)" && go build -o /tmp/vdb ./cmd/vdb' && lima bash "$(CURDIR)/scripts/integration_test.sh"

web-dev:          ## Run the web UI dev server (http://localhost:5173) against the API
	npm --prefix web install --no-audit --no-fund && npm --prefix web run dev

web-build:        ## Build the web UI to web/dist (same-origin; embedded into the engine binary)
	npm --prefix web ci && VITE_API_URL= npm --prefix web run build

wsl-zfs:          ## Build the OpenZFS modules + userland for the stock WSL2 kernel (Linux builder/CI only)
	deploy/wsl-zfs/build.sh

wsl-distro:       ## Build the prebuilt WSL2 distro image (Linux builder/CI with Docker)
	deploy/wsl-distro/build.sh
